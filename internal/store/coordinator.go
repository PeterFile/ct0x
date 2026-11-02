package store

import (
	"context"

	"agentworkspace/internal/domain"
)

// SetCoordinator is a trusted local-user command, not a member tool entry point.
func (s *Store) SetCoordinator(ctx context.Context, groupID, memberID string, expectedRevision int64, key string) (domain.GroupCoordinatorBinding, error) {
	tx, revision, err := s.beginCoordinatorConfig(ctx, groupID, []string{memberID}, expectedRevision, key, false)
	if err != nil {
		return domain.GroupCoordinatorBinding{}, admissionError("设置协调员失败", err)
	}
	defer tx.Rollback()
	binding, err := coordinatorBindingInTx(ctx, tx, groupID, revision)
	if err != nil {
		return domain.GroupCoordinatorBinding{}, storeError("读取协调员版本失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.GroupCoordinatorBinding{}, storeError("保存协调配置失败", err)
	}
	return binding, nil
}

// SetDispatchPolicy stores an explicit worker set; an empty set grants no dispatch.
func (s *Store) SetDispatchPolicy(ctx context.Context, groupID string, allowedWorkerIDs []string, expectedRevision int64, key string) (domain.GroupDispatchPolicy, error) {
	workers := recipientSet(allowedWorkerIDs)
	if workers == nil {
		workers = []string{}
	}
	tx, revision, err := s.beginCoordinatorConfig(ctx, groupID, workers, expectedRevision, key, true)
	if err != nil {
		return domain.GroupDispatchPolicy{}, admissionError("设置分派策略失败", err)
	}
	defer tx.Rollback()
	policy, err := coordinatorPolicyInTx(ctx, tx, groupID, revision)
	if err != nil {
		return domain.GroupDispatchPolicy{}, storeError("读取分派策略版本失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.GroupDispatchPolicy{}, storeError("保存协调配置失败", err)
	}
	return policy, nil
}
