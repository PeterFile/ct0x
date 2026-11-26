package store

import (
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"agentworkspace/internal/domain"
)

func TestMigrationsV44ThreadStateRegistration(t *testing.T) {
	if got := slices.Index(migrations, threadStateSchema) + 1; got != 44 {
		t.Fatalf("threadStateSchema migration is v%d, want v44", got)
	}
}

func TestMigrationsV44ThreadStateUpgrade(t *testing.T) {
	public := migrations
	if len(public) < 44 {
		t.Fatalf("public migrations stop at v%d, want at least v44", len(public))
	}
	t.Cleanup(func() { migrations = public })
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "v43.sqlite")

	migrations = public[:43]
	before, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = before.Close() })
	g := group(t, before)
	scope := domain.ThreadStateScope{ThreadRef: domain.ThreadRef{GroupID: g.ID, TopicID: "export"}}
	request := MessageRequest{
		GroupID: g.ID, TopicID: scope.TopicID, ActorID: "user",
		Content: "兼容旧版导出是本轮目标。", IdempotencyKey: "v43-goal-message",
	}
	message, err := before.CreateLocalUserMessage(ctx, request, nil)
	if err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, before, 43)
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	migrations = public
	after, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = after.Close() })
	messageSequenceLedger(t, after, len(public))

	original, err := after.CreateLocalUserMessage(ctx, request, nil)
	if err != nil || original.ID != message.ID || original.Content != message.Content {
		t.Fatalf("v43 source message changed across upgrade: %+v, %v", original, err)
	}
	empty, err := after.GetLocalThreadState(ctx, scope, 0)
	if err != nil || empty.Revision != 0 || len(empty.Records) != 0 {
		t.Fatalf("upgrade unexpectedly backfilled thread state: %+v, %v", empty, err)
	}
	update := ThreadStateUpdate{
		Kind: domain.ThreadStateGoal, Action: "set", Content: message.Content, SourceMessage: message.ID,
	}
	written, err := after.SetLocalThreadState(ctx, scope, update, "v44-confirm-goal")
	if err != nil || written.Revision != 1 || len(written.Records) != 1 {
		t.Fatalf("v44 thread state write failed: %+v, %v", written, err)
	}
	replay, err := after.SetLocalThreadState(ctx, scope, update, "v44-confirm-goal")
	if err != nil || !reflect.DeepEqual(replay, written) {
		t.Fatalf("v44 thread state replay changed: %+v, %v", replay, err)
	}
	assertSQLiteQuickCheckOK(t, after)
	dispatchAssertFK(t, after)
}
