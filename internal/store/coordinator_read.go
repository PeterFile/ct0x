package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"agentworkspace/internal/domain"
)

func (s *Store) GetCoordinator(ctx context.Context, groupID string) (domain.GroupCoordinatorBinding, error) {
	tx, err := s.beginCoordinatorRead(ctx, groupID)
	if err != nil {
		return domain.GroupCoordinatorBinding{}, storeError("读取协调员失败", err)
	}
	defer tx.Rollback()
	binding, err := coordinatorBindingInTx(ctx, tx, groupID, 0)
	if err != nil {
		return domain.GroupCoordinatorBinding{}, storeError("读取协调员失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.GroupCoordinatorBinding{}, storeError("读取协调员失败", err)
	}
	return binding, nil
}

func (s *Store) GetDispatchPolicy(ctx context.Context, groupID string) (domain.GroupDispatchPolicy, error) {
	tx, err := s.beginCoordinatorRead(ctx, groupID)
	if err != nil {
		return domain.GroupDispatchPolicy{}, storeError("读取分派策略失败", err)
	}
	defer tx.Rollback()
	policy, err := coordinatorPolicyInTx(ctx, tx, groupID, 0)
	if err != nil {
		return domain.GroupDispatchPolicy{}, storeError("读取分派策略失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.GroupDispatchPolicy{}, storeError("读取分派策略失败", err)
	}
	return policy, nil
}

func (s *Store) beginCoordinatorRead(ctx context.Context, groupID string) (*sql.Tx, error) {
	if !validTriggerID(groupID) {
		return nil, fmt.Errorf("%w：群组标识无效", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	if err := requireMessageActor(ctx, tx, groupID, "user"); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func coordinatorBindingInTx(ctx context.Context, tx *sql.Tx, groupID string, revision int64) (binding domain.GroupCoordinatorBinding, err error) {
	query := "SELECT r.group_id,r.member_id,r.revision FROM group_coordinator_binding_revisions r WHERE r.group_id=? AND r.revision=?"
	args := []any{groupID, revision}
	if revision == 0 {
		query = `SELECT r.group_id,r.member_id,r.revision FROM group_coordinator_bindings c
		 JOIN group_coordinator_binding_revisions r USING(group_id,revision) WHERE c.group_id=?`
		args = args[:1]
	}
	err = tx.QueryRowContext(ctx, query, args...).Scan(&binding.GroupID, &binding.MemberID, &binding.Revision)
	return
}

func coordinatorPolicyInTx(ctx context.Context, tx *sql.Tx, groupID string, revision int64) (domain.GroupDispatchPolicy, error) {
	query := "SELECT r.group_id,r.revision,r.allowed_worker_ids FROM group_dispatch_policy_revisions r WHERE r.group_id=? AND r.revision=?"
	args := []any{groupID, revision}
	if revision == 0 {
		query = `SELECT r.group_id,r.revision,r.allowed_worker_ids FROM group_dispatch_policies c
		 JOIN group_dispatch_policy_revisions r USING(group_id,revision) WHERE c.group_id=?`
		args = args[:1]
	}
	var policy domain.GroupDispatchPolicy
	var encoded string
	err := tx.QueryRowContext(ctx, query, args...).Scan(&policy.GroupID, &policy.Revision, &encoded)
	if revision == 0 && errors.Is(err, sql.ErrNoRows) {
		return domain.GroupDispatchPolicy{GroupID: groupID, AllowedWorkerIDs: []string{}}, nil
	}
	if err != nil {
		return domain.GroupDispatchPolicy{}, err
	}
	if err := json.Unmarshal([]byte(encoded), &policy.AllowedWorkerIDs); err != nil {
		return domain.GroupDispatchPolicy{}, err
	}
	return policy, nil
}
