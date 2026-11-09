package platform

import (
	"context"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
)

// DispatchRequests is local-user only and borrows Engine without owning its lifecycle.
type DispatchRequests struct{ engine *coordination.Engine }

func NewDispatchRequests(engine *coordination.Engine) *DispatchRequests {
	return &DispatchRequests{engine: engine}
}

func (s *DispatchRequests) RequestDispatch(ctx context.Context, groupID, taskID string, expectedTaskRevision, expectedAuthorityGeneration int64, reason, key string) (domain.DispatchRequest, error) {
	return s.engine.RequestDispatch(ctx, groupID, taskID, expectedTaskRevision, expectedAuthorityGeneration, reason, key)
}

func (s *DispatchRequests) GetDispatchRequest(ctx context.Context, groupID, requestID string) (domain.DispatchView, error) {
	return s.engine.GetDispatchRequest(ctx, groupID, requestID)
}
