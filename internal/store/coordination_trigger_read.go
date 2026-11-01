package store

import (
	"context"
	"database/sql"
	"fmt"

	"agentworkspace/internal/domain"
)

func (s *Store) GetTrigger(ctx context.Context, thread domain.ThreadRef, triggerID string) (domain.Trigger, error) {
	if err := validateTriggerThread(thread); err != nil {
		return domain.Trigger{}, err
	}
	if !validTriggerID(triggerID) {
		return domain.Trigger{}, fmt.Errorf("%w：触发记录标识无效", ErrInvalid)
	}
	trigger, err := scanTrigger(s.db.QueryRowContext(ctx,
		"SELECT "+triggerColumns+" FROM coordination_triggers WHERE group_id=? AND topic_id=? AND id=?", thread.GroupID, thread.TopicID, triggerID))
	return trigger, storeError("读取触发记录失败", err)
}

// ListTriggers paginates capture-time display history, never Source consumption progress.
func (s *Store) ListTriggers(ctx context.Context, thread domain.ThreadRef, options ListOptions) (Page[domain.Trigger], error) {
	if err := validateTriggerThread(thread); err != nil {
		return Page[domain.Trigger]{}, err
	}
	limit, err := listLimit(options)
	if err != nil {
		return Page[domain.Trigger]{}, err
	}
	if c := options.Before; c != nil && !validTriggerID(c.ID) {
		return Page[domain.Trigger]{}, fmt.Errorf("%w：分页游标无效", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Page[domain.Trigger]{}, storeError("列出触发记录失败", err)
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM groups WHERE id=?", thread.GroupID).Scan(&exists); err != nil {
		return Page[domain.Trigger]{}, storeError("列出触发记录失败", err)
	}
	query := "SELECT " + triggerColumns + " FROM coordination_triggers WHERE group_id=? AND topic_id=?"
	args := []any{thread.GroupID, thread.TopicID}
	if c := options.Before; c != nil {
		stamp := c.CreatedAt.UTC().Format(timestampLayout)
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM coordination_triggers
		 WHERE group_id=? AND topic_id=? AND id=? AND created_at=?`, thread.GroupID, thread.TopicID, c.ID, stamp).Scan(&exists); err != nil {
			return Page[domain.Trigger]{}, storeError("读取分页游标失败", err)
		}
		query += " AND (created_at,id)<(?,?)"
		args = append(args, stamp, c.ID)
	}
	query += " ORDER BY created_at DESC,id DESC LIMIT ?"
	rows, err := tx.QueryContext(ctx, query, append(args, limit+1)...)
	if err != nil {
		return Page[domain.Trigger]{}, storeError("列出触发记录失败", err)
	}
	defer rows.Close()
	page := Page[domain.Trigger]{Items: make([]domain.Trigger, 0, limit+1)}
	for rows.Next() {
		trigger, err := scanTrigger(rows)
		if err != nil {
			return Page[domain.Trigger]{}, storeError("读取触发记录列表失败", err)
		}
		page.Items = append(page.Items, trigger)
	}
	if err := rows.Err(); err != nil {
		return Page[domain.Trigger]{}, storeError("读取触发记录列表失败", err)
	}
	if err := rows.Close(); err != nil {
		return Page[domain.Trigger]{}, storeError("读取触发记录列表失败", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	if err := tx.Commit(); err != nil {
		return Page[domain.Trigger]{}, storeError("列出触发记录失败", err)
	}
	return page, nil
}
