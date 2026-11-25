package store

import (
	"context"
	"database/sql"
)

// EarliestActiveMemberID returns "" for an existing group with no active members.
// A missing group returns ErrNotFound; this read does not reserve membership.
func (s *Store) EarliestActiveMemberID(ctx context.Context, groupID string) (string, error) {
	id, err := earliestActiveMemberID(ctx, s.db, groupID)
	return id, storeError("读取消息默认接收者失败", err)
}

func earliestActiveMemberID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, groupID string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT COALESCE((
		SELECT m.id FROM members m WHERE m.group_id=g.id AND m.status='active'
		ORDER BY m.created_at ASC, m.id ASC LIMIT 1
	), '') FROM groups g WHERE g.id=?`, groupID).Scan(&id)
	return id, err
}
