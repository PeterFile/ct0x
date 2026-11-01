package store

import (
	"context"
	"fmt"

	"agentworkspace/internal/domain"
)

func (s *Store) ListGroups(ctx context.Context, options ListOptions) (Page[domain.Group], error) {
	limit, err := listLimit(options)
	if err != nil {
		return Page[domain.Group]{}, err
	}
	query := "SELECT " + groupColumns + " FROM groups"
	args := []any{}
	if c := options.Before; c != nil {
		query += " WHERE (created_at, id) < (?, ?)"
		args = append(args, c.CreatedAt.UTC().Format(timestampLayout), c.ID)
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT ?"
	rows, err := s.db.QueryContext(ctx, query, append(args, limit+1)...)
	if err != nil {
		return Page[domain.Group]{}, storeError("列出群组失败", err)
	}
	defer rows.Close()
	page := Page[domain.Group]{Items: make([]domain.Group, 0, limit+1)}
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return Page[domain.Group]{}, storeError("读取群组列表失败", err)
		}
		page.Items = append(page.Items, g)
	}
	if err := rows.Err(); err != nil {
		return Page[domain.Group]{}, storeError("读取群组列表失败", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func (s *Store) ListMembers(ctx context.Context, groupID string, options ListOptions) (Page[domain.Member], error) {
	limit, err := listLimit(options)
	if err != nil {
		return Page[domain.Member]{}, err
	}
	if _, err := s.GetGroup(ctx, groupID); err != nil {
		return Page[domain.Member]{}, err
	}
	query := "SELECT " + memberColumns + " FROM members WHERE group_id = ?"
	args := []any{groupID}
	if c := options.Before; c != nil {
		query += " AND (created_at, id) < (?, ?)"
		args = append(args, c.CreatedAt.UTC().Format(timestampLayout), c.ID)
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT ?"
	rows, err := s.db.QueryContext(ctx, query, append(args, limit+1)...)
	if err != nil {
		return Page[domain.Member]{}, storeError("列出成员失败", err)
	}
	defer rows.Close()
	page := Page[domain.Member]{Items: make([]domain.Member, 0, limit+1)}
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return Page[domain.Member]{}, storeError("读取成员列表失败", err)
		}
		page.Items = append(page.Items, m)
	}
	if err := rows.Err(); err != nil {
		return Page[domain.Member]{}, storeError("读取成员列表失败", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func listLimit(options ListOptions) (int, error) {
	if options.Limit < 0 || options.Limit > 200 {
		return 0, fmt.Errorf("%w：列表数量须在 1 到 200 之间", ErrInvalid)
	}
	if c := options.Before; c != nil {
		stamp := c.CreatedAt.UTC()
		if c.ID == "" || stamp.IsZero() || stamp.Year() < 1 || stamp.Year() > 9999 {
			return 0, fmt.Errorf("%w：分页游标无效", ErrInvalid)
		}
	}
	if options.Limit == 0 {
		return 50, nil
	}
	return options.Limit, nil
}
