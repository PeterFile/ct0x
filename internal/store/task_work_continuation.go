package store

import (
	"context"
	"database/sql"
	"slices"

	"agentworkspace/internal/domain"
)

// This authority is constructed only after the live, owner-only readiness
// checks. The common activation gate still performs its own same-scope CAS.
type taskWorkActivationAuthority struct {
	scope    MemberToolScope
	delivery domain.TaskWorkDelivery
	previous string
}

func (a *taskWorkActivationAuthority) authorizeTaskAttemptInTx(_ context.Context, _ *sql.Tx, in ActivateTaskAttemptInput) error {
	d := a.delivery
	if !d.SourceKind.IsOwnerContinuation() || d.OwnerMemberID != d.MemberID || in.MemberScope == nil || *in.MemberScope != a.scope ||
		in.ActorID != d.OwnerMemberID || in.GroupID != d.GroupID || in.TaskID != d.TaskID || in.ExpectedRevision != d.TaskRevision ||
		in.ExpectedActiveAttemptID != a.previous || a.previous == "" || in.IdempotencyKey != "" {
		return ErrMemberToolDenied
	}
	return nil
}

// BindTaskWorkContinuation binds one live owner Turn to one new formal Attempt.
// An existing same-Turn binding is returned only while it still owns the Task.
func (s *Store) BindTaskWorkContinuation(ctx context.Context, scope MemberToolScope) (out domain.TaskAttempt, err error) {
	phase := "begin"
	var tx *sql.Tx
	defer func() { err = coordinationAssignmentFinish(tx, phase, err) }()
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	phase = "scope"
	if err := memberToolScopeInTx(ctx, tx, scope); err != nil {
		return out, err
	}
	phase = "delivery"
	f, err := taskWorkExecutionInTx(ctx, tx, scope)
	if err != nil {
		return out, err
	}
	d := f.d
	if !d.SourceKind.IsOwnerContinuation() {
		return out, ErrMemberToolDenied
	}
	blocked, previous, err := s.taskWorkReadinessInTx(ctx, tx, f, scope.Claim.TurnID)
	if err != nil {
		return out, err
	}
	if blocked != "" {
		return out, ErrConflict
	}
	attempt, err := taskWorkBoundAttemptInTx(ctx, tx, d, "")
	if err != nil {
		return out, err
	}
	if attempt.ID == "" {
		phase = "activate"
		attempt, err = activateTaskAttemptInTx(ctx, tx, ActivateTaskAttemptInput{
			GroupID: d.GroupID, TaskID: d.TaskID, ActorID: d.OwnerMemberID, MemberScope: &scope,
			ExpectedRevision: d.TaskRevision, ExpectedActiveAttemptID: previous,
		}, &taskWorkActivationAuthority{scope: scope, delivery: d, previous: previous})
		if err != nil {
			return out, err
		}
		phase = "binding"
		if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO task_work_continuations(delivery_id,turn_id,attempt_id) VALUES(?,?,?)`,
			d.DeliveryID, scope.Claim.TurnID, attempt.ID); err != nil {
			return out, err
		}
		// Confirm the actual write and current pointers, including trigger effects.
		f, err = taskWorkExecutionInTx(ctx, tx, scope)
		if err != nil {
			return out, err
		}
		if f.d != d {
			return out, ErrConflict
		}
		saved, err := taskWorkBoundAttemptInTx(ctx, tx, d, "")
		if err != nil {
			return out, err
		}
		blocked, current, err := s.taskWorkReadinessInTx(ctx, tx, f, scope.Claim.TurnID)
		if err != nil {
			return out, err
		}
		if saved != attempt || blocked != "" || current != attempt.ID {
			return out, ErrConflict
		}
	}
	phase = "commit"
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return attempt, nil
}

// SubmitTaskWorkResult derives the exact bound Attempt from the live claim.
// Only a new write rechecks current readiness; a same-key replay is historical
// evidence but still requires the exact live continuation scope.
func (s *Store) SubmitTaskWorkResult(ctx context.Context, scope MemberToolScope, input AssignmentResultInput, key string) (out domain.Submission, err error) {
	phase := "begin"
	var tx *sql.Tx
	defer func() { err = coordinationAssignmentFinish(tx, phase, err) }()
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	phase = "scope"
	if err := memberToolScopeInTx(ctx, tx, scope); err != nil {
		return out, err
	}
	phase = "delivery"
	f, err := taskWorkExecutionInTx(ctx, tx, scope)
	if err != nil {
		return out, err
	}
	d := f.d
	if !d.SourceKind.IsOwnerContinuation() {
		return out, ErrMemberToolDenied
	}
	attempt, err := taskWorkBoundAttemptInTx(ctx, tx, d, "")
	if err != nil {
		return out, err
	}
	if attempt.ID == "" || attempt.TurnID != scope.Claim.TurnID {
		return out, ErrMemberToolDenied
	}
	phase = "submit"
	derived := SubmitTaskInput{
		GroupID: d.GroupID, TaskID: d.TaskID, AttemptID: attempt.ID, ExpectedRevision: d.TaskRevision,
		ActorID: d.OwnerMemberID, MemberScope: &scope, Summary: input.Summary,
		ArtifactVersions: input.ArtifactVersions, IdempotencyKey: key,
	}
	receipt := mutationScope{operation: "submit_task", actorID: d.OwnerMemberID, groupID: d.GroupID, key: key}
	payload := taskSubmissionPayload(derived)
	_, replay, err := lookupMutation(ctx, tx, receipt, payload)
	if err != nil {
		return out, err
	}
	if !replay {
		blocked, current, err := s.taskWorkReadinessInTx(ctx, tx, f, scope.Claim.TurnID)
		if err != nil {
			return out, err
		}
		if blocked != "" || current != attempt.ID {
			return out, ErrConflict
		}
		if f.decisionAnswer {
			handled, err := taskWorkInputHandledInTx(ctx, tx, f.d.SourceID)
			if err != nil {
				return out, err
			}
			if !handled {
				return out, ErrConflict
			}
		}
	}
	result, err := s.submitTaskInTx(ctx, tx, derived, &d.TopicID)
	if err != nil {
		return out, err
	}
	if !replay {
		task, err := taskInTx(ctx, tx, d.GroupID, d.TaskID)
		if err != nil {
			return out, err
		}
		if task.Status != domain.TaskStatusSubmitted || task.Revision != d.TaskRevision || task.ActiveAttemptID != attempt.ID || task.CurrentSubmissionID != result.ID {
			return out, ErrConflict
		}
	}
	phase = "readback"
	f, err = taskWorkExecutionInTx(ctx, tx, scope)
	if err != nil {
		return out, err
	}
	bound, err := taskWorkBoundAttemptInTx(ctx, tx, d, "")
	if err != nil {
		return out, err
	}
	if f.d != d || bound != attempt {
		return out, ErrMemberToolDenied
	}
	saved, err := taskSubmissionInTx(ctx, tx, d.GroupID, d.TaskID, result.ID)
	if err != nil {
		return out, err
	}
	if saved.ID != result.ID || saved.TaskID != d.TaskID || saved.GroupID != d.GroupID || saved.TaskRevision != d.TaskRevision ||
		saved.AttemptID != attempt.ID || saved.SubmittedBy != d.OwnerMemberID || saved.Summary != result.Summary ||
		saved.CreatedAt != result.CreatedAt || !slices.Equal(saved.ArtifactVersions, result.ArtifactVersions) {
		return out, ErrConflict
	}
	if key != "" {
		id, present, err := lookupMutation(ctx, tx, receipt, payload)
		if err != nil {
			return out, err
		}
		if !present || id != result.ID {
			return out, ErrConflict
		}
	}
	phase = "commit"
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return saved, nil
}
