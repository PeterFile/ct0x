package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

func TestMigrationsV40GroupBackgroundDispatchOrder(t *testing.T) {
	idx := slices.Index(migrations, groupBackgroundDispatchSchema)
	if len(migrations) < 40 || idx != 39 {
		t.Fatalf("expected groupBackgroundDispatchSchema at v40 (index 39), got index=%d total=%d", idx, len(migrations))
	}
}

func TestMigrationsV40GroupBackgroundDispatchUpgradeReplayAndThreeStates(t *testing.T) {
	prior := migrations
	t.Cleanup(func() { migrations = prior })

	// 1. Missing state (v39): ordinary message path works; background dispatch stays closed.
	migrations = prior[:39]
	dbPath := filepath.Join(t.TempDir(), "v40-upgrade.sqlite")
	s39, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if s39.background.state != groupBackgroundSchemaAbsent {
		t.Fatalf("v39 state = %d; want absent (%d)", s39.background.state, groupBackgroundSchemaAbsent)
	}
	ctx := t.Context()
	if err := s39.EnableGroupBackgroundDispatch(ctx); !errors.Is(err, ErrInvalid) {
		t.Fatalf("v39 enabled background dispatch while absent: %v", err)
	}
	legacyGroup, err := s39.CreateLocalUserGroup(ctx, CreateGroupInput{Name: "v39旧群", ActorID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	legacyMember := member(t, s39, legacyGroup.ID)
	legacyReq := MessageRequest{
		GroupID: legacyGroup.ID, ActorID: "user", TopicID: "legacy-topic",
		Content: "v39 旧入口消息", IdempotencyKey: "v40-reg-legacy-key",
	}
	legacyMsg, err := s39.PostLocalUserMessage(ctx, legacyReq)
	if err != nil || len(legacyMsg.Deliveries) != 1 || legacyMsg.Deliveries[0].RecipientID != legacyMember.ID {
		t.Fatalf("v39 legacy message failed: %+v, %v", legacyMsg, err)
	}
	if err := s39.Close(); err != nil {
		t.Fatal(err)
	}

	// 2. Complete state (v40): upgrade preserves v39 message, replays match, normal entry works.
	migrations = prior
	s40, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s40.Close() })
	if s40.background.state != groupBackgroundSchemaComplete {
		t.Fatalf("v40 upgraded state = %d; want complete (%d)", s40.background.state, groupBackgroundSchemaComplete)
	}
	messageSequenceLedger(t, s40, len(migrations))
	legacyReplay, err := s40.PostLocalUserMessage(ctx, legacyReq)
	if err != nil || legacyReplay.ID != legacyMsg.ID {
		t.Fatalf("v39 message replay after v40 upgrade drifted: %+v vs %+v, %v", legacyReplay, legacyMsg, err)
	}
	if err := s40.EnableGroupBackgroundDispatch(ctx); err != nil {
		t.Fatal(err)
	}
	boundGroup, err := s40.CreateLocalUserGroup(ctx, CreateGroupInput{Name: "v40新群", ActorID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	bgReq := MessageRequest{
		GroupID: boundGroup.ID, ActorID: "user", TopicID: "topic-a1",
		Content: "请自动路由本消息。", IdempotencyKey: "v40-reg-bg-key",
	}
	m1, err := s40.PostLocalUserMessage(ctx, bgReq)
	if err != nil || len(m1.Deliveries) != 0 {
		t.Fatalf("bound group unaddressed post: %+v, %v", m1, err)
	}
	m1Replay, err := s40.PostLocalUserMessage(ctx, bgReq)
	if err != nil || m1Replay.ID != m1.ID {
		t.Fatalf("bound group post replay drifted: %+v vs %+v, %v", m1Replay, m1, err)
	}
	profile := BackgroundDispatchProfile{ProviderID: "approved-pi", Model: "verified-model"}
	if err := s40.RunGroupBackgroundDispatch(ctx, BackgroundDispatchOptions{
		MaxActiveMembers: 2,
		ReadyProfiles:    []BackgroundDispatchProfile{profile},
		Create:           &BackgroundDispatchTemplate{ID: "helper-v1", Name: "讨论助手", Role: "纯文本", Profile: profile},
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := s40.GetMessage(ctx, boundGroup.ID, m1.ID); err != nil || len(got.Deliveries) != 1 {
		t.Fatalf("expected 1 background delivery after pass: %+v, %v", got, err)
	}
	assertSQLiteQuickCheckOK(t, s40)
	dispatchAssertFK(t, s40)
	if err := s40.Close(); err != nil {
		t.Fatal(err)
	}

	// 3. Damaged state: dropping an A1 trigger causes Open to fail closed cleanly.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TRIGGER group_background_delivery_insert`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if bad, err := Open(dbPath); err == nil {
		_ = bad.Close()
		t.Fatal("Open unexpectedly succeeded on damaged v40 schema")
	}
}
