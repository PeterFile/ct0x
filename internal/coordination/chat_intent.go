package coordination

import (
	"context"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// UsesStore lets trusted composition reject cross-store capability registries.
func (e *Engine) UsesStore(s *store.Store) bool { return e != nil && e.store == s && s != nil }

func (e *Engine) SetChatPolicy(ctx context.Context, thread domain.ThreadRef, input domain.ChatPolicyRequest, expected int64, key string) (domain.ChatPolicy, error) {
	return e.store.SetChatPolicy(ctx, thread, input, expected, key)
}

func (e *Engine) GetChatPolicy(ctx context.Context, thread domain.ThreadRef) (domain.ChatPolicy, error) {
	return e.store.GetChatPolicy(ctx, thread)
}

func (e *Engine) GetChatAdmission(ctx context.Context, groupID, taskID string) (domain.ChatAssessmentReceipt, error) {
	return e.store.GetChatAdmission(ctx, groupID, taskID)
}

// CaptureChatCandidate is runtime-only, after claim and before model assessment.
func (e *Engine) CaptureChatCandidate(ctx context.Context, scope store.MemberToolScope) (domain.ChatCandidate, error) {
	return e.store.CaptureChatCandidate(ctx, scope)
}

func (e *Engine) AssessChatIntent(ctx context.Context, scope store.MemberToolScope, candidateID string, input domain.ChatAssessment, content, key string) (domain.ChatAssessmentReceipt, error) {
	return e.store.ApplyChatAssessment(ctx, scope, candidateID, input, content, key)
}
