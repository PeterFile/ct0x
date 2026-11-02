package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"agentworkspace/internal/domain"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// DispatchTransientCode describes local SQLite failure, never an execution outcome.
type DispatchTransientCode string

const (
	DispatchTransientSQLiteBusy   DispatchTransientCode = domain.DispatchErrorSQLiteBusy
	DispatchTransientSQLiteLocked DispatchTransientCode = domain.DispatchErrorSQLiteLocked
)

// ClassifyDispatchTransient recognizes only retryable local SQLite failures.
func ClassifyDispatchTransient(err error) (DispatchTransientCode, bool) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "", false
	}
	var cause *sqlite.Error
	if errors.As(err, &cause) && cause != nil {
		switch cause.Code() & 0xff {
		case sqlite3.SQLITE_BUSY:
			return DispatchTransientSQLiteBusy, true
		case sqlite3.SQLITE_LOCKED:
			return DispatchTransientSQLiteLocked, true
		}
	}
	return "", false
}

// DeferDispatchRequest is called only after a failed materialization transaction has rolled back.
// It records local storage backoff; it never retries execution or releases resources.
func (s *Store) DeferDispatchRequest(ctx context.Context, requestID string, expectedGeneration int64, at time.Time, code DispatchTransientCode) (err error) {
	defer func() { err = storeError("延后分派请求失败", err) }()
	if code != DispatchTransientSQLiteBusy && code != DispatchTransientSQLiteLocked {
		return fmt.Errorf("%w：分派暂态错误码无效", ErrInvalid)
	}
	stamp, err := coordinationDispatchInput(requestID, expectedGeneration, at)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, state, err := coordinationDispatchLoadInTx(ctx, tx, requestID)
	if err != nil {
		return err
	}
	if err := coordinationDispatchPending(state, expectedGeneration); err != nil {
		return err
	}
	if state.AttemptCount == math.MaxInt64 {
		return fmt.Errorf("%w：分派退避次数溢出", ErrInvalid)
	}
	delay := []time.Duration{2, 4, 8, 16, 32, 60}[min(state.AttemptCount, 5)] * time.Second
	next, err := coordinationDispatchTime(at.UTC().Add(delay).Format(timestampLayout))
	if err != nil {
		return err
	}
	if err := coordinationDispatchWrite(ctx, tx, `UPDATE coordination_dispatch_state SET generation=generation+1,
	 attempt_count=attempt_count+1,next_attempt_at=?,updated_at=?,last_error_code=?
	 WHERE request_id=? AND generation=? AND status='pending'`, next.Format(timestampLayout), stamp, string(code), requestID, expectedGeneration); err != nil {
		return err
	}
	state.Generation++
	state.AttemptCount++
	state.NextAttemptAt, state.LastErrorCode = next, string(code)
	actual, err := coordinationDispatchStateInTx(ctx, tx, r)
	if err != nil {
		return err
	}
	if !coordinationDispatchSameState(actual, state) {
		return fmt.Errorf("%w：分派退避状态未保存", ErrConflict)
	}
	return tx.Commit()
}
