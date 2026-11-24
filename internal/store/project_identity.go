package store

import "context"

// GroupOwnership is the persistent structural location of a legacy Group.
type GroupOwnership struct {
	WorkspaceID, ProjectID, ChannelID, GroupID string
}

// GetGroupOwnership resolves an exact legacy Group ID. It does not authorize
// access to the Group or grant access to other Channels in the Project.
func (s *Store) GetGroupOwnership(ctx context.Context, groupID string) (GroupOwnership, error) {
	var ownership GroupOwnership
	err := s.db.QueryRowContext(ctx, `SELECT w.id, p.id, c.id, g.id
		FROM groups g
		JOIN channels c ON c.legacy_group_id=g.id
		JOIN projects p ON p.id=c.project_id
		JOIN workspaces w ON w.id=p.workspace_id
		WHERE g.id=?`, groupID).Scan(&ownership.WorkspaceID, &ownership.ProjectID, &ownership.ChannelID, &ownership.GroupID)
	if err != nil {
		return GroupOwnership{}, storeError("读取群组归属失败", err)
	}
	return ownership, nil
}
