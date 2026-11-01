package store

import (
	"context"
	"database/sql"
	"slices"

	"agentworkspace/internal/domain"
)

// ListPreparationHistory returns the newest messages strictly before the trigger
// in its authoritative group/topic, ordered oldest first and without deliveries.
// Zero limit selects 50; explicit limits must be 1 through 200.
func (s *Store) ListPreparationHistory(ctx context.Context, groupID, triggerMessageID string, limit int) ([]domain.Message, error) {
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
	trigger, err := scanMessage(tx.QueryRowContext(ctx,
		"SELECT "+messageColumns+" FROM messages WHERE group_id=? AND id=?", groupID, triggerMessageID))
	if err != nil {
		return nil, queueError(action, err)
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+messageColumns+` FROM messages
	 WHERE group_id=? AND topic_id=? AND (created_at, id) < (?, ?)
	 ORDER BY created_at DESC, id DESC LIMIT ?`,
		groupID, trigger.TopicID, trigger.CreatedAt.UTC().Format(timestampLayout), trigger.ID, limit)
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
		history = append(history, m)
	}
	if err := rows.Err(); err != nil {
		return nil, queueError(action, err)
	}
	if err := rows.Close(); err != nil {
		return nil, queueError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, queueError(action, err)
	}
	slices.Reverse(history)
	return history, nil
}
