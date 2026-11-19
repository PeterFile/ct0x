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
	return s.listPreparationHistory(ctx, groupID, triggerMessageID, limit, nil)
}

// The optional hook binds and loads state in the same read snapshot. It may
// return an explicit lower message_log bound; zero preserves legacy ordering.
func (s *Store) listPreparationHistory(ctx context.Context, groupID, triggerMessageID string, limit int,
	checkTrigger func(*sql.Tx, domain.Message) (int64, error)) ([]domain.Message, error) {
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
	var coveredThrough int64
	if checkTrigger != nil {
		coveredThrough, err = checkTrigger(tx, trigger)
		if err != nil {
			return nil, queueError(action, err)
		}
	}
	var rows *sql.Rows
	if coveredThrough > 0 {
		// Seek the exact scope before looking up bodies. A missing message in
		// the selected log range fails scanning instead of widening the window.
		rows, err = tx.QueryContext(ctx, `SELECT m.id,m.group_id,m.topic_id,m.sender_id,m.content,m.created_at
		 FROM message_log l INDEXED BY message_log_topic_sequence
		 LEFT JOIN messages m ON (m.id,m.group_id,m.topic_id)=(l.message_id,l.group_id,l.topic_id)
		 WHERE l.group_id=?1 AND l.topic_id=?2 AND l.sequence>?3 AND l.sequence<(
		    SELECT sequence FROM message_log WHERE group_id=?1 AND topic_id=?2 AND message_id=?4)
		 ORDER BY l.sequence DESC LIMIT ?5`, groupID, trigger.TopicID, coveredThrough, trigger.ID, limit)
	} else {
		rows, err = tx.QueryContext(ctx, "SELECT "+messageColumns+` FROM messages
		 WHERE group_id=? AND topic_id=? AND (created_at, id) < (?, ?)
		 ORDER BY created_at DESC, id DESC LIMIT ?`,
			groupID, trigger.TopicID, trigger.CreatedAt.UTC().Format(timestampLayout), trigger.ID, limit)
	}
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
	if err := messageSourcesInTx(ctx, tx, history); err != nil {
		return nil, queueError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, queueError(action, err)
	}
	slices.Reverse(history)
	return history, nil
}
