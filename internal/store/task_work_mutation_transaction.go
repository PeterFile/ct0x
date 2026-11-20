package store

import (
	"context"
	"database/sql"
	"errors"
)

// ErrTaskWorkMutationUnknown marks an unconfirmed outcome for work inputs,
// decision answers, and task-work delegation, report, or handback. Check it
// before business categories: the cause may still match ErrConflict or ErrMemberToolDenied.
var ErrTaskWorkMutationUnknown = errors.New("任务工作事务结果未确认；请保留原幂等键和原请求内容用于对账")

type taskWorkTransactionError struct {
	cause          error
	message, phase string
	rolledBack     bool
}

func (e *taskWorkTransactionError) Error() string              { return e.message }
func (e *taskWorkTransactionError) Unwrap() error              { return e.cause }
func (e *taskWorkTransactionError) Phase() string              { return e.phase }
func (e *taskWorkTransactionError) CommitOutcomeUnknown() bool { return e.phase == "commit" }

// A failed Begin is also confirmed: no transaction ran the scope or command.
func (e *taskWorkTransactionError) RollbackConfirmed() bool { return e.rolledBack }

func (e *taskWorkTransactionError) Is(target error) bool {
	return target == ErrTaskWorkMutationUnknown && !e.rolledBack
}

func taskWorkMutationFailure(action, phase string, cause, rollback error) *taskWorkTransactionError {
	e := &taskWorkTransactionError{cause: cause, phase: phase, rolledBack: rollback == nil && phase != "commit"}
	if !e.rolledBack {
		e.message = ErrTaskWorkMutationUnknown.Error()
		// ErrTxDone after Commit says nothing about its outcome. Before Commit,
		// even ErrTxDone is an unconfirmed rollback and must remain in the chain.
		if rollback != nil && !(phase == "commit" && rollback == sql.ErrTxDone) {
			e.cause = errors.Join(cause, rollback)
		}
		return e
	}
	classified := cause
	if phase == "mutation" && errors.Is(cause, ErrTaskInvestigationNotReady) {
		classified = ErrMemberToolDenied
	}
	// Keep determined failures' legacy display and category without losing the cause.
	legacy := memberToolOperationError(action, classified)
	e.message, e.cause = legacy.Error(), errors.Join(legacy, cause)
	return e
}

// The scope fence is itself a write, so beginMemberToolTx would hide its
// rollback status on failure.
func taskWorkMutationTx[T any](ctx context.Context, s *Store, scope MemberToolScope, action string, run func(*sql.Tx) (T, error)) (zero T, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, taskWorkMutationFailure(action, "begin", err, nil)
	}
	phase := "scope"
	defer func() {
		rollback := tx.Rollback()
		if err != nil {
			err = taskWorkMutationFailure(action, phase, err, rollback)
		}
	}()
	if err := memberToolScopeInTx(ctx, tx, scope); err != nil {
		return zero, err
	}
	phase = "mutation" // Includes source, validation, writes, and receipt checks.
	out, err := run(tx)
	if err != nil {
		return zero, err
	}
	phase = "commit"
	if err := tx.Commit(); err != nil {
		return zero, err
	}
	return out, nil
}

// B3 writers share the v30 schema guard and final live-scope check. Work input
// effects still perform their own schema and receipt checks in their callback.
func taskWorkB3MutationTx[T any](ctx context.Context, s *Store, scope MemberToolScope, action string, run func(*sql.Tx) (T, error)) (T, error) {
	return taskWorkMutationTx(ctx, s, scope, action, func(tx *sql.Tx) (zero T, err error) {
		if err := requireTaskWorkReportsInTx(ctx, tx); err != nil {
			return zero, coordinationRoleRelationError(err)
		}
		out, err := run(tx)
		if err != nil {
			return zero, err
		}
		return out, coordinationDecideLiveInTx(ctx, tx, scope)
	})
}
