package store

import (
	"context"
	"database/sql"
	"slices"
	"time"

	"agentworkspace/internal/domain"
)

// ListPreparationHistory uses the trigger's storage topic as the bound scope
// and the current effective membership of ordinary history candidates. The
// result is oldest first and without deliveries. Zero limit selects 50.
func (s *Store) ListPreparationHistory(ctx context.Context, groupID, triggerMessageID string, limit int) ([]domain.Message, error) {
	return s.listPreparationHistory(ctx, groupID, triggerMessageID, limit, effectiveHistory)
}

// ListStoredPreparationHistory preserves storage-scoped context for task roles.
func (s *Store) ListStoredPreparationHistory(ctx context.Context, groupID, triggerMessageID string, limit int) ([]domain.Message, error) {
	return s.listPreparationHistory(ctx, groupID, triggerMessageID, limit, storageHistory)
}

func (s *Store) listPreparationHistory(ctx context.Context, groupID, triggerMessageID string, limit int, mode historyScopeMode) ([]domain.Message, error) {
	const action = "读取准备历史失败"
	limit, err := listLimit(ListOptions{Limit: limit})
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, queueError(action, err)
	}
	defer tx.Rollback()
	var topic, stamp string
	if err := tx.QueryRowContext(ctx, `SELECT topic_id,created_at FROM main.messages WHERE group_id=? AND id=?`, groupID, triggerMessageID).
		Scan(&topic, &stamp); err != nil {
		return nil, queueError(action, err)
	}
	createdAt, err := time.Parse(timestampLayout, stamp)
	if err != nil {
		return nil, queueError(action, err)
	}
	rows, err := scopedThreadMessagesInTx(ctx, tx, groupID, topic, mode, &Cursor{CreatedAt: createdAt, ID: triggerMessageID}, "", limit)
	if err != nil {
		return nil, queueError(action, err)
	}
	defer rows.Close()
	history := make([]domain.Message, 0, limit)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, queueError(action, err)
		}
		if mode == effectiveHistory {
			projectEffectiveTopic(&m, topic)
		}
		history = append(history, m)
	}
	if err := rows.Err(); err != nil {
		return nil, queueError(action, err)
	}
	if err := rows.Close(); err != nil {
		return nil, queueError(action, err)
	}
	if err := messageSourcesInTx(ctx, tx, history); err != nil {
		return nil, queueError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, queueError(action, err)
	}
	slices.Reverse(history)
	return history, nil
}
