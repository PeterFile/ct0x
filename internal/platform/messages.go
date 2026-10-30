package platform

import (
	"context"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

type MessageInput struct {
	Content      string   `json:"content"`
	RecipientIDs []string `json:"recipient_ids"`
	TopicID      string   `json:"topic_id"`
}

func (s *LocalUser) CreateMessage(ctx context.Context, groupID string, input MessageInput, key string) (domain.Message, error) {
	// Preserve original recipient intent for both idempotency checks.
	request := store.MessageRequest{
		GroupID: groupID, ActorID: "user", Content: input.Content, TopicID: input.TopicID,
		RecipientIDs: input.RecipientIDs, IdempotencyKey: key,
	}
	if receipt, found, err := s.db.LookupMessageReceipt(ctx, request); err != nil || found {
		return receipt, err
	}
	resolved := input.RecipientIDs
	if resolved == nil {
		id, err := s.db.EarliestActiveMemberID(ctx, groupID)
		if err != nil {
			return domain.Message{}, err
		}
		if id != "" {
			resolved = []string{id}
		}
	}
	// Routing only persists deliveries; it never creates a member or starts a model.
	return s.db.CreateMessage(ctx, request, resolved)
}

func (s *LocalUser) GetMessage(ctx context.Context, groupID, messageID string) (domain.Message, error) {
	return s.db.GetMessage(ctx, groupID, messageID)
}

func (s *LocalUser) ListMessages(ctx context.Context, groupID string, options store.ListOptions) (store.Page[domain.Message], error) {
	return s.db.ListMessages(ctx, groupID, store.MessageListOptions{Limit: options.Limit, Before: options.Before})
}
