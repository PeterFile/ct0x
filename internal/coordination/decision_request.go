package coordination

import (
	"context"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// ProposeTaskDecisionRequest is restricted to the current, server-bound owner
// Turn. Recording the proposal has no execution or authority side effects.
func (e *Engine) ProposeTaskDecisionRequest(ctx context.Context, scope store.MemberToolScope, input domain.TaskDecisionRequestInput, key string) (domain.DecisionRequest, error) {
	return e.store.ProposeTaskDecisionRequest(ctx, scope, input, key)
}

// AssessChatDecisionClarification is the explicit human-needed intake path,
// not the existing ordinary-chat assessment command or a standalone producer.
func (e *Engine) AssessChatDecisionClarification(ctx context.Context, scope store.MemberToolScope, candidateID string, assessment domain.ChatAssessment, details domain.DecisionContext, content, key string) (domain.DecisionRequest, error) {
	return e.store.ApplyChatDecisionClarification(ctx, scope, candidateID, assessment, details, content, key)
}

// ListPendingDecisionRequests is a trusted local-user capability, not a member
// tool. It does not project history, Submission Review or eventual delivery.
func (e *Engine) ListPendingDecisionRequests(ctx context.Context, opts store.ListOptions) (store.Page[domain.DecisionRequest], error) {
	return e.store.ListPendingDecisionRequests(ctx, opts)
}
