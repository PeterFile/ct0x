package store

import (
	"context"
	"crypto/rand"
	"database/sql"
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
}

type MessageListOptions struct {
	Limit         int     // Zero selects 50; maximum 200.
	Before, After *Cursor // Mutually exclusive; After reads oldest first.
}

const messageColumns = "id, group_id, topic_id, sender_id, content, created_at"

func (r MessageRequest) scope() mutationScope {
	return mutationScope{operation: "create_message", actorID: r.ActorID, groupID: r.GroupID, key: r.IdempotencyKey}
}

func (r MessageRequest) payload() []byte {
	fields := []string{r.GroupID, r.TopicID, r.Content, "omitted"}
	if r.RecipientIDs != nil {
		fields[3] = "explicit"
	}
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
	if err != nil {
		return domain.Message{}, false, storeError("读取消息回执失败", err)
	}
	var m domain.Message
	if found {
		m, err = messageInTx(ctx, tx, request.GroupID, id)
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
	id, replay, err := reserveMutation(ctx, tx, r.scope(), r.payload())
	if err != nil {
		return domain.Message{}, err
	}
	if replay {
		return messageInTx(ctx, tx, r.GroupID, id)
	}
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
		ID: id, GroupID: r.GroupID, TopicID: r.TopicID, SenderID: r.ActorID,
		Content: r.Content, CreatedAt: time.Now().UTC(),
	}
	if err := insertMessageRow(ctx, tx, message); err != nil {
		return domain.Message{}, err
	}
	stamp := message.CreatedAt.Format(timestampLayout)
	for _, recipient := range resolved {
		c, err := conversationInTx(ctx, tx, r.GroupID, recipient, r.TopicID)
		if err != nil {
			return domain.Message{}, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO deliveries(id, message_id, conversation_id, recipient_id, group_id, topic_id, created_at)
			 VALUES(?, ?, ?, ?, ?, ?, ?)`, rand.Text(), id, c.ID, recipient, r.GroupID, r.TopicID, stamp); err != nil {
			return domain.Message{}, err
		}
	}
	return messageInTx(ctx, tx, r.GroupID, id)
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
	if err := tx.Commit(); err != nil {
		return domain.Message{}, storeError("读取消息失败", err)
	}
	return m, nil
}

func messageInTx(ctx context.Context, tx *sql.Tx, groupID, messageID string) (domain.Message, error) {
	m, err := scanMessage(tx.QueryRowContext(ctx,
		"SELECT "+messageColumns+" FROM messages WHERE group_id=? AND id=?", groupID, messageID))
	if err == nil {
		err = messageDeliveries(ctx, tx, &m)
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
