package coordination

import (
	"context"
	"fmt"
	"strings"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// SetCoordinator is explicitly local-user-only, never a model/tool entry point.
func (e *Engine) SetCoordinator(ctx context.Context, groupID, memberID string, expectedRevision int64, key string) (domain.GroupCoordinatorBinding, error) {
	if strings.TrimSpace(key) == "" {
		return domain.GroupCoordinatorBinding{}, fmt.Errorf("%w：设置协调员必须提供幂等键", store.ErrInvalid)
	}
	return e.store.SetCoordinator(ctx, groupID, memberID, expectedRevision, key)
}

func (e *Engine) GetCoordinator(ctx context.Context, groupID string) (domain.GroupCoordinatorBinding, error) {
	return e.store.GetCoordinator(ctx, groupID)
}

// SetDispatchPolicy forwards only explicit configuration to the borrowed Store.
func (e *Engine) SetDispatchPolicy(ctx context.Context, groupID string, allowedWorkerIDs []string, expectedRevision int64, key string) (domain.GroupDispatchPolicy, error) {
	if strings.TrimSpace(key) == "" {
		return domain.GroupDispatchPolicy{}, fmt.Errorf("%w：设置分派策略必须提供幂等键", store.ErrInvalid)
	}
	return e.store.SetDispatchPolicy(ctx, groupID, allowedWorkerIDs, expectedRevision, key)
}

func (e *Engine) GetDispatchPolicy(ctx context.Context, groupID string) (domain.GroupDispatchPolicy, error) {
	return e.store.GetDispatchPolicy(ctx, groupID)
}
