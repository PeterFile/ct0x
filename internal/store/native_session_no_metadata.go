package store

import (
	"context"
	"crypto/sha256"
	"database/sql"

	"agentworkspace/internal/domain"
)

// Ordinary unbound work can only produce an unknown/noRef barrier. Test raw
// purpose candidates before any parent join: damaged role/intake evidence must
// still take the full independent purpose path. A saved barrier is never touched.
func applyNativeNoMetadataInTx(ctx context.Context, tx *sql.Tx, turn domain.Turn) (bool, error) {
	digest := sha256.Sum256([]byte(turn.ClaimToken))
	result, err := tx.ExecContext(ctx, `INSERT INTO native_run_bindings(turn_id,group_id,member_id,conversation_id,purpose,binding_version,claim_digest)
	 SELECT ?,?,?,?,'unknown',0,?
	 WHERE NOT EXISTS(SELECT 1 FROM native_run_bindings WHERE turn_id=?)
	 AND NOT EXISTS(SELECT 1 FROM coordination_dispatch_state WHERE delivery_id=?)
	 AND NOT EXISTS(SELECT 1 FROM coordination_assignment_dispatch WHERE delivery_id=?)
	 AND NOT EXISTS(SELECT 1 FROM chat_candidates WHERE turn_id=?)
	 AND NOT EXISTS(SELECT 1 FROM chat_intake_executions WHERE turn_id=?)`,
		turn.ID, turn.GroupID, turn.MemberID, turn.ConversationID, digest[:], turn.ID, turn.DeliveryID, turn.DeliveryID, turn.ID, turn.ID)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}
