package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"agentworkspace/internal/domain"
)

const decisionThreadAcceptedTurn = `t.state='running' AND t.accepted_at IS NOT NULL AND t.native_session_id<>''
 AND t.cancel_requested_at IS NULL AND t.candidate_terminal='' AND t.receipt_json='' AND t.reply_message_id=''`

// AcknowledgeThreadDecision records explicit handling by the exact accepted
// typed Turn. The caller binds claim and answerID to the runtime session; key
// identifies this call within that Turn. Even replay requires live authority.
// Check ErrTaskWorkMutationUnknown before business errors and reconcile using
// GetDecisionAnswerView; a failed call does not imply that no receipt exists.
func (s *Store) AcknowledgeThreadDecision(ctx context.Context, claim TurnClaim, answerID, key string) (_ domain.DecisionAnswerView, err error) {
	const action = "确认话题答复已处理失败"
	var zero domain.DecisionAnswerView
	if !validTriggerID(claim.GroupID) || !validTriggerID(claim.TurnID) || !validTriggerID(claim.Token) ||
		!validTriggerID(answerID) || !memberToolKeyValid(key) {
		return zero, ErrInvalid
	}
	if !isThreadDecisionRef(answerID) {
		return zero, ErrMemberToolDenied
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, taskWorkMutationFailure(action, "begin", err, nil)
	}
	phase := "scope"
	defer func() {
		rollback := tx.Rollback()
		if err != nil {
			err = taskWorkMutationFailure(action, phase, err, rollback)
		}
	}()
	// The first statement is a write fence even on a deferred SQLite handle.
	turn, err := lockTurnClaim(ctx, tx, claim)
	if err != nil {
		return zero, err
	}
	if !isThreadDecisionRef(turn.DeliveryID) || turn.State != domain.TurnStateRunning || turn.AcceptedAt == nil ||
		!validNativeSession(turn.NativeSessionID) || turn.CancelRequestedAt != nil || turn.CandidateTerminal != "" {
		return zero, ErrMemberToolDenied
	}
	ready, err := s.threadDecisionExecutionReady(ctx, tx)
	if err != nil {
		return zero, err
	}
	if !ready {
		return zero, ErrMemberToolDenied
	}
	phase = "mutation"
	view, err := acknowledgeThreadDecisionInTx(ctx, tx, turn, answerID, key)
	if err != nil {
		return zero, err
	}
	phase = "commit"
	if err := tx.Commit(); err != nil {
		return zero, err
	}
	return view, nil
}

func acknowledgeThreadDecisionInTx(ctx context.Context, tx *sql.Tx, turn domain.Turn, answerID, key string) (zero domain.DecisionAnswerView, err error) {
	wake, err := acceptedThreadDecisionWakeInTx(ctx, tx, turn, answerID)
	if err != nil {
		return zero, err
	}
	c, err := decisionThreadConsumptionInTx(ctx, tx, wake.Receipt)
	if errors.Is(err, sql.ErrNoRows) {
		now := time.Now().UTC()
		if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO decision_thread_answer_consumptions(answer_id,delivery_id,turn_id,call_key,consumed_at)
		 VALUES(?,?,?,?,?)`, answerID, turn.DeliveryID, turn.ID, key, now.Format(timestampLayout)); err != nil {
			return zero, err
		}
		// Prove the same live route and actual receipt after all writes/triggers.
		current, err := acceptedThreadDecisionWakeInTx(ctx, tx, turn, answerID)
		if err != nil {
			return zero, err
		}
		if current != wake {
			return zero, ErrMemberToolDenied
		}
		saved, err := decisionThreadConsumptionInTx(ctx, tx, current.Receipt)
		if errors.Is(err, sql.ErrNoRows) || err == nil && !saved.handling.ConsumedAt.Equal(now) {
			return zero, ErrConflict
		}
		if err != nil {
			return zero, err
		}
		c = saved
	} else if err != nil {
		return zero, err
	}
	if c.key != key || c.handling.TurnID != turn.ID {
		return zero, ErrConflict
	}
	return domain.DecisionAnswerView{Receipt: wake.Receipt, Handling: &c.handling}, nil
}

// UPDATE RETURNING does not see AFTER triggers. Re-read the persisted claim,
// accepted session and exact wake under the same write fence.
func acceptedThreadDecisionWakeInTx(ctx context.Context, tx *sql.Tx, turn domain.Turn, answerID string) (ThreadDecisionWake, error) {
	wake, state, turnID, err := decisionThreadWakeInTx(ctx, tx, turn.GroupID, turn.DeliveryID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (state != domain.DeliveryStateAccepted || turnID != turn.ID ||
		wake.Receipt.ID != answerID || wake.AskerTurnID == turn.ID || wake.AskerID != turn.MemberID || wake.ConversationID != turn.ConversationID) {
		return ThreadDecisionWake{}, ErrMemberToolDenied
	}
	if err != nil {
		return ThreadDecisionWake{}, err
	}
	var exact bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM turns t
	 WHERE (t.id,t.claim_token,t.group_id,t.delivery_id,t.member_id,t.conversation_id,t.native_session_id,t.accepted_at)=(?,?,?,?,?,?,?,?)
	 AND `+decisionThreadAcceptedTurn+`)`, turn.ID, turn.ClaimToken, turn.GroupID, turn.DeliveryID, turn.MemberID,
		turn.ConversationID, turn.NativeSessionID, turnStamp(turn.AcceptedAt)).Scan(&exact)
	if err == nil && !exact {
		err = ErrMemberToolDenied
	}
	return wake, err
}

type threadDecisionConsumption struct {
	key      string
	handling domain.DecisionAnswerHandling
}

// Both the ACK and historical view project the same exact ledger row. Its
// foreign keys and insert guard bind the original delivery and Turn, not their
// mutable execution state. The call key is never part of the projection.
func decisionThreadConsumptionInTx(ctx context.Context, tx *sql.Tx, receipt domain.DecisionAnswerReceipt) (c threadDecisionConsumption, err error) {
	var stamp string
	err = tx.QueryRowContext(ctx, `SELECT turn_id,call_key,consumed_at FROM decision_thread_answer_consumptions
	 WHERE (answer_id,delivery_id)=(?,?)`, receipt.ID, receipt.WakeDeliveryID).Scan(&c.handling.TurnID, &c.key, &stamp)
	if err == nil {
		c.handling.ConsumedAt, err = time.Parse(timestampLayout, stamp)
	}
	return
}

func decisionThreadAnswerHandlingInTx(ctx context.Context, tx *sql.Tx, receipt domain.DecisionAnswerReceipt) (*domain.DecisionAnswerHandling, error) {
	var installed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.sqlite_schema
	 WHERE type='table' AND name='decision_thread_answer_consumptions')`).Scan(&installed)
	if err != nil || !installed {
		return nil, err
	}
	c, err := decisionThreadConsumptionInTx(ctx, tx, receipt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c.handling, nil
}
