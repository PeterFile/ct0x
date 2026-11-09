package store

import (
	"context"
	"database/sql"
	"slices"

	"agentworkspace/internal/domain"
)

// AssignmentResultInput contains work evidence only, never execution identity.
type AssignmentResultInput struct {
	Summary          string                      `json:"summary"`
	ArtifactVersions []domain.ArtifactVersionRef `json:"artifact_versions"`
}

// SubmitAssignmentResult preserves submitTaskInTx's operation, payload, replay
// order and same-Turn fence. Starting readiness is deliberately not consulted.
func (s *Store) SubmitAssignmentResult(ctx context.Context, scope CoordinationToolScope, input AssignmentResultInput, key string) (out domain.Submission, err error) {
	phase := "begin"
	var tx *sql.Tx
	defer func() { err = coordinationAssignmentFinish(tx, phase, err) }()
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	phase = "scope"
	if err := memberToolScopeInTx(ctx, tx, scope.Member); err != nil {
		return out, err
	}
	phase = "source"
	_, view, err := coordinationAssignmentWorkerInTx(ctx, tx, scope)
	if err != nil {
		return out, err
	}
	if scope.AttemptID == "" || !coordinationAssignmentBound(scope, view.Binding) {
		return out, ErrMemberToolDenied
	}
	binding := *view.Binding
	phase = "submit"
	derived := SubmitTaskInput{
		GroupID: binding.GroupID, TaskID: binding.TaskID, AttemptID: binding.AttemptID, ExpectedRevision: binding.TaskRevision,
		ActorID: binding.WorkerMemberID, MemberScope: &scope.Member, Summary: input.Summary,
		ArtifactVersions: input.ArtifactVersions, IdempotencyKey: key,
	}
	receiptScope := mutationScope{operation: "submit_task", actorID: binding.WorkerMemberID, groupID: binding.GroupID, key: key}
	payload := taskSubmissionPayload(derived)
	priorID, _, err := lookupMutation(ctx, tx, receiptScope, payload)
	if err != nil {
		return out, err
	}
	result, err := submitTaskInTx(ctx, tx, derived)
	if err != nil {
		return out, err
	}
	phase = "final"
	_, view, err = coordinationAssignmentWorkerInTx(ctx, tx, scope)
	if err != nil {
		return out, err
	}
	if !coordinationAssignmentBound(scope, view.Binding) || *view.Binding != binding {
		return out, ErrMemberToolDenied
	}
	saved, err := taskSubmissionInTx(ctx, tx, binding.GroupID, binding.TaskID, result.ID)
	if err != nil {
		return out, err
	}
	if saved.ID != result.ID || saved.TaskID != binding.TaskID || saved.GroupID != binding.GroupID ||
		saved.TaskRevision != binding.TaskRevision || saved.AttemptID != binding.AttemptID || saved.SubmittedBy != binding.WorkerMemberID ||
		saved.Summary != result.Summary || saved.CreatedAt != result.CreatedAt || !slices.Equal(saved.ArtifactVersions, result.ArtifactVersions) {
		return out, ErrConflict
	}
	if result.ID != priorID {
		// Only a new write must still own the current Task pointer. Historical
		// same-key replay remains independent of later Task/policy changes.
		task, err := taskInTx(ctx, tx, binding.GroupID, binding.TaskID)
		if err != nil {
			return out, err
		}
		if task.Status != domain.TaskStatusSubmitted || task.Revision != binding.TaskRevision ||
			task.ActiveAttemptID != binding.AttemptID || task.CurrentSubmissionID != result.ID {
			return out, ErrConflict
		}
	}
	if key != "" {
		// Verify the original helper's receipt, without reserving a second key.
		id, present, err := lookupMutation(ctx, tx, receiptScope, payload)
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
	return result, nil
}
