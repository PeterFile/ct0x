package store

import (
	"context"
	"database/sql"
	"strings"

	"agentworkspace/internal/domain"
)

// MessageThreadAttribution is a metadata-only lookup for trusted context
// builders after their normal visibility checks. Never use it to bind a Turn
// or widen a member's read scope.
func (s *Store) MessageThreadAttribution(ctx context.Context, groupID, messageID string) (domain.ThreadAttribution, error) {
	const action = "读取话题归属失败"
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.ThreadAttribution{}, storeError(action, err)
	}
	defer tx.Rollback()
	attr, err := messageThreadAttributionInTx(ctx, tx, groupID, messageID)
	if err != nil {
		return domain.ThreadAttribution{}, storeError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return domain.ThreadAttribution{}, storeError(action, err)
	}
	return attr, nil
}

// Resolve inside the caller's snapshot/confirmation transaction. A caller must
// separately check group visibility and its own confirmation or read authority.
func messageThreadAttributionInTx(ctx context.Context, tx *sql.Tx, groupID, messageID string) (domain.ThreadAttribution, error) {
	ref := domain.ThreadRef{GroupID: groupID}
	if err := tx.QueryRowContext(ctx, `SELECT topic_id FROM main.messages WHERE group_id=? AND id=?`, groupID, messageID).Scan(&ref.TopicID); err != nil {
		return domain.ThreadAttribution{}, err
	}
	attr := domain.ThreadAttribution{Storage: ref, Effective: ref}
	installed, err := threadCorrectionInstalledInTx(ctx, tx)
	if err != nil || !installed {
		return attr, err
	}
	err = tx.QueryRowContext(ctx, `SELECT effective_topic_id,command_message_id FROM main.thread_corrections
	 WHERE group_id=? AND target_message_id=? AND previous_topic_id<>effective_topic_id ORDER BY sequence DESC LIMIT 1`, groupID, messageID).
		Scan(&attr.Effective.TopicID, &attr.CorrectionMessageID)
	if err == sql.ErrNoRows {
		err = nil
	}
	return attr, err
}

// Receipts are keyed by the command, never by re-resolving the target.
func messageThreadCorrectionsInTx(ctx context.Context, tx *sql.Tx, groupID string, items []domain.Message) error {
	if len(items) == 0 {
		return nil
	}
	installed, err := threadCorrectionInstalledInTx(ctx, tx)
	if err != nil || !installed {
		return err
	}
	positions := make(map[string]int, len(items))
	args := make([]any, 1, len(items)+1)
	args[0] = groupID
	for i := range items {
		positions[items[i].ID] = i
		args = append(args, items[i].ID)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(items)), ",")
	rows, err := tx.QueryContext(ctx, `SELECT command_message_id,coalesce(target_message_id,''),coalesce(original_topic_id,''),
	 coalesce(previous_topic_id,''),coalesce(effective_topic_id,''),question FROM main.thread_corrections
	 WHERE group_id=? AND command_message_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var correction domain.ThreadCorrection
		if err := rows.Scan(&id, &correction.MessageID, &correction.OriginalTopicID, &correction.PreviousTopicID, &correction.EffectiveTopicID, &correction.Question); err != nil {
			return err
		}
		items[positions[id]].ThreadCorrection = &correction
	}
	return rows.Err()
}

// Exact page keys only. The one visible timeline and its cursors are unchanged.
func messageThreadLinksInTx(ctx context.Context, tx *sql.Tx, groupID string, items []domain.Message) error {
	if len(items) == 0 {
		return nil
	}
	installed, err := threadRoutingInstalledInTx(ctx, tx)
	if err != nil || !installed {
		return err
	}
	positions := make(map[string]int, len(items))
	args := make([]any, 1, len(items)+1)
	args[0] = groupID
	for i := range items {
		positions[items[i].ID] = i
		args = append(args, items[i].ID)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(items)), ",")
	rows, err := tx.QueryContext(ctx, `SELECT l.message_id,coalesce(l.reply_to_message_id,''),coalesce(l.task_id,'')
	 FROM main.thread_routing_links l WHERE l.group_id=? AND l.message_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var links domain.ThreadLinks
		if err := rows.Scan(&id, &links.ReplyToMessageID, &links.RelatedTaskID); err != nil {
			return err
		}
		items[positions[id]].ThreadLinks = links
	}
	return rows.Err()
}
