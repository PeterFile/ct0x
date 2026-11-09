package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func TestDeferAssignmentSixStagesAndReopen(t *testing.T) {
	f, r, d, _ := assignmentPending(t)
	id := d.Assignment.ID
	before := coordinationDecideSnapshot(t, f.s)
	for i, seconds := range []int{2, 4, 8, 16, 32, 60, 60} {
		gen := int64(i + 1)
		at := materializeAt.Add(time.Duration(i) * time.Minute)
		code := DispatchTransientSQLiteBusy
		if i%2 == 1 {
			code = DispatchTransientSQLiteLocked
		}
		if err := f.s.DeferAssignment(ctx, id, gen, at, code); err != nil {
			t.Fatalf("stage %d: %v", i, err)
		}
		view, err := f.s.GetAssignment(ctx, r.GroupID, id)
		if err != nil || view.Dispatch.Status != domain.AssignmentDispatchStatusPending || view.Dispatch.Generation != gen+1 || view.Dispatch.AttemptCount != gen || view.Dispatch.NextAttemptAt != at.Add(time.Duration(seconds)*time.Second) || view.Dispatch.UpdatedAt != at || view.Dispatch.LastErrorCode != string(code) || view.Dispatch.HeldReason != "" || view.Dispatch.Delivery != nil {
			t.Fatalf("bad durable backoff: %+v, %v", view, err)
		}
		if err := f.s.DeferAssignment(ctx, id, gen, at, code); !errors.Is(err, ErrConflict) {
			t.Fatalf("old error overwrote new generation: %v", err)
		}
	}
	delete(before, "coordination_assignment_dispatch")
	coordinationDecidePreservedSubset(t, f.s, before)
	frozen := coordinationDecideSnapshot(t, f.s)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, f.path, false)
	coordinationDecidePreserved(t, reopened, frozen)
	if err := reopened.DeferAssignment(ctx, id, 1, materializeAt, DispatchTransientSQLiteBusy); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale error after reopen: %v", err)
	}
	// Due time belongs to future composition, not this trusted single primitive.
	got, err := reopened.MaterializeAssignment(ctx, id, 8, materializeAt)
	if err != nil {
		t.Fatal(err)
	}
	assignmentAssertReceipt(t, reopened, r, *d.Assignment, got, 8, materializeAt)
	view, err := reopened.GetAssignment(ctx, r.GroupID, id)
	if err != nil || view.Dispatch.AttemptCount != 7 || view.Dispatch.NextAttemptAt != materializeAt.Add(7*time.Minute) {
		t.Fatalf("materialization changed backoff metadata: %+v, %v", view, err)
	}
	frozen = coordinationDecideSnapshot(t, reopened)
	for _, gen := range []int64{1, 8, 9} {
		if err := reopened.DeferAssignment(ctx, id, gen, materializeAt, DispatchTransientSQLiteLocked); !errors.Is(err, ErrConflict) {
			t.Fatalf("late defer overwrote success: %v", err)
		}
	}
	coordinationDecidePreserved(t, reopened, frozen)
}

func TestDeferAssignmentValidationAndOverflow(t *testing.T) {
	f, _, d, _ := assignmentPending(t)
	id := d.Assignment.ID
	before := coordinationDecideSnapshot(t, f.s)
	for _, code := range []DispatchTransientCode{"", "SQLITE_BUSY", "native_unknown", "constraint"} {
		if err := f.s.DeferAssignment(ctx, id, 1, materializeAt, code); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid code accepted: %v", err)
		}
	}
	for _, id := range []string{"", " padded", "id\x00", "id\n", "\xff"} {
		if err := f.s.DeferAssignment(ctx, id, 1, materializeAt, DispatchTransientSQLiteBusy); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid ID accepted: %v", err)
		}
	}
	for _, gen := range []int64{0, -1} {
		if err := f.s.DeferAssignment(ctx, id, gen, materializeAt, DispatchTransientSQLiteBusy); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid generation: %v", err)
		}
	}
	for _, at := range []time.Time{time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)} {
		if err := f.s.DeferAssignment(ctx, id, 1, at, DispatchTransientSQLiteBusy); !errors.Is(err, ErrInvalid) {
			t.Fatalf("date overflow: %v", err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := f.s.DeferAssignment(cancelled, id, 1, materializeAt, DispatchTransientSQLiteBusy); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	if err := f.s.DeferAssignment(ctx, "missing", 1, materializeAt, DispatchTransientSQLiteBusy); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing ID: %v", err)
	}
	coordinationDecidePreserved(t, f.s, before)
	for _, mode := range []string{"generation", "attempt", "last-attempt"} {
		t.Run(mode, func(t *testing.T) {
			f, _, d, _ := assignmentPending(t)
			gen, attempt := int64(1), int64(math.MaxInt64)
			if mode == "generation" {
				gen, attempt = math.MaxInt64, 0
			}
			if mode == "last-attempt" {
				attempt--
			}
			// Counterfactual boundary setup only, not a normal intent-creation path.
			coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_assignment_dispatch_update")
			code := ""
			if attempt > 0 {
				code = "sqlite_busy"
			}
			coordinationDecideExec(t, f.s, "UPDATE coordination_assignment_dispatch SET generation=?,attempt_count=?,last_error_code=? WHERE assignment_id=?", gen, attempt, code, d.Assignment.ID)
			before := coordinationDecideSnapshot(t, f.s)
			err := f.s.DeferAssignment(ctx, d.Assignment.ID, gen, materializeAt, DispatchTransientSQLiteBusy)
			if mode == "last-attempt" {
				view, readErr := f.s.GetAssignment(ctx, f.groupID, d.Assignment.ID)
				if err != nil || readErr != nil || view.Dispatch.AttemptCount != math.MaxInt64 || view.Dispatch.NextAttemptAt != materializeAt.Add(time.Minute) {
					t.Fatalf("capped duration overflowed: %+v, %v / %v", view, err, readErr)
				}
			} else {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("counter overflow: %v", err)
				}
				coordinationDecidePreserved(t, f.s, before)
				if mode == "generation" {
					got, err := f.s.MaterializeAssignment(ctx, d.Assignment.ID, gen, materializeAt)
					requestDispatchZero(t, got, err, ErrInvalid)
					coordinationDecidePreserved(t, f.s, before)
				}
			}
		})
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeferAssignment(ctx, id, 1, materializeAt, DispatchTransientSQLiteBusy); err == nil {
		t.Fatal("closed DB returned success")
	}
}

func TestDeferAssignmentVersusSuccess(t *testing.T) {
	f, r, d, _ := assignmentPending(t)
	second := queueHandle(t, f.path, false)
	// This auxiliary test uses an explicit 60s SQLite wait budget, not the 5s default.
	for _, handle := range []*Store{f.s, second} {
		coordinationDecideExec(t, handle, "PRAGMA busy_timeout=60000")
	}
	type result struct {
		v   domain.AssignmentMaterialization
		err error
	}
	start := make(chan struct{})
	materialized := make(chan result, 1)
	deferred := make(chan error, 1)
	go func() {
		<-start
		v, err := f.s.MaterializeAssignment(ctx, d.Assignment.ID, 1, materializeAt)
		materialized <- result{v, err}
	}()
	go func() {
		<-start
		deferred <- second.DeferAssignment(ctx, d.Assignment.ID, 1, materializeAt, DispatchTransientSQLiteBusy)
	}()
	close(start)
	m, err := <-materialized, <-deferred
	if m.err == nil {
		assignmentAssertReceipt(t, second, r, *d.Assignment, m.v, 1, materializeAt)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("defer failed outside the expected CAS conflict: %v, cause=%v", err, errors.Unwrap(err))
		}
	} else {
		if !errors.Is(m.err, ErrConflict) {
			t.Fatalf("materialization failed outside the expected CAS conflict: %v, cause=%v", m.err, errors.Unwrap(m.err))
		}
		requestDispatchZero(t, m.v, m.err, ErrConflict)
		if err != nil {
			t.Fatalf("neither CAS won: %v, cause=%v", err, errors.Unwrap(err))
		}
		view, err := second.GetAssignment(ctx, r.GroupID, d.Assignment.ID)
		if err != nil || view.Dispatch.Generation != 2 || view.Dispatch.AttemptCount != 1 || view.Dispatch.Delivery != nil {
			t.Fatalf("defer winner: %+v, %v", view, err)
		}
		got, err := second.MaterializeAssignment(ctx, d.Assignment.ID, 2, materializeAt)
		if err != nil {
			t.Fatal(err)
		}
		assignmentAssertReceipt(t, second, r, *d.Assignment, got, 2, materializeAt)
	}
	before := coordinationDecideSnapshot(t, second)
	if err := second.DeferAssignment(ctx, d.Assignment.ID, 1, materializeAt, DispatchTransientSQLiteLocked); !errors.Is(err, ErrConflict) {
		t.Fatalf("old error changed receipt: %v", err)
	}
	coordinationDecidePreserved(t, second, before)
}

func TestDeferAssignmentHeldNeverRecovers(t *testing.T) {
	f, _, d, _ := assignmentPending(t)
	materializeChangeEligibility(t, f, "worker-left")
	got, err := f.s.MaterializeAssignment(ctx, d.Assignment.ID, 1, materializeAt)
	if err != nil || got.Disposition != "held" {
		t.Fatalf("fixture not held: %+v %v", got, err)
	}
	coordinationDecideExec(t, f.s, "UPDATE members SET status='active' WHERE id=?", f.worker)
	before := coordinationDecideSnapshot(t, f.s)
	for _, gen := range []int64{1, 2} {
		if err := f.s.DeferAssignment(ctx, d.Assignment.ID, gen, materializeAt, DispatchTransientSQLiteBusy); !errors.Is(err, ErrConflict) {
			t.Fatalf("held auto recovery: %v", err)
		}
	}
	coordinationDecidePreserved(t, f.s, before)
}

func TestDeferAssignmentFailuresAndCommitUnknown(t *testing.T) {
	for _, mode := range []string{"abort", "ignore", "mutate", "commit", "refuse", "ack-error"} {
		t.Run(mode, func(t *testing.T) {
			f, r, d, _ := assignmentPending(t)
			s := f.s
			observer := queueHandle(t, f.path, false)
			if mode == "refuse" || mode == "ack-error" {
				s, _ = coordinationDecideWrapped(t, f.path, mode, errors.Join(errors.New("PRIVATE COMMIT"), sql.ErrNoRows))
			} else {
				timing, body := "AFTER", "SELECT RAISE(ABORT,'PRIVATE FAULT');"
				if mode == "ignore" {
					timing, body = "BEFORE", "SELECT RAISE(IGNORE);"
				}
				if mode == "mutate" {
					body = "UPDATE coordination_assignment_dispatch SET generation=generation+1,attempt_count=attempt_count+1,last_error_code='sqlite_locked',next_attempt_at=strftime('%Y-%m-%dT%H:%M:%S',substr(updated_at,1,19)||'Z','+4 seconds')||substr(updated_at,20) WHERE assignment_id=NEW.assignment_id;"
				}
				if mode == "commit" {
					coordinationDecideExec(t, f.s, "CREATE TABLE assignment_defer_commit(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)")
					body = "INSERT INTO assignment_defer_commit VALUES('missing');"
				}
				coordinationDecideExec(t, f.s, "CREATE TRIGGER assignment_defer_fault "+timing+" UPDATE ON coordination_assignment_dispatch BEGIN "+body+" END")
			}
			before := coordinationDecideSnapshot(t, observer)
			err := s.DeferAssignment(ctx, d.Assignment.ID, 1, materializeAt, DispatchTransientSQLiteBusy)
			if err == nil {
				t.Fatal("failed write/ack returned success")
			}
			var failure interface {
				Phase() string
				CommitOutcomeUnknown() bool
				RollbackConfirmed() bool
			}
			if !errors.As(err, &failure) {
				t.Fatalf("missing phase/outcome safety type: %v", err)
			}
			unknown := mode == "commit" || mode == "refuse" || mode == "ack-error"
			if failure.CommitOutcomeUnknown() != unknown || unknown && (failure.Phase() != "commit" || failure.RollbackConfirmed()) {
				t.Fatalf("wrong phase/rollback evidence: %+v", failure)
			}
			if mode == "refuse" || mode == "ack-error" {
				if !errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
					t.Fatal("commit cause incorrectly normalized to not found")
				}
			}
			if mode == "ack-error" {
				view, readErr := observer.GetAssignment(ctx, r.GroupID, d.Assignment.ID)
				if readErr != nil || view.Dispatch.Generation != 2 || view.Dispatch.AttemptCount != 1 {
					t.Fatalf("lost postcommit state: %+v, %v", view, readErr)
				}
				frozen := coordinationDecideSnapshot(t, observer)
				if err := observer.DeferAssignment(ctx, d.Assignment.ID, 1, materializeAt, DispatchTransientSQLiteLocked); !errors.Is(err, ErrConflict) {
					t.Fatalf("original-generation reconciliation overwrote committed backoff: %v", err)
				}
				coordinationDecidePreserved(t, observer, frozen)
			} else {
				coordinationDecidePreserved(t, observer, before)
			}
		})
	}
}

func TestDeferAssignmentDefaultBusyPending(t *testing.T) {
	f, r, d, _ := assignmentPending(t)
	second := assignmentDefaultHandle(t, f.path)
	observer := assignmentDefaultHandle(t, f.path)
	before := coordinationDecideSnapshot(t, observer)
	l := &assignmentTimeline{}
	defer l.log(t)
	assignmentObserveOpen(t, f.s, l, "locker", nil)
	assignmentObserveOpen(t, second, l, "pending", nil)
	l.record("locker.begin_call")
	lock, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	l.record("locker.begin_return")
	// No statement or write is executed in this transaction. Real contention
	// therefore verifies that Store.Open's BEGIN already reserved the write lock.
	watch := assignmentWatchBusy(t, l, "pending")
	contender := assignmentStartMaterialize(t, second, watch, d.Assignment.ID, l, "pending")
	assignmentWait(t, watch.observed)
	failed := assignmentWait(t, contender)
	assignmentRequireKnownBusy(t, failed)
	if !l.first("pending.first_source_read").IsZero() {
		t.Fatal("failed BEGIN executed source SQL")
	}
	coordinationDecidePreserved(t, observer, before)
	l.record("locker.release_call")
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	l.record("locker.release_return")
	l.ordered(t, "locker.begin_prepare", "locker.begin_return", "pending.begin_prepare", "pending.busy_handler", "pending.call_return", "locker.release_call", "locker.rollback_hook", "locker.release_return")
	t.Logf("actual_immediate_lock_begin_return_to_release=%s busy_callback_to_error=%s", l.first("locker.release_return").Sub(l.first("locker.begin_return")), l.first("pending.call_return").Sub(l.first("pending.busy_handler")))
	// The error's known no-transaction fact above is the precondition; the raw
	// SQLite code alone is not permission to persist a backoff.
	code, ok := ClassifyDispatchTransient(failed.err)
	if !ok {
		t.Fatal("lost real BUSY cause")
	}
	if err := second.DeferAssignment(ctx, d.Assignment.ID, 1, materializeAt, code); err != nil {
		t.Fatal(err)
	}
	view, err := second.GetAssignment(ctx, r.GroupID, d.Assignment.ID)
	if err != nil || view.Dispatch.Status != domain.AssignmentDispatchStatusPending || view.Dispatch.Generation != 2 || view.Dispatch.AttemptCount != 1 || view.Dispatch.NextAttemptAt != materializeAt.Add(2*time.Second) || view.Dispatch.LastErrorCode != string(code) || view.Dispatch.Delivery != nil {
		t.Fatalf("known rollback did not persist exactly one backoff: %+v %v", view, err)
	}
	delete(before, "coordination_assignment_dispatch")
	coordinationDecidePreservedSubset(t, observer, before)
	frozen := coordinationDecideSnapshot(t, observer)
	for _, s := range []*Store{f.s, second, observer} {
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	reopened := assignmentDefaultHandle(t, f.path)
	coordinationDecidePreserved(t, reopened, frozen)
	if err := reopened.DeferAssignment(ctx, d.Assignment.ID, 1, materializeAt, code); !errors.Is(err, ErrConflict) {
		t.Fatalf("old BUSY persisted backoff twice: %v", err)
	}
	coordinationDecidePreserved(t, reopened, frozen)
	dispatchAssertFK(t, reopened)
}

func TestDeferAssignmentRealBusyRequiresRollbackEvidence(t *testing.T) {
	f, _, d, _ := assignmentPending(t)
	second := queueHandle(t, f.path, false)
	coordinationDecideExec(t, second, "PRAGMA busy_timeout=0")
	before := coordinationDecideSnapshot(t, f.s)
	lock, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	got, busy := second.MaterializeAssignment(ctx, d.Assignment.ID, 1, materializeAt)
	requestDispatchZero(t, got, busy, nil)
	dispatchTransientRequire(t, busy, 5, DispatchTransientSQLiteBusy)
	var failure interface {
		Phase() string
		CommitOutcomeUnknown() bool
		RollbackConfirmed() bool
	}
	if !errors.As(busy, &failure) || failure.Phase() != "begin" || failure.CommitOutcomeUnknown() || !failure.RollbackConfirmed() {
		t.Fatalf("begin failure lacks no-transaction evidence: %v", busy)
	}
	if err := second.DeferAssignment(ctx, d.Assignment.ID, 1, materializeAt, DispatchTransientSQLiteBusy); err == nil {
		t.Fatal("busy DB falsely persisted backoff")
	}
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	coordinationDecidePreserved(t, f.s, before)
	code, ok := ClassifyDispatchTransient(busy)
	if !ok {
		t.Fatal("lost busy cause")
	}
	if err := second.DeferAssignment(ctx, d.Assignment.ID, 1, materializeAt, code); err != nil {
		t.Fatal(err)
	}
	delete(before, "coordination_assignment_dispatch")
	coordinationDecidePreservedSubset(t, second, before)
}
