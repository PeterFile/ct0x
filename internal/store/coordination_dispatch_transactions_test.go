package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"modernc.org/sqlite"
)

func TestRequestDispatchConcurrentHandles(t *testing.T) {
	for _, mode := range []string{"same-key", "changed-payload", "different-key"} {
		t.Run(mode, func(t *testing.T) {
			f := requestDispatchSetup(t)
			second := queueHandle(t, f.path, false)
			apis := []requestDispatchAPI{requestDispatchStore(t, f.s), requestDispatchStore(t, second)}
			type result struct {
				index int
				value domain.DispatchRequest
				err   error
			}
			keys, reasons := []string{"race", "race"}, []string{"安排", "安排"}
			if mode == "changed-payload" {
				reasons[1] += " "
			} else if mode == "different-key" {
				keys[1] = "other"
			}
			before := messageSequenceSnapshot(t, f.s)
			start, results := make(chan struct{}), make(chan result, 2)
			for i, api := range apis {
				go func() {
					<-start
					value, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, reasons[i], keys[i])
					results <- result{i, value, err}
				}()
			}
			close(start)
			a, b := <-results, <-results
			if mode == "same-key" {
				if a.err != nil || b.err != nil || a.value != b.value {
					t.Fatalf("identical concurrent receipt diverged: %+v / %+v", a, b)
				}
			} else {
				if a.err != nil {
					a, b = b, a
				}
				if a.err != nil {
					t.Fatalf("no concurrent winner: %+v / %+v", a, b)
				}
				requestDispatchZero(t, b.value, b.err, ErrConflict)
			}
			requestDispatchOnlyRows(t, before, messageSequenceSnapshot(t, second))
			committed := messageSequenceSnapshot(t, second)
			if got, err := apis[a.index].RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, reasons[a.index], keys[a.index]); err != nil || got != a.value {
				t.Fatalf("winner lost immutable receipt: %+v, %v", got, err)
			}
			messageSequencePreserved(t, second, committed)
			if mode == "different-key" {
				rows := coordinationAdmissionFixture(t, second, domain.ThreadRef{GroupID: f.groupID})
				if err := coordinationAdmissionCommit(second, rows); err != nil {
					t.Fatal(err)
				}
				if _, err := apis[b.index].RequestDispatch(ctx, f.groupID, rows.current[0].(string), 1, 1, "恢复", keys[b.index]); err != nil {
					t.Fatalf("loser key was burned: %v", err)
				}
			}
			dispatchAssertFK(t, second)
		})
	}
}

func TestRequestDispatchUnfinishedSlotIncludesHeldAndEnqueued(t *testing.T) {
	for _, status := range []string{"pending", "held", "enqueued"} {
		t.Run(status, func(t *testing.T) {
			f := requestDispatchSetup(t)
			api := requestDispatchStore(t, f.s)
			original, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "安排", "original")
			if err != nil {
				t.Fatal(err)
			}
			rows := requestDispatchSavedRows(t, f.s, original.ID)
			if status == "held" {
				coordinatorMustCommit(t, f.s, coordinatorSQL{`UPDATE coordination_dispatch_state
				 SET generation=generation+1,status='held',held_reason='task_stale' WHERE request_id=?`, []any{original.ID}})
			} else if status == "enqueued" {
				coordinatorMustCommit(t, f.s, dispatchUpdate(dispatchReceipt(t, f.s, rows)))
			}
			second := queueHandle(t, f.path, false)
			before := messageSequenceSnapshot(t, f.s)
			got, err := requestDispatchStore(t, second).RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "另一安排", "new-key")
			requestDispatchZero(t, got, err, ErrConflict)
			messageSequencePreserved(t, f.s, before)
			messageSequencePreserved(t, second, before)
			if got, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "安排", "original"); err != nil || got != original {
				t.Fatalf("state contaminated immutable replay: %+v, %v", got, err)
			}
			if view, err := api.GetDispatchRequest(ctx, f.groupID, original.ID); err != nil || string(view.State.Status) != status {
				t.Fatalf("wrong current state: %+v, %v", view, err)
			}
			messageSequencePreserved(t, second, before)
		})
	}
}

func TestRequestDispatchEveryWriteAndCommitFailureRollsBack(t *testing.T) {
	for _, phase := range []string{"request", "state", "receipt", "ignore-request", "ignore-state", "ignore-receipt", "commit"} {
		t.Run(phase, func(t *testing.T) {
			f := requestDispatchSetup(t)
			api := requestDispatchStore(t, f.s)
			observer := queueHandle(t, f.path, false)
			part := strings.TrimPrefix(phase, "ignore-")
			table := map[string]string{"request": "coordination_dispatch_requests", "state": "coordination_dispatch_state", "receipt": "mutation_idempotency", "commit": "mutation_idempotency"}[part]
			timing, body := "AFTER", "SELECT RAISE(ABORT,'注入分派语句错误');"
			if strings.HasPrefix(phase, "ignore-") {
				timing, body = "BEFORE", "SELECT RAISE(IGNORE);"
			} else if phase == "commit" {
				if _, err := f.s.db.Exec("CREATE TABLE request_dispatch_commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)"); err != nil {
					t.Fatal(err)
				}
				body = "INSERT INTO request_dispatch_commit_guard VALUES('missing');"
			}
			if _, err := f.s.db.Exec("CREATE TRIGGER request_dispatch_fault " + timing + " INSERT ON " + table + " BEGIN " + body + " END"); err != nil {
				t.Fatal(err)
			}
			before := messageSequenceSnapshot(t, f.s)
			got, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "安排", "retryable")
			requestDispatchZero(t, got, err, nil)
			if strings.HasPrefix(phase, "ignore-") {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("ignored required write not classified as conflict: %v", err)
				}
			} else {
				var sqliteErr *sqlite.Error
				if !errors.As(err, &sqliteErr) {
					t.Fatalf("lost underlying SQLite evidence: %v", err)
				}
				if phase == "commit" {
					coordinationAdmissionRequireFK(t, err)
					if !strings.Contains(err.Error(), "保存分派请求失败") {
						t.Fatalf("expected failure at COMMIT: %v", err)
					}
				} else if !strings.Contains(err.Error(), "注入分派语句错误") {
					t.Fatalf("lost trigger error: %v", err)
				}
			}
			messageSequencePreserved(t, f.s, before)
			messageSequencePreserved(t, observer, before)
			if _, err := f.s.db.Exec("DROP TRIGGER request_dispatch_fault"); err != nil {
				t.Fatal(err)
			}
			// Changed payload detects a burned receipt, even if the request rows rolled back.
			if _, err := requestDispatchStore(t, observer).RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "恢复后安排", "retryable"); err != nil {
				t.Fatalf("failed command burned key/state: %v", err)
			}
			requestDispatchOnlyRows(t, before, messageSequenceSnapshot(t, observer))
		})
	}
}

func TestRequestDispatchExactCurrentMandateJoin(t *testing.T) {
	for _, change := range []string{"topic", "mandate", "group", "missing-coordination", "admission-revision"} {
		t.Run(change, func(t *testing.T) {
			f := requestDispatchSetup(t)
			api := requestDispatchStore(t, f.s)
			other := coordinationAdmissionFixture(t, f.s, domain.ThreadRef{GroupID: f.groupID})
			if err := coordinationAdmissionCommit(f.s, other); err != nil {
				t.Fatal(err)
			}
			// Simulate a damaged current projection; the reader must not loosen the exact join.
			if _, err := f.s.db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
				t.Fatal(err)
			}
			var err error
			switch change {
			case "topic":
				_, err = f.s.db.Exec("UPDATE task_coordination SET topic_id='wrong' WHERE task_id=?", f.task.ID)
			case "mandate":
				_, err = f.s.db.Exec("UPDATE task_coordination SET current_mandate_id=? WHERE task_id=?", other.mandate[0], f.task.ID)
			case "group":
				_, err = f.s.db.Exec("UPDATE task_coordination SET group_id=? WHERE task_id=?", group(t, f.s).ID, f.task.ID)
			case "missing-coordination":
				_, err = f.s.db.Exec("DELETE FROM task_coordination WHERE task_id=?", f.task.ID)
			case "admission-revision":
				if _, err = f.s.db.Exec("DROP TRIGGER routing_decisions_no_update"); err == nil {
					_, err = f.s.db.Exec("UPDATE routing_decisions SET task_revision_at_admission=2 WHERE id=?", f.admission.Decision.ID)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.db.Exec("PRAGMA foreign_keys=ON"); err != nil {
				t.Fatal(err)
			}
			before := messageSequenceSnapshot(t, f.s)
			got, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "安排", "broken-projection")
			requestDispatchZero(t, got, err, ErrNotFound)
			messageSequencePreserved(t, f.s, before)
		})
	}
}

func TestRequestDispatchLivePolicyWorkersMustStillBelongToGroup(t *testing.T) {
	f := requestDispatchSetup(t)
	api := requestDispatchStore(t, f.s)
	other := group(t, f.s)
	if _, err := f.s.db.Exec("UPDATE members SET group_id=? WHERE id=?", other.ID, f.worker); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	got, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "安排", "foreign-worker")
	requestDispatchZero(t, got, err, ErrConflict)
	messageSequencePreserved(t, f.s, before)
}

func TestRequestDispatchPreservesUnknownCancelAndResources(t *testing.T) {
	old, path, groupID, coord, worker, check := coordinatorV10Fixture(t)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s := queueHandle(t, path, false)
	check(s)
	if _, err := s.SetCoordinator(ctx, groupID, coord, 0, "binding"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDispatchPolicy(ctx, groupID, []string{worker}, 0, "policy"); err != nil {
		t.Fatal(err)
	}
	rows := coordinationAdmissionFixture(t, s, domain.ThreadRef{GroupID: groupID})
	if err := coordinationAdmissionCommit(s, rows); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, s)
	if len(before["resource_claims"]) < 2 || len(before["turns"]) < 2 {
		t.Fatal("fixture lacks real uncertain execution and held resources")
	}
	api := requestDispatchStore(t, s)
	request, err := api.RequestDispatch(ctx, groupID, rows.current[0].(string), 1, 1, "仅安排协调", "unknown-safe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.GetDispatchRequest(ctx, groupID, request.ID); err != nil {
		t.Fatal(err)
	}
	requestDispatchOnlyRows(t, before, messageSequenceSnapshot(t, s))
	check(s)
	committed := messageSequenceSnapshot(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, path, false)
	check(reopened)
	messageSequencePreserved(t, reopened, committed)
	if got, err := requestDispatchStore(t, reopened).RequestDispatch(ctx, groupID, rows.current[0].(string), 1, 1, "仅安排协调", "unknown-safe"); err != nil || !reflect.DeepEqual(got, request) {
		t.Fatalf("reopen original receipt: %+v, %v", got, err)
	}
}

func TestRequestDispatchClosedStorageReturnsZero(t *testing.T) {
	f := requestDispatchSetup(t)
	api := requestDispatchStore(t, f.s)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "安排", "closed")
	requestDispatchZero(t, got, err, nil)
	view, err := api.GetDispatchRequest(ctx, f.groupID, "missing")
	requestDispatchZero(t, view, err, nil)
}
