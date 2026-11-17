package store

import (
	"context"
	"database/sql"
)

// Intake is not a renamed all-purpose capability. It can only publish bounded
// discussion after assessment; member-triggered turns cannot invite recursively.
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
	var used int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM message_command_sources s WHERE s.turn_id=? AND
  EXISTS(SELECT 1 FROM deliveries d WHERE d.message_id=s.message_id)=?`, scope.Claim.TurnID, directed).Scan(&used)
	if err != nil {
		return err
	}
	if used >= 1 {
		return ErrMemberToolDenied
	}
	return nil
}
