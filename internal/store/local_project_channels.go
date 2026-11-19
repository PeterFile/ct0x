package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type ChannelView struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	GroupID     string    `json:"group_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

type CreateChannelInput struct {
	ProjectID, Name, Description, IdempotencyKey string
}

// Keep channels as the outer loop for index-ordered project paging. Each
// candidate counts toward LIMIT even if its primary-key Group lookup misses.
const channelViewSelect = `SELECT c.id, c.project_id, g.id, c.name, c.description, c.created_at
 FROM channels c LEFT JOIN groups g ON g.id = c.legacy_group_id
 WHERE c.project_id = ?`

// CreateChannel is a trusted local-user command. The channel, legacy group,
// and optional idempotency reservation commit together.
func (s *Store) CreateChannel(ctx context.Context, input CreateChannelInput) (ChannelView, error) {
	const action = "创建频道失败"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ChannelView{}, storeError(action, err)
	}
	defer tx.Rollback()
	// Verify scope in the same transaction, before even reading a receipt.
	if err := requireLocalProjectInTx(ctx, tx, input.ProjectID); err != nil {
		return ChannelView{}, storeError(action, err)
	}
	scope := mutationScope{operation: "create_channel", actorID: "user", groupID: input.ProjectID, key: input.IdempotencyKey}
	id, replay, err := reserveMutation(ctx, tx, scope, mutationPayload(input.ProjectID, input.Name, input.Description))
	if err != nil {
		return ChannelView{}, storeError(action, err)
	}
	if !replay {
		if strings.TrimSpace(input.Name) == "" {
			return ChannelView{}, storeError(action, fmt.Errorf("%w：频道名称不能为空", ErrInvalid))
		}
		groupID := rand.Text()
		createdAt := time.Now().UTC().Format(timestampLayout)
		// Reserve the binding first (the group FK is deferred) so the legacy
		// group's insert trigger does not create a second, default project.
		var insertedID string
		err := tx.QueryRowContext(ctx, `INSERT INTO channels(id, project_id, legacy_group_id, name, description, created_at)
		 VALUES(?, ?, ?, ?, ?, ?) RETURNING id`,
			id, input.ProjectID, groupID, input.Name, input.Description, createdAt).Scan(&insertedID)
		if err != nil {
			return ChannelView{}, storeError(action, err)
		}
		if _, err := insertGroupInTx(ctx, tx, groupID, input.Name, input.Description, createdAt); err != nil {
			return ChannelView{}, storeError(action, err)
		}
	}
	channel, err := scanChannelView(tx.QueryRowContext(ctx, channelViewSelect+" AND c.id = ?", input.ProjectID, id))
	if err != nil {
		return ChannelView{}, storeError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return ChannelView{}, storeError(action, err)
	}
	return channel, nil
}

func (s *Store) GetChannel(ctx context.Context, projectID, channelID string) (ChannelView, error) {
	query := channelViewSelect + ` AND c.id = ?
	 AND EXISTS(SELECT 1 FROM projects p WHERE p.id = c.project_id AND ` + localProjectScope + `)`
	channel, err := scanChannelView(s.db.QueryRowContext(ctx, query, projectID, channelID))
	return channel, storeError("读取频道失败", err)
}

func (s *Store) ListChannels(ctx context.Context, projectID string, options ListOptions) (Page[ChannelView], error) {
	const action = "列出频道失败"
	limit, err := listLimit(options)
	if err != nil {
		return Page[ChannelView]{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Page[ChannelView]{}, storeError(action, err)
	}
	defer tx.Rollback()
	if err := requireLocalProjectInTx(ctx, tx, projectID); err != nil {
		return Page[ChannelView]{}, storeError(action, err)
	}
	query, args := channelPageQuery(projectID, options.Before, limit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return Page[ChannelView]{}, storeError(action, err)
	}
	page, err := collectLocalProjectPage(rows, limit, scanChannelView, func(c ChannelView) Cursor {
		return Cursor{CreatedAt: c.CreatedAt, ID: c.ID}
	})
	if err != nil {
		return Page[ChannelView]{}, storeError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return Page[ChannelView]{}, storeError(action, err)
	}
	return page, nil
}

func channelPageQuery(projectID string, before *Cursor, limit int) (string, []any) {
	query, args := channelViewSelect, []any{projectID}
	if before != nil {
		query += " AND (c.created_at, c.id) < (?, ?)"
		args = append(args, before.CreatedAt.UTC().Format(timestampLayout), before.ID)
	}
	return query + " ORDER BY c.created_at DESC, c.id DESC LIMIT ?", append(args, limit)
}

func scanChannelView(row scanner) (ChannelView, error) {
	var c ChannelView
	var stamp string
	var groupID sql.NullString
	if err := row.Scan(&c.ID, &c.ProjectID, &groupID, &c.Name, &c.Description, &stamp); err != nil {
		return ChannelView{}, err
	}
	if !groupID.Valid {
		return ChannelView{}, sql.ErrNoRows
	}
	createdAt, err := time.Parse(timestampLayout, stamp)
	if err != nil {
		return ChannelView{}, err
	}
	c.GroupID = groupID.String
	c.CreatedAt = createdAt
	return c, nil
}
