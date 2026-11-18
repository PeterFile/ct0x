package store

import (
	"context"
	"database/sql"
)

// Legacy intake messages keep their original visibility and sender semantics.
// Member-triggered turns use the structured handoff for any further wakes.
func chatDiscussionGateInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, candidateID string, input MemberToolMessageInput, replay bool) error {
	if err := chatIntakePurposeInTx(ctx, tx, scope, candidateID); err != nil {
		return err
	}
	candidate, err := chatCandidateInTx(ctx, tx, scope, candidateID)
	if err != nil {
		return err
	}
	var assessed bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM chat_assessments WHERE candidate_id=?)", candidateID).Scan(&assessed); err != nil {
		return err
	}
	if !assessed {
		return ErrMemberToolDenied
	}
	if replay {
		return nil
	}
	recipients := recipientSet(input.RecipientIDs)
	directed := len(recipients) > 0
	if directed {
		if len(recipients) != 1 || recipients[0] == scope.MemberID || candidate.SourceKind != "local_user_chat" {
			return ErrMemberToolDenied
		}
		var active bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM members WHERE id=? AND group_id=? AND status='active')", recipients[0], scope.Claim.GroupID).Scan(&active); err != nil {
			return err
		}
		if !active {
			return ErrMemberToolDenied
		}
	}
	return chatDiscussionMessageSlotInTx(ctx, tx, scope.Claim.TurnID, directed)
}

// Both command paths share the slots. A structured return still spends the
// directed slot when the intended recipient has left and no wake is sent.
func chatDiscussionMessageSlotInTx(ctx context.Context, tx *sql.Tx, turnID string, directed bool) error {
	var used bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM message_command_sources s WHERE s.turn_id=? AND (
  EXISTS(SELECT 1 FROM deliveries d WHERE d.message_id=s.message_id)
  OR EXISTS(SELECT 1 FROM chat_discussion_events e WHERE e.message_id=s.message_id AND e.recipient_id IS NOT NULL))=?)`,
		turnID, directed).Scan(&used); err != nil {
		return err
	}
	if used {
		return ErrMemberToolDenied
	}
	return nil
}
