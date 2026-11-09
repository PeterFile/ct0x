package store

import (
	"context"
	"database/sql"
	"errors"

	"agentworkspace/internal/domain"
)

type CoordinationReadinessCode string

const (
	CoordinationReady                 CoordinationReadinessCode = ""
	CoordinationDependencyNotAccepted CoordinationReadinessCode = "dependency_not_accepted"
	CoordinationTaskStale             CoordinationReadinessCode = "task_stale"
	CoordinationAuthorityStale        CoordinationReadinessCode = "authority_stale"
	CoordinationBindingStale          CoordinationReadinessCode = "binding_stale"
	CoordinationPolicyStale           CoordinationReadinessCode = "policy_stale"
	CoordinationMemberInactive        CoordinationReadinessCode = "member_inactive"
	CoordinationAlreadyDecided        CoordinationReadinessCode = "already_decided"
	CoordinationAttemptConflict       CoordinationReadinessCode = "attempt_conflict"
)

// CoordinationRoleState is trusted process-local evidence, not a transport DTO.
// Blocked describes a new start, never permission to replay a historical command.
// Ready is a bounded hint; Bind still owns full-history and activation gates.
type CoordinationRoleState struct {
	Provenance        CoordinationRoleProvenance `json:"-"`
	AttemptID         string                     `json:"-"`
	ProviderID, Model string                     `json:"-"`
	Blocked           CoordinationReadinessCode  `json:"-"`
}

// ResolveCoordinationDelivery observes queued identity in one read snapshot. It
// neither claims a Turn nor promises authorization for a later write transaction.
func (s *Store) ResolveCoordinationDelivery(ctx context.Context, groupID, deliveryID string) (out CoordinationRoleState, err error) {
	phase := "input"
	var tx *sql.Tx
	defer func() { err = coordinationAssignmentFinish(tx, phase, err) }()
	if !validTriggerID(groupID) || !validTriggerID(deliveryID) {
		return out, ErrInvalid
	}
	phase = "begin"
	tx, err = s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	phase = "source"
	state := CoordinationRoleState{}
	p := &state.Provenance
	p.Kind, p.GroupID, p.DeliveryID = CoordinationRoleOrdinary, groupID, deliveryID
	var valid bool
	var sender string
	err = tx.QueryRowContext(ctx, `SELECT d.message_id,d.conversation_id,d.recipient_id,d.topic_id,
 coalesce(m.sender_id,''),coalesce(w.provider_id,''),coalesce(w.model,''),
 coalesce(d.state='queued' AND d.turn_id='' AND m.group_id=d.group_id AND m.topic_id=d.topic_id
 AND c.group_id=d.group_id AND c.member_id=d.recipient_id AND c.topic_id=d.topic_id AND w.group_id=d.group_id
 AND EXISTS(SELECT 1 FROM groups WHERE id=d.group_id)
 AND NOT EXISTS(SELECT 1 FROM turns WHERE delivery_id=d.id),0),
 CASE WHEN w.status='active' THEN '' ELSE 'member_inactive' END
 FROM deliveries d LEFT JOIN messages m ON m.id=d.message_id
 LEFT JOIN conversations c ON c.id=d.conversation_id LEFT JOIN members w ON w.id=d.recipient_id
 WHERE d.id=? AND d.group_id=?`, deliveryID, groupID).Scan(&p.MessageID, &p.ConversationID, &p.MemberID, &p.TopicID,
		&sender, &state.ProviderID, &state.Model, &valid, &state.Blocked)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if !valid {
		return out, ErrMemberToolDenied
	}
	actual, err := coordinationQueuedProvenanceInTx(ctx, tx, *p, sender)
	if err != nil {
		return out, err
	}
	if actual.Kind != CoordinationRoleOrdinary {
		state, err = coordinationRoleStateInTx(ctx, tx, MemberToolScope{}, actual)
		if err != nil {
			return out, err
		}
	}
	phase = "commit"
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return state, nil
}

// Raw candidates precede parent joins. A broken second relation still counts;
// only successful absence from BOTH unique delivery indexes permits ordinary.
func coordinationQueuedProvenanceInTx(ctx context.Context, tx *sql.Tx, base CoordinationRoleProvenance, sender string) (CoordinationRoleProvenance, error) {
	rows, err := tx.QueryContext(ctx, `WITH source AS (
 SELECT 'coordinator' kind,s.request_id,s.task_id,s.group_id,s.delivery_id,s.message_id,s.conversation_id,
 s.delivered_member_id,'' assignment_id,s.status IN ('enqueued','assigned','declined') linked
 FROM coordination_dispatch_state s WHERE s.delivery_id=?
 UNION ALL SELECT 'worker',a.request_id,s.task_id,s.group_id,s.delivery_id,s.message_id,s.conversation_id,
 s.delivered_member_id,s.assignment_id,(s.status='enqueued' AND a.task_id=s.task_id AND a.group_id=s.group_id
 AND a.worker_member_id=s.worker_member_id AND s.worker_member_id=s.delivered_member_id)
 FROM coordination_assignment_dispatch s LEFT JOIN coordination_assignments a ON a.id=s.assignment_id WHERE s.delivery_id=?)
 SELECT p.kind,p.group_id,p.delivery_id,p.message_id,p.conversation_id,p.delivered_member_id,
 coalesce(r.topic_id,''),coalesce(r.id,''),coalesce(r.task_id,''),coalesce(r.task_revision,0),p.assignment_id,
 coalesce(p.linked AND r.task_id=p.task_id AND r.group_id=p.group_id
 AND ((p.kind='coordinator' AND r.coordinator_member_id=p.delivered_member_id AND ?='user')
 OR (p.kind='worker' AND r.coordinator_member_id=?)),0)
 FROM source p LEFT JOIN coordination_dispatch_requests r ON r.id=p.request_id`, base.DeliveryID, base.DeliveryID, sender, sender)
	if err != nil {
		return CoordinationRoleProvenance{}, err
	}
	defer rows.Close()
	var actual CoordinationRoleProvenance
	count, valid := 0, true
	for rows.Next() {
		var linked bool
		if err := rows.Scan(&actual.Kind, &actual.GroupID, &actual.DeliveryID, &actual.MessageID, &actual.ConversationID,
			&actual.MemberID, &actual.TopicID, &actual.RequestID, &actual.TaskID, &actual.TaskRevision, &actual.AssignmentID, &linked); err != nil {
			return CoordinationRoleProvenance{}, err
		}
		count++
		valid = valid && linked
	}
	if err := rows.Err(); err != nil {
		return CoordinationRoleProvenance{}, err
	}
	if err := rows.Close(); err != nil {
		return CoordinationRoleProvenance{}, err
	}
	if count == 0 {
		return base, nil
	}
	identity := actual
	identity.Kind, identity.RequestID, identity.TaskID, identity.TaskRevision, identity.AssignmentID = CoordinationRoleOrdinary, "", "", 0, ""
	if count != 1 || !valid || identity != base {
		return CoordinationRoleProvenance{}, ErrMemberToolDenied
	}
	return actual, nil
}

// ResolveCoordinationExecution keeps identity separate from startup readiness.
// The same transaction fences the live claim, then re-derives all provenance.
func (s *Store) ResolveCoordinationExecution(ctx context.Context, scope MemberToolScope, expected CoordinationRoleProvenance) (out CoordinationRoleState, err error) {
	phase := "begin"
	var tx *sql.Tx
	defer func() { err = coordinationAssignmentFinish(tx, phase, err) }()
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	phase = "scope"
	if err := memberToolScopeInTx(ctx, tx, scope); err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		return out, err
	}
	phase = "source"
	if err := coordinationDecideLiveInTx(ctx, tx, scope); err != nil {
		return out, err
	}
	if expected.Kind != CoordinationRoleCoordinator && expected.Kind != CoordinationRoleWorker {
		return out, ErrMemberToolDenied
	}
	p, err := coordinationExecutionProvenanceInTx(ctx, tx, scope, expected)
	if err != nil {
		return out, err
	}
	state, err := coordinationRoleStateInTx(ctx, tx, scope, p)
	if err != nil {
		return out, err
	}
	phase = "commit"
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return state, nil
}

// Structural failures are denials, not current-readiness codes. Unknown SQL and
// context causes must survive the outer sanitized transaction error unchanged.
func coordinationRoleRelationError(err error) error {
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) {
		return ErrMemberToolDenied
	}
	return err
}

func coordinationRoleStateInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, p CoordinationRoleProvenance) (CoordinationRoleState, error) {
	state := CoordinationRoleState{Provenance: p}
	if p.Kind == CoordinationRoleWorker {
		attempt, err := coordinationRoleAttemptInTx(ctx, tx, scope, p)
		if err != nil {
			return CoordinationRoleState{}, err
		}
		state.AttemptID = attempt
	}
	var valid bool
	var status string
	err := tx.QueryRowContext(ctx, `SELECT coalesce(
 r.group_id=:grp AND r.task_id=:task AND r.task_revision=:revision AND r.topic_id=:topic AND r.requested_by='user'
 AND s.task_id=r.task_id AND s.group_id=r.group_id AND t.group_id=r.group_id
 AND EXISTS(SELECT 1 FROM groups WHERE id=r.group_id)
 AND EXISTS(SELECT 1 FROM deliveries cd JOIN conversations cc ON cc.id=cd.conversation_id
 WHERE cd.id=s.delivery_id AND cc.group_id=r.group_id AND cc.member_id=r.coordinator_member_id AND cc.topic_id=r.topic_id)
 AND m.task_id=r.task_id AND m.group_id=r.group_id AND m.topic_id=r.topic_id AND m.task_revision_at_admission=r.task_revision
 AND m.authority_kind='local_user_explicit' AND m.authorized_by='user' AND m.strength='explicit'
 AND m.policy_revision='local-user-explicit/v1' AND rd.actor_id='user' AND rd.policy_revision=m.policy_revision
 AND rd.mandate_id=m.id AND rd.trigger_id=m.origin_trigger_id AND rd.task_id=m.task_id
 AND rd.group_id=m.group_id AND rd.topic_id=m.topic_id AND rd.task_revision_at_admission=m.task_revision_at_admission
 AND h.member_id=r.coordinator_member_id AND hp.group_id=r.group_id AND co.group_id=r.group_id AND w.group_id=r.group_id
 AND ((:kind='coordinator' AND :member=r.coordinator_member_id AND s.status IN ('enqueued','assigned','declined'))
 OR (:kind='worker' AND s.status='assigned' AND (m.assignee_id='' OR m.assignee_id=:member)
 AND EXISTS(SELECT 1 FROM json_each(hp.allowed_worker_ids) WHERE type='text' AND value=:member)))
 AND NOT EXISTS(SELECT 1 FROM task_dependencies dep LEFT JOIN tasks parent ON parent.id=dep.dependency_id AND parent.group_id=dep.group_id
 WHERE dep.task_id=r.task_id AND dep.group_id=r.group_id AND parent.id IS NULL),0),coalesce(s.status,''),
 coalesce(w.provider_id,''),coalesce(w.model,''),CASE
 WHEN :kind='coordinator' AND s.status IN ('assigned','declined') THEN 'already_decided'
 WHEN t.status<>'open' OR t.revision<>r.task_revision OR t.current_submission_id IS NOT NULL
 OR (:kind='worker' AND coalesce(t.assignee_id,'')<>'' AND t.assignee_id<>:member) THEN 'task_stale'
 WHEN t.active_attempt_id IS NOT nullif(:attempt,'') THEN CASE WHEN :kind='worker' THEN 'attempt_conflict' ELSE 'task_stale' END
 WHEN tc.task_id IS NULL OR tc.group_id<>r.group_id OR tc.topic_id<>r.topic_id
 OR tc.current_mandate_id<>r.mandate_id OR tc.authority_generation<>r.authority_generation THEN 'authority_stale'
 WHEN cb.revision IS NULL OR cb.revision<>r.binding_revision THEN 'binding_stale'
 WHEN cp.revision IS NULL OR cp.revision<>r.policy_revision THEN 'policy_stale'
 WHEN co.status<>'active' OR w.status<>'active' THEN 'member_inactive'
 WHEN :kind='worker' AND EXISTS(SELECT 1 FROM task_dependencies dep JOIN tasks parent ON parent.id=dep.dependency_id AND parent.group_id=dep.group_id
 WHERE dep.task_id=r.task_id AND dep.group_id=r.group_id AND parent.status<>'accepted') THEN 'dependency_not_accepted'
 ELSE '' END
 FROM coordination_dispatch_requests r LEFT JOIN coordination_dispatch_state s ON s.request_id=r.id
 LEFT JOIN tasks t ON t.id=r.task_id LEFT JOIN mandates m ON m.id=r.mandate_id LEFT JOIN routing_decisions rd ON rd.id=m.decision_id
 LEFT JOIN group_coordinator_binding_revisions h ON h.group_id=r.group_id AND h.revision=r.binding_revision
 LEFT JOIN group_dispatch_policy_revisions hp ON hp.group_id=r.group_id AND hp.revision=r.policy_revision
 LEFT JOIN members co ON co.id=r.coordinator_member_id LEFT JOIN members w ON w.id=:member
 LEFT JOIN task_coordination tc ON tc.task_id=r.task_id LEFT JOIN group_coordinator_bindings cb ON cb.group_id=r.group_id
 LEFT JOIN group_dispatch_policies cp ON cp.group_id=r.group_id WHERE r.id=:request`,
		sql.Named("grp", p.GroupID), sql.Named("task", p.TaskID), sql.Named("revision", p.TaskRevision), sql.Named("topic", p.TopicID),
		sql.Named("kind", string(p.Kind)), sql.Named("member", p.MemberID), sql.Named("attempt", state.AttemptID), sql.Named("request", p.RequestID)).Scan(
		&valid, &status, &state.ProviderID, &state.Model, &state.Blocked)
	if err != nil {
		return CoordinationRoleState{}, coordinationRoleRelationError(err)
	}
	if !valid {
		return CoordinationRoleState{}, ErrMemberToolDenied
	}
	if status == "assigned" || status == "declined" {
		receipt, err := coordinationDecideReceiptInTx(ctx, tx, p.GroupID, p.RequestID)
		if err != nil {
			return CoordinationRoleState{}, coordinationRoleRelationError(err)
		}
		if p.Kind == CoordinationRoleWorker {
			if receipt.Assignment == nil || receipt.Assignment.ID != p.AssignmentID || receipt.Assignment.WorkerMemberID != p.MemberID {
				return CoordinationRoleState{}, ErrMemberToolDenied
			}
			dispatch, err := coordinationAssignmentState(ctx, tx, *receipt.Assignment)
			if err != nil {
				return CoordinationRoleState{}, coordinationRoleRelationError(err)
			}
			d := dispatch.Delivery
			if dispatch.Status != domain.AssignmentDispatchStatusEnqueued || d == nil || d.DeliveryID != p.DeliveryID ||
				d.MessageID != p.MessageID || d.ConversationID != p.ConversationID || d.WorkerMemberID != p.MemberID {
				return CoordinationRoleState{}, ErrMemberToolDenied
			}
		} else if receipt.Decision.CoordinatorTurnID != scope.Claim.TurnID {
			return CoordinationRoleState{}, ErrMemberToolDenied
		}
	}
	return state, nil
}

// Only the Assignment's exact persisted binding can identify an Attempt. This
// does not run the Task-wide start/ownership gate: Bind alone authorizes a start.
func coordinationRoleAttemptInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, p CoordinationRoleProvenance) (string, error) {
	var b domain.AssignmentAttemptBinding
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT b.assignment_id,b.group_id,b.task_id,b.worker_member_id,b.turn_id,b.attempt_id,b.task_revision,
 coalesce(a.id=b.attempt_id AND a.turn_id=b.turn_id AND a.group_id=b.group_id AND a.task_id=b.task_id
 AND a.actor_id=b.worker_member_id AND a.task_revision=b.task_revision,0)
 FROM coordination_assignment_attempts b LEFT JOIN task_attempts a ON a.id=b.attempt_id WHERE b.assignment_id=?`, p.AssignmentID).Scan(
		&b.AssignmentID, &b.GroupID, &b.TaskID, &b.WorkerMemberID, &b.TurnID, &b.AttemptID, &b.TaskRevision, &valid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !valid || !coordinationAssignmentBound(CoordinationToolScope{Member: scope, Provenance: p}, &b) {
		return "", ErrMemberToolDenied
	}
	return b.AttemptID, nil
}
