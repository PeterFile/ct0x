package store

import (
	"context"
	"database/sql"

	"agentworkspace/internal/domain"
)

func taskWorkFollowupProvenance(f taskWorkFacts) CoordinationRoleProvenance {
	d := f.d
	return CoordinationRoleProvenance{
		Kind: CoordinationRoleWorker, GroupID: d.GroupID, DeliveryID: d.DeliveryID, MessageID: d.MessageID,
		ConversationID: d.ConversationID, MemberID: d.MemberID, TopicID: d.TopicID, RequestID: f.v.r.ID,
		TaskID: d.TaskID, AssignmentID: d.AssignmentID, TaskRevision: d.TaskRevision, SourceKind: d.SourceKind, SourceID: d.SourceID,
	}
}

func (s *Store) taskWorkFollowupRoleInTx(ctx context.Context, tx *sql.Tx, f taskWorkFacts, turnID string) (CoordinationRoleState, error) {
	// Other typed sources do not fall through as chat. Their role policy is
	// deliberately not defined by the followup path.
	if f.d.SourceKind != domain.TaskWorkSourceFollowup || f.turnID != turnID {
		return CoordinationRoleState{}, ErrMemberToolDenied
	}
	p := taskWorkFollowupProvenance(f)
	state := CoordinationRoleState{Provenance: p}
	if err := tx.QueryRowContext(ctx, `SELECT provider_id,model FROM members WHERE group_id=? AND id=?`, p.GroupID, p.MemberID).
		Scan(&state.ProviderID, &state.Model); err != nil {
		return CoordinationRoleState{}, coordinationRoleRelationError(err)
	}
	blocked, _, err := s.taskWorkReadinessInTx(ctx, tx, f, turnID)
	if err != nil {
		return CoordinationRoleState{}, err
	}
	state.Blocked = CoordinationReadinessCode(blocked)
	if turnID != "" {
		attempt, err := taskWorkBoundAttemptInTx(ctx, tx, f.d, "")
		if err != nil {
			return CoordinationRoleState{}, err
		}
		if attempt.ID != "" && attempt.TurnID != turnID {
			return CoordinationRoleState{}, ErrMemberToolDenied
		}
		state.AttemptID = attempt.ID
	}
	return state, nil
}
