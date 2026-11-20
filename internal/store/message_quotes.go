package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

// Registered by composition, with no dependency on a migration number.
//
//go:embed message_quote_schema.sql
var messageQuoteSchema string

const messageQuoteAvailable = `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='message_quotes')`

func resolveMessageQuoteInTx(ctx context.Context, tx *sql.Tx, r MessageRequest, localUser bool) (*domain.MessageQuote, bool, error) {
	if r.ReplyToMessageID == "." || r.ReplyToMessageID == ".." || !utf8.ValidString(r.ReplyToMessageID) || len(r.ReplyToMessageID) > 256 ||
		strings.TrimSpace(r.ReplyToMessageID) != r.ReplyToMessageID || strings.ContainsFunc(r.ReplyToMessageID, unicode.IsControl) {
		return nil, false, fmt.Errorf("%w：引用消息标识无效", ErrInvalid)
	}
	query := `SELECT m.id,m.group_id,m.topic_id,m.sender_id,coalesce(a.name,''),coalesce(a.status,''),m.content
	 FROM messages m LEFT JOIN members a ON a.id=m.sender_id AND a.group_id=m.group_id
	 WHERE m.group_id=? AND m.id=?`
	args := []any{r.GroupID, r.ReplyToMessageID}
	if !localUser {
		// A member is already fenced to the reply topic. Quoting is not a history grant.
		query += " AND m.topic_id=?"
		args = append(args, r.TopicID)
	}
	var q domain.MessageQuote
	var status, body string
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&q.MessageID, &q.GroupID, &q.TopicID, &q.SenderID, &q.SenderName, &status, &body); err != nil {
		if !localUser && errors.Is(err, sql.ErrNoRows) {
			return nil, false, ErrMemberToolDenied
		}
		return nil, false, err
	}
	kind, _, err := messageSourceInTx(ctx, tx, q.MessageID)
	if err != nil {
		return nil, false, err
	}
	q.SourceKind, q.Excerpt = kind, messageQuoteExcerpt(body)
	// Legacy member speech can lack provenance. A same-group member identity is
	// sufficient to address it, but no platform notice or human command is a member wake.
	activeAuthor := q.SenderID != "user" && status == string(domain.MemberStatusActive) && (kind == "member" || kind == "unknown")
	return &q, activeAuthor, nil
}

func messageQuoteExcerpt(content string) string {
	const limit = 120
	var text strings.Builder
	length, space := 0, false
	for _, char := range content {
		if unicode.IsSpace(char) || unicode.IsControl(char) {
			space = length != 0
			continue
		}
		if space && length < limit {
			text.WriteByte(' ')
			length++
		}
		if length == limit {
			return strings.TrimRight(text.String(), " ") + "…"
		}
		text.WriteRune(char)
		length++
		space = false
	}
	return text.String()
}

func insertMessageQuoteInTx(ctx context.Context, tx *sql.Tx, message domain.Message, q *domain.MessageQuote) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO message_quotes
	 (message_id,group_id,topic_id,quoted_message_id,quoted_topic_id,quoted_source_kind,excerpt) VALUES(?,?,?,?,?,?,?)`,
		message.ID, message.GroupID, message.TopicID, q.MessageID, q.TopicID, q.SourceKind, q.Excerpt)
	return err
}

const messageQuoteColumns = `m.id,m.group_id,m.topic_id,m.sender_id,coalesce(a.name,''),q.quoted_source_kind,q.excerpt`
const messageQuoteJoin = ` FROM message_quotes q
	 JOIN messages m ON m.id=q.quoted_message_id AND m.group_id=q.group_id AND m.topic_id=q.quoted_topic_id
	 LEFT JOIN members a ON a.id=m.sender_id AND a.group_id=m.group_id`

func messageQuoteFields(q *domain.MessageQuote) []any {
	return []any{&q.MessageID, &q.GroupID, &q.TopicID, &q.SenderID, &q.SenderName, &q.SourceKind, &q.Excerpt}
}

// Hydrate at most one bounded batch after the page limit, without reading original bodies.
// For members, both the reply and the target must be in the existing visible topic.
func projectMessageQuotesInTx(ctx context.Context, tx *sql.Tx, groupID string, topicID *string, messages []domain.Message) error {
	if len(messages) == 0 {
		return nil
	}
	var available bool
	if err := tx.QueryRowContext(ctx, messageQuoteAvailable).Scan(&available); err != nil || !available {
		return err // Older databases have no quote edges to project.
	}
	query := "SELECT q.message_id," + messageQuoteColumns + messageQuoteJoin + " WHERE q.group_id=?"
	args := make([]any, 0, len(messages)+3)
	args = append(args, groupID)
	if topicID != nil {
		query += " AND q.topic_id=? AND q.quoted_topic_id=?"
		args = append(args, *topicID, *topicID)
	}
	positions := make(map[string]int, len(messages))
	for i, message := range messages {
		args = append(args, message.ID)
		positions[message.ID] = i
	}
	query += " AND q.message_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(messages)), ",") + ")"
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var q domain.MessageQuote
		if err := rows.Scan(append([]any{&id}, messageQuoteFields(&q)...)...); err != nil {
			return err
		}
		messages[positions[id]].ReplyTo = &q
	}
	return rows.Err()
}

// DeliveryQuote is original content, not the display excerpt or an instruction grant.
type DeliveryQuote struct {
	domain.MessageQuote
	Content string `json:"content"`
}

// ReadDeliveryQuote is for trusted preparation after binding the actual delivery.
// It never widens that delivery's group/topic. Further turn/purpose/grant and
// context-budget checks still belong to the preparer; this is not a member tool.
func (s *Store) ReadDeliveryQuote(ctx context.Context, groupID, deliveryID string) (*DeliveryQuote, error) {
	const action = "读取投递的被引原文失败"
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, storeError(action, err)
	}
	defer tx.Rollback()
	var available bool
	if err := tx.QueryRowContext(ctx, messageQuoteAvailable).Scan(&available); err != nil || !available {
		return nil, storeError(action, err)
	}
	var quote DeliveryQuote
	err = tx.QueryRowContext(ctx, "SELECT "+messageQuoteColumns+",m.content"+messageQuoteJoin+`
	 JOIN deliveries d ON d.message_id=q.message_id AND d.group_id=q.group_id AND d.topic_id=q.topic_id
	 JOIN members r ON r.id=d.recipient_id AND r.group_id=d.group_id AND r.status='active'
	 WHERE d.group_id=? AND d.id=? AND q.quoted_topic_id=d.topic_id`, groupID, deliveryID).
		Scan(append(messageQuoteFields(&quote.MessageQuote), &quote.Content)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // Missing and not visible in this delivery are indistinguishable.
	}
	if err != nil {
		return nil, storeError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, storeError(action, err)
	}
	return &quote, nil
}
