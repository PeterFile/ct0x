package platform

import (
	"context"

	"agentworkspace/internal/domain"
)

// ChatDiscussionPolicyAvailable reports whether local-user budget settings are available.
func (s *LocalUser) ChatDiscussionPolicyAvailable(ctx context.Context) (bool, error) {
	return s.db.ChatDiscussionPolicyAvailable(ctx)
}

func (s *LocalUser) GetChatDiscussionPolicy(ctx context.Context, thread domain.ThreadRef) (domain.ChatDiscussionPolicy, error) {
	return s.db.GetChatDiscussionPolicy(ctx, thread)
}

func (s *LocalUser) SetChatDiscussionPolicy(ctx context.Context, thread domain.ThreadRef, maxInviteHops int, expected int64, key string) (domain.ChatDiscussionPolicy, error) {
	return s.db.SetChatDiscussionPolicy(ctx, thread, maxInviteHops, expected, key)
}
