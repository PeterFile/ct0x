package store

import (
	"context"
	"database/sql"

	"agentworkspace/internal/domain"
)

// ListArtifacts is a trusted scoped read; the platform binds caller grants.
func (s *Store) ListArtifacts(ctx context.Context, groupID string, options ListOptions) (Page[domain.Artifact], error) {
	limit, err := listLimit(options)
	if err != nil {
		return Page[domain.Artifact]{}, storeError("列出成果失败", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Page[domain.Artifact]{}, storeError("列出成果失败", err)
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM groups WHERE id=?", groupID).Scan(&exists); err != nil {
		return Page[domain.Artifact]{}, storeError("列出成果失败", err)
	}
	query := "SELECT " + artifactColumns + " FROM artifacts WHERE group_id=?"
	args := []any{groupID}
	if c := options.Before; c != nil {
		query += " AND (created_at,id)<(?,?)"
		args = append(args, c.CreatedAt.UTC().Format(timestampLayout), c.ID)
	}
	query += " ORDER BY created_at DESC,id DESC LIMIT ?"
	rows, err := tx.QueryContext(ctx, query, append(args, limit+1)...)
	if err != nil {
		return Page[domain.Artifact]{}, storeError("列出成果失败", err)
	}
	defer rows.Close()
	page := Page[domain.Artifact]{Items: make([]domain.Artifact, 0, limit+1)}
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return Page[domain.Artifact]{}, storeError("读取成果列表失败", err)
		}
		page.Items = append(page.Items, a)
	}
	if err := rows.Err(); err != nil {
		return Page[domain.Artifact]{}, storeError("读取成果列表失败", err)
	}
	if err := rows.Close(); err != nil {
		return Page[domain.Artifact]{}, storeError("读取成果列表失败", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	if err := tx.Commit(); err != nil {
		return Page[domain.Artifact]{}, storeError("列出成果失败", err)
	}
	return page, nil
}

// ListArtifactVersions includes historical and conflict evidence regardless of the shared head.
func (s *Store) ListArtifactVersions(ctx context.Context, groupID, artifactID string, options ListOptions) (Page[domain.ArtifactVersion], error) {
	limit, err := listLimit(options)
	if err != nil {
		return Page[domain.ArtifactVersion]{}, storeError("列出成果版本失败", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Page[domain.ArtifactVersion]{}, storeError("列出成果版本失败", err)
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM artifacts WHERE group_id=? AND id=?", groupID, artifactID).Scan(&exists); err != nil {
		return Page[domain.ArtifactVersion]{}, storeError("列出成果版本失败", err)
	}
	query := "SELECT " + artifactVersionColumns + " FROM artifact_versions WHERE group_id=? AND artifact_id=?"
	args := []any{groupID, artifactID}
	if c := options.Before; c != nil {
		query += " AND (created_at,id)<(?,?)"
		args = append(args, c.CreatedAt.UTC().Format(timestampLayout), c.ID)
	}
	query += " ORDER BY created_at DESC,id DESC LIMIT ?"
	rows, err := tx.QueryContext(ctx, query, append(args, limit+1)...)
	if err != nil {
		return Page[domain.ArtifactVersion]{}, storeError("列出成果版本失败", err)
	}
	defer rows.Close()
	page := Page[domain.ArtifactVersion]{Items: make([]domain.ArtifactVersion, 0, limit+1)}
	for rows.Next() {
		v, err := scanArtifactVersion(rows)
		if err != nil {
			return Page[domain.ArtifactVersion]{}, storeError("读取成果版本列表失败", err)
		}
		page.Items = append(page.Items, v)
	}
	if err := rows.Err(); err != nil {
		return Page[domain.ArtifactVersion]{}, storeError("读取成果版本列表失败", err)
	}
	if err := rows.Close(); err != nil {
		return Page[domain.ArtifactVersion]{}, storeError("读取成果版本列表失败", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	if err := tx.Commit(); err != nil {
		return Page[domain.ArtifactVersion]{}, storeError("列出成果版本失败", err)
	}
	return page, nil
}
