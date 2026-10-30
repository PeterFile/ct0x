package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func TestQueueRecoveryFencesAllNonterminalStatesAndRetainsEvidence(t *testing.T) {
	f := setupMessages(t)
	states := []string{"starting", "running", "cancel_requested", "unknown", "not_started", "succeeded", "failed", "aborted", "truncated"}
	stamp := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC).Format(timestampLayout)
	ids := make([]string, 0, len(states))
	for i, state := range states {
		m := member(t, f.s, f.groupID)
		d := queueDelivery(t, f.s, f.groupID, m.ID, state)
		turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: d.ID, ResourceKeys: []string{state}, MaxActive: 9})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.db.Exec(`UPDATE turns SET state=?,native_session_id='native-not-run-id',accepted_at=?,cancel_requested_at=?,
		 candidate_text='latest snapshot',candidate_terminal='succeeded',reason='retained evidence',receipt_json='{"fixture":true}' WHERE id=?`, state, stamp, stamp, turn.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.db.Exec("UPDATE deliveries SET state='accepted' WHERE id=?", d.ID); err != nil {
			t.Fatal(err)
		}
		if i >= 4 {
			if _, err := f.s.db.Exec("DELETE FROM resource_claims WHERE turn_id=?", turn.ID); err != nil {
				t.Fatal(err)
			}
		}
		ids = append(ids, turn.ID)
	}
	before := make([]domainTurnSnapshot, 0, len(ids))
	for _, id := range ids {
		turn, err := f.s.GetTurn(ctx, f.groupID, id)
		if err != nil {
			t.Fatal(err)
		}
		before = append(before, domainTurnSnapshot{turn.ClaimToken, turn.State, turn.UpdatedAt})
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	s := queueHandle(t, f.path, false)
	for i, id := range ids {
		turn, err := s.GetTurn(ctx, f.groupID, id)
		if err != nil || turn.ClaimToken != before[i].token || turn.State != before[i].state {
			t.Fatalf("Open recovered implicitly: %+v, %v", turn, err)
		}
	}
	for range 2 {
		count, err := s.RecoverInFlight(ctx)
		if err != nil || count != 4 {
			t.Fatalf("recovery count: %d, %v", count, err)
		}
		for i, id := range ids {
			turn, err := s.GetTurn(ctx, f.groupID, id)
			if err != nil || turn.NativeSessionID != "native-not-run-id" || turn.CandidateText != "latest snapshot" || turn.CandidateTerminal != "succeeded" || turn.Reason != "retained evidence" || turn.AcceptedAt == nil || turn.CancelRequestedAt == nil || turn.AcceptedAt.Location() != time.UTC {
				t.Fatalf("recovery lost evidence: %+v, %v", turn, err)
			}
			if i < 4 {
				if turn.State != "unknown" || turn.ClaimToken == before[i].token || turn.UpdatedAt.Before(before[i].updated) {
					t.Fatalf("active turn not fenced: %+v", turn)
				}
			} else if turn.ClaimToken != before[i].token || turn.State != before[i].state || !turn.UpdatedAt.Equal(before[i].updated) {
				t.Fatalf("terminal turn changed: %+v", turn)
			}
			var receipt, state string
			if err := s.db.QueryRow("SELECT receipt_json FROM turns WHERE id=?", id).Scan(&receipt); err != nil || receipt != `{"fixture":true}` {
				t.Fatalf("receipt changed: %s, %v", receipt, err)
			}
			if err := s.db.QueryRow("SELECT state FROM deliveries WHERE id=?", turn.DeliveryID).Scan(&state); err != nil || state != "accepted" {
				t.Fatalf("accepted fact changed: %s, %v", state, err)
			}
			before[i] = domainTurnSnapshot{turn.ClaimToken, turn.State, turn.UpdatedAt}
		}
		queueCounts(t, s, 9, 9, 4)
	}
	d := queueDelivery(t, s, f.otherID, f.foreign, "waiting")
	if _, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.otherID, DeliveryID: d.ID, MaxActive: 4}); !errors.Is(err, ErrTurnBusy) {
		t.Fatalf("unknown turns released global capacity: %v", err)
	}
	if _, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.otherID, DeliveryID: d.ID, ResourceKeys: []string{"cancel_requested"}, MaxActive: 9}); !errors.Is(err, ErrTurnBusy) {
		t.Fatalf("unknown turns released resources: %v", err)
	}
	if _, err := s.db.Exec(fmt.Sprintf(`CREATE TRIGGER queue_recover_fail BEFORE UPDATE ON turns
	 WHEN OLD.id='%s' AND NEW.claim_token<>OLD.claim_token BEGIN SELECT RAISE(ABORT,'failure'); END`, ids[3])); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverInFlight(ctx); err == nil {
		t.Fatal("recovery fault was ignored")
	}
	for i, id := range ids {
		turn, err := s.GetTurn(ctx, f.groupID, id)
		if err != nil || !reflect.DeepEqual(domainTurnSnapshot{turn.ClaimToken, turn.State, turn.UpdatedAt}, before[i]) {
			t.Fatalf("failed recovery partially committed: %+v, %v", turn, err)
		}
	}
}

type domainTurnSnapshot struct {
	token   string
	state   domain.TurnState
	updated time.Time
}
