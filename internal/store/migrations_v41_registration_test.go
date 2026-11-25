package store

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"agentworkspace/internal/domain"
)

func TestMigrationsV41ChatAutomaticTextOrder(t *testing.T) {
	idx := slices.Index(migrations, chatAutomaticTextSchema)
	if len(migrations) < 41 || idx != 40 {
		t.Fatalf("expected chatAutomaticTextSchema at v41 (index 40), got index=%d total=%d", idx, len(migrations))
	}
}

func TestMigrationsV41ChatAutomaticTextUpgradeReplayAndFailClosed(t *testing.T) {
	prior := migrations
	t.Cleanup(func() { migrations = prior })

	// 1. Pre-v41 (v40): existing group/message entry works; v41 auto-text & group-import fail closed.
	migrations = prior[:40]
	dbPath := filepath.Join(t.TempDir(), "v41-upgrade.sqlite")
	s40, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	legacyGroup, err := s40.CreateLocalUserGroup(ctx, CreateGroupInput{Name: "v40旧群", ActorID: "user", IdempotencyKey: "v41-reg-legacy-group"})
	if err != nil {
		t.Fatal(err)
	}
	legacyMember := member(t, s40, legacyGroup.ID)
	legacyReq := MessageRequest{
		GroupID: legacyGroup.ID, ActorID: "user", TopicID: "legacy-topic",
		Content: "v40 旧入口消息", IdempotencyKey: "v41-reg-legacy-msg",
	}
	legacyMsg, err := s40.PostLocalUserMessage(ctx, legacyReq)
	if err != nil || len(legacyMsg.Deliveries) != 1 || legacyMsg.Deliveries[0].RecipientID != legacyMember.ID {
		t.Fatalf("v40 legacy message failed: %+v, %v", legacyMsg, err)
	}
	if _, err := s40.SetChatPolicy(ctx, domain.ThreadRef{GroupID: legacyGroup.ID, TopicID: "t"}, automaticTextPolicy(1), 0, "pre-v41-auto"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("v40 accepted automatic_text_dispatch: %v", err)
	}
	if _, err := s40.CreateLocalUserGroup(ctx, CreateGroupInput{Name: "pre-v41-import", ActorID: "user", ImportAuthorizationFromGroupID: legacyGroup.ID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("v40 accepted group auth import: %v", err)
	}
	if err := s40.Close(); err != nil {
		t.Fatal(err)
	}

	// 2. Upgraded v41: replays preserve v40 records, auto tables stay empty, normal one-line import entry works.
	migrations = prior
	s41, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s41.Close() })
	messageSequenceLedger(t, s41, len(migrations))
	replayedMsg, err := s41.PostLocalUserMessage(ctx, legacyReq)
	if err != nil || replayedMsg.ID != legacyMsg.ID {
		t.Fatalf("v40 message replay after v41 upgrade drifted: %+v vs %+v, %v", replayedMsg, legacyMsg, err)
	}
	for _, table := range []string{"chat_automatic_text_policy_revisions", "chat_automatic_text_routes", "chat_automatic_text_group_imports"} {
		if got := messageSequenceTableRows(t, s41, table); len(got) != 0 {
			t.Fatalf("v41 upgrade promoted rows into %s: %v", table, got)
		}
	}
	if _, err := s41.SetChatPolicy(ctx, domain.ThreadRef{GroupID: legacyGroup.ID}, automaticTextPolicy(2), 0, "v41-src-policy"); err != nil {
		t.Fatal(err)
	}
	imported, err := s41.CreateLocalUserGroup(ctx, CreateGroupInput{
		Name: "v41导入新群", ActorID: "user", IdempotencyKey: "v41-imported-group",
		ImportAuthorizationFromGroupID: legacyGroup.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	replayedImport, err := s41.CreateLocalUserGroup(ctx, CreateGroupInput{
		Name: "v41导入新群", ActorID: "user", IdempotencyKey: "v41-imported-group",
		ImportAuthorizationFromGroupID: legacyGroup.ID,
	})
	if err != nil || replayedImport.ID != imported.ID {
		t.Fatalf("v41 group import replay mismatch: %+v vs %+v, %v", replayedImport, imported, err)
	}
	assertSQLiteQuickCheckOK(t, s41)
	dispatchAssertFK(t, s41)
}
