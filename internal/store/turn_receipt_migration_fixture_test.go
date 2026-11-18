package store

import (
	"encoding/json"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

// seedPreV23TurnReceipt writes historical fixture data on a real pre-v23
// schema. Live settlement and receipt replay must still use ApplyTurnReceipt.
func seedPreV23TurnReceipt(t *testing.T, s *Store, claim TurnClaim, evidence provider.ExecutionReceipt) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	turn, err := lockTurnClaim(ctx, tx, claim)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := tx.QueryRowContext(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	receipt := normalizeTurnReceipt(turn, evidence)
	if version >= 23 || !activeTurn(turn.State) || evidence.NativeRef != nil || receipt.Outcome != evidence.Outcome {
		t.Fatalf("invalid historical receipt fixture: schema=v%d state=%s outcome=%s (normalized %s)", version, turn.State, evidence.Outcome, receipt.Outcome)
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}

	// Some unknown receipts still carry matching native acceptance. Preserve it
	// (and the original delivery) independently of execution settlement.
	if receipt.Started && receipt.NativeSessionID != "" && (turn.NativeSessionID == "" || turn.NativeSessionID == receipt.NativeSessionID) {
		turn.NativeSessionID = receipt.NativeSessionID
		if receipt.Accepted {
			acceptTurn(&turn)
		}
	}
	turn.State, turn.Reason = domain.TurnState(receipt.Outcome), receipt.Reason
	if terminalOutcome(receipt.Outcome) && strings.TrimSpace(turn.CandidateText) != "" {
		if err := publishTurnReply(ctx, tx, &turn); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeClaimedTurn(ctx, tx, &turn); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE turns SET receipt_json=? WHERE group_id=? AND id=? AND claim_token=?",
		string(data), claim.GroupID, claim.TurnID, claim.Token); err != nil {
		t.Fatal(err)
	}
	// Unknown stays active: do not fail its delivery or release its claims.
	if !activeTurn(turn.State) {
		if turn.AcceptedAt == nil {
			if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET state='failed',error=?
			 WHERE group_id=? AND id=? AND turn_id=? AND state<>'accepted'`, turn.Reason, turn.GroupID, turn.DeliveryID, turn.ID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM resource_claims WHERE turn_id=?", turn.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
