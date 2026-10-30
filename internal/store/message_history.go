package store

import (
	"context"
	"database/sql"
	"fmt"

	"agentworkspace/internal/domain"
)

func (s *Store) ListMessages(ctx context.Context, groupID string, options MessageListOptions) (Page[domain.Message], error) {
	if options.Before != nil && options.After != nil {
		return Page[domain.Message]{}, fmt.Errorf("%w：不能同时指定前向和后向游标", ErrInvalid)
	}
	cursor, relation, order := options.Before, "<", "DESC"
	if options.After != nil {
		cursor, relation, order = options.After, ">", "ASC"
	}
	limit, err := listLimit(ListOptions{Limit: options.Limit, Before: cursor})
	if err != nil {
		return Page[domain.Message]{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Page[domain.Message]{}, storeError("读取消息历史失败", err)
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM groups WHERE id=?", groupID).Scan(&exists); err != nil {
		return Page[domain.Message]{}, storeError("读取消息历史失败", err)
	}
	query := "SELECT " + messageColumns + " FROM messages WHERE group_id=?"
	args := []any{groupID}
	if cursor != nil {
		query += " AND (created_at, id) " + relation + " (?, ?)"
		args = append(args, cursor.CreatedAt.UTC().Format(timestampLayout), cursor.ID)
	}
	query += " ORDER BY created_at " + order + ", id " + order + " LIMIT ?"
	rows, err := tx.QueryContext(ctx, query, append(args, limit+1)...)
	if err != nil {
		return Page[domain.Message]{}, storeError("读取消息历史失败", err)
	}
	defer rows.Close()
	page := Page[domain.Message]{Items: make([]domain.Message, 0, limit+1)}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return Page[domain.Message]{}, storeError("读取消息历史失败", err)
		}
		page.Items = append(page.Items, m)
	}
	if err := rows.Err(); err != nil {
		return Page[domain.Message]{}, storeError("读取消息历史失败", err)
	}
	if err := rows.Close(); err != nil {
		return Page[domain.Message]{}, storeError("读取消息历史失败", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	for i := range page.Items {
		if err := messageDeliveries(ctx, tx, &page.Items[i]); err != nil {
			return Page[domain.Message]{}, storeError("读取消息投递记录失败", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Page[domain.Message]{}, storeError("读取消息历史失败", err)
	}
	return page, nil
}
