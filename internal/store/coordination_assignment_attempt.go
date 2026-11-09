package store

import (
	"context"
	"database/sql"
	"time"

	"agentworkspace/internal/domain"
)

// This authority is private and never accepted from a DTO. The shared Activate
// gate re-derives its worker receipt and current eligibility in the same tx.
type assignmentActivationAuthority struct {
	scope CoordinationToolScope
}

func coordinationAssignmentWorkerInTx(ctx context.Context, tx *sql.Tx, scope CoordinationToolScope) (v assignmentSnapshot, view domain.AssignmentView, err error) {
	if err = coordinationDecideLiveInTx(ctx, tx, scope.Member); err != nil {
		return
	}
	if scope.Provenance.Kind != CoordinationRoleWorker {
		return v, view, ErrMemberToolDenied
	}
	p, err := coordinationExecutionProvenanceInTx(ctx, tx, scope.Member, scope.Provenance)
	if err != nil {
		return v, view, err
	}
	v, state, err := coordinationAssignmentLoad(ctx, tx, p.AssignmentID)
	if err != nil {
		return v, view, err
	}
	if v.a.GroupID != p.GroupID || v.a.TaskID != p.TaskID || v.a.RequestID != p.RequestID ||
		v.a.WorkerMemberID != p.MemberID || v.r.TaskRevision != p.TaskRevision || v.r.TopicID != p.TopicID ||
		state.Status != domain.AssignmentDispatchStatusEnqueued || state.Delivery == nil ||
		state.Delivery.DeliveryID != p.DeliveryID || state.Delivery.MessageID != p.MessageID ||
		state.Delivery.ConversationID != p.ConversationID || state.Delivery.WorkerMemberID != p.MemberID {
		return v, view, ErrMemberToolDenied
	}
	view, err = coordinationDecideAssignmentInTx(ctx, tx, p.GroupID, p.AssignmentID)
	return
}

func coordinationAssignmentBound(scope CoordinationToolScope, b *domain.AssignmentAttemptBinding) bool {
	p := scope.Provenance
	return b != nil && b.AssignmentID == p.AssignmentID && b.GroupID == p.GroupID && b.TaskID == p.TaskID &&
		b.WorkerMemberID == p.MemberID && b.TurnID == scope.Member.Claim.TurnID && b.TaskRevision == p.TaskRevision &&
		b.AttemptID != "" && (scope.AttemptID == "" || scope.AttemptID == b.AttemptID)
}

// No history window, worker filter, or revision filter may hide an old live Turn.
// An own Attempt is excluded only after the exact binding has been read back.
const coordinationAssignmentStart = ` AND t.status='open' AND t.revision=r.task_revision
 AND t.active_attempt_id IS nullif(:own,'') AND t.current_submission_id IS NULL
 AND (coalesce(t.assignee_id,'')='' OR t.assignee_id=a.worker_member_id)
 AND ((:own='' AND NOT EXISTS(SELECT 1 FROM coordination_assignment_attempts b WHERE b.assignment_id=a.id))
 OR (:own<>'' AND EXISTS(SELECT 1 FROM coordination_assignment_attempts b WHERE b.assignment_id=a.id
 AND b.attempt_id=:own AND b.turn_id=:turn AND b.group_id=a.group_id AND b.task_id=a.task_id
 AND b.worker_member_id=a.worker_member_id AND b.task_revision=r.task_revision)))
 AND NOT EXISTS(SELECT 1 FROM coordination_assignments other WHERE other.task_id=a.task_id AND other.id<>a.id)
 AND NOT EXISTS(SELECT 1 FROM coordination_dispatch_state other WHERE other.task_id=a.task_id
 AND other.status IN ('pending','enqueued','held','declined'))
 AND NOT EXISTS(SELECT 1 FROM task_attempts ha JOIN turns ht ON ht.id=ha.turn_id
 WHERE ha.group_id=a.group_id AND ha.task_id=a.task_id AND ha.id<>:own
 AND ht.state IN ('starting','running','cancel_requested','unknown'))
 AND EXISTS(SELECT 1 FROM task_coordination tc WHERE tc.task_id=r.task_id AND tc.group_id=r.group_id
 AND tc.topic_id=r.topic_id AND tc.current_mandate_id=r.mandate_id AND tc.authority_generation=r.authority_generation)
 AND EXISTS(SELECT 1 FROM group_coordinator_bindings cb WHERE cb.group_id=r.group_id AND cb.revision=r.binding_revision)
 AND EXISTS(SELECT 1 FROM group_dispatch_policies cp WHERE cp.group_id=r.group_id AND cp.revision=r.policy_revision)
 AND EXISTS(SELECT 1 FROM members co WHERE co.id=r.coordinator_member_id AND co.group_id=r.group_id AND co.status='active')
 AND EXISTS(SELECT 1 FROM members w WHERE w.id=a.worker_member_id AND w.group_id=a.group_id AND w.status='active')
 AND NOT EXISTS(SELECT 1 FROM task_dependencies dep JOIN tasks parent ON parent.id=dep.dependency_id AND parent.group_id=dep.group_id
 WHERE dep.task_id=a.task_id AND dep.group_id=a.group_id AND parent.status<>'accepted')`

func coordinationAssignmentStartInTx(ctx context.Context, tx *sql.Tx, v assignmentSnapshot, scope CoordinationToolScope, own string) error {
	args := append(v.args(), sql.Named("own", own), sql.Named("turn", scope.Member.Claim.TurnID))
	var allowed bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1"+coordinationAssignmentRelations+coordinationAssignmentStart+")", args...).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return ErrConflict
	}
	return nil
}

func coordinationActivateOwnershipInTx(ctx context.Context, tx *sql.Tx, input ActivateTaskAttemptInput, authority *assignmentActivationAuthority) error {
	var reserved bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM coordination_dispatch_state
 WHERE task_id=? AND group_id=? AND status IN ('pending','enqueued','held','declined'))
 OR EXISTS(SELECT 1 FROM coordination_assignments WHERE task_id=? AND group_id=?)`, input.TaskID, input.GroupID, input.TaskID, input.GroupID).Scan(&reserved); err != nil {
		// A missing legacy table or storage failure is not proof of no owner.
		return err
	}
	if authority == nil {
		if reserved {
			return ErrConflict
		}
		return nil
	}
	scope := authority.scope
	p := scope.Provenance
	if input.MemberScope == nil || *input.MemberScope != scope.Member || input.ActorID != p.MemberID ||
		input.GroupID != p.GroupID || input.TaskID != p.TaskID || input.ExpectedRevision != p.TaskRevision ||
		input.ExpectedActiveAttemptID != "" || input.IdempotencyKey != "" || scope.AttemptID != "" {
		return ErrMemberToolDenied
	}
	v, view, err := coordinationAssignmentWorkerInTx(ctx, tx, scope)
	if err != nil {
		return err
	}
	if view.Binding != nil {
		return ErrConflict
	}
	return coordinationAssignmentStartInTx(ctx, tx, v, scope, "")
}

// BindAssignmentAttempt is a postclaim Store primitive, not permission to execute
// a provider. Commit errors stay unknown; no receipt, retry, or claim release is inferred.
func (s *Store) BindAssignmentAttempt(ctx context.Context, scope CoordinationToolScope) (out domain.AssignmentAttemptBinding, err error) {
	phase := "begin"
	var tx *sql.Tx
	defer func() { err = coordinationAssignmentFinish(tx, phase, err) }()
	tx, err = s.beginMemberToolTx(ctx, scope.Member)
	if err != nil {
		return out, err
	}
	phase = "source"
	v, view, err := coordinationAssignmentWorkerInTx(ctx, tx, scope)
	if err != nil {
		return out, err
	}
	var binding domain.AssignmentAttemptBinding
	if view.Binding != nil {
		if !coordinationAssignmentBound(scope, view.Binding) {
			return out, ErrMemberToolDenied
		}
		binding = *view.Binding
	} else {
		if scope.AttemptID != "" {
			return out, ErrMemberToolDenied
		}
		phase = "activate"
		p := scope.Provenance
		attempt, err := activateTaskAttemptInTx(ctx, tx, ActivateTaskAttemptInput{
			GroupID: p.GroupID, TaskID: p.TaskID, ExpectedRevision: p.TaskRevision,
			ActorID: p.MemberID, MemberScope: &scope.Member,
		}, &assignmentActivationAuthority{scope: scope})
		if err != nil {
			return out, err
		}
		// Read actual persistence before inserting a binding; INSERT IGNORE must
		// never turn a synthesized result into authority.
		saved, actor, err := taskAttemptInTx(ctx, tx, p.GroupID, p.TaskID, attempt.ID)
		if err != nil {
			return out, err
		}
		task, err := taskInTx(ctx, tx, p.GroupID, p.TaskID)
		if err != nil {
			return out, err
		}
		if saved != attempt || actor != p.MemberID || saved.TurnID != scope.Member.Claim.TurnID ||
			saved.TaskRevision != p.TaskRevision || task.Revision != p.TaskRevision || task.ActiveAttemptID != attempt.ID ||
			task.Status != domain.TaskStatusOpen || task.CurrentSubmissionID != "" {
			return out, ErrConflict
		}
		binding = domain.AssignmentAttemptBinding{AssignmentID: p.AssignmentID, GroupID: p.GroupID, TaskID: p.TaskID,
			WorkerMemberID: p.MemberID, TurnID: scope.Member.Claim.TurnID, AttemptID: attempt.ID,
			TaskRevision: p.TaskRevision, CreatedAt: time.Now().UTC()}
		phase = "binding"
		if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO coordination_assignment_attempts
 (assignment_id,group_id,task_id,worker_member_id,turn_id,attempt_id,task_revision,created_at) VALUES(?,?,?,?,?,?,?,?)`,
			binding.AssignmentID, binding.GroupID, binding.TaskID, binding.WorkerMemberID, binding.TurnID,
			binding.AttemptID, binding.TaskRevision, binding.CreatedAt.Format(timestampLayout)); err != nil {
			return out, err
		}
	}
	phase = "final"
	v, view, err = coordinationAssignmentWorkerInTx(ctx, tx, scope)
	if err != nil {
		return out, err
	}
	if !coordinationAssignmentBound(scope, view.Binding) || *view.Binding != binding {
		return out, ErrConflict
	}
	if err := coordinationAssignmentStartInTx(ctx, tx, v, scope, binding.AttemptID); err != nil {
		return out, err
	}
	phase = "commit"
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return binding, nil
}
