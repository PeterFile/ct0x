package store

import (
	"context"
	"database/sql"
	"errors"

	"agentworkspace/internal/domain"
)

// GetWorkInputForMember rereads one disclosed input in the live owner's inbox.
// It preserves the original source and handling receipts without recording one.
func (s *Store) GetWorkInputForMember(ctx context.Context, scope MemberToolScope, inputID string) (domain.TaskWorkInput, error) {
	const action = "读取执行成员指定后续输入失败"
	var zero domain.TaskWorkInput
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return zero, memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	role, err := s.workInputWorkerInTx(ctx, tx, scope)
	if err != nil {
		return zero, memberToolOperationError(action, err)
	}
	if !validTriggerID(inputID) {
		return zero, ErrInvalid
	}
	installed, err := workInputSchemaInTx(ctx, tx)
	if err != nil {
		return zero, memberToolOperationError(action, err)
	}
	if !installed {
		return zero, ErrMemberToolDenied
	}
	p := role.Provenance
	r, err := coordinationDispatchRequestInTx(ctx, tx, p.GroupID, p.RequestID)
	if err != nil {
		return zero, memberToolOperationError(action, coordinationRoleRelationError(err))
	}
	i, err := workerWorkInputInTx(ctx, tx, p, inputID, r.MandateID)
	if err != nil {
		return zero, memberToolOperationError(action, err)
	}
	if err := workInputReceiptsInTx(ctx, tx, role, r, i); err != nil {
		return zero, memberToolOperationError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return zero, memberToolOperationError(action, err)
	}
	return i, nil
}

func workInputReceiptsInTx(ctx context.Context, tx *sql.Tx, role CoordinationToolScope, r domain.DispatchRequest, i domain.TaskWorkInput) error {
	if c := i.Consumption; c != nil {
		if err := workInputReceiptInTx(ctx, tx, role, r, c.TurnID, c.AttemptID); err != nil {
			return err
		}
	}
	if h := i.Return; h != nil {
		if h.MemberID != role.Provenance.MemberID {
			return ErrMemberToolDenied
		}
		return workInputReceiptInTx(ctx, tx, role, r, h.TurnID, h.AttemptID)
	}
	return nil
}

// Historical handling need not come from the current Turn, but it must have
// been bound to this Assignment, not just to the same Task, revision or member.
func workInputReceiptInTx(ctx context.Context, tx *sql.Tx, role CoordinationToolScope, r domain.DispatchRequest, turnID, attemptID string) error {
	if turnID == role.Member.Claim.TurnID && attemptID == role.AttemptID {
		return nil
	}
	p := role.Provenance
	if turnID == "" || attemptID == "" || p.SourceKind != domain.TaskWorkSourceFollowup {
		return ErrMemberToolDenied
	}
	// The worker gate already verified this Assignment's original binding.
	var originalTurn string
	err := tx.QueryRowContext(ctx, `SELECT turn_id FROM coordination_assignment_attempts WHERE assignment_id=? AND attempt_id=?`,
		p.AssignmentID, attemptID).Scan(&originalTurn)
	if err == nil {
		if originalTurn != turnID {
			return ErrMemberToolDenied
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	d := domain.TaskWorkDelivery{AssignmentID: p.AssignmentID, TaskID: p.TaskID, GroupID: p.GroupID, TopicID: p.TopicID,
		TaskRevision: p.TaskRevision, OwnerMemberID: p.MemberID, MandateID: r.MandateID, AuthorityGeneration: r.AuthorityGeneration}
	attempt, err := taskWorkBoundAttemptInTx(ctx, tx, d, attemptID)
	if err != nil {
		return err
	}
	if attempt.ID != attemptID || attempt.TurnID != turnID {
		return ErrMemberToolDenied
	}
	return nil
}

// ReadTaskWorkMandate reads immutable admission scope after the owner binds an
// Attempt. The claim selects the Assignment's request; no Task or Mandate ID
// comes from the caller and mutable Task text does not contribute to scope.
func (s *Store) ReadTaskWorkMandate(ctx context.Context, scope MemberToolScope) (domain.Mandate, error) {
	const action = "读取执行成员原任务授权失败"
	var zero domain.Mandate
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return zero, memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	r, err := s.taskWorkMandateRequestInTx(ctx, tx, scope)
	if err != nil {
		return zero, memberToolOperationError(action, err)
	}
	// The Assignment loader already verified the immutable request and its
	// Mandate/routing pair. Read back that exact admission, not the Task head.
	var decisionID string
	if err := tx.QueryRowContext(ctx, `SELECT decision_id FROM mandates WHERE group_id=? AND id=?`, r.GroupID, r.MandateID).Scan(&decisionID); err != nil {
		return zero, memberToolOperationError(action, coordinationRoleRelationError(err))
	}
	receipt, err := admissionReceiptInTx(ctx, tx, r.GroupID, decisionID)
	if err != nil {
		return zero, memberToolOperationError(action, coordinationRoleRelationError(err))
	}
	m := receipt.Mandate
	if m.ID != r.MandateID || m.TaskID != r.TaskID || m.TaskRevisionAtAdmission != r.TaskRevision || m.GroupID != r.GroupID || m.TopicID != r.TopicID {
		return zero, ErrMemberToolDenied
	}
	if err := tx.Commit(); err != nil {
		return zero, memberToolOperationError(action, err)
	}
	return m, nil
}

func (s *Store) taskWorkMandateRequestInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope) (domain.DispatchRequest, error) {
	f, err := lookupTaskWorkExecutionInTx(ctx, tx, scope)
	if errors.Is(err, ErrNotFound) {
		_, v, err := s.boundTaskWorkOriginalInTx(ctx, tx, scope)
		return v.r, err
	}
	if err != nil {
		return domain.DispatchRequest{}, err
	}
	blocked, active, err := s.taskWorkReadinessInTx(ctx, tx, f, scope.Claim.TurnID)
	if err != nil {
		return domain.DispatchRequest{}, err
	}
	if blocked != "" || active == "" {
		return domain.DispatchRequest{}, ErrMemberToolDenied
	}
	attempt, err := taskWorkBoundAttemptInTx(ctx, tx, f.d, "")
	if err != nil {
		return domain.DispatchRequest{}, err
	}
	if attempt.ID != active || attempt.TurnID != scope.Claim.TurnID {
		return domain.DispatchRequest{}, ErrMemberToolDenied
	}
	return f.v.r, nil
}
