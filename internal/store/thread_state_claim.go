package store

import (
	"context"
	"database/sql"

	"agentworkspace/internal/domain"
)

// PreparedThreadState fences ordinary Thread preparation (including E1b-only
// history) or an initial formal role's exact Task state. Installed empty state
// still participates. A nil stamp preserves legacy and separate typed paths.
type PreparedThreadState struct {
	domain.ThreadRef
	MessageID      string
	Revision       int64
	stateInstalled bool
	correctionTip  int64
	role           *CoordinationRoleState
}

// Runs after the first claim write reserves the binding and before any Turn or
// resource insert. The same write lock serializes this read and user updates.
func (s *Store) verifyPreparedThreadStateInTx(ctx context.Context, tx *sql.Tx, turn domain.Turn, expected *PreparedThreadState) error {
	if expected.role != nil {
		return s.verifyPreparedTaskThreadStateInTx(ctx, tx, turn, expected)
	}
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
	if err := verifyPreparedStateRevisionInTx(ctx, tx, domain.ThreadStateScope{ThreadRef: thread}, messageID, expected); err != nil {
		return err
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

func (s *Store) verifyPreparedTaskThreadStateInTx(ctx context.Context, tx *sql.Tx, turn domain.Turn, expected *PreparedThreadState) error {
	bound := *expected.role
	p := bound.Provenance
	thread := domain.ThreadRef{GroupID: p.GroupID, TopicID: p.TopicID}
	if p.GroupID != turn.GroupID || p.DeliveryID != turn.DeliveryID || p.ConversationID != turn.ConversationID || p.MemberID != turn.MemberID ||
		expected.ThreadRef != thread || expected.MessageID != p.MessageID {
		return ErrMemberToolDenied
	}
	current, err := s.resolveQueuedCoordinationInTx(ctx, tx, turn.GroupID, turn.DeliveryID, turn.ID, nil)
	if err != nil {
		return err
	}
	blocked, err := preparedTaskRoleReadiness(ctx, tx, bound, current)
	if err != nil {
		return err
	}
	if blocked != CoordinationReady {
		return ErrTurnBusy
	}
	// Formal state uses stored evidence, not the ordinary correction ledger.
	return coordinationRoleRelationError(verifyPreparedStateRevisionInTx(ctx, tx, domain.ThreadStateScope{ThreadRef: thread,
		TaskID: p.TaskID, TaskRevision: p.TaskRevision}, p.MessageID, expected))
}

// Only readiness may differ from a ready bound role. A different identity,
// profile, or persisted source is not ordinary drift.
func preparedTaskRoleReadiness(ctx context.Context, tx *sql.Tx, bound, current CoordinationRoleState) (CoordinationReadinessCode, error) {
	blocked := current.Blocked
	if blocked != CoordinationReady && current.AutomaticTextWorkerID == "" && bound.AutomaticTextWorkerID != "" {
		// A preexisting blocker can skip loading the automatic role. Recover
		// its frozen worker from the same snapshot before comparing profiles.
		p := current.Provenance
		worker, _, err := chatAutomaticTextRoleInTx(ctx, tx, p.GroupID, p.RequestID, false)
		if err != nil {
			return CoordinationReady, err
		}
		current.AutomaticTextWorkerID = worker
	}
	current.Blocked = CoordinationReady
	if current != bound {
		return CoordinationReady, ErrMemberToolDenied
	}
	return blocked, nil
}

func verifyPreparedStateRevisionInTx(ctx context.Context, tx *sql.Tx, scope domain.ThreadStateScope, messageID string, expected *PreparedThreadState) error {
	// Keep E2's integrity checks, even for an empty or partly undisclosed state.
	state, err := preparationThreadStateInTx(ctx, tx, scope, messageID, noCorrectionLedger)
	if err != nil {
		return err
	}
	if state == nil && expected.stateInstalled {
		return errThreadStateIntegrity
	}
	if state != nil && (!expected.stateInstalled || state.Revision != expected.Revision) {
		return ErrTurnBusy
	}
	return nil
}
