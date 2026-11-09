package store

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"modernc.org/sqlite"
)

type deferDispatchAPI interface {
	DeferDispatchRequest(context.Context, string, int64, time.Time, DispatchTransientCode) error
}

func deferDispatchStore(t *testing.T, s *Store) deferDispatchAPI {
	t.Helper()
	api, ok := any(s).(deferDispatchAPI)
	if !ok {
		t.Fatal("Store lacks generation-fenced persistent DispatchRequest backoff")
	}
	return api
}

func TestDeferDispatchSixStagesPersistAcrossReopen(t *testing.T) {
	f, r := materializePending(t)
	api := deferDispatchStore(t, f.s)
	before := messageSequenceSnapshot(t, f.s)
	for i, seconds := range []int{2, 4, 8, 16, 32, 60, 60} {
		gen := int64(i + 1)
		at := materializeAt.Add(time.Duration(i) * time.Minute)
		code := DispatchTransientSQLiteBusy
		if i%2 == 1 {
			code = DispatchTransientSQLiteLocked
		}
		if err := api.DeferDispatchRequest(ctx, r.ID, gen, at, code); err != nil {
			t.Fatalf("stage %d: %v", i, err)
		}
		v, err := f.s.GetDispatchRequest(ctx, r.GroupID, r.ID)
		if err != nil || v.State.Status != domain.DispatchStatusPending || v.State.Generation != gen+1 || v.State.AttemptCount != gen || v.State.NextAttemptAt != at.Add(time.Duration(seconds)*time.Second) || v.State.LastErrorCode != string(code) || v.State.HeldReason != "" || v.State.Delivery != nil {
			t.Fatalf("stage %d was not persisted: %+v, %v", i, v, err)
		}
		if err := api.DeferDispatchRequest(ctx, r.ID, gen, at, code); !errors.Is(err, ErrConflict) {
			t.Fatalf("old completion overwrote new generation: %v", err)
		}
	}
	delete(before, "coordination_dispatch_state")
	messageSequencePreserved(t, f.s, before)
	committed := messageSequenceSnapshot(t, f.s)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, f.path, false)
	messageSequencePreserved(t, reopened, committed)
	stale, err := materializeAPI(t, reopened).MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	requestDispatchZero(t, stale, err, ErrConflict)
	messageSequencePreserved(t, reopened, committed)
	if err := deferDispatchStore(t, reopened).DeferDispatchRequest(ctx, r.ID, 1, materializeAt, DispatchTransientSQLiteBusy); !errors.Is(err, ErrConflict) {
		t.Fatalf("old error survived reopen as a new grant: %v", err)
	}
	// A single trusted primitive has no implicit due-time gate; a future pass owns that decision.
	got, err := materializeAPI(t, reopened).MaterializeDispatchRequest(ctx, r.ID, 8, materializeAt)
	if err != nil {
		t.Fatal(err)
	}
	materializeAssertReceipt(t, reopened, r, got, 8, materializeAt)
	committed = messageSequenceSnapshot(t, reopened)
	stale, err = materializeAPI(t, reopened).MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	requestDispatchZero(t, stale, err, ErrConflict)
	messageSequencePreserved(t, reopened, committed)
	if err := deferDispatchStore(t, reopened).DeferDispatchRequest(ctx, r.ID, 8, materializeAt, DispatchTransientSQLiteLocked); !errors.Is(err, ErrConflict) {
		t.Fatalf("late transient error overwrote a committed receipt: %v", err)
	}
}

func TestDeferDispatchValidationOverflowAndStorageFailureAreZeroWrite(t *testing.T) {
	f, r := materializePending(t)
	api := deferDispatchStore(t, f.s)
	before := messageSequenceSnapshot(t, f.s)
	for _, code := range []DispatchTransientCode{"", "SQLITE_BUSY", "native_unknown", "constraint"} {
		if err := api.DeferDispatchRequest(ctx, r.ID, 1, materializeAt, code); !errors.Is(err, ErrInvalid) {
			t.Fatalf("untrusted code accepted: %q, %v", code, err)
		}
	}
	for _, id := range []string{"", " id", "id\x00", "id\n", "\xff"} {
		if err := api.DeferDispatchRequest(ctx, id, 1, materializeAt, DispatchTransientSQLiteBusy); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid ID accepted: %q, %v", id, err)
		}
	}
	for _, gen := range []int64{0, -1} {
		if err := api.DeferDispatchRequest(ctx, r.ID, gen, materializeAt, DispatchTransientSQLiteBusy); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid generation accepted: %d, %v", gen, err)
		}
	}
	for _, at := range []time.Time{time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)} {
		if err := api.DeferDispatchRequest(ctx, r.ID, 1, at, DispatchTransientSQLiteBusy); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid/overflowing date accepted: %s, %v", at, err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := api.DeferDispatchRequest(cancelled, r.ID, 1, materializeAt, DispatchTransientSQLiteBusy); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if err := api.DeferDispatchRequest(ctx, "missing", 1, materializeAt, DispatchTransientSQLiteBusy); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing request: %v", err)
	}
	messageSequencePreserved(t, f.s, before)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := api.DeferDispatchRequest(ctx, r.ID, 1, materializeAt, DispatchTransientSQLiteBusy); err == nil {
		t.Fatal("closed database falsely persisted backoff")
	}
}

func TestDeferDispatchCountersCannotOverflow(t *testing.T) {
	for _, mode := range []string{"generation", "attempt", "last-attempt"} {
		t.Run(mode, func(t *testing.T) {
			f, r := materializePending(t)
			api := deferDispatchStore(t, f.s)
			if _, err := f.s.db.Exec("DROP TRIGGER coordination_dispatch_state_update"); err != nil {
				t.Fatal(err)
			}
			gen, attempt := int64(1), int64(math.MaxInt64)
			if mode == "generation" {
				gen, attempt = math.MaxInt64, 0
			} else if mode == "last-attempt" {
				attempt--
			}
			code := ""
			if attempt > 0 {
				code = "sqlite_busy"
			}
			if _, err := f.s.db.Exec("UPDATE coordination_dispatch_state SET generation=?,attempt_count=?,last_error_code=? WHERE request_id=?", gen, attempt, code, r.ID); err != nil {
				t.Fatal(err)
			}
			before := messageSequenceSnapshot(t, f.s)
			err := api.DeferDispatchRequest(ctx, r.ID, gen, materializeAt, DispatchTransientSQLiteBusy)
			if mode != "last-attempt" {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("counter overflow was not rejected: %v", err)
				}
				messageSequencePreserved(t, f.s, before)
				if mode == "generation" {
					got, err := materializeAPI(t, f.s).MaterializeDispatchRequest(ctx, r.ID, gen, materializeAt)
					requestDispatchZero(t, got, err, ErrInvalid)
					messageSequencePreserved(t, f.s, before)
				}
			} else {
				v, readErr := f.s.GetDispatchRequest(ctx, r.GroupID, r.ID)
				if err != nil || readErr != nil || v.State.AttemptCount != math.MaxInt64 || v.State.NextAttemptAt != materializeAt.Add(time.Minute) {
					t.Fatalf("capped backoff overflowed duration: %+v, %v / %v", v, err, readErr)
				}
			}
		})
	}
}

func TestDeferDispatchVersusSuccessfulMaterialization(t *testing.T) {
	f, r := materializePending(t)
	materialize := materializeAPI(t, f.s)
	second := queueHandle(t, f.path, false)
	deferAPI := deferDispatchStore(t, second)
	type result struct {
		value domain.DispatchMaterialization
		err   error
	}
	start, completed, deferred := make(chan struct{}), make(chan result, 1), make(chan error, 1)
	go func() {
		<-start
		value, err := materialize.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
		completed <- result{value, err}
	}()
	go func() {
		<-start
		deferred <- deferAPI.DeferDispatchRequest(ctx, r.ID, 1, materializeAt, DispatchTransientSQLiteBusy)
	}()
	close(start)
	m, d := <-completed, <-deferred
	if m.err == nil {
		materializeAssertReceipt(t, second, r, m.value, 1, materializeAt)
		if !errors.Is(d, ErrConflict) {
			t.Fatalf("defer overwrote success: %v", d)
		}
	} else {
		requestDispatchZero(t, m.value, m.err, ErrConflict)
		v, err := second.GetDispatchRequest(ctx, r.GroupID, r.ID)
		if d != nil || err != nil || v.State.Generation != 2 || v.State.AttemptCount != 1 || v.State.Status != domain.DispatchStatusPending || v.State.Delivery != nil {
			t.Fatalf("defer winner was not preserved: %+v, %v / %v", v, d, err)
		}
		got, err := materialize.MaterializeDispatchRequest(ctx, r.ID, 2, materializeAt)
		if err != nil {
			t.Fatal(err)
		}
		materializeAssertReceipt(t, second, r, got, 2, materializeAt)
	}
	before := messageSequenceSnapshot(t, f.s)
	if err := deferAPI.DeferDispatchRequest(ctx, r.ID, 1, materializeAt, DispatchTransientSQLiteLocked); !errors.Is(err, ErrConflict) {
		t.Fatalf("late error changed a later generation: %v", err)
	}
	messageSequencePreserved(t, second, before)
}

func TestDeferDispatchWriteAndCommitFailuresRollback(t *testing.T) {
	for _, mode := range []string{"abort", "ignore", "commit", "mutate"} {
		t.Run(mode, func(t *testing.T) {
			f, r := materializePending(t)
			api := deferDispatchStore(t, f.s)
			timing, body := "AFTER", "SELECT RAISE(ABORT,'注入退避失败');"
			if mode == "ignore" {
				timing, body = "BEFORE", "SELECT RAISE(IGNORE);"
			} else if mode == "commit" {
				if _, err := f.s.db.Exec("CREATE TABLE defer_commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)"); err != nil {
					t.Fatal(err)
				}
				body = "INSERT INTO defer_commit_guard VALUES('missing');"
			} else if mode == "mutate" {
				body = "UPDATE coordination_dispatch_state SET generation=generation+1,attempt_count=attempt_count+1,last_error_code='sqlite_locked',next_attempt_at=strftime('%Y-%m-%dT%H:%M:%S',substr(updated_at,1,19)||'Z','+4 seconds')||substr(updated_at,20) WHERE request_id=NEW.request_id;"
			}
			if _, err := f.s.db.Exec("CREATE TRIGGER defer_fault " + timing + " UPDATE ON coordination_dispatch_state BEGIN " + body + " END"); err != nil {
				t.Fatal(err)
			}
			before := messageSequenceSnapshot(t, f.s)
			err := api.DeferDispatchRequest(ctx, r.ID, 1, materializeAt, DispatchTransientSQLiteBusy)
			if err == nil {
				t.Fatal("backoff failure returned success")
			}
			if mode == "abort" || mode == "commit" {
				var cause *sqlite.Error
				if !errors.As(err, &cause) {
					t.Fatalf("lost SQLite cause: %v", err)
				}
			} else if !errors.Is(err, ErrConflict) {
				t.Fatalf("unpersisted backoff not a conflict: %v", err)
			}
			messageSequencePreserved(t, f.s, before)
			if _, err := f.s.db.Exec("DROP TRIGGER defer_fault"); err != nil {
				t.Fatal(err)
			}
			if err := api.DeferDispatchRequest(ctx, r.ID, 1, materializeAt, DispatchTransientSQLiteBusy); err != nil {
				t.Fatalf("rollback left request unretryable: %v", err)
			}
			if !reflect.DeepEqual(messageSequenceTableRows(t, f.s, "mutation_idempotency"), before["mutation_idempotency"]) {
				t.Fatal("backoff changed mutation receipts")
			}
		})
	}
}
