package store

import (
	"context"
	"database/sql"

	"agentworkspace/internal/domain"
)

type historyScopeMode bool

const (
	storageHistory   historyScopeMode = false
	effectiveHistory historyScopeMode = true
)

const qualifiedMessageColumns = "m.id, m.group_id, m.topic_id, m.sender_id, m.content, m.created_at"

// Only an already authorized ordinary scope may opt into effective membership.
// Apply the cursor and limit to key-only candidates before reading full rows.
// A search additionally tests LIKE before LIMIT, but only for in-scope keys.
func scopedThreadMessagesInTx(ctx context.Context, tx *sql.Tx, group, topic string, mode historyScopeMode, before *Cursor, like string, limit int) (*sql.Rows, error) {
	installed, err := threadCorrectionInstalledInTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	const orderLimit = ` ORDER BY m.created_at DESC,m.id DESC LIMIT ?`
	cursor := ""
	var filters []any
	if before != nil {
		cursor = ` AND (m.created_at,m.id)<(?,?)`
		filters = append(filters, before.CreatedAt.UTC().Format(timestampLayout), before.ID)
	}
	if like != "" {
		cursor += ` AND m.content LIKE ? ESCAPE '\'`
		filters = append(filters, like)
	}
	filters = append(filters, limit)
	from := ` FROM main.messages m WHERE m.group_id=? AND m.topic_id=?`
	args := append([]any{group, topic}, filters...)
	if !installed {
		return tx.QueryContext(ctx, `SELECT `+qualifiedMessageColumns+from+cursor+orderLimit, args...)
	}
	// Correction commands are visible in the group timeline, never model context.
	from += ` AND NOT EXISTS(SELECT 1 FROM main.thread_corrections cmd WHERE cmd.command_message_id=m.id)`
	if mode == storageHistory {
		return tx.QueryContext(ctx, `SELECT `+qualifiedMessageColumns+from+cursor+orderLimit, args...)
	}
	from += ` AND coalesce((` + latestEffectiveTopicSQL + `),m.topic_id)=m.topic_id`
	// The outgoing branch uses the raw topic cursor index. The incoming branch
	// starts with the indexed destination and accepts only the latest move. The
	// branches are disjoint and each contributes at most one page of row keys.
	query := `WITH original AS (SELECT m.id,m.created_at` + from + cursor + orderLimit + `), incoming AS (
	 SELECT m.id,m.created_at FROM main.thread_corrections c CROSS JOIN main.messages m
	 WHERE c.group_id=? AND c.effective_topic_id=? AND c.previous_topic_id<>c.effective_topic_id
	 AND c.original_topic_id<>c.effective_topic_id
	 AND c.sequence=(SELECT latest.sequence FROM main.thread_corrections latest
	   WHERE latest.group_id=c.group_id AND latest.target_message_id=c.target_message_id
	   AND latest.previous_topic_id<>latest.effective_topic_id ORDER BY latest.sequence DESC LIMIT 1)
	 AND (m.id,m.group_id,m.topic_id)=(c.target_message_id,c.group_id,c.original_topic_id)` + cursor + orderLimit + `),
	 page AS (SELECT * FROM original UNION ALL SELECT * FROM incoming ORDER BY created_at DESC,id DESC LIMIT ?)
	 SELECT ` + qualifiedMessageColumns + ` FROM page CROSS JOIN main.messages m WHERE m.id=page.id ORDER BY page.created_at DESC,page.id DESC`
	args = append(args, group, topic)
	args = append(args, filters...)
	return tx.QueryContext(ctx, query, append(args, limit)...)
}

// References to c must stay keyed by both immutable group and target ID.
const latestEffectiveTopicSQL = `SELECT c.effective_topic_id FROM main.thread_corrections c
	 WHERE c.group_id=m.group_id AND c.target_message_id=m.id AND c.previous_topic_id<>c.effective_topic_id
	 ORDER BY c.sequence DESC LIMIT 1`

func projectEffectiveTopic(message *domain.Message, topic string) {
	if message.TopicID != topic {
		message.EffectiveTopicID = &topic
	}
}
