package platform

import (
	"context"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
)

// CoordinatorConfiguration is local-user only, never a member capability.
// It borrows the supplied Engine without owning Store, execution, or lifecycle.
type CoordinatorConfiguration struct{ engine *coordination.Engine }

func NewCoordinatorConfiguration(engine *coordination.Engine) *CoordinatorConfiguration {
	return &CoordinatorConfiguration{engine: engine}
}

func (s *CoordinatorConfiguration) SetCoordinator(ctx context.Context, groupID, memberID string, expectedRevision int64, key string) (domain.GroupCoordinatorBinding, error) {
	return s.engine.SetCoordinator(ctx, groupID, memberID, expectedRevision, key)
}

func (s *CoordinatorConfiguration) GetCoordinator(ctx context.Context, groupID string) (domain.GroupCoordinatorBinding, error) {
	return s.engine.GetCoordinator(ctx, groupID)
}

func (s *CoordinatorConfiguration) SetDispatchPolicy(ctx context.Context, groupID string, allowedWorkerIDs []string, expectedRevision int64, key string) (domain.GroupDispatchPolicy, error) {
	return s.engine.SetDispatchPolicy(ctx, groupID, allowedWorkerIDs, expectedRevision, key)
}

func (s *CoordinatorConfiguration) GetDispatchPolicy(ctx context.Context, groupID string) (domain.GroupDispatchPolicy, error) {
	return s.engine.GetDispatchPolicy(ctx, groupID)
}
