package coordination

import (
	"context"

	"agentworkspace/internal/domain"
)

// RequestDispatch is trusted local-user-only; it neither materializes nor executes work.
func (e *Engine) RequestDispatch(ctx context.Context, groupID, taskID string, expectedTaskRevision, expectedAuthorityGeneration int64, reason, key string) (domain.DispatchRequest, error) {
	return e.store.RequestDispatch(ctx, groupID, taskID, expectedTaskRevision, expectedAuthorityGeneration, reason, key)
}

func (e *Engine) GetDispatchRequest(ctx context.Context, groupID, requestID string) (domain.DispatchView, error) {
	return e.store.GetDispatchRequest(ctx, groupID, requestID)
}
