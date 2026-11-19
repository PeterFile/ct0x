package platform

import (
	"context"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

func (s *LocalUser) ListPendingTaskDecisions(ctx context.Context, opts store.ListOptions) (store.Page[domain.DecisionRequest], error) {
	return coordination.New(s.db).ListPendingTaskDecisions(ctx, opts)
}

func (s *LocalUser) AnswerDecisionRequest(ctx context.Context, thread domain.ThreadRef, id string, input domain.DecisionAnswerInput, key string) (domain.DecisionAnswerReceipt, error) {
	return coordination.New(s.db).AnswerDecisionRequest(ctx, thread, id, input, key)
}
