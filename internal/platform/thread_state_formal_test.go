package platform

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/contextpack"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func formalTaskStateScope(f *rolePrepareFixture) domain.ThreadStateScope {
	role := f.state.Provenance
	return domain.ThreadStateScope{ThreadRef: domain.ThreadRef{GroupID: role.GroupID, TopicID: role.TopicID},
		TaskID: role.TaskID, TaskRevision: role.TaskRevision}
}

func confirmFormalTaskState(t *testing.T, f *rolePrepareFixture) domain.ThreadStateSnapshot {
	t.Helper()
	scope := formalTaskStateScope(f)
	source := stateCorrectionPost(t, f.prepareFixture, scope.GroupID, scope.TopicID, "task-source", "Keep the signed release list.")
	// The anchor is past the trigger; formal Task coverage must not cut the role's stored history.
	return stateCorrectionSet(t, f.prepareFixture, scope, 0, 0, domain.ThreadStateConstraint, "set", source, source.ID)
}

func TestThreadStateFormalPreparation(t *testing.T) {
	for _, role := range []store.CoordinationRoleKind{store.CoordinationRoleCoordinator, store.CoordinationRoleWorker} {
		t.Run(string(role), func(t *testing.T) {
			f := newRolePrepareFixture(t)
			f.enableRuntime(t)
			if role == store.CoordinationRoleWorker {
				f.selectWorker(t)
			}
			p := f.preparer(t)
			empty, before := stateCorrectionPrepare(t, p, f.trigger)
			claim := store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.pending.Delivery.ID, MaxActive: 1,
				PreparedThreadState: empty.PreparedThreadState, ResourceKeys: empty.ResourceKeys}
			if before.State != nil {
				t.Fatalf("empty Task state appeared in prompt: %+v", before.State)
			}
			if !threadStateFixtureInstalled(t, f.prepareFixture) {
				assertEmptyStatePreparationStamp(t, empty.PreparedThreadState, f.trigger, true)
				if state, err := f.s.LoadAuthorizedTaskThreadState(t.Context(), f.state); state != nil || err != nil {
					t.Fatalf("unregistered E2 changed state-only read: %+v, %v", state, err)
				}
				turn, err := f.s.ClaimTurn(t.Context(), claim)
				if err != nil || turn.State != domain.TurnStateStarting || turn.DeliveryID != claim.DeliveryID {
					t.Fatalf("unregistered E2 prevented formal claim: %+v, %v", turn, err)
				}
				return
			}
			withoutState := minimumRolePacket(t, f.contextPlan(t, p)).Required
			want := confirmFormalTaskState(t, f)
			other := stateCorrectionPost(t, f.prepareFixture, f.group.ID, f.trigger.TopicID, "thread-source", "Ordinary Thread state is not Task state.")
			_, err := f.s.SetLocalThreadState(t.Context(), domain.ThreadStateScope{ThreadRef: want.Scope.ThreadRef}, store.ThreadStateUpdate{
				Kind: domain.ThreadStateGoal, Action: "set", Content: other.Content, SourceMessage: other.ID}, "ordinary-state")
			prepareRoleOK(t, err)
			otherSource := stateCorrectionPost(t, f.prepareFixture, f.group.ID, f.trigger.TopicID, "other-task-source", "Only the other Task may use this constraint.")
			otherTask, err := f.s.RequestWork(t.Context(), f.group.ID, domain.RequestWorkRequest{TopicID: f.trigger.TopicID,
				MessageID: otherSource.ID, Mode: domain.RoutingDecisionCreateNew, Reason: "independent work in the same Thread",
				NewTask: &domain.NewTaskRequest{Title: "Other Task", Acceptance: "Separate conclusion"}}, "other-task")
			prepareRoleOK(t, err)
			otherScope := domain.ThreadStateScope{ThreadRef: want.Scope.ThreadRef, TaskID: otherTask.Task.ID, TaskRevision: otherTask.Task.RevisionAtAdmission}
			otherState, err := f.s.SetLocalThreadState(t.Context(), otherScope, store.ThreadStateUpdate{
				Kind: domain.ThreadStateConstraint, Action: "set", Content: otherSource.Content, SourceMessage: otherSource.ID}, "other-task-state")
			prepareRoleOK(t, err)
			if otherState.Revision != 1 || otherState.Scope.TaskID == want.Scope.TaskID {
				t.Fatal("fixture did not confirm state for a second Task in this Thread")
			}
			prepared, pack := stateCorrectionPrepare(t, p, f.trigger)
			if pack.State == nil {
				t.Fatalf("formal %s prompt is missing the confirmed Task state", role)
			}
			if !reflect.DeepEqual(want, *pack.State) || !reflect.DeepEqual(before.Pending, pack.Pending) ||
				len(before.History) == 0 || !reflect.DeepEqual(before.History, pack.History) || pack.Summary != nil ||
				prepared.PreparedThreadState == nil || prepared.PreparedThreadState.Revision != want.Revision {
				t.Fatalf("not the exact Task snapshot, stored history and claim fence: %+v", pack)
			}
			f.opts.RoleMaxContextBytes = withoutState
			tooSmall, err := f.preparer(t).Prepare(t.Context(), f.pending)
			assertRoleBlocked(t, tooSmall, err, f.state, "context_budget")
			claim.PreparedThreadState, claim.ResourceKeys = prepared.PreparedThreadState, prepared.ResourceKeys
			turn, err := f.s.ClaimTurn(t.Context(), claim)
			if err != nil || turn.State != domain.TurnStateStarting || turn.DeliveryID != claim.DeliveryID {
				t.Fatalf("fresh exact Task stamp prevented formal claim: %+v, %v", turn, err)
			}
		})
	}
}

func TestThreadStateFormalSchedulerRepreparesBeforeClaim(t *testing.T) {
	f := newRolePrepareFixture(t)
	installed := threadStateFixtureInstalled(t, f.prepareFixture)
	f.enableRuntime(t)
	p := f.preparer(t)
	ctx, stop := context.WithTimeout(t.Context(), 30*time.Second)
	defer stop()
	var want domain.ThreadStateSnapshot
	var launched provider.ExecuteRequest
	var live domain.Turn
	var operations []string
	attempts := 0
	var cleanErr, liveErr error
	err := scheduler.Run(ctx, f.s, scheduler.Options{MaxActive: 1, Recovered: true,
		RuntimeDir: f.runtimeOptions.RoleExecutionRoot, PollInterval: time.Millisecond}, func(ctx context.Context, pending store.PendingDelivery) (scheduler.PreparedExecution, error) {
		if pending.Delivery.ID != f.pending.Delivery.ID {
			return scheduler.PreparedExecution{}, scheduler.ErrNotReady
		}
		attempts++
		got, err := p.Prepare(ctx, pending)
		if !installed && attempts == 2 {
			assertRoleBlocked(t, got, err, f.state, string(store.CoordinationPolicyStale))
			cleanErr = formalClaimRollback(f)
			stop()
			return got, err
		}
		if err != nil {
			return scheduler.PreparedExecution{}, err
		}
		if attempts == 1 {
			if installed {
				// Commit revision 1 after real role preparation and before its claim.
				want = confirmFormalTaskState(t, f)
			} else {
				// Without E2 installed, formal claims still fence post-prepare policy
				// drift before inserting a Turn, and the scheduler retries cleanly.
				if _, err := f.s.SetDispatchPolicy(ctx, f.group.ID, nil, 1, "policy-before-claim"); err != nil {
					return scheduler.PreparedExecution{}, err
				}
			}
		} else if cleanErr = formalClaimRollback(f); cleanErr != nil {
			return scheduler.PreparedExecution{}, cleanErr
		}
		// Keep real role startup, issued config and preflight. Replace only the
		// provider invocation after the existing bridge factory succeeds.
		run := got.Executor.(*memberToolRuntimeRun)
		factory := run.factory
		run.factory = func(path, id string, names []string) (provider.Executor, error) {
			if _, err := factory(path, id, names); err != nil {
				return nil, err
			}
			operations = slices.Clone(names)
			return memberToolRuntimeExecutorFunc(func(ctx context.Context, req provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
				launched = req
				live, liveErr = f.s.GetTurn(ctx, pending.GroupID, req.RunID)
				stop()
				return provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}
			}), nil
		}
		return got, nil
	})
	if cleanErr != nil {
		t.Fatal(cleanErr)
	}
	prepareRoleOK(t, liveErr)
	if !installed {
		if !errors.Is(err, context.Canceled) || attempts != 2 || launched.RunID != "" {
			t.Fatalf("unregistered formal scheduler did not roll back and reprepare on post-prepare drift: attempts=%d, launched=%q, err=%v", attempts, launched.RunID, err)
		}
		f.assertQueued(t)
		return
	}
	if !errors.Is(err, context.Canceled) || live.State != domain.TurnStateStarting || live.DeliveryID != f.pending.Delivery.ID || live.ID != launched.RunID {
		t.Fatalf("scheduler did not reach the real starting claim and drain: %+v, %v", live, err)
	}
	if attempts != 2 {
		t.Fatalf("scheduler launched stale formal preparation without retrying: %d preparation(s)", attempts)
	}
	var pack stateCorrectionPacket
	prepareRoleOK(t, json.Unmarshal([]byte(launched.Prompt), &pack))
	if !reflect.DeepEqual(&want, pack.State) || !slices.Equal(operations, memberToolRoleOperations(store.CoordinationRoleCoordinator)) {
		t.Fatalf("fresh role launch lacks updated Task state or changed operations: %+v, %v", pack.State, operations)
	}
}

func TestThreadStateFormalSchedulerRetriesRoleDrift(t *testing.T) {
	f := newRolePrepareFixture(t)
	f.enableRuntime(t)
	p := f.preparer(t)
	ctx, stop := context.WithTimeout(t.Context(), 30*time.Second)
	defer stop()
	checks := 0
	err := scheduler.Run(ctx, f.s, scheduler.Options{MaxActive: 1, Recovered: true,
		RuntimeDir: f.runtimeOptions.RoleExecutionRoot, PollInterval: time.Millisecond}, func(ctx context.Context, pending store.PendingDelivery) (scheduler.PreparedExecution, error) {
		if pending.Delivery.ID != f.pending.Delivery.ID {
			return scheduler.PreparedExecution{}, scheduler.ErrNotReady
		}
		checks++
		if checks == 1 {
			// Split Prepare at its checked-input boundary so the real policy
			// command commits before the planner loads bound Task state.
			in, err := p.loadPreparationInput(ctx, pending)
			prepareRoleOK(t, err)
			if in.state != f.state {
				t.Fatal("initial checked role was not ready")
			}
			_, err = f.s.SetDispatchPolicy(ctx, f.group.ID, nil, 1, "policy-during-preparation")
			prepareRoleOK(t, err)
			got, err := p.prepareCoordinationRole(ctx, in.state, in.group, in.member, in.conversation, in.trigger)
			assertRoleBlocked(t, got, err, f.state, string(store.CoordinationPolicyStale))

			// The public state-only caller still receives a denial. An invalid
			// bound revision must not retry even when the real policy is stale.
			if state, err := f.s.LoadAuthorizedTaskThreadState(ctx, in.state); state != nil || !errors.Is(err, store.ErrMemberToolDenied) {
				t.Fatalf("state-only API classified changed role differently: %+v, %v", state, err)
			}
			spoof := in.state
			spoof.Provenance.TaskRevision++
			denied, denial := p.prepareCoordinationRole(ctx, spoof, in.group, in.member, in.conversation, in.trigger)
			if !reflect.ValueOf(denied).IsZero() || !errors.Is(denial, store.ErrMemberToolDenied) || errors.Is(denial, scheduler.ErrNotReady) {
				t.Fatalf("wrong bound revision became retryable: %v", denial)
			}
			return got, err
		}
		got, err := p.Prepare(ctx, pending)
		assertRoleBlocked(t, got, err, f.state, string(store.CoordinationPolicyStale))
		stop()
		return got, err
	})
	if checks != 2 || !errors.Is(err, context.Canceled) {
		t.Fatalf("scheduler did not recheck the same queued formal delivery: checks=%d, err=%v", checks, err)
	}
	f.assertQueued(t)
}

func formalClaimRollback(f *rolePrepareFixture) error {
	var clean bool
	err := f.db.QueryRow(`SELECT state='queued' AND turn_id='' AND NOT EXISTS(SELECT 1 FROM turns WHERE delivery_id=d.id)
	 AND NOT EXISTS(SELECT 1 FROM resource_claims) FROM deliveries d WHERE id=?`, f.pending.Delivery.ID).Scan(&clean)
	if err == nil && !clean {
		err = errors.New("rejected formal claim consumed its delivery or left a Turn/resource")
	}
	return err
}

func TestThreadStateFormalClaimBindingAndIntegrity(t *testing.T) {
	for _, tc := range []struct {
		change string
		want   error
	}{
		{"other recipient of the same trigger", store.ErrMemberToolDenied},
		{"new Task version", store.ErrTurnBusy},
		{"damaged E2", nil},
		{"damaged return schema guard", store.ErrConflict},
		{"contradictory return message marker", store.ErrMemberToolDenied},
	} {
		t.Run(tc.change, func(t *testing.T) {
			f := newRolePrepareFixture(t)
			installed := threadStateFixtureInstalled(t, f.prepareFixture)
			f.enableRuntime(t)
			p := f.preparer(t)
			if installed && tc.change == "other recipient of the same trigger" {
				// Confirm nonzero Task state before preparation so cross-recipient
				// fencing covers non-empty Task snapshots too.
				confirmFormalTaskState(t, f)
			}
			prepared, pack := stateCorrectionPrepare(t, p, f.trigger)
			if installed && tc.change == "other recipient of the same trigger" && (pack.State == nil || pack.State.Revision != 1) {
				t.Fatalf("fixture did not prepare nonzero Task state: %+v", pack.State)
			}
			claim := store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.pending.Delivery.ID, MaxActive: 1,
				PreparedThreadState: prepared.PreparedThreadState, ResourceKeys: prepared.ResourceKeys}
			switch tc.change {
			case "other recipient of the same trigger":
				// Preserve the authentic trigger and formal receipt. The other
				// recipient's delivery passes the stamp's Thread/MessageID check.
				claim.DeliveryID = "other-recipient-delivery"
				f.exec(t, `INSERT INTO conversations(id,group_id,member_id,topic_id,created_at)
				 SELECT 'other-recipient-conversation',group_id,?,topic_id,created_at FROM deliveries WHERE id=?`, f.worker.ID, f.pending.Delivery.ID)
				f.exec(t, `INSERT INTO deliveries(id,message_id,conversation_id,recipient_id,group_id,topic_id,created_at)
				 SELECT ?,message_id,'other-recipient-conversation',?,group_id,topic_id,created_at FROM deliveries WHERE id=?`, claim.DeliveryID, f.worker.ID, f.pending.Delivery.ID)
				other, err := f.s.ResolveCoordinationDelivery(t.Context(), claim.GroupID, claim.DeliveryID)
				prepareRoleOK(t, err)
				if other.Provenance.Kind != store.CoordinationRoleOrdinary || other.Provenance.MessageID != prepared.PreparedThreadState.MessageID || other.Provenance.MemberID != f.worker.ID {
					t.Fatalf("fixture is not the same trigger's other delivery: %+v", other.Provenance)
				}
			case "new Task version":
				f.exec(t, "UPDATE tasks SET revision=revision+1 WHERE id=?", f.state.Provenance.TaskID)
			case "damaged E2":
				if installed {
					f.exec(t, "DROP INDEX thread_state_by_slot")
				} else {
					f.exec(t, "CREATE TABLE thread_state_revisions(id TEXT PRIMARY KEY)")
				}
			case "damaged return schema guard":
				f.exec(t, "DROP TRIGGER chat_discussion_work_return_deliveries_identity")
			case "contradictory return message marker":
				// Seed bad raw evidence by message, not by delivery. Restore
				// the insert guard so the schema itself still verifies.
				var ddl string
				prepareRoleOK(t, f.db.QueryRow(`SELECT sql FROM sqlite_schema WHERE name='chat_discussion_work_returns_insert'`).Scan(&ddl))
				f.exec(t, "DROP TRIGGER chat_discussion_work_returns_insert")
				f.exec(t, `INSERT INTO chat_discussion_work_returns(origin_message_id,group_id,topic_id,invitation_message_id,handback_id,message_id)
				 VALUES('missing-root',?,?,'missing-invitation','missing-handback',?)`, f.group.ID, f.trigger.TopicID, f.trigger.ID)
				f.exec(t, ddl)
			}
			if tc.change == "damaged return schema guard" || tc.change == "contradictory return message marker" {
				if _, err := f.s.ResolveCoordinationDelivery(t.Context(), claim.GroupID, claim.DeliveryID); !errors.Is(err, tc.want) {
					t.Fatalf("queued resolver did not detect %s: %v", tc.change, err)
				}
				var in contextpack.Input
				stamp, err := p.attachFormalTaskState(t.Context(), f.state, &in)
				if stamp != nil || in.ThreadState != nil || !errors.Is(err, tc.want) || errors.Is(err, scheduler.ErrNotReady) {
					t.Fatalf("bound state read fell back or retried on %s: %v", tc.change, err)
				}
			}
			// A post-insert rollback would return the tripwire's error
			// instead of the classification/integrity error from the preclaim fence.
			f.exec(t, `CREATE TRIGGER test_formal_preinsert BEFORE INSERT ON turns
			 BEGIN SELECT RAISE(ABORT,'must reject before inserting a turn'); END`)
			turn, err := f.s.ClaimTurn(t.Context(), claim)
			if turn.ID != "" || err == nil || strings.Contains(err.Error(), "must reject before inserting a turn") ||
				(tc.want != nil && !errors.Is(err, tc.want)) || errors.Is(err, store.ErrTurnBusy) != (tc.want == store.ErrTurnBusy) {
				t.Fatalf("claim failed to fence %s (want %v): %+v, %v", tc.change, tc.want, turn, err)
			}
			var queued bool
			prepareRoleOK(t, f.db.QueryRow("SELECT state='queued' AND turn_id='' FROM deliveries WHERE id=?", claim.DeliveryID).Scan(&queued))
			if !queued {
				t.Fatal("rejected claim consumed its target delivery")
			}
			prepareRoleOK(t, formalClaimRollback(f))
			if tc.change == "damaged E2" {
				got, err := p.Prepare(t.Context(), f.pending)
				if err == nil || got.Executor != nil || got.Request.Prompt != "" || errors.Is(err, store.ErrThreadStateUnavailable) || errors.Is(err, scheduler.ErrNotReady) {
					t.Fatalf("formal prompt fell back on damaged E2: executor=%T, err=%v", got.Executor, err)
				}
			}
		})
	}
}
