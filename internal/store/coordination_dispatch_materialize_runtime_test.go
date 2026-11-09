package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"modernc.org/sqlite"
)

func materializeAssertNoRetryOrRelease(t *testing.T, s *Store, r domain.DispatchRequest, original domain.DispatchMaterialization, turn domain.Turn) {
	t.Helper()
	before := messageSequenceSnapshot(t, s)
	got, err := materializeAPI(t, s).MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt.Add(time.Hour))
	if err != nil || !reflect.DeepEqual(got, original) {
		t.Fatalf("uncertain execution caused rematerialization: %+v, %v", got, err)
	}
	for _, gen := range []int64{1, 2} {
		if err := deferDispatchStore(t, s).DeferDispatchRequest(ctx, r.ID, gen, materializeAt, DispatchTransientSQLiteBusy); !errors.Is(err, ErrConflict) {
			t.Fatalf("uncertain execution was rescheduled: %v", err)
		}
	}
	locks := messageSequenceRows(t, s.db, "SELECT resource_key,turn_id FROM resource_claims WHERE resource_key='materialize-runtime-held'")
	if !reflect.DeepEqual(locks, [][]any{{"materialize-runtime-held", turn.ID}}) {
		t.Fatalf("nonterminal execution released resources: %v", locks)
	}
	page, err := dispatchDueStore(t, s).ListDueDispatchRequests(ctx, materializeAt.Add(24*time.Hour), DispatchDueOptions{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("enqueued uncertainty reentered due: %+v, %v", page, err)
	}
	_, err = s.RequestDispatch(ctx, r.GroupID, r.TaskID, 1, 1, "不能绕过未知执行", "another-dispatch")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("unfinished Task slot was released: %v", err)
	}
	messageSequencePreserved(t, s, before)
}

func TestMaterializeDispatchUnknownAndCancelRequestedKeepClaimsAndReceipt(t *testing.T) {
	f, r := materializePending(t)
	original, err := materializeAPI(t, f.s).MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.GroupID, DeliveryID: original.Delivery.DeliveryID, ResourceKeys: []string{"materialize-runtime-held"}, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	claim := TurnClaim{GroupID: r.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	// Real Store execution-state transitions; no provider process is started.
	queueEvent(t, f.s, claim, provider.AcceptedEvent, "", "")
	cancelled, err := f.s.RequestTurnCancel(ctx, r.GroupID, turn.ID)
	if err != nil || cancelled.State != domain.TurnStateCancelRequested {
		t.Fatalf("not a real cancel_requested Turn: %+v, %v", cancelled, err)
	}
	materializeAssertNoRetryOrRelease(t, f.s, r, original, cancelled)
	receipt := queueReceipt(provider.OutcomeSucceeded)
	receipt.Outcome, receipt.OutputComplete = provider.OutcomeUnknown, false
	unknown, err := f.s.ApplyTurnReceipt(ctx, claim, receipt)
	if err != nil || unknown.State != domain.TurnStateUnknown {
		t.Fatalf("not a real unknown Turn: %+v, %v", unknown, err)
	}
	materializeAssertNoRetryOrRelease(t, f.s, r, original, unknown)
	before := messageSequenceSnapshot(t, f.s)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, f.path, false)
	materializeAssertNoRetryOrRelease(t, reopened, r, original, unknown)
	messageSequencePreserved(t, reopened, before)
}

func TestMaterializeDispatchHeldWriteFailureDoesNotReturnHeldSuccess(t *testing.T) {
	for _, mode := range []string{"abort", "ignore", "commit"} {
		t.Run(mode, func(t *testing.T) {
			f, r := materializePending(t)
			materializeChangeEligibility(t, f, "coordinator-left")
			timing, body := "AFTER", "SELECT RAISE(ABORT,'注入暂停分派失败');"
			if mode == "ignore" {
				timing, body = "BEFORE", "SELECT RAISE(IGNORE);"
			} else if mode == "commit" {
				if _, err := f.s.db.Exec("CREATE TABLE materialize_held_commit(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)"); err != nil {
					t.Fatal(err)
				}
				body = "INSERT INTO materialize_held_commit VALUES('missing');"
			}
			if _, err := f.s.db.Exec("CREATE TRIGGER materialize_held_fault " + timing + " UPDATE ON coordination_dispatch_state BEGIN " + body + " END"); err != nil {
				t.Fatal(err)
			}
			before := messageSequenceSnapshot(t, f.s)
			got, err := materializeAPI(t, f.s).MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
			requestDispatchZero(t, got, err, nil)
			if mode != "ignore" {
				var cause *sqlite.Error
				if !errors.As(err, &cause) {
					t.Fatalf("hold lost database failure evidence: %v", err)
				}
			}
			messageSequencePreserved(t, f.s, before)
			if _, err := f.s.db.Exec("DROP TRIGGER materialize_held_fault"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.db.Exec("UPDATE members SET status='active' WHERE id=?", f.coord); err != nil {
				t.Fatal(err)
			}
			got, err = materializeAPI(t, f.s).MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
			if err != nil {
				t.Fatalf("failed held write burned pending generation: %v", err)
			}
			materializeAssertReceipt(t, f.s, r, got, 1, materializeAt)
		})
	}
}

func TestMaterializeDispatchUTCClockAndHistoricalScope(t *testing.T) {
	f, r := materializePending(t)
	if _, err := f.s.db.Exec("UPDATE tasks SET title='当前显示标题',acceptance='当前显示验收',description=? WHERE id=?", "无关描述", r.TaskID); err != nil {
		t.Fatal(err)
	}
	at := materializeAt.In(time.FixedZone("trusted-clock", 8*60*60))
	got, err := materializeAPI(t, f.s).MaterializeDispatchRequest(ctx, r.ID, 1, at)
	if err != nil {
		t.Fatal(err)
	}
	materializeAssertReceipt(t, f.s, r, got, 1, materializeAt)
	message, err := f.s.GetMessage(ctx, r.GroupID, got.Delivery.MessageID)
	if err != nil || message.Content != materializeWantBody(r, f.admission.Mandate.Scope.Objective, f.admission.Mandate.Scope.Acceptance) {
		t.Fatalf("body substituted current Task display for immutable authority: %q, %v", message.Content, err)
	}
}
