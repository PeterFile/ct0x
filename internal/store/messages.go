package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"agentworkspace/internal/domain"
)

// ActorID is platform-bound; recipients describe the original request, not routing output.
type MessageRequest struct {
	GroupID, TopicID, Content, IdempotencyKey string
	ActorID                                   string   `json:"-"`
	RecipientIDs                              []string // Nil permits routing; non-nil (even empty) is an explicit set.
	TopicIDExplicit                           bool     `json:"-"`
	domain.ThreadLinks
}

type MessageListOptions struct {
	Limit         int     // Zero selects 50; maximum 200.
	Before, After *Cursor // Mutually exclusive; After reads oldest first.
}

const messageColumns = "id, group_id, topic_id, sender_id, content, created_at"

func (r MessageRequest) scope() mutationScope {
	return mutationScope{operation: "create_message", actorID: r.ActorID, groupID: r.GroupID, key: r.IdempotencyKey}
}

// Member sends retain A3's quote-only command bytes. Local posts use E's
// thread-link format, whether they persist an A quote or a legacy E edge.
func (r MessageRequest) payload() []byte { return r.encodePayload("", r.ActorID != "user") }

// Only the idempotency record needs the new selector discriminator. Keep the
// immutable command-source payload in its existing exact format.
func (r MessageRequest) topicSelectionPayload() []byte {
	if r.TopicIDExplicit {
		return r.encodePayload("+topic-selector/v1:main", false)
	}
	return r.encodePayload("+topic-selector/v1:auto", false)
}

// Pre-composition A3 used the quote-only fingerprint for local user messages
// too. This is a lookup alias only, never a new reservation or source proof.
func (r MessageRequest) hasLegacyQuotePayload() bool {
	return r.ActorID == "user" && r.ReplyToMessageID != "" && r.RelatedTaskID == ""
}

func (r MessageRequest) encodePayload(selector string, quoteOnly bool) []byte {
	fields := []string{r.GroupID, r.TopicID, r.Content, "omitted"}
	if r.RecipientIDs != nil {
		fields[3] = "explicit"
	}
	// Neither marker can collide with a legacy recipient list of any length.
	if quoteOnly && r.ReplyToMessageID != "" && r.RelatedTaskID == "" {
		fields[3] += "/quote"
		fields = append(fields, r.ReplyToMessageID)
	} else if r.ThreadLinks != (domain.ThreadLinks{}) {
		fields[3] += "+thread-links/v1"
		fields = append(fields, r.ReplyToMessageID, r.RelatedTaskID)
	}
	fields[3] += selector // Cannot collide with a legacy recipient list or link payload.
	return mutationPayload(append(fields, recipientSet(r.RecipientIDs)...)...)
}

func recipientSet(ids []string) []string {
	set := slices.Clone(ids)
	slices.Sort(set)
	return slices.Compact(set)
}

func (s *Store) LookupMessageReceipt(ctx context.Context, request MessageRequest) (domain.Message, bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.Message{}, false, storeError("读取消息回执失败", err)
	}
	defer tx.Rollback()
	if err := requireMessageActor(ctx, tx, request.GroupID, request.ActorID); err != nil {
		return domain.Message{}, false, storeError("读取消息回执失败", err)
	}
	id, found, err := lookupMutation(ctx, tx, request.scope(), request.payload())
	if request.ActorID == "user" && request.TopicID == "" && errors.Is(err, ErrConflict) {
		id, found, err = lookupMutation(ctx, tx, request.scope(), request.topicSelectionPayload())
	}
	if errors.Is(err, ErrConflict) && request.hasLegacyQuotePayload() {
		id, found, err = lookupMutation(ctx, tx, request.scope(), request.encodePayload("", true))
	}
	if err != nil {
		return domain.Message{}, false, storeError("读取消息回执失败", err)
	}
	var m domain.Message
	if found {
		if request.ActorID == "user" {
			m, err = localMessageReceiptInTx(ctx, tx, request, id, true)
		} else {
			m, err = scopedMessageReceiptInTx(ctx, tx, request, id)
		}
		if err != nil {
			return domain.Message{}, false, storeError("读取消息回执失败", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Message{}, false, storeError("读取消息回执失败", err)
	}
	return m, found, nil
}

func (s *Store) CreateMessage(ctx context.Context, request MessageRequest, resolvedRecipientIDs []string) (domain.Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Message{}, storeError("创建消息失败", err)
	}
	defer tx.Rollback()
	m, err := createMessageInTx(ctx, tx, request, resolvedRecipientIDs)
	if err != nil {
		return domain.Message{}, storeError("创建消息失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Message{}, storeError("保存消息失败", err)
	}
	return m, nil
}

// insertMessageRow requires a fully populated row from a trusted caller: ID,
// bound GroupID/SenderID/TopicID, content, UTC CreatedAt, and zero deliveries.
// The caller owns authorization/fencing and the transaction; this only inserts the row.
func insertMessageRow(ctx context.Context, tx *sql.Tx, message domain.Message) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO messages("+messageColumns+") VALUES(?, ?, ?, ?, ?, ?)",
		message.ID, message.GroupID, message.TopicID, message.SenderID, message.Content,
		message.CreatedAt.UTC().Format(timestampLayout))
	return err
}

func createMessageInTx(ctx context.Context, tx *sql.Tx, r MessageRequest, resolved []string) (domain.Message, error) {
	// Access precedes receipt lookup, so inaccessible actors cannot probe keys or conflicts.
	if err := requireMessageActor(ctx, tx, r.GroupID, r.ActorID); err != nil {
		return domain.Message{}, err
	}
	if r.RelatedTaskID != "" {
		return domain.Message{}, fmt.Errorf("%w：任务关联仅可在本机用户发言中使用", ErrInvalid)
	}
	id, replay, err := reserveMutation(ctx, tx, r.scope(), r.payload())
	if err != nil {
		return domain.Message{}, err
	}
	if replay {
		return scopedMessageReceiptInTx(ctx, tx, r, id)
	}
	var quote *domain.MessageQuote
	if r.ReplyToMessageID != "" {
		quote, _, err = resolveMessageQuoteInTx(ctx, tx, r, false)
		if err != nil {
			return domain.Message{}, err
		}
	}
	return insertRequestedMessageInTx(ctx, tx, r, id, r.TopicID, resolved, quote)
}

// The request, quote and id are already verified. Only the new row and its
// deliveries take the resolved storage topic, not the original request proof.
func insertRequestedMessageInTx(ctx context.Context, tx *sql.Tx, r MessageRequest, id, topicID string, resolved []string, quote *domain.MessageQuote) (domain.Message, error) {
	if strings.TrimSpace(r.Content) == "" {
		return domain.Message{}, fmt.Errorf("%w：消息内容不能为空", ErrInvalid)
	}
	resolved = recipientSet(resolved)
	// Nil permits routing; every explicit set, including an empty one, is authoritative.
	if r.RecipientIDs != nil && !slices.Equal(recipientSet(r.RecipientIDs), resolved) {
		return domain.Message{}, fmt.Errorf("%w：实际接收者必须与明确指定的接收者一致", ErrInvalid)
	}
	for _, recipient := range resolved {
		var active int
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM members WHERE group_id=? AND id=? AND status='active'", r.GroupID, recipient).Scan(&active); err != nil {
			return domain.Message{}, err
		}
	}
	message := domain.Message{
		ID: id, GroupID: r.GroupID, TopicID: topicID, SenderID: r.ActorID,
		Content: r.Content, CreatedAt: time.Now().UTC(),
	}
	if err := insertMessageRow(ctx, tx, message); err != nil {
		return domain.Message{}, err
	}
	if quote != nil {
		if err := insertMessageQuoteInTx(ctx, tx, message, quote); err != nil {
			return domain.Message{}, err
		}
	}
	stamp := message.CreatedAt.Format(timestampLayout)
	for _, recipient := range resolved {
		c, err := conversationInTx(ctx, tx, r.GroupID, recipient, topicID)
		if err != nil {
			return domain.Message{}, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO deliveries(id, message_id, conversation_id, recipient_id, group_id, topic_id, created_at)
			 VALUES(?, ?, ?, ?, ?, ?, ?)`, rand.Text(), id, c.ID, recipient, r.GroupID, topicID, stamp); err != nil {
			return domain.Message{}, err
		}
	}
	message, err := messageInTx(ctx, tx, r.GroupID, id)
	message.ReplyTo = quote
	return message, err
}

func scopedMessageReceiptInTx(ctx context.Context, tx *sql.Tx, r MessageRequest, id string) (domain.Message, error) {
	m, err := messageReceiptInTx(ctx, tx, r.GroupID, id)
	if err != nil || r.ReplyToMessageID == "" {
		return m, err
	}
	items := []domain.Message{m}
	if err := projectMessageQuotesInTx(ctx, tx, r.GroupID, &r.TopicID, items); err != nil {
		return domain.Message{}, err
	}
	if q := items[0].ReplyTo; q == nil || q.MessageID != r.ReplyToMessageID {
		return domain.Message{}, errors.New("已保存的消息与引用回执不符")
	}
	return items[0], nil
}

func requireMessageActor(ctx context.Context, tx *sql.Tx, groupID, actorID string) error {
	var allowed int
	return tx.QueryRowContext(ctx,
		`SELECT 1 FROM groups g WHERE g.id=? AND (?='user' OR EXISTS(
		 SELECT 1 FROM members m WHERE m.group_id=g.id AND m.id=? AND m.status='active'))`,
		groupID, actorID, actorID).Scan(&allowed)
}

func (s *Store) GetMessage(ctx context.Context, groupID, messageID string) (domain.Message, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.Message{}, storeError("读取消息失败", err)
	}
	defer tx.Rollback()
	m, err := messageInTx(ctx, tx, groupID, messageID)
	if err != nil {
		return domain.Message{}, storeError("读取消息失败", err)
	}
	messages := []domain.Message{m}
	if err := messageThreadLinksInTx(ctx, tx, groupID, messages); err != nil {
		return domain.Message{}, storeError("读取消息引用失败", err)
	}
	if err := projectMessageDiscussionsInTx(ctx, tx, groupID, messages); err != nil {
		return domain.Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Message{}, storeError("读取消息失败", err)
	}
	return messages[0], nil
}

// GET observes live delivery facts. An asynchronous POST receipt instead
// freezes what that command committed: original visibility and no deliveries.
func messageReceiptInTx(ctx context.Context, tx *sql.Tx, groupID, messageID string) (domain.Message, error) {
	m, err := messageInTx(ctx, tx, groupID, messageID)
	if err != nil || len(m.Deliveries) == 0 {
		return m, err
	}
	var installed, deferred bool
	if err := tx.QueryRowContext(ctx, "SELECT "+groupBackgroundSchemaExists).Scan(&installed); err != nil || !installed {
		return m, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM group_background_intents WHERE message_id=? AND group_id=?)`, messageID, groupID).Scan(&deferred); err != nil {
		return domain.Message{}, err
	}
	if deferred {
		m.Deliveries = []domain.Delivery{}
	}
	return m, nil
}

func messageInTx(ctx context.Context, tx *sql.Tx, groupID, messageID string) (domain.Message, error) {
	m, err := scanMessage(tx.QueryRowContext(ctx,
		"SELECT "+messageColumns+" FROM messages WHERE group_id=? AND id=?", groupID, messageID))
	if err == nil {
		err = messageDeliveries(ctx, tx, &m)
	}
	if err == nil {
		messages := []domain.Message{m}
		err = messageSourcesInTx(ctx, tx, messages)
		m = messages[0]
	}
	return m, err
}

func scanMessage(row scanner) (m domain.Message, err error) {
	var stamp string
	m.Deliveries = []domain.Delivery{}
	if err = row.Scan(&m.ID, &m.GroupID, &m.TopicID, &m.SenderID, &m.Content, &stamp); err == nil {
		m.CreatedAt, err = time.Parse(timestampLayout, stamp)
	}
	return
}

func messageDeliveries(ctx context.Context, tx *sql.Tx, m *domain.Message) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, message_id, conversation_id, recipient_id, state, turn_id, error, created_at
		 FROM deliveries WHERE group_id=? AND message_id=? ORDER BY recipient_id`, m.GroupID, m.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var d domain.Delivery
		var stamp string
		if err := rows.Scan(&d.ID, &d.MessageID, &d.ConversationID, &d.RecipientID, &d.State, &d.TurnID, &d.Error, &stamp); err != nil {
			return err
		}
		d.CreatedAt, err = time.Parse(timestampLayout, stamp)
		if err != nil {
			return err
		}
		m.Deliveries = append(m.Deliveries, d)
	}
	return rows.Err()
}
