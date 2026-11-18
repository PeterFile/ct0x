package store

import (
	"context"
	"database/sql"
	"errors"
)

// ErrTaskWorkMutationUnknown marks an unconfirmed outcome for work input
// acknowledgement, work input return, or ReportWork. Check it before business
// categories: the preserved cause may still match ErrConflict or ErrMemberToolDenied.
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

func taskWorkMutationFailure(action, phase string, cause, rollback error) error {
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

// Use only for work input effects and ReportWork. The scope fence is itself
// a write, so beginMemberToolTx would hide its rollback status on failure.
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
