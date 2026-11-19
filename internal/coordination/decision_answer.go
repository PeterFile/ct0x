package coordination

import (
	"context"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// AnswerDecisionRequest is for trusted local composition; neither a member
// token nor an input body can select the answering actor.
func (e *Engine) AnswerDecisionRequest(ctx context.Context, thread domain.ThreadRef, id string, input domain.DecisionAnswerInput, key string) (domain.DecisionAnswerReceipt, error) {
	return e.store.AnswerDecisionRequest(ctx, "user", thread, id, input, key)
}

func (e *Engine) GetDecisionAnswer(ctx context.Context, thread domain.ThreadRef, id string, version int64) (domain.DecisionAnswerReceipt, error) {
	return e.store.GetDecisionAnswer(ctx, thread, id, version)
}

func (e *Engine) GetDecisionAnswerView(ctx context.Context, thread domain.ThreadRef, id string, version int64) (domain.DecisionAnswerView, error) {
	return e.store.GetDecisionAnswerView(ctx, thread, id, version)
}

func (e *Engine) ListPendingTaskDecisions(ctx context.Context, opts store.ListOptions) (store.Page[domain.DecisionRequest], error) {
	return e.store.ListPendingTaskDecisions(ctx, opts)
}

func (e *Engine) ListAnswerableDecisions(ctx context.Context, opts store.ListOptions) (store.Page[domain.DecisionRequest], error) {
	return e.store.ListAnswerableDecisions(ctx, opts)
}
