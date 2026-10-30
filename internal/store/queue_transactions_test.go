package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestQueueEventAndCancellationRacesAcrossHandles(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%t", deferred), func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			handles := []*Store{queueHandle(t, f.path, deferred), queueHandle(t, f.path, deferred)}
			start, results := make(chan struct{}), make(chan error, 8)
			for i := range 8 {
				go func() {
					<-start
					if i%2 == 0 {
						_, err := handles[(i/2)%2].RequestTurnCancel(ctx, claim.GroupID, claim.TurnID)
						results <- err
					} else {
						results <- handles[(i/2)%2].RecordTurnEvent(ctx, claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "native-process-session"})
					}
				}()
			}
			close(start)
			for range 8 {
				if err := <-results; err != nil {
					t.Errorf("serialized event/cancel failed: %v", err)
				}
			}
			turn := queueTurn(t, f.s, claim)
			if turn.State != domain.TurnStateCancelRequested || turn.AcceptedAt == nil || turn.CancelRequestedAt == nil {
				t.Fatalf("competing cancellation or acceptance lost: %+v", turn)
			}
			queueDeliveryState(t, f.s, turn, domain.DeliveryStateAccepted)
			queuePrivateAndHeld(t, f, claim)
		})
	}
}

func TestQueueReceiptReplayRacesAcrossHandles(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%t", deferred), func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			queueCandidate(t, f.s, claim, "one durable reply", provider.OutcomeSucceeded)
			handles := []*Store{queueHandle(t, f.path, deferred), queueHandle(t, f.path, deferred)}
			type result struct {
				turn domain.Turn
				err  error
			}
			start, results := make(chan struct{}), make(chan result, 8)
			for i := range 8 {
				go func() {
					<-start
					turn, err := handles[i%2].ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeSucceeded))
					results <- result{turn, err}
				}()
			}
			close(start)
			var first domain.Turn
			for range 8 {
				r := <-results
				if first.ID == "" {
					first = r.turn
				}
				if r.err != nil || r.turn.ReplyMessageID == "" || !reflect.DeepEqual(r.turn, first) {
					t.Errorf("receipt replay did not converge: %+v, %v", r.turn, r.err)
				}
			}
			queueCounts(t, f.s, 1, 1, 0)
			var messages, deliveries int
			if err := f.s.db.QueryRow("SELECT (SELECT count(*) FROM messages),(SELECT count(*) FROM deliveries)").Scan(&messages, &deliveries); err != nil || messages != 2 || deliveries != 1 {
				t.Fatalf("concurrent settlement duplicated replies or woke members: %d, %d, %v", messages, deliveries, err)
			}
		})
	}
}

func TestQueueRecoveryRacesWithCallbackWithoutLosingFence(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%t", deferred), func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			a, b := queueHandle(t, f.path, deferred), queueHandle(t, f.path, deferred)
			start, recovered, callback := make(chan struct{}), make(chan error, 1), make(chan error, 1)
			go func() {
				<-start
				_, err := a.RecoverInFlight(ctx)
				recovered <- err
			}()
			go func() {
				<-start
				callback <- b.RecordTurnEvent(ctx, claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "native-process-session"})
			}()
			close(start)
			if err := <-recovered; err != nil {
				t.Fatal(err)
			}
			err := <-callback
			if err != nil && !errors.Is(err, ErrStaleClaim) {
				t.Fatalf("callback did not serialize with recovery: %v", err)
			}
			turn := queueTurn(t, f.s, claim)
			if turn.State != domain.TurnStateUnknown || turn.ClaimToken == claim.Token || (turn.AcceptedAt != nil) != (err == nil) {
				t.Fatalf("recovery lost evidence or fencing: %+v, callback=%v", turn, err)
			}
			if err := b.RecordTurnEvent(ctx, claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "native-process-session"}); !errors.Is(err, ErrStaleClaim) {
				t.Fatalf("late callback passed rotated fence: %v", err)
			}
			if got := queueTurn(t, f.s, claim); !reflect.DeepEqual(got, turn) {
				t.Fatalf("late callback mutated recovery: %+v", got)
			}
			queuePrivateAndHeld(t, f, claim)
		})
	}
}

func TestQueueCancellationRacesWithSettlementWithoutResurrection(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%t", deferred), func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			queueCandidate(t, f.s, claim, "final answer", provider.OutcomeSucceeded)
			a, b := queueHandle(t, f.path, deferred), queueHandle(t, f.path, deferred)
			start, results := make(chan struct{}), make(chan error, 2)
			go func() {
				<-start
				_, err := a.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID)
				results <- err
			}()
			go func() {
				<-start
				_, err := b.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeSucceeded))
				results <- err
			}()
			close(start)
			for range 2 {
				if err := <-results; err != nil {
					t.Errorf("cancel/settlement did not serialize: %v", err)
				}
			}
			turn := queueTurn(t, f.s, claim)
			if turn.State != domain.TurnStateSucceeded || turn.ReplyMessageID == "" || turn.AcceptedAt == nil {
				t.Fatalf("cancellation resurrected terminal execution: %+v", turn)
			}
			queueCounts(t, f.s, 1, 1, 0)
		})
	}
}

func TestQueueEventAndCancellationCommitFailuresAreAtomic(t *testing.T) {
	for _, operation := range []string{"accept", "cancel"} {
		t.Run(operation, func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			before := queueTurn(t, f.s, claim)
			if _, err := f.s.db.Exec("CREATE TABLE queue_event_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.db.Exec(`CREATE TRIGGER queue_event_fail AFTER UPDATE ON turns
			 WHEN NEW.accepted_at IS NOT NULL OR NEW.cancel_requested_at IS NOT NULL
			 BEGIN INSERT INTO queue_event_guard VALUES('PRIVATE-COMMIT-SQL'); END`); err != nil {
				t.Fatal(err)
			}
			var err error
			if operation == "accept" {
				err = f.s.RecordTurnEvent(ctx, claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "native-process-session"})
			} else {
				var turn domain.Turn
				turn, err = f.s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID)
				if turn.ID != "" {
					t.Fatal("failed cancellation commit returned success")
				}
			}
			if err == nil || strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), "INSERT") {
				t.Fatalf("commit failure was lost or leaked: %v", err)
			}
			if got := queueTurn(t, f.s, claim); !reflect.DeepEqual(got, before) {
				t.Fatalf("failed commit changed turn: %+v", got)
			}
			queueDeliveryState(t, f.s, before, domain.DeliveryStateQueued)
			queueCounts(t, f.s, 1, 1, 1)
		})
	}
}
