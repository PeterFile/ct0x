package store

import (
	"context"
	"database/sql"
	"errors"
)

// GetChannelOwnershipForTool resolves an exact Project/Channel only if it owns
// the live session's bound Group. This does not grant topic or resource access.
func (s *Store) GetChannelOwnershipForTool(ctx context.Context, scope MemberToolScope, projectID, channelID string) (GroupOwnership, error) {
	const action = "读取成员频道归属失败"
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return GroupOwnership{}, memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	if !memberToolIDsValid(projectID, channelID) {
		return GroupOwnership{}, ErrMemberToolDenied
	}
	var ownership GroupOwnership
	err = tx.QueryRowContext(ctx, `SELECT p.workspace_id, c.project_id, c.id, g.id
	 FROM channels c JOIN projects p ON p.id = c.project_id JOIN groups g ON g.id = c.legacy_group_id
	 WHERE c.legacy_group_id = ? AND c.project_id = ? AND c.id = ?`, scope.Claim.GroupID, projectID, channelID).
		Scan(&ownership.WorkspaceID, &ownership.ProjectID, &ownership.ChannelID, &ownership.GroupID)
	if errors.Is(err, sql.ErrNoRows) {
		return GroupOwnership{}, ErrMemberToolDenied
	}
	if err != nil {
		return GroupOwnership{}, memberToolOperationError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return GroupOwnership{}, memberToolOperationError(action, err)
	}
	return ownership, nil
}
