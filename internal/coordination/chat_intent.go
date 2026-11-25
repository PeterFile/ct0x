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

// EnableInvestigationExecution is trusted startup wiring, after the read-only
// profile and its scoped tools are installed. Policy writes cannot enable it.
func (e *Engine) EnableInvestigationExecution() { e.store.EnableInvestigationExecution() }

func (e *Engine) GetTaskInvestigationScope(ctx context.Context, groupID, taskID string) (domain.TaskInvestigationScope, error) {
	return e.store.GetTaskInvestigationScope(ctx, groupID, taskID)
}

func (e *Engine) CheckTaskInvestigationReady(ctx context.Context, groupID, taskID string) error {
	return e.store.CheckTaskInvestigationReady(ctx, groupID, taskID)
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

// AssessChatIntentWithAutomaticText is the trusted runtime's existing intake
// evaluation with optional role facts; it does not expose another model tool.
func (e *Engine) AssessChatIntentWithAutomaticText(ctx context.Context, scope store.MemberToolScope, candidateID string, input domain.ChatAssessment, content, key string, facts store.ChatAutomaticTextFacts) (domain.ChatAssessmentReceipt, error) {
	return e.store.ApplyChatAssessmentWithAutomaticText(ctx, scope, candidateID, input, content, key, facts)
}

// AssessChatIntentDormant records official input without native delivery. It is
// for trusted callers explicitly choosing the attempt-only path.
func (e *Engine) AssessChatIntentDormant(ctx context.Context, scope store.MemberToolScope, candidateID string, input domain.ChatAssessment, content, key string) (domain.ChatAssessmentReceipt, error) {
	return e.store.ApplyChatAssessmentDormant(ctx, scope, candidateID, input, content, key)
}

func (e *Engine) ListWorkInputsForMember(ctx context.Context, scope store.MemberToolScope) ([]domain.TaskWorkInput, error) {
	return e.store.ListWorkInputsForMember(ctx, scope)
}

func (e *Engine) ListWorkInputHistoryForMember(ctx context.Context, scope store.MemberToolScope, options store.ListOptions) (store.Page[domain.TaskWorkInput], error) {
	return e.store.ListWorkInputHistoryForMember(ctx, scope, options)
}

// GetWorkInputForMember reads one exact input handle under the current claim.
func (e *Engine) GetWorkInputForMember(ctx context.Context, scope store.MemberToolScope, inputID string) (domain.TaskWorkInput, error) {
	return e.store.GetWorkInputForMember(ctx, scope, inputID)
}

func (e *Engine) AcknowledgeWorkInput(ctx context.Context, scope store.MemberToolScope, inputID string) (domain.TaskWorkInput, error) {
	return e.store.AcknowledgeWorkInput(ctx, scope, inputID)
}

// AcknowledgeWorkInputWithKey retries the exact confirmation under a live owner claim.
func (e *Engine) AcknowledgeWorkInputWithKey(ctx context.Context, scope store.MemberToolScope, inputID, key string) (domain.TaskWorkInput, error) {
	return e.store.AcknowledgeWorkInputWithKey(ctx, scope, inputID, key)
}

func (e *Engine) HandBackWorkInput(ctx context.Context, scope store.MemberToolScope, inputID, reason string) (domain.TaskWorkInput, error) {
	return e.store.HandBackWorkInput(ctx, scope, inputID, reason)
}

// HandBackWorkInputWithKey retries the exact escalation, preserving its original notice.
func (e *Engine) HandBackWorkInputWithKey(ctx context.Context, scope store.MemberToolScope, inputID, reason, key string) (domain.TaskWorkInput, error) {
	return e.store.HandBackWorkInputWithKey(ctx, scope, inputID, reason, key)
}
