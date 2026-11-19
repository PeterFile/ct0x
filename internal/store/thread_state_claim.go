package store

import (
	"context"
	"database/sql"

	"agentworkspace/internal/domain"
)

// PreparedThreadState fences the exact history and state used by an ordinary
// prompt. Empty state still participates, as does E1b without state installed.
// A nil stamp preserves legacy and separate typed preparation paths.
type PreparedThreadState struct {
	domain.ThreadRef
	MessageID      string
	Revision       int64
	stateInstalled bool
	correctionTip  int64
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
	// Keep E2's integrity checks, even for an empty or partly undisclosed state.
	state, err := preparationThreadStateInTx(ctx, tx, domain.ThreadStateScope{ThreadRef: thread}, messageID, noCorrectionLedger)
	if err != nil {
		return err
	}
	if state == nil && expected.stateInstalled {
		return errThreadStateIntegrity
	}
	if state != nil && (!expected.stateInstalled || state.Revision != expected.Revision) {
		return ErrTurnBusy
	}
	tip, err := correctionTipInTx(ctx, tx)
	if err != nil {
		return err
	}
	if tip < expected.correctionTip {
		return errThreadStateIntegrity
	}
	if expected.correctionTip == noCorrectionLedger && tip != noCorrectionLedger {
		return ErrTurnBusy
	}
	if tip > expected.correctionTip {
		changed, err := correctionTailTouchesThreadInTx(ctx, tx, thread, expected.correctionTip)
		if err != nil {
			return err
		}
		if changed {
			return ErrTurnBusy
		}
	}
	return nil
}
