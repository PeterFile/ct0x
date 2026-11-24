package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"agentworkspace/internal/domain"
)

// MessageThreadAttribution is a metadata-only lookup for trusted context
// builders after their normal visibility checks. Never use it to bind a Turn
// or widen a member's read scope.
func (s *Store) MessageThreadAttribution(ctx context.Context, groupID, messageID string) (domain.ThreadAttribution, error) {
	ref := domain.ThreadRef{GroupID: groupID}
	err := s.db.QueryRowContext(ctx, `SELECT topic_id FROM main.messages WHERE group_id=? AND id=?`, groupID, messageID).Scan(&ref.TopicID)
	if err != nil {
		return domain.ThreadAttribution{}, storeError("读取话题归属失败", err)
	}
	return domain.ThreadAttribution{Storage: ref, Effective: ref}, nil
}

// Local-user projection for exact page keys, not a member history API.
// Prefer the canonical quote, but preserve old E-only reply edges and Task links.
func messageThreadLinksInTx(ctx context.Context, tx *sql.Tx, groupID string, items []domain.Message) error {
	if len(items) == 0 {
		return nil
	}
	if err := projectMessageQuotesInTx(ctx, tx, groupID, nil, items); err != nil {
		return err
	}
	for i := range items {
		if q := items[i].ReplyTo; q != nil {
			items[i].ReplyToMessageID = q.MessageID
		}
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
		item := &items[positions[id]]
		if links.ReplyToMessageID != "" {
			if item.ReplyTo != nil {
				return errors.New("消息存在重复引用记录")
			}
			item.ReplyToMessageID = links.ReplyToMessageID
		}
		item.RelatedTaskID = links.RelatedTaskID
	}
	return rows.Err()
}
