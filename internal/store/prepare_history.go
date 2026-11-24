package store

import (
	"context"
	"database/sql"
	"slices"
	"time"

	"agentworkspace/internal/domain"
)

const preparationHistoryError = "读取准备历史失败"

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
	limit, err := listLimit(ListOptions{Limit: limit})
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, queueError(preparationHistoryError, err)
	}
	defer tx.Rollback()
	thread, before, err := preparationTriggerInTx(ctx, tx, groupID, triggerMessageID)
	if err != nil {
		return nil, queueError(preparationHistoryError, err)
	}
	rows, err := scopedThreadMessagesInTx(ctx, tx, groupID, thread.TopicID, mode, &before, "", limit)
	if err != nil {
		return nil, queueError(preparationHistoryError, err)
	}
	history, err := scanPreparationHistoryInTx(ctx, tx, rows, thread.TopicID, mode, limit)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return nil, queueError(preparationHistoryError, err)
	}
	return history, nil
}

func preparationTriggerInTx(ctx context.Context, tx *sql.Tx, groupID, messageID string) (domain.ThreadRef, Cursor, error) {
	thread := domain.ThreadRef{GroupID: groupID}
	var stamp string
	if err := tx.QueryRowContext(ctx, `SELECT topic_id,created_at FROM main.messages WHERE group_id=? AND id=?`, groupID, messageID).
		Scan(&thread.TopicID, &stamp); err != nil {
		return thread, Cursor{}, err
	}
	createdAt, err := time.Parse(timestampLayout, stamp)
	return thread, Cursor{CreatedAt: createdAt, ID: messageID}, err
}

// Only a verified mark can select log order. The caller has proved that no
// incoming move is left unaccounted for by the mark. Filter out commands and
// moved-out rows before LIMIT, keeping the exact indexed storage-log seek.
func coveredPreparationHistoryInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, triggerID string, coveredThrough int64, correctionsInstalled bool, limit int) (*sql.Rows, error) {
	filters := ""
	if correctionsInstalled {
		filters = ` AND (m.id IS NULL OR (` + notCorrectionCommandSQL + ` AND ` + effectiveHistoryTopicSQL + `=l.topic_id))`
	}
	// A missing message in the selected log range should fail scanning, not
	// silently widen the window. Hence the left join and the IS NULL branch.
	return tx.QueryContext(ctx, `SELECT `+qualifiedMessageColumns+`
	 FROM main.message_log l INDEXED BY message_log_topic_sequence
	 LEFT JOIN main.messages m ON (m.id,m.group_id,m.topic_id)=(l.message_id,l.group_id,l.topic_id)
	 WHERE l.group_id=?1 AND l.topic_id=?2 AND l.sequence>?3 AND l.sequence<(
	    SELECT sequence FROM main.message_log WHERE group_id=?1 AND topic_id=?2 AND message_id=?4)`+filters+`
	 ORDER BY l.sequence DESC LIMIT ?5`, thread.GroupID, thread.TopicID, coveredThrough, triggerID, limit)
}

func scanPreparationHistoryInTx(ctx context.Context, tx *sql.Tx, rows *sql.Rows, topic string, mode historyScopeMode, limit int) ([]domain.Message, error) {
	defer rows.Close()
	history := make([]domain.Message, 0, limit)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		if mode == effectiveHistory {
			projectEffectiveTopic(&m, topic)
		}
		history = append(history, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := messageSourcesInTx(ctx, tx, history); err != nil {
		return nil, err
	}
	slices.Reverse(history)
	return history, nil
}
