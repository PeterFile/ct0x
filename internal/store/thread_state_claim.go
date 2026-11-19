package store

import (
	"context"
	"database/sql"

	"agentworkspace/internal/domain"
)

// PreparedThreadState is the server-bound Thread snapshot used by an ordinary
// prompt. Zero is an installed, empty snapshot, not an opt-out from rechecking.
// The absence of a stamp preserves legacy and separate typed preparation paths.
type PreparedThreadState struct {
	domain.ThreadRef
	MessageID string
	Revision  int64
}

// Runs after the first claim write reserves the binding and before any Turn or
// resource insert. The same write lock serializes this read and user updates.
func verifyPreparedThreadStateInTx(ctx context.Context, tx *sql.Tx, turn domain.Turn, expected *PreparedThreadState) error {
	thread := domain.ThreadRef{GroupID: turn.GroupID}
	var messageID string
	if err := tx.QueryRowContext(ctx, `SELECT d.topic_id,d.message_id FROM deliveries d
	 JOIN messages m ON (m.id,m.group_id,m.topic_id)=(d.message_id,d.group_id,d.topic_id)
	 JOIN conversations c ON (c.id,c.group_id,c.member_id,c.topic_id)=(d.conversation_id,d.group_id,d.recipient_id,d.topic_id)
	 WHERE d.id=? AND d.group_id=? AND d.turn_id=?`, turn.DeliveryID, turn.GroupID, turn.ID).Scan(&thread.TopicID, &messageID); err != nil {
		return err
	}
	if thread != expected.ThreadRef || messageID != expected.MessageID {
		return ErrTurnBusy
	}
	state, err := preparationThreadStateInTx(ctx, tx, domain.ThreadStateScope{ThreadRef: thread}, messageID)
	if err != nil {
		return err
	}
	if state == nil {
		return errThreadStateIntegrity
	}
	if state.Revision != expected.Revision {
		return ErrTurnBusy
	}
	return nil
}
