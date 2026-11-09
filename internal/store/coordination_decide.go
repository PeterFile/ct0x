package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"math"
	"strings"
	"time"

	"agentworkspace/internal/domain"
)

type CoordinatorDecisionInput struct {
	Kind           string `json:"kind"`
	WorkerMemberID string `json:"worker_member_id"`
	Reason         string `json:"reason"`
}

// DecideDispatch is an unwired primitive; business callers require the reviewed N3b3/N4 gates.
func (s *Store) DecideDispatch(ctx context.Context, scope CoordinationToolScope, input CoordinatorDecisionInput, key string) (out domain.DecisionReceipt, err error) {
	defer func() { err = coordinationDecideError(err) }()
	if !memberToolKeyValid(key) || strings.TrimSpace(input.Reason) == "" ||
		(input.Kind != "assign" && input.Kind != "decline") ||
		(input.Kind == "assign" && !validTriggerID(input.WorkerMemberID)) || (input.Kind == "decline" && input.WorkerMemberID != "") {
		return out, ErrInvalid
	}
	tx, err := s.beginMemberToolTx(ctx, scope.Member)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err := coordinationDecideLiveInTx(ctx, tx, scope.Member); err != nil {
		return out, err
	}
	if scope.Provenance.Kind != CoordinationRoleCoordinator || scope.AttemptID != "" {
		return out, ErrMemberToolDenied
	}
	p, err := coordinationExecutionProvenanceInTx(ctx, tx, scope.Member, scope.Provenance)
	if err != nil {
		return out, err
	}
	mutation := mutationScope{operation: "decide_dispatch", actorID: p.MemberID, groupID: p.GroupID, key: key}
	payload := mutationPayload(p.RequestID, input.Kind, input.WorkerMemberID, input.Reason)
	id, replay, err := lookupMutation(ctx, tx, mutation, payload)
	if err != nil {
		return out, err
	}
	r, err := coordinationDispatchRequestInTx(ctx, tx, p.GroupID, p.RequestID)
	if err != nil {
		return out, err
	}
	if !replay {
		state, err := coordinationDispatchStateInTx(ctx, tx, r)
		if err != nil {
			return out, err
		}
		if state.Status != domain.DispatchStatusEnqueued {
			return out, ErrConflict
		}
		if state.Generation == math.MaxInt64 {
			return out, ErrInvalid
		}
		if err := coordinationDecideEligibleInTx(ctx, tx, r, input.WorkerMemberID, ""); err != nil {
			return out, err
		}
		at := time.Now().UTC()
		stamp := at.Format(timestampLayout)
		id = rand.Text()
		want := domain.DecisionReceipt{Decision: domain.CoordinatorDecision{ID: id, RequestID: r.ID, TaskID: r.TaskID, GroupID: r.GroupID,
			CoordinatorMemberID: p.MemberID, CoordinatorTurnID: scope.Member.Claim.TurnID, Kind: domain.CoordinatorDecisionKind(input.Kind),
			WorkerMemberID: input.WorkerMemberID, Reason: input.Reason, CreatedAt: at}}
		if input.Kind == "assign" {
			want.Decision.AssignmentID = rand.Text()
			want.Assignment = &domain.Assignment{ID: want.Decision.AssignmentID, RequestID: r.ID, DecisionID: id,
				TaskID: r.TaskID, GroupID: r.GroupID, WorkerMemberID: input.WorkerMemberID, CreatedAt: at}
		}
		if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO coordination_decisions
		 (id,request_id,task_id,group_id,coordinator_member_id,coordinator_turn_id,kind,worker_member_id,assignment_id,reason,created_at)
		 VALUES(?,?,?,?,?,?,?,nullif(?,''),nullif(?,''),?,?)`, id, r.ID, r.TaskID, r.GroupID, p.MemberID,
			scope.Member.Claim.TurnID, input.Kind, input.WorkerMemberID, want.Decision.AssignmentID, input.Reason, stamp); err != nil {
			return out, err
		}
		if a := want.Assignment; a != nil {
			if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO coordination_assignments
			 (id,request_id,decision_id,task_id,group_id,worker_member_id,created_at) VALUES(?,?,?,?,?,?,?)`,
				a.ID, r.ID, id, r.TaskID, r.GroupID, a.WorkerMemberID, stamp); err != nil {
				return out, err
			}
			if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO coordination_assignment_dispatch
			 (assignment_id,task_id,group_id,worker_member_id,generation,status,attempt_count,next_attempt_at,updated_at,last_error_code,held_reason)
			 VALUES(?,?,?,?,1,'pending',0,?,?,'','')`, a.ID, r.TaskID, r.GroupID, a.WorkerMemberID, stamp, stamp); err != nil {
				return out, err
			}
		}
		status := domain.DispatchStatusDeclined
		if want.Assignment != nil {
			status = domain.DispatchStatusAssigned
		}
		if err := coordinationDispatchWrite(ctx, tx, `UPDATE coordination_dispatch_state SET decision_id=?,status=?,generation=generation+1,updated_at=?
		 WHERE request_id=? AND group_id=? AND generation=? AND status='enqueued' AND decision_id IS NULL`, id, status, stamp, r.ID, r.GroupID, state.Generation); err != nil {
			return out, err
		}
		if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO mutation_idempotency(operation,actor_id,group_id,idempotency_key,payload,record_id)
		 VALUES(?,?,?,?,?,?)`, mutation.operation, mutation.actorID, mutation.groupID, key, payload, id); err != nil {
			return out, err
		}
		actual, err := coordinationDecideReceiptInTx(ctx, tx, r.GroupID, r.ID)
		if err != nil {
			return out, err
		}
		if actual.Decision != want.Decision || (actual.Assignment == nil) != (want.Assignment == nil) ||
			actual.Assignment != nil && *actual.Assignment != *want.Assignment {
			return out, ErrConflict
		}
		state.Generation++
		state.Status = status
		saved, err := coordinationDispatchStateInTx(ctx, tx, r)
		if err != nil {
			return out, err
		}
		var exact bool
		if err := tx.QueryRowContext(ctx, `SELECT decision_id=? AND updated_at=? FROM coordination_dispatch_state WHERE request_id=? AND group_id=?`, id, stamp, r.ID, r.GroupID).Scan(&exact); err != nil {
			return out, err
		}
		if !exact || !coordinationDispatchSameState(state, saved) {
			return out, ErrConflict
		}
		if want.Assignment != nil {
			view, err := coordinationDecideAssignmentInTx(ctx, tx, r.GroupID, want.Assignment.ID)
			if err != nil {
				return out, err
			}
			initial := domain.AssignmentDispatchState{AssignmentID: want.Assignment.ID, Generation: 1,
				Status: domain.AssignmentDispatchStatusPending, NextAttemptAt: at, UpdatedAt: at}
			if view.Dispatch != initial || view.Binding != nil {
				return out, ErrConflict
			}
		}
		savedID, savedKey, err := lookupMutation(ctx, tx, mutation, payload)
		if err != nil {
			return out, err
		}
		if !savedKey || savedID != id {
			return out, ErrConflict
		}
		if err := coordinationDecideLiveInTx(ctx, tx, scope.Member); err != nil {
			return out, err
		}
		if _, err := coordinationExecutionProvenanceInTx(ctx, tx, scope.Member, p); err != nil {
			return out, err
		}
		if err := coordinationDecideEligibleInTx(ctx, tx, r, input.WorkerMemberID, want.Decision.AssignmentID); err != nil {
			return out, err
		}
	}
	result, err := coordinationDecideReceiptInTx(ctx, tx, r.GroupID, r.ID)
	if err != nil {
		return out, err
	}
	d := result.Decision
	if d.ID != id || d.RequestID != p.RequestID || d.TaskID != p.TaskID || d.GroupID != p.GroupID || d.CoordinatorMemberID != p.MemberID ||
		d.CoordinatorTurnID != scope.Member.Claim.TurnID || string(d.Kind) != input.Kind || d.WorkerMemberID != input.WorkerMemberID || d.Reason != input.Reason {
		return out, ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return out, &coordinationDecideFailure{cause: err, unknown: true}
	}
	return result, nil
}

func coordinationDecideEligibleInTx(ctx context.Context, tx *sql.Tx, r domain.DispatchRequest, worker, ownAssignment string) error {
	code, err := coordinationDispatchEligibility(ctx, tx, r)
	if err != nil || code != "" && !(worker == "" && code == domain.DispatchHeldNoEligibleWorker) {
		return coordinationDispatchFenceError(code, err)
	}
	if worker == "" {
		return nil
	}
	var eligible bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM group_dispatch_policy_revisions p
	 JOIN json_each(p.allowed_worker_ids) allowed JOIN members w ON w.id=allowed.value AND w.group_id=p.group_id
	 JOIN tasks t ON t.id=? AND t.group_id=p.group_id JOIN mandates m ON m.id=? AND m.task_id=t.id AND m.group_id=t.group_id
	 WHERE p.group_id=? AND p.revision=? AND w.id=? AND w.status='active' AND w.id<>?
	 AND (coalesce(t.assignee_id,'')='' OR t.assignee_id=w.id) AND (m.assignee_id='' OR m.assignee_id=w.id))
	 AND NOT EXISTS(SELECT 1 FROM coordination_assignments WHERE task_id=? AND id<>?)
	 AND NOT EXISTS(SELECT 1 FROM task_attempts a JOIN turns t ON t.id=a.turn_id
	 WHERE a.group_id=? AND a.task_id=? AND t.state IN ('starting','running','cancel_requested','unknown'))`,
		r.TaskID, r.MandateID, r.GroupID, r.PolicyRevision, worker, r.CoordinatorMemberID,
		r.TaskID, ownAssignment, r.GroupID, r.TaskID).Scan(&eligible)
	if err != nil {
		return err
	}
	if !eligible {
		return ErrConflict
	}
	return nil
}
