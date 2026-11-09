package store

import (
	"context"
	"database/sql"
	"math"
	"time"
)

// DeferAssignment persists backoff only after the trusted caller has established
// rollback of the failed materialization. ClassifyDispatchTransient alone cannot
// establish that precondition, especially for Commit errors. No sleep or retry occurs.
func (s *Store) DeferAssignment(ctx context.Context, assignmentID string, expectedGeneration int64, at time.Time, code DispatchTransientCode) (err error) {
	phase := "input"
	var tx *sql.Tx
	defer func() { err = coordinationAssignmentFinish(tx, phase, err) }()
	if code != DispatchTransientSQLiteBusy && code != DispatchTransientSQLiteLocked {
		return ErrInvalid
	}
	stamp, err := coordinationDispatchInput(assignmentID, expectedGeneration, at)
	if err != nil {
		return err
	}
	phase = "begin"
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	phase = "source"
	v, state, err := coordinationAssignmentLoad(ctx, tx, assignmentID)
	if err != nil {
		return err
	}
	if err := coordinationAssignmentPending(state, expectedGeneration); err != nil {
		return err
	}
	if state.AttemptCount == math.MaxInt64 {
		return ErrInvalid
	}
	delay := []time.Duration{2, 4, 8, 16, 32, 60}[min(state.AttemptCount, 5)] * time.Second
	next, err := coordinationDispatchTime(at.UTC().Add(delay).Format(timestampLayout))
	if err != nil {
		return err
	}
	phase = "backoff"
	if err := coordinationDispatchWrite(ctx, tx, `UPDATE coordination_assignment_dispatch SET generation=generation+1,
 attempt_count=attempt_count+1,next_attempt_at=?,updated_at=?,last_error_code=?
 WHERE assignment_id=? AND generation=? AND status='pending'`, next.Format(timestampLayout), stamp, string(code), assignmentID, expectedGeneration); err != nil {
		return err
	}
	state.Generation++
	state.AttemptCount++
	state.NextAttemptAt, state.UpdatedAt, state.LastErrorCode = next, at.UTC(), string(code)
	phase = "readback"
	actual, err := coordinationAssignmentState(ctx, tx, v.a)
	if err != nil {
		return err
	}
	if !coordinationAssignmentSame(actual, state) {
		return ErrConflict
	}
	phase = "commit"
	return tx.Commit()
}
