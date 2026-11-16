package coordination

import (
	"context"

	"agentworkspace/internal/domain"
)

// GetDispatchExecution is a trusted local-user read, never a member-tool grant.
func (e *Engine) GetDispatchExecution(ctx context.Context, groupID, taskID, requestID string) (domain.DispatchExecution, error) {
	return e.store.GetDispatchExecution(ctx, groupID, taskID, requestID)
}
