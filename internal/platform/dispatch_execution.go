package platform

import (
	"context"

	"agentworkspace/internal/domain"
)

func (s *DispatchRequests) GetDispatchExecution(ctx context.Context, groupID, taskID, requestID string) (domain.DispatchExecution, error) {
	return s.engine.GetDispatchExecution(ctx, groupID, taskID, requestID)
}
