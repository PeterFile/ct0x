package store

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"agentworkspace/internal/domain"
)

func TestMigrationsV42V43ThreadRoutingAndCorrectionOrder(t *testing.T) {
	routingIdx := slices.Index(migrations, threadRoutingSchema)
	correctionIdx := slices.Index(migrations, threadCorrectionSchema)
	if len(migrations) < 43 || routingIdx != 41 || correctionIdx != 42 {
		t.Fatalf("expected threadRoutingSchema=v42 (idx 41) & threadCorrectionSchema=v43 (idx 42), got routing=%d correction=%d total=%d", routingIdx, correctionIdx, len(migrations))
	}
}

func TestMigrationsV42V43UpgradeReplayAndFailClosed(t *testing.T) {
	prior := migrations
	t.Cleanup(func() { migrations = prior })

	// 1. Pre-v42 (v41): legacy posts work; related_task_id and one-sentence correction fail closed.
	migrations = prior[:41]
	dbPath := filepath.Join(t.TempDir(), "v42-v43-upgrade.sqlite")
	s41, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	group, err := s41.CreateLocalUserGroup(ctx, CreateGroupInput{Name: "v41旧入口群", ActorID: "user", IdempotencyKey: "v42-v43-grp"})
	if err != nil {
		t.Fatal(err)
	}
	owner := member(t, s41, group.ID)
	openReq := MessageRequest{
		GroupID: group.ID, ActorID: "user",
		Content: "新话题：PAY-431 下载超时", IdempotencyKey: "v42-v43-legacy-open",
	}
	legacyOpen, err := s41.PostLocalUserMessage(ctx, openReq)
	if err != nil || legacyOpen.TopicID != "" || len(legacyOpen.Deliveries) != 1 || legacyOpen.Deliveries[0].RecipientID != owner.ID {
		t.Fatalf("v41 legacy message failed: %+v, %v", legacyOpen, err)
	}
	if _, err := s41.PostLocalUserMessage(ctx, MessageRequest{
		GroupID: group.ID, ActorID: "user", Content: "关联缺失", ThreadLinks: domain.ThreadLinks{RelatedTaskID: "missing-task"},
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("v41 unexpectedly allowed related_task_id: %v", err)
	}
	if _, err := s41.PostLocalUserMessage(ctx, MessageRequest{
		GroupID: group.ID, ActorID: "user", Content: "这句话指的是超时",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("v41 unexpectedly allowed thread correction: %v", err)
	}
	if err := s41.Close(); err != nil {
		t.Fatal(err)
	}

	// 2. Upgraded v43: legacy records replay intact, new routing & one-sentence correction work & replay identically.
	migrations = prior
	s43, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s43.Close() })
	messageSequenceLedger(t, s43, len(migrations))
	replayedOpen, err := s43.PostLocalUserMessage(ctx, openReq)
	if err != nil || replayedOpen.ID != legacyOpen.ID {
		t.Fatalf("v41 message replay after v43 upgrade drifted: %+v vs %+v, %v", replayedOpen, legacyOpen, err)
	}
	for _, table := range []string{"thread_routing_topics", "thread_corrections"} {
		if got := messageSequenceTableRows(t, s43, table); len(got) != 0 {
			t.Fatalf("v43 upgrade promoted rows into %s: %v", table, got)
		}
	}
	topicMsg, err := s43.PostLocalUserMessage(ctx, MessageRequest{
		GroupID: group.ID, ActorID: "user", Content: "新话题：NET-99 告警", RecipientIDs: []string{}, IdempotencyKey: "v42-open",
	})
	if err != nil || topicMsg.TopicID == "" {
		t.Fatalf("v42 routing failed after upgrade: %+v, %v", topicMsg, err)
	}
	unrouted, err := s43.PostLocalUserMessage(ctx, MessageRequest{
		GroupID: group.ID, ActorID: "user", Content: "补充仅升级一次。", IdempotencyKey: "v43-unrouted",
	})
	if err != nil || unrouted.TopicID != "" {
		t.Fatalf("expected main-topic unrouted message: %+v, %v", unrouted, err)
	}
	corrReq := MessageRequest{
		GroupID: group.ID, ActorID: "user", Content: "这句话指的是告警", IdempotencyKey: "v43-correct",
	}
	corr, err := s43.PostLocalUserMessage(ctx, corrReq)
	if err != nil || corr.ThreadCorrection == nil || corr.ThreadCorrection.EffectiveTopicID != topicMsg.TopicID || len(corr.Deliveries) != 0 {
		t.Fatalf("v43 correction failed after upgrade: %+v, %v", corr, err)
	}
	corrReplay, err := s43.PostLocalUserMessage(ctx, corrReq)
	if err != nil || corrReplay.ID != corr.ID || corrReplay.ThreadCorrection == nil || corrReplay.ThreadCorrection.EffectiveTopicID != topicMsg.TopicID {
		t.Fatalf("v43 correction replay mismatch: %+v vs %+v, %v", corrReplay, corr, err)
	}
	assertSQLiteQuickCheckOK(t, s43)
	dispatchAssertFK(t, s43)
}
