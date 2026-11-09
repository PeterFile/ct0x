package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"modernc.org/sqlite"
)

func materializeChangeEligibility(t *testing.T, f requestDispatchFixture, change string) string {
	t.Helper()
	var query string
	var args []any
	code := domain.DispatchHeldAuthorityStale
	switch change {
	case "task-revision":
		query, args, code = "UPDATE tasks SET revision=2 WHERE id=?", []any{f.task.ID}, domain.DispatchHeldTaskStale
	case "active-attempt", "submitted", "accepted":
		a, err := dispatchHistoricalAttempt(t, f.s, taskAttemptInput(f.task))
		if err != nil {
			t.Fatal(err)
		}
		if change != "active-attempt" {
			s, err := f.s.SubmitTask(ctx, taskSubmissionInput(a))
			if err != nil {
				t.Fatal(err)
			}
			if change == "accepted" {
				if _, err := f.s.ReviewTask(ctx, taskReviewInput(s)); err != nil {
					t.Fatal(err)
				}
			}
		}
		return domain.DispatchHeldTaskStale
	case "authority":
		query, args = "UPDATE task_coordination SET authority_generation=2 WHERE task_id=?", []any{f.task.ID}
	case "missing-current":
		query, args = "DELETE FROM task_coordination WHERE task_id=?", []any{f.task.ID}
	case "binding":
		if _, err := f.s.SetCoordinator(ctx, f.groupID, f.coord, 1, "new-binding"); err != nil {
			t.Fatal(err)
		}
		return domain.DispatchHeldBindingStale
	case "policy":
		if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.worker}, 1, "new-policy"); err != nil {
			t.Fatal(err)
		}
		return domain.DispatchHeldPolicyStale
	case "coordinator-left", "both-left":
		query, args, code = "UPDATE members SET status='left' WHERE id=?", []any{f.coord}, domain.DispatchHeldCoordinatorInactive
		if change == "both-left" {
			query, args = "UPDATE members SET status='left' WHERE group_id=?", []any{f.groupID}
		}
	case "worker-left":
		query, args, code = "UPDATE members SET status='left' WHERE id=?", []any{f.worker}, domain.DispatchHeldNoEligibleWorker
	case "worker-other-group":
		query, args, code = "UPDATE members SET group_id=? WHERE id=?", []any{group(t, f.s).ID, f.worker}, domain.DispatchHeldNoEligibleWorker
	case "task-assignee":
		query, args, code = "UPDATE tasks SET assignee_id=? WHERE id=?", []any{f.coord, f.task.ID}, domain.DispatchHeldNoEligibleWorker
	case "current-topic", "current-mandate", "current-group", "history-revision", "history-trigger", "mandate-assignee":
		// Deliberately damaged historical/current metadata must not loosen the exact joins.
		if _, err := f.s.db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := f.s.db.Exec("PRAGMA foreign_keys=ON"); err != nil {
				t.Fatal(err)
			}
		}()
		switch change {
		case "current-topic":
			query, args = "UPDATE task_coordination SET topic_id='wrong' WHERE task_id=?", []any{f.task.ID}
		case "current-mandate":
			query, args = "UPDATE task_coordination SET current_mandate_id='wrong' WHERE task_id=?", []any{f.task.ID}
		case "current-group":
			query, args = "UPDATE task_coordination SET group_id='wrong' WHERE task_id=?", []any{f.task.ID}
		case "history-revision", "history-trigger":
			if _, err := f.s.db.Exec("DROP TRIGGER routing_decisions_no_update"); err != nil {
				t.Fatal(err)
			}
			query, args = "UPDATE routing_decisions SET task_revision_at_admission=2 WHERE id=?", []any{f.admission.Decision.ID}
			if change == "history-trigger" {
				query = "UPDATE routing_decisions SET trigger_id='wrong' WHERE id=?"
			}
		case "mandate-assignee":
			if _, err := f.s.db.Exec("DROP TRIGGER mandates_no_update"); err != nil {
				t.Fatal(err)
			}
			query, args, code = "UPDATE mandates SET assignee_id=? WHERE id=?", []any{f.coord, f.admission.Mandate.ID}, domain.DispatchHeldNoEligibleWorker
		}
	default:
		t.Fatalf("unknown fixture change %q", change)
	}
	if _, err := f.s.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
	return code
}

func TestMaterializeDispatchStaleIsHeldAndNeverAutomaticallyRetried(t *testing.T) {
	for _, change := range []string{"task-revision", "active-attempt", "submitted", "accepted", "authority", "missing-current", "current-topic", "current-mandate", "current-group", "history-revision", "history-trigger", "binding", "policy", "coordinator-left", "worker-left", "both-left", "worker-other-group", "task-assignee", "mandate-assignee"} {
		t.Run(change, func(t *testing.T) {
			f, r := materializePending(t)
			api := materializeAPI(t, f.s)
			code := materializeChangeEligibility(t, f, change)
			before := messageSequenceSnapshot(t, f.s)
			got, err := api.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
			if err != nil || got.Disposition != "held" || got.Generation != 2 || got.HeldReason != code || got.Delivery != nil {
				t.Fatalf("wrong deterministic hold: %+v, %v; want %s", got, err, code)
			}
			v, err := f.s.GetDispatchRequest(ctx, r.GroupID, r.ID)
			if err != nil || v.State.Status != domain.DispatchStatusHeld || v.State.Generation != 2 || v.State.AttemptCount != 0 || v.State.LastErrorCode != "" || v.State.HeldReason != code || v.State.Delivery != nil || v.State.NextAttemptAt != r.CreatedAt {
				t.Fatalf("hold not persisted exactly: %+v, %v", v, err)
			}
			delete(before, "coordination_dispatch_state")
			messageSequencePreserved(t, f.s, before)
			if _, err := f.s.db.Exec("UPDATE members SET status='active' WHERE group_id=?", r.GroupID); err != nil {
				t.Fatal(err)
			}
			before = messageSequenceSnapshot(t, f.s)
			for _, gen := range []int64{1, 2} {
				got, err := api.MaterializeDispatchRequest(ctx, r.ID, gen, materializeAt)
				requestDispatchZero(t, got, err, ErrConflict)
			}
			messageSequencePreserved(t, f.s, before)
		})
	}
}

func TestMaterializeDispatchHeldPriority(t *testing.T) {
	for _, first := range []string{"task-revision", "authority", "binding", "policy", "coordinator-left", "worker-left"} {
		t.Run(first, func(t *testing.T) {
			f, r := materializePending(t)
			api := materializeAPI(t, f.s)
			want, apply := "", false
			for _, change := range []string{"task-revision", "authority", "binding", "policy", "coordinator-left", "worker-left"} {
				apply = apply || change == first
				if apply {
					code := materializeChangeEligibility(t, f, change)
					if want == "" {
						want = code
					}
				}
			}
			got, err := api.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
			if err != nil || got.HeldReason != want {
				t.Fatalf("unstable held precedence: %+v, %v; want %s", got, err, want)
			}
		})
	}
}

func TestMaterializeDispatchEveryWriteAndCommitFailureRollsBack(t *testing.T) {
	for _, phase := range []string{"message", "conversation", "delivery", "state"} {
		for _, mode := range []string{"abort", "ignore", "commit"} {
			t.Run(phase+"/"+mode, func(t *testing.T) {
				f, r := materializePending(t)
				api := materializeAPI(t, f.s)
				observer := queueHandle(t, f.path, false)
				table := map[string]string{"message": "messages", "conversation": "conversations", "delivery": "deliveries", "state": "coordination_dispatch_state"}[phase]
				op, timing, body := "INSERT", "AFTER", "SELECT RAISE(ABORT,'注入物化失败');"
				if phase == "state" {
					op = "UPDATE"
				}
				if mode == "ignore" {
					timing, body = "BEFORE", "SELECT RAISE(IGNORE);"
				} else if mode == "commit" {
					if _, err := f.s.db.Exec("CREATE TABLE materialize_commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)"); err != nil {
						t.Fatal(err)
					}
					body = "INSERT INTO materialize_commit_guard VALUES('missing');"
				}
				if _, err := f.s.db.Exec("CREATE TRIGGER materialize_fault " + timing + " " + op + " ON " + table + " BEGIN " + body + " END"); err != nil {
					t.Fatal(err)
				}
				before := messageSequenceSnapshot(t, f.s)
				got, err := api.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
				requestDispatchZero(t, got, err, nil)
				if mode != "ignore" {
					var cause *sqlite.Error
					if !errors.As(err, &cause) {
						t.Fatalf("SQLite cause lost: %v", err)
					}
					if mode == "commit" {
						coordinationAdmissionRequireFK(t, err)
						if !strings.Contains(err.Error(), "保存") {
							t.Fatalf("not a COMMIT failure: %v", err)
						}
					}
				}
				messageSequencePreserved(t, observer, before)
				messageSequencePreserved(t, f.s, before)
				if _, err := f.s.db.Exec("DROP TRIGGER materialize_fault"); err != nil {
					t.Fatal(err)
				}
				got, err = materializeAPI(t, observer).MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
				if err != nil {
					t.Fatalf("failed transaction made original request unretryable: %v", err)
				}
				materializeAssertReceipt(t, observer, r, got, 1, materializeAt)
				if !reflect.DeepEqual(messageSequenceTableRows(t, observer, "mutation_idempotency"), before["mutation_idempotency"]) {
					t.Fatal("internal materialization reserved a user create_message key")
				}
			})
		}
	}
}

func TestMaterializeDispatchAllWritePointsRecheckEligibility(t *testing.T) {
	for _, phase := range []string{"messages", "conversations", "deliveries", "coordination_dispatch_state"} {
		for _, change := range []string{"task", "authority", "mandate", "history", "binding", "policy", "coordinator", "worker", "cas"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				f, r := materializePending(t)
				api := materializeAPI(t, f.s)
				other := coordinationAdmissionRows(f.task, coordinationAdmissionSource(t, f.s, domain.ThreadRef{GroupID: r.GroupID, TopicID: r.TopicID}))
				other.current = nil
				if err := coordinationAdmissionCommit(f.s, other); err != nil {
					t.Fatal(err)
				}
				if change == "history" {
					if _, err := f.s.db.Exec("DROP TRIGGER mandates_no_update; DROP TRIGGER routing_decisions_no_update"); err != nil {
						t.Fatal(err)
					}
				}
				mutation := map[string]string{
					"task":        fmt.Sprintf("UPDATE tasks SET revision=2 WHERE id='%s';", r.TaskID),
					"authority":   fmt.Sprintf("UPDATE task_coordination SET authority_generation=2 WHERE task_id='%s';", r.TaskID),
					"mandate":     fmt.Sprintf("UPDATE task_coordination SET current_mandate_id='%s' WHERE task_id='%s';", other.mandate[0], r.TaskID),
					"history":     fmt.Sprintf("UPDATE mandates SET task_revision_at_admission=2 WHERE id='%s'; UPDATE routing_decisions SET task_revision_at_admission=2 WHERE id='%s';", r.MandateID, f.admission.Decision.ID),
					"binding":     fmt.Sprintf("INSERT INTO group_coordinator_binding_revisions SELECT group_id,2,member_id FROM group_coordinator_binding_revisions WHERE group_id='%s' AND revision=1; UPDATE group_coordinator_bindings SET revision=2 WHERE group_id='%s';", r.GroupID, r.GroupID),
					"policy":      fmt.Sprintf("INSERT INTO group_dispatch_policy_revisions SELECT group_id,2,allowed_worker_ids FROM group_dispatch_policy_revisions WHERE group_id='%s' AND revision=1; UPDATE group_dispatch_policies SET revision=2 WHERE group_id='%s';", r.GroupID, r.GroupID),
					"coordinator": fmt.Sprintf("UPDATE members SET status='left' WHERE id='%s';", f.coord),
					"worker":      fmt.Sprintf("UPDATE members SET status='left' WHERE id='%s';", f.worker),
					"cas":         fmt.Sprintf("UPDATE coordination_dispatch_state SET generation=generation+1,attempt_count=attempt_count+1,last_error_code='sqlite_busy',updated_at='%s',next_attempt_at='%s' WHERE request_id='%s';", materializeAt.Format(timestampLayout), materializeAt.Add(2e9).Format(timestampLayout), r.ID),
				}[change]
				op := "INSERT"
				if phase == "coordination_dispatch_state" {
					op = "UPDATE"
					if change == "cas" {
						// A BEFORE state trigger consumes the pending CAS, then ignores the outer write.
						mutation += "SELECT RAISE(IGNORE);"
					}
				}
				timing := "AFTER"
				if phase == "coordination_dispatch_state" && change == "cas" {
					timing = "BEFORE"
				}
				if _, err := f.s.db.Exec("PRAGMA recursive_triggers=OFF"); err != nil {
					t.Fatal(err)
				}
				if _, err := f.s.db.Exec("CREATE TRIGGER materialize_mutate " + timing + " " + op + " ON " + phase + " BEGIN " + mutation + " END"); err != nil {
					t.Fatal(err)
				}
				before := messageSequenceSnapshot(t, f.s)
				got, err := api.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
				requestDispatchZero(t, got, err, ErrConflict)
				messageSequencePreserved(t, f.s, before)
				if _, err := f.s.db.Exec("DROP TRIGGER materialize_mutate"); err != nil {
					t.Fatal(err)
				}
				got, err = api.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
				if err != nil {
					t.Fatalf("fence failure left business rows behind: %v", err)
				}
				materializeAssertReceipt(t, f.s, r, got, 1, materializeAt)
			})
		}
	}
}

func TestMaterializeDispatchEligibilityDatabaseErrorIsNotHeld(t *testing.T) {
	f, r := materializePending(t)
	api := materializeAPI(t, f.s)
	if _, err := f.s.db.Exec("ALTER TABLE task_coordination RENAME TO missing_task_coordination"); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	got, err := api.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	requestDispatchZero(t, got, err, nil)
	var cause *sqlite.Error
	if !errors.As(err, &cause) {
		t.Fatalf("database fault was hidden: %v", err)
	}
	messageSequencePreserved(t, f.s, before)
}
