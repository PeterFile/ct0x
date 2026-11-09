package coordination

import (
	"context"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// DecideDispatch stays closed until shared Activate ownership and role runtime are reviewed.
func (e *Engine) DecideDispatch(ctx context.Context, scope store.CoordinationToolScope, input store.CoordinatorDecisionInput, key string) (domain.DecisionReceipt, error) {
	return domain.DecisionReceipt{}, ErrDispatchRuntimeNotReady
}
