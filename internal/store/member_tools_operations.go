package store

import (
	"context"
	"database/sql"
	"errors"

	"agentworkspace/internal/domain"
)

type MemberToolMessageInput struct {
	Content      string   `json:"content"`
	RecipientIDs []string `json:"recipient_ids"`
}

func (s *Store) ListMembersForTool(ctx context.Context, scope MemberToolScope, options ListOptions) (Page[domain.Member], error) {
	const action = "列出群成员失败"
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return Page[domain.Member]{}, memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	limit, err := listLimit(options)
	if err != nil {
		return Page[domain.Member]{}, err
	}
	query, args := "SELECT "+memberColumns+" FROM members WHERE group_id=?", []any{scope.Claim.GroupID}
	if c := options.Before; c != nil {
		query += " AND (created_at,id)<(?,?)"
		args = append(args, c.CreatedAt.UTC().Format(timestampLayout), c.ID)
	}
	rows, err := tx.QueryContext(ctx, query+" ORDER BY created_at DESC,id DESC LIMIT ?", append(args, limit+1)...)
	if err != nil {
		return Page[domain.Member]{}, memberToolOperationError(action, err)
	}
	defer rows.Close()
	page := Page[domain.Member]{Items: make([]domain.Member, 0, limit+1)}
	for rows.Next() {
		member, err := scanMember(rows)
		if err != nil {
			return Page[domain.Member]{}, memberToolOperationError(action, err)
		}
		page.Items = append(page.Items, member)
	}
	if err := rows.Err(); err != nil {
		return Page[domain.Member]{}, memberToolOperationError(action, err)
	}
	if err := rows.Close(); err != nil {
		return Page[domain.Member]{}, memberToolOperationError(action, err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	if err := tx.Commit(); err != nil {
		return Page[domain.Member]{}, memberToolOperationError(action, err)
	}
	return page, nil
}

func (s *Store) ReadChannelForTool(ctx context.Context, scope MemberToolScope, options ListOptions) (Page[domain.Message], error) {
	const action = "读取当前频道失败"
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return Page[domain.Message]{}, memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	limit, err := listLimit(options)
	if err != nil {
		return Page[domain.Message]{}, err
	}
	query, args := "SELECT "+messageColumns+" FROM messages WHERE group_id=? AND topic_id=?", []any{scope.Claim.GroupID, scope.TopicID}
	if c := options.Before; c != nil {
		query += " AND (created_at,id)<(?,?)"
		args = append(args, c.CreatedAt.UTC().Format(timestampLayout), c.ID)
	}
	rows, err := tx.QueryContext(ctx, query+" ORDER BY created_at DESC,id DESC LIMIT ?", append(args, limit+1)...)
	if err != nil {
		return Page[domain.Message]{}, memberToolOperationError(action, err)
	}
	defer rows.Close()
	page := Page[domain.Message]{Items: make([]domain.Message, 0, limit+1)}
	for rows.Next() {
		message, err := scanMessage(rows)
		if err != nil {
			return Page[domain.Message]{}, memberToolOperationError(action, err)
		}
		page.Items = append(page.Items, message)
	}
	if err := rows.Err(); err != nil {
		return Page[domain.Message]{}, memberToolOperationError(action, err)
	}
	if err := rows.Close(); err != nil {
		return Page[domain.Message]{}, memberToolOperationError(action, err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	for i := range page.Items {
		if err := messageDeliveries(ctx, tx, &page.Items[i]); err != nil {
			return Page[domain.Message]{}, memberToolOperationError(action, err)
		}
	}
	if err := messageSourcesInTx(ctx, tx, page.Items); err != nil {
		return Page[domain.Message]{}, memberToolOperationError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return Page[domain.Message]{}, memberToolOperationError(action, err)
	}
	return page, nil
}

// SendMessageForTool never default-routes: both omitted and empty recipients are public-only.
func (s *Store) SendMessageForTool(ctx context.Context, scope MemberToolScope, input MemberToolMessageInput, key string) (domain.Message, error) {
	return s.sendMemberMessage(ctx, scope, input, key, "")
}

// A separate operation gate allows bounded discussion only after durable intake.
func (s *Store) SendMessageForChatIntake(ctx context.Context, scope MemberToolScope, candidateID string, input MemberToolMessageInput, key string) (domain.Message, error) {
	if candidateID == "" {
		return domain.Message{}, ErrMemberToolDenied
	}
	return s.sendMemberMessage(ctx, scope, input, key, candidateID)
}

func (s *Store) sendMemberMessage(ctx context.Context, scope MemberToolScope, input MemberToolMessageInput, key, candidateID string) (domain.Message, error) {
	const action = "发送成员消息失败"
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return domain.Message{}, memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	if !memberToolKeyValid(key) {
		return domain.Message{}, ErrInvalid
	}
	r := MessageRequest{GroupID: scope.Claim.GroupID, TopicID: scope.TopicID, ActorID: scope.MemberID, Content: input.Content, RecipientIDs: input.RecipientIDs, IdempotencyKey: key}
	_, replay, err := lookupMutation(ctx, tx, r.scope(), r.payload())
	if err != nil {
		return domain.Message{}, memberToolOperationError(action, err)
	}
	if candidateID != "" {
		if err := chatDiscussionGateInTx(ctx, tx, scope, candidateID, input, replay); err != nil {
			return domain.Message{}, memberToolOperationError(action, err)
		}
	}
	message, err := createMessageInTx(ctx, tx, r, input.RecipientIDs)
	if err != nil {
		return domain.Message{}, memberToolOperationError(action, err)
	}
	if !replay {
		sourceID, err := messageCommandSourceInTx(ctx, tx, message.ID, "member", r.payload(), key, &scope)
		if err != nil {
			return domain.Message{}, memberToolOperationError(action, err)
		}
		message.SourceID, message.SourceKind = sourceID, "member"
	}
	if err := tx.Commit(); err != nil {
		return domain.Message{}, memberToolOperationError(action, err)
	}
	return message, nil
}

func (s *Store) beginMemberToolTx(ctx context.Context, scope MemberToolScope) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if err := memberToolScopeInTx(ctx, tx, scope); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func memberToolOperationError(action string, err error) error {
	for _, category := range []error{ErrMemberToolDenied, ErrMemberCapacity} {
		if errors.Is(err, category) {
			return category
		}
	}
	return queueError(action, err)
}

// Match the transport's exact 1..128-byte visible ASCII key contract.
func memberToolKeyValid(key string) bool {
	if len(key) == 0 || len(key) > 128 {
		return false
	}
	for _, char := range []byte(key) {
		if char < '!' || char > '~' || char == ',' {
			return false
		}
	}
	return true
}
