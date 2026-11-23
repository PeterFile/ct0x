package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

// v29 permits only one marker for (source_kind, source_id). A replacement has
// its own physical marker key; the verified delivery loader resolves it to the
// same logical answer ID before passing it to readiness, runtime or tools.
const decisionAnswerReplacementPrefix = "decision-answer-replacement:"

const decisionAnswerPriorTurn = ` FROM deliveries prior
 JOIN turns h ON (h.id,h.delivery_id,h.group_id,h.conversation_id,h.member_id)=
    (prior.turn_id,prior.id,prior.group_id,prior.conversation_id,prior.recipient_id)
 WHERE (prior.id,prior.message_id,prior.group_id,prior.topic_id,prior.conversation_id,prior.recipient_id)=
    (work.delivery_id,work.message_id,work.group_id,work.topic_id,work.conversation_id,work.member_id)
 AND h.cancel_requested_at IS NULL AND h.reply_message_id=''
 AND NOT EXISTS(SELECT 1 FROM turns other WHERE other.delivery_id=prior.id AND other.id<>h.id)`

const decisionAnswerPriorSettled = decisionAnswerPriorTurn + ` AND prior.state='failed' AND prior.error=h.reason
 AND h.state='not_started' AND ` + taskWorkTurnSettled

const decisionAnswerReplacementJoin = `
 LEFT JOIN decision_answer_wake_replacements retry ON retry.answer_id=ans.id
    AND EXISTS(SELECT 1` + decisionAnswerPriorSettled + ` AND h.id=retry.prior_turn_id)
 LEFT JOIN task_work_deliveries replacement ON (replacement.delivery_id,replacement.source_kind,replacement.source_id)=
    (retry.delivery_id,'followup','` + decisionAnswerReplacementPrefix + `' || ans.id)
    AND (replacement.group_id,replacement.topic_id,replacement.task_id,replacement.task_revision,replacement.assignment_id,
         replacement.mandate_id,replacement.authority_generation,replacement.owner_member_id,replacement.member_id,
         replacement.sender_member_id,replacement.conversation_id)=
        (work.group_id,work.topic_id,work.task_id,work.task_revision,work.assignment_id,work.mandate_id,
         work.authority_generation,work.owner_member_id,work.member_id,work.sender_member_id,work.conversation_id)`

func decisionAnswerReplacementCandidateInTx(ctx context.Context, tx *sql.Tx, turn domain.Turn) (taskWorkFacts, error) {
	var empty taskWorkFacts
	installed, err := decisionAnswerInstalledInTx(ctx, tx)
	if err != nil || !installed {
		return empty, err
	}
	var candidate bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM decision_answers ans
	 JOIN turns t ON t.delivery_id=ans.wake_delivery_id AND t.receipt_json=''
	 WHERE (ans.wake_delivery_id,t.id)=(?,?)
	 AND NOT EXISTS(SELECT 1 FROM native_run_bindings WHERE turn_id=t.id)
	 AND NOT EXISTS(SELECT 1 FROM decision_answer_consumptions WHERE answer_id=ans.id)
	 AND NOT EXISTS(SELECT 1 FROM decision_answer_wake_replacements WHERE answer_id=ans.id))`, turn.DeliveryID, turn.ID).Scan(&candidate)
	if err != nil || !candidate {
		return empty, err
	}
	f, err := taskWorkDeliveryInTx(ctx, tx, turn.GroupID, turn.DeliveryID)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrMemberToolDenied) {
		return empty, nil // Invalid source proof is not eligible for recovery.
	}
	if err != nil || !f.decisionAnswer || f.turnID != turn.ID {
		return empty, err
	}
	blocked, _, err := taskWorkCurrentInTx(ctx, tx, f)
	if err != nil || blocked != "" {
		return empty, err
	}
	if err := decisionTaskChatAuthorityInTx(ctx, tx, f); errors.Is(err, ErrConflict) {
		return empty, nil
	} else if err != nil {
		return empty, err
	}
	return f, nil
}

// Only ApplyTurnReceipt's first, normal adapter refusal enters this path,
// never a preflight write or a replay of an already terminal Turn. The ledger
// must be inserted while the original is still fresh, but its deferred FK can
// commit only with a new marker that verifies terminal proof and resource release.
func applyDecisionAnswerRefusalInTx(ctx context.Context, tx *sql.Tx, turn domain.Turn, receipt provider.ExecutionReceipt) (domain.Turn, error) {
	// Fix the expiry check to the enrollment instant; a later start rechecks
	// the real clock. Crossing a deadline here must not undo native settlement.
	now := time.Now().UTC()
	f, err := decisionAnswerReplacementCandidateInTx(ctx, tx, turn)
	if err != nil {
		return domain.Turn{}, err
	}
	if !f.decisionAnswer {
		return applyTurnReceiptInTx(ctx, tx, turn, receipt)
	}
	var settled domain.Turn
	want := f.d
	want.SourceID = decisionAnswerReplacementPrefix + f.d.SourceID
	wake, err := taskWorkCreateWakeInTx(ctx, tx, want, func(next domain.TaskWorkDelivery) error {
		if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO decision_answer_wake_replacements(answer_id,prior_turn_id,delivery_id,created_at)
		 VALUES(?,?,?,?)`, f.d.SourceID, turn.ID, next.DeliveryID, now.Format(timestampLayout)); err != nil {
			return err
		}
		var err error
		settled, err = applyTurnReceiptInTx(ctx, tx, turn, receipt)
		return err
	})
	if err != nil {
		return domain.Turn{}, err
	}
	wake.SourceID = f.d.SourceID
	saved, err := taskWorkDeliveryInTx(ctx, tx, wake.GroupID, wake.DeliveryID)
	if err != nil {
		return domain.Turn{}, err
	}
	if !saved.decisionAnswer || saved.d != wake || saved.turnID != "" {
		return domain.Turn{}, ErrConflict
	}
	// Verify the version and scope again after both the receipt and wake writes.
	live, err := decisionAnswerLiveAtInTx(ctx, tx, f.d.SourceID, now)
	if err != nil {
		return domain.Turn{}, err
	}
	if !live {
		return domain.Turn{}, ErrConflict
	}
	return settled, nil
}
