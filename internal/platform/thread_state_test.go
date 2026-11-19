package platform

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func threadStateFixtureInstalled(t *testing.T, f *prepareFixture) bool {
	t.Helper()
	var installed bool
	if err := f.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='thread_state_revisions')`).Scan(&installed); err != nil {
		t.Fatal(err)
	}
	return installed
}

func assertLegacyThreadPreparation(t *testing.T, f *prepareFixture, pending store.PendingDelivery) scheduler.PreparedExecution {
	t.Helper()
	got, err := f.preparer(t).Prepare(t.Context(), pending)
	if err != nil || got.Executor == nil || got.PreparedThreadState != nil || strings.Contains(got.Request.Prompt, `"thread_state"`) {
		t.Fatalf("ordinary preparation without E2 schema changed: stamp=%+v, err=%v, prompt=%q", got.PreparedThreadState, err, got.Request.Prompt)
	}
	pack := decodePreparation(t, got.Request.Prompt)
	if pack.Scope.GroupID != pending.GroupID || pack.Scope.ConversationID != pending.Delivery.ConversationID ||
		len(pack.Pending) != 1 || pack.Pending[0].ID != pending.Delivery.MessageID {
		t.Fatalf("legacy prompt lost the exact delivery trigger: %+v", pack)
	}
	t.Log("unregistered E2: ordinary Prepare retains the exact trigger without a Thread state stamp")
	return got
}

func TestThreadStatePreparationFailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, damage string }{
		{"table", `DROP TABLE thread_state_revisions`},
		{"index", `DROP INDEX thread_state_by_slot`},
		{"guard", `DROP TRIGGER thread_state_append`},
		{"missing anchor", `DROP TRIGGER message_log_no_delete; DELETE FROM message_log`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newPrepareFixture(t)
			thread := domain.ThreadRef{GroupID: f.group.ID, TopicID: "wrong topic"}
			if got, err := f.s.LoadThreadPreparation(ctx, thread, f.trigger.ID, 50); got.History != nil || got.State != nil || got.Claim != nil || !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("changed or missing authoritative scope fell back: %+v, %v", got, err)
			}
			if !threadStateFixtureInstalled(t, f) {
				assertLegacyThreadPreparation(t, f, f.pending)
				f.assertQueued(t)
				return
			}
			f.exec(t, tc.damage)
			p := f.preparer(t)
			prepared, err := p.Prepare(ctx, f.pending)
			if prepared.Executor != nil || prepared.Request.Prompt != "" || err == nil || errors.Is(err, store.ErrThreadStateUnavailable) {
				t.Fatalf("broken scope/schema returned a prompt or legacy fallback: %+v, %v", prepared, err)
			}
			f.assertQueued(t)
		})
	}
}

func TestThreadStateRoleLoadingBoundary(t *testing.T) {
	ctx := context.Background()
	f := newRolePrepareFixture(t)
	installed := threadStateFixtureInstalled(t, f.prepareFixture)
	p := f.state.Provenance
	thread := domain.ThreadRef{GroupID: p.GroupID, TopicID: p.TopicID}
	scope := domain.ThreadStateScope{ThreadRef: thread, TaskID: p.TaskID, TaskRevision: p.TaskRevision}
	const content = "a separately confirmed task constraint"
	m, err := NewLocalUser(f.s).CreateMessage(ctx, p.GroupID, MessageInput{TopicID: p.TopicID, Content: content, RecipientIDs: []string{}}, "source")
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewLocalUser(f.s).SetThreadState(ctx, scope, store.ThreadStateUpdate{
		Kind: domain.ThreadStateConstraint, Action: "set", Content: content, SourceMessage: m.ID,
	}, "state")
	if !installed {
		if !errors.Is(err, store.ErrThreadStateUnavailable) {
			t.Fatalf("Task state command without E2 schema: %v", err)
		}
		if got, err := f.s.LoadAuthorizedTaskThreadState(ctx, f.state); got != nil || err != nil {
			t.Fatalf("Task preparation without E2 schema returned state: %+v, %v", got, err)
		}
		assertLegacyThreadPreparation(t, f.prepareFixture, f.ordinary)
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := f.s.LoadAuthorizedTaskThreadState(ctx, f.state)
	if err != nil || loaded == nil || loaded.Scope != scope || loaded.Revision != 1 || len(loaded.Records) != 1 || loaded.Records[0].Source.SourceID != m.SourceID {
		t.Fatalf("authorized task state missing: %+v, %v", loaded, err)
	}
	spoof := f.state
	spoof.Provenance.DeliveryID = f.ordinary.Delivery.ID
	if got, err := f.s.LoadAuthorizedTaskThreadState(ctx, spoof); got != nil || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatalf("ordinary delivery selected a task: %+v, %v", got, err)
	}
	spoof = f.state
	spoof.Provenance.TaskRevision++
	if got, err := f.s.LoadAuthorizedTaskThreadState(ctx, spoof); got != nil || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatalf("wrong task revision was readable: %+v, %v", got, err)
	}
	ordinary, err := f.preparer(t).Prepare(ctx, f.ordinary)
	if err != nil || strings.Contains(ordinary.Request.Prompt, `"thread_state"`) {
		t.Fatalf("ordinary prompt used task state: %s, %v", ordinary.Request.Prompt, err)
	}
}
