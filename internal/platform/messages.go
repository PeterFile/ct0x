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
	// The HTTP DTO sets this for an explicitly selected main topic.
	TopicIDExplicit bool `json:"-"`
	domain.ThreadLinks
}

func (s *LocalUser) CreateMessage(ctx context.Context, groupID string, input MessageInput, key string) (domain.Message, error) {
	return s.db.PostLocalUserMessage(ctx, store.MessageRequest{
		GroupID: groupID, ActorID: "user", Content: input.Content, TopicID: input.TopicID,
		RecipientIDs: input.RecipientIDs, IdempotencyKey: key,
		TopicIDExplicit: input.TopicIDExplicit, ThreadLinks: input.ThreadLinks,
	})
}

func (s *LocalUser) GetMessage(ctx context.Context, groupID, messageID string) (domain.Message, error) {
	return s.db.GetMessage(ctx, groupID, messageID)
}

func (s *LocalUser) ListMessages(ctx context.Context, groupID string, options store.ListOptions) (store.Page[domain.Message], error) {
	return s.db.ListMessages(ctx, groupID, store.MessageListOptions{Limit: options.Limit, Before: options.Before})
}
