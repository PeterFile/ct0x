package store

import (
	"encoding/json"
	"errors"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func nativePeerWithoutWaiting(t *testing.T, f messageFixture) *Store {
	t.Helper()
	peer := queueHandle(t, f.path, true)
	if _, err := peer.db.Exec("PRAGMA busy_timeout=0"); err != nil {
		t.Fatal(err)
	}
	return peer
}

func requireNativeWriterHeld(t *testing.T, peer *Store, turnID string) {
	t.Helper()
	// Test-only nonwaiting write from a different connection. No schedules,
	// sleeps, or timing thresholds are needed to prove the writer is excluded.
	_, err := peer.db.Exec("UPDATE turns SET receipt_json='fixture peer write' WHERE id=?", turnID)
	var busy *sqlite.Error
	if !errors.As(err, &busy) || busy.Code() != sqlite3.SQLITE_BUSY {
		t.Fatalf("the exact write fence did not exclude a peer writer: %v", err)
	}
}

func TestNativeSessionLaunchCommitSerializesStoreWriters(t *testing.T) {
	f := nativeFixture(t)
	turn, scope, binding := nativeClaim(t, f, f.groupID, f.a, "launch")
	// Use a deferred transaction on purpose: memberToolScopeInTx must acquire
	// the write fence before any snapshot, even without Open's immediate DSN.
	gate, peer := queueHandle(t, f.path, true), nativePeerWithoutWaiting(t, f)
	selected, err := gate.SelectNativeSession(ctx, scope, binding)
	if err != nil {
		t.Fatal(err)
	}
	launch := NativeSessionLaunch{Scope: scope, Binding: binding, DeliveryID: turn.DeliveryID, Selection: selected}
	checked := false
	if err := gate.CommitNativeSessionLaunch(ctx, launch, func() bool {
		checked = true
		// SQL qualification is complete but the final gate has not committed.
		requireNativeWriterHeld(t, peer, turn.ID)
		return true
	}); err != nil || !checked {
		t.Fatalf("current native launch did not commit: %v", err)
	}
	if stored := queueTurn(t, f.s, scope.Claim); stored.State != domain.TurnStateStarting || stored.CancelRequestedAt != nil ||
		queueReceiptJSON(t, f.s, scope.Claim) != "" {
		t.Fatal("launch commitment invented execution evidence")
	}
	// The exact same Store mutation immediately after commit succeeds. The
	// selected snapshot cannot authorize a new launch once cancel wins.
	if cancelled, err := peer.RequestTurnCancel(ctx, turn.GroupID, turn.ID); err != nil || cancelled.CancelRequestedAt == nil {
		t.Fatalf("cancel after commit did not persist: %v", err)
	}
	if err := gate.CommitNativeSessionLaunch(ctx, launch, func() bool {
		t.Fatal("cancel committed before gate but launch reached admission")
		return false
	}); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("cancelled scope did not reject launch: %v", err)
	}
}

func TestNativeSessionPreflightFirstReceiptWins(t *testing.T) {
	f := nativeFixture(t)
	turn, scope, _ := nativeClaim(t, f, f.groupID, f.a, "first-receipt")
	peer := nativePeerWithoutWaiting(t, f)
	// An absence observation of the kind the old runtime used is now stale.
	if snapshot, err := f.s.GetCoordinationRoleReceipt(ctx, turn.GroupID, turn.ID); err != nil || snapshot.Receipt != nil {
		t.Fatalf("fixture expected no prior receipt: %v", err)
	}
	first := provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Started: true, Accepted: true,
		NativeSessionID: "first-native", ExitCode: 13, DirectChildReaped: true, StdoutPath: "/fixture/first.jsonl"}
	if _, err := peer.ApplyTurnReceipt(ctx, scope.Claim, first); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.RequestTurnCancel(ctx, turn.GroupID, turn.ID); err != nil {
		t.Fatal(err)
	}
	want := queueReceiptJSON(t, f.s, scope.Claim)
	seq, _, _, ref := nativeStored(t, f.s, turn.ID)
	got, err := f.s.RecordNativePreflightFailure(ctx, scope, turn.DeliveryID, true)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(got)
	if err != nil || string(encoded) != want {
		t.Fatal("failure classification did not return the first canonical evidence")
	}
	// Even if a caller submits stale local not_started, only durable evidence
	// already selected in the locked transaction counts for preflight.
	replay, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, provider.ExecutionReceipt{
		NativePreflightFailure: true, Outcome: provider.OutcomeNotStarted, ExitCode: -1})
	if err != nil || replay.State != domain.TurnStateUnknown || replay.CancelRequestedAt == nil || replay.ClaimToken != scope.Claim.Token {
		t.Fatalf("preflight replay changed unknown/cancel/claim: %v", err)
	}
	if after, _, _, afterRef := nativeStored(t, f.s, turn.ID); after != seq || ref.Valid || afterRef.Valid || queueReceiptJSON(t, f.s, scope.Claim) != want {
		t.Fatal("preflight overwrote the first receipt or upgraded noRef")
	}
	queuePrivateAndHeld(t, f, scope.Claim)
}

func TestNativeSessionPreflightBarrierOrMalformedReceiptDoesNotProveAbsence(t *testing.T) {
	for _, damagedReceipt := range []bool{false, true} {
		name := "no-ref-only"
		if damagedReceipt {
			name = "malformed-receipt"
		}
		t.Run(name, func(t *testing.T) {
			f := nativeFixture(t)
			turn, scope, binding := nativeClaim(t, f, f.groupID, f.a, "old-evidence")
			selected, err := f.s.SelectNativeSession(ctx, scope, binding)
			if err != nil {
				t.Fatal(err)
			}
			var seq int64
			if damagedReceipt {
				if _, err := f.s.db.Exec("UPDATE turns SET receipt_json='malformed' WHERE id=?", turn.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := f.s.db.QueryRow(`INSERT INTO native_run_bindings(turn_id,group_id,member_id,conversation_id,purpose,binding_version)
				 VALUES(?,?,?,?,'unknown',0) RETURNING seq`, turn.ID, turn.GroupID, turn.MemberID, turn.ConversationID).Scan(&seq); err != nil {
					t.Fatal(err)
				}
			}
			launch := NativeSessionLaunch{Scope: scope, Binding: binding, DeliveryID: turn.DeliveryID, Selection: selected}
			if err := f.s.CommitNativeSessionLaunch(ctx, launch, func() bool {
				t.Fatal("existing evidence reached launch admission")
				return false
			}); !errors.Is(err, ErrMemberToolDenied) {
				t.Fatalf("evidence failed to block launch: %v", err)
			}
			got, err := f.s.RecordNativePreflightFailure(ctx, scope, turn.DeliveryID, true)
			if err != nil || got.Outcome != provider.OutcomeUnknown || got.NativeRef != nil {
				t.Fatalf("old evidence became absence: %s, %v", got.Outcome, err)
			}
			if _, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, got); err != nil {
				t.Fatal(err)
			}
			if damagedReceipt {
				if queueReceiptJSON(t, f.s, scope.Claim) != "malformed" {
					t.Fatal("malformed evidence was silently replaced")
				}
			} else if after, _, _, ref := nativeStored(t, f.s, turn.ID); after != seq || ref.Valid {
				t.Fatal("first noRef barrier changed")
			}
			queueCounts(t, f.s, 1, 1, 1)
		})
	}
}

func TestNativeSessionPreflightAbsenceAndSettlementUseOneWriter(t *testing.T) {
	f := nativeFixture(t)
	turn, scope, _ := nativeClaim(t, f, f.groupID, f.a, "preflight-commit")
	gate, peer := queueHandle(t, f.path, true), nativePeerWithoutWaiting(t, f)
	tx, err := gate.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	locked, err := lockTurnClaim(ctx, tx, scope.Claim)
	if err != nil {
		t.Fatal(err)
	}
	var previous string
	if err := tx.QueryRowContext(ctx, "SELECT receipt_json FROM turns WHERE id=?", turn.ID).Scan(&previous); err != nil || previous != "" {
		t.Fatalf("expected locked absence: %v", err)
	}
	// This is the old failure window: absence observed, not classified yet.
	// A first external receipt writer can no longer interleave here.
	requireNativeWriterHeld(t, peer, turn.ID)
	if _, got, err := nativePreflightFailureInTx(ctx, tx, locked, &scope); err != nil || got.Outcome != provider.OutcomeNotStarted {
		t.Fatalf("exact clean preflight failure did not settle: %s, %v", got.Outcome, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if observed, err := peer.RequestTurnCancel(ctx, turn.GroupID, turn.ID); err != nil || observed.State != domain.TurnStateNotStarted || observed.CancelRequestedAt != nil {
		t.Fatalf("late cancellation did not observe terminal preflight: %v", err)
	}
	if _, err := peer.ApplyTurnReceipt(ctx, scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}); !errors.Is(err, ErrConflict) {
		t.Fatalf("late writer replaced a committed clean refusal: %v", err)
	}
	queueCounts(t, f.s, 1, 1, 0)
}
