package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"modernc.org/sqlite"
)

func materializeTransactionEvidence(t *testing.T, err error, phase string, rolledBack bool) {
	t.Helper()
	var evidence interface {
		Phase() string
		RollbackConfirmed() bool
		CommitOutcomeUnknown() bool
	}
	if !errors.As(err, &evidence) {
		t.Errorf("N2 transaction evidence unavailable: %v", err)
		return
	}
	if evidence.Phase() != phase || evidence.RollbackConfirmed() != rolledBack || evidence.CommitOutcomeUnknown() != (phase == "commit") {
		t.Errorf("phase=%s rollback_confirmed=%t commit_unknown=%t; want %s/%t/%t", evidence.Phase(), evidence.RollbackConfirmed(), evidence.CommitOutcomeUnknown(), phase, rolledBack, phase == "commit")
	}
	t.Logf("N2 phase=%s rollback_confirmed=%t commit_unknown=%t", evidence.Phase(), evidence.RollbackConfirmed(), evidence.CommitOutcomeUnknown())
}

func TestMaterializeDispatchTransactionInputAndBegin(t *testing.T) {
	f, r := materializePending(t)
	before := coordinationDecideSnapshot(t, f.s)
	got, err := f.s.MaterializeDispatchRequest(ctx, "", 1, materializeAt)
	requestDispatchZero(t, got, err, ErrInvalid)
	materializeTransactionEvidence(t, err, "input", true)
	if want := "物化分派请求失败：" + ErrInvalid.Error() + "：分派请求标识或世代无效"; err.Error() != want {
		t.Errorf("input display changed: %q; want %q", err, want)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	got, err = f.s.MaterializeDispatchRequest(cancelled, r.ID, 1, materializeAt)
	requestDispatchZero(t, got, err, context.Canceled)
	materializeTransactionEvidence(t, err, "begin", true)
	if want := "物化分派请求失败：" + context.Canceled.Error(); err.Error() != want {
		t.Errorf("cancelled begin display changed: %q", err)
	}
	coordinationDecidePreserved(t, f.s, before)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	got, err = f.s.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	requestDispatchZero(t, got, err, nil)
	materializeTransactionEvidence(t, err, "begin", true)
}

func TestMaterializeDispatchTransactionRollback(t *testing.T) {
	for _, tc := range []struct {
		name string
		ack  error
	}{
		{"confirmed", nil},
		{"lost-rollback-ack", errors.New("private rollback acknowledgment lost")},
		{"tx-done", sql.ErrTxDone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := materializePending(t)
			wrapped, observed := assignmentBoundaryStore(t, f.path, tc.ack)
			before := coordinationDecideSnapshot(t, f.s)
			got, err := wrapped.MaterializeDispatchRequest(ctx, "missing-dispatch", 1, materializeAt)
			requestDispatchZero(t, got, err, ErrNotFound)
			materializeTransactionEvidence(t, err, "source", tc.ack == nil)
			if err.Error() != "物化分派请求失败："+ErrNotFound.Error() || errors.Is(err, sql.ErrNoRows) {
				t.Errorf("legacy NoRows normalization changed: %v", err)
			}
			if tc.ack != nil && !errors.Is(err, tc.ack) {
				t.Errorf("observed rollback error was discarded: %v", err)
			}
			if len(observed.events) != 1 || observed.events[0].rollbackCalls != 1 || observed.events[0].rollbackErr != nil {
				t.Fatalf("real SQLite rollback was not observed: %+v", observed.events)
			}
			// The driver fault loses only the acknowledgment after a real rollback.
			if err := wrapped.Close(); err != nil {
				t.Fatal(err)
			}
			assignmentReopenPreserved(t, f.s, f.path, before)
		})
	}
}

func TestMaterializeDispatchTransactionPrecommit(t *testing.T) {
	for _, mode := range []string{"receipt-abort", "log-ignore"} {
		t.Run(mode, func(t *testing.T) {
			f, r := materializePending(t)
			query := `CREATE TRIGGER dispatch_transaction_fault BEFORE UPDATE ON coordination_dispatch_state
			 BEGIN SELECT RAISE(ABORT,'private dispatch receipt failure'); END`
			phase := "receipt"
			if mode == "log-ignore" {
				query = `CREATE TRIGGER dispatch_transaction_fault BEFORE INSERT ON message_log
				 BEGIN SELECT RAISE(IGNORE); END`
				phase = "readback"
			}
			coordinationDecideExec(t, f.s, query)
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
			requestDispatchZero(t, got, err, nil)
			materializeTransactionEvidence(t, err, phase, true)
			if mode == "receipt-abort" {
				var cause *sqlite.Error
				if !errors.As(err, &cause) || cause.Code()&0xff != 19 {
					t.Fatalf("real SQLite receipt ABORT cause lost: %v", err)
				}
				if err.Error() != "物化分派请求失败："+cause.Error() {
					t.Errorf("precommit display changed: %v", err)
				}
			} else if !errors.Is(err, ErrConflict) {
				t.Errorf("readback conflict sentinel lost: %v", err)
			}
			coordinationDecidePreserved(t, f.s, before)
			observer := queueHandle(t, f.path, false)
			coordinationDecidePreserved(t, observer, before)
			coordinationDecideExec(t, f.s, "DROP TRIGGER dispatch_transaction_fault")
			retried, err := observer.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
			if err != nil {
				t.Fatalf("explicit original-generation retry: %v", err)
			}
			materializeAssertReceipt(t, observer, r, retried, 1, materializeAt)
		})
	}
}

func TestMaterializeDispatchTransactionBusyAndLostAcknowledgment(t *testing.T) {
	for _, mode := range []string{"refuse", "ack-error"} {
		t.Run(mode, func(t *testing.T) {
			f, r := materializePending(t)
			second := queueHandle(t, f.path, false)
			coordinationDecideExec(t, second, "PRAGMA busy_timeout=0")
			before := coordinationDecideSnapshot(t, second)
			lock, err := f.s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback()
			got, beginErr := second.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
			if err := lock.Rollback(); err != nil {
				t.Fatal(err)
			}
			requestDispatchZero(t, got, beginErr, nil)
			materializeTransactionEvidence(t, beginErr, "begin", true)
			var busy *sqlite.Error
			if !errors.As(beginErr, &busy) || busy.Code()&0xff != 5 || beginErr.Error() != "物化分派请求失败："+busy.Error() {
				t.Fatalf("real BEGIN BUSY cause/display lost: %v", beginErr)
			}
			coordinationDecidePreserved(t, second, before)
			// Reuse that exact real SQLite cause at the two observed Commit boundaries.
			wrapped, observed := coordinationDecideWrapped(t, f.path, mode, busy)
			got, err = wrapped.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
			requestDispatchZero(t, got, err, busy)
			materializeTransactionEvidence(t, err, "commit", false)
			var commitCause *sqlite.Error
			if !errors.As(err, &commitCause) || commitCause != busy || err.Error() != "物化分派请求失败：保存分派物化失败："+busy.Error() {
				t.Errorf("Commit display/SQLite cause changed: %v", err)
			}
			if len(observed.begins) != 1 {
				t.Fatalf("unexpected materialization transaction count: %d", len(observed.begins))
			}
			for _, s := range []*Store{wrapped, f.s, second} {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			reopened := queueHandle(t, f.path, false)
			view, err := reopened.GetDispatchRequest(ctx, r.GroupID, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			after := coordinationDecideSnapshot(t, reopened)
			if mode == "refuse" {
				coordinationDecidePreserved(t, reopened, before)
				if view.State.Generation != 1 || view.State.Delivery != nil {
					t.Fatalf("refused Commit persisted a receipt: %+v", view.State)
				}
			} else if view.State.Generation != 2 || view.State.Status != domain.DispatchStatusEnqueued || view.State.Delivery == nil || view.State.AttemptCount != 0 {
				t.Fatalf("committed lost acknowledgment did not persist its receipt: %+v", view.State)
			}
			reconciled, err := reopened.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt.Add(time.Hour))
			if err != nil {
				t.Fatalf("explicit original-generation reconciliation: %v", err)
			}
			if mode == "ack-error" {
				materializeAssertReceipt(t, reopened, r, reconciled, 1, materializeAt)
				if !reflect.DeepEqual(reconciled.Delivery, view.State.Delivery) {
					t.Fatal("lost-ack reconciliation allocated another identity")
				}
				coordinationDecidePreserved(t, reopened, after)
			} else {
				materializeAssertReceipt(t, reopened, r, reconciled, 1, materializeAt.Add(time.Hour))
			}
			final := coordinationDecideSnapshot(t, reopened)
			for _, table := range []string{"messages", "message_log", "deliveries", "conversations"} {
				if len(final[table]) != len(before[table])+1 {
					t.Errorf("explicit reconciliation did not persist exactly one %s row", table)
				}
			}
			replay, err := reopened.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt.Add(2*time.Hour))
			if err != nil || !reflect.DeepEqual(replay, reconciled) {
				t.Fatalf("reconciled receipt replay changed: %+v, %v", replay, err)
			}
			coordinationDecidePreserved(t, reopened, final)
		})
	}
}

func TestMaterializeDispatchTransactionCommitBusy(t *testing.T) {
	f, r := materializePending(t)
	reader := queueHandle(t, f.path, true)
	coordinationDecideExec(t, f.s, "PRAGMA busy_timeout=0")
	before := coordinationDecideSnapshot(t, f.s)
	tx, err := reader.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var generation int64
	if err := tx.QueryRow("SELECT generation FROM coordination_dispatch_state WHERE request_id=?", r.ID).Scan(&generation); err != nil || generation != 1 {
		t.Fatalf("reader did not hold the original snapshot: %d, %v", generation, err)
	}
	got, callErr := f.s.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	requestDispatchZero(t, got, callErr, nil)
	materializeTransactionEvidence(t, callErr, "commit", false)
	var cause *sqlite.Error
	if !errors.As(callErr, &cause) || cause.Code()&0xff != 5 || callErr.Error() != "物化分派请求失败：保存分派物化失败："+cause.Error() {
		t.Fatalf("not a real COMMIT BUSY with legacy display: %v", callErr)
	}
	assignmentReopenPreserved(t, f.s, f.path, before)
}

func TestMaterializeDispatchTransactionDeferredCommit(t *testing.T) {
	f, r := materializePending(t)
	coordinationDecideExec(t, f.s, `CREATE TABLE dispatch_transaction_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED);
	 CREATE TRIGGER dispatch_transaction_fault AFTER UPDATE ON coordination_dispatch_state
	 BEGIN INSERT INTO dispatch_transaction_guard VALUES('missing'); END`)
	before := coordinationDecideSnapshot(t, f.s)
	got, err := f.s.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	requestDispatchZero(t, got, err, nil)
	materializeTransactionEvidence(t, err, "commit", false)
	coordinationAdmissionRequireFK(t, err)
	var cause *sqlite.Error
	if !errors.As(err, &cause) || err.Error() != "物化分派请求失败：保存分派物化失败："+cause.Error() {
		t.Errorf("deferred Commit display/cause changed: %v", err)
	}
	assignmentReopenPreserved(t, f.s, f.path, before)
}

func TestMaterializeDispatchTransactionSuccess(t *testing.T) {
	for _, held := range []bool{false, true} {
		name := "enqueued-replay"
		if held {
			name = "held"
		}
		t.Run(name, func(t *testing.T) {
			f, r := materializePending(t)
			if held {
				coordinationDecideExec(t, f.s, "UPDATE members SET status='left' WHERE id=?", f.worker)
			}
			got, err := f.s.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
			if err != nil {
				t.Fatalf("successful Commit was replaced by deferred Rollback ErrTxDone: %v", err)
			}
			if held {
				if got.Disposition != "held" || got.Generation != 2 || got.HeldReason != domain.DispatchHeldNoEligibleWorker || got.Delivery != nil {
					t.Fatalf("held success changed: %+v", got)
				}
			} else {
				materializeAssertReceipt(t, f.s, r, got, 1, materializeAt)
			}
			after := coordinationDecideSnapshot(t, f.s)
			replayed, err := f.s.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt.Add(time.Hour))
			if held {
				requestDispatchZero(t, replayed, err, ErrConflict)
				materializeTransactionEvidence(t, err, "source", true)
			} else if err != nil || !reflect.DeepEqual(replayed, got) {
				t.Fatalf("successful historical replay changed: %+v, %v", replayed, err)
			}
			coordinationDecidePreserved(t, f.s, after)
		})
	}
}
