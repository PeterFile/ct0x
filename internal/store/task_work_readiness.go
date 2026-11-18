package store

import (
	"context"
	"database/sql"
	"errors"

	"agentworkspace/internal/domain"
)

// The request and Assignment have already passed immutable provenance checks.
// These are current pointers, evaluated again in the postclaim write transaction.
const taskWorkCurrent = `SELECT coalesce(t.active_attempt_id,''),CASE
 WHEN t.status<>'open' OR t.revision<>:revision OR t.current_submission_id IS NOT NULL
 OR (t.assignee_id IS NOT NULL AND t.assignee_id NOT IN ('',:owner)) THEN 'task_stale'
 WHEN NOT EXISTS(SELECT 1 FROM task_coordination tc WHERE tc.task_id=t.id AND tc.group_id=t.group_id
 AND tc.topic_id=:topic AND tc.current_mandate_id=:mandate AND tc.authority_generation=:authority) THEN 'authority_stale'
 WHEN NOT EXISTS(SELECT 1 FROM group_coordinator_bindings cb JOIN group_coordinator_binding_revisions h USING(group_id,revision)
 WHERE cb.group_id=t.group_id AND cb.revision=:binding AND h.member_id=:coordinator) THEN 'binding_stale'
 WHEN NOT EXISTS(SELECT 1 FROM group_dispatch_policies cp JOIN group_dispatch_policy_revisions h USING(group_id,revision)
 WHERE cp.group_id=t.group_id AND cp.revision=:policy
 AND EXISTS(SELECT 1 FROM json_each(h.allowed_worker_ids) WHERE type='text' AND value=:owner)) THEN 'policy_stale'
 WHEN NOT EXISTS(SELECT 1 FROM members WHERE group_id=t.group_id AND id=:owner AND status='active')
 OR NOT EXISTS(SELECT 1 FROM members WHERE group_id=t.group_id AND id=:coordinator AND status='active') THEN 'member_inactive'
 WHEN EXISTS(SELECT 1 FROM task_dependencies d LEFT JOIN tasks parent ON parent.id=d.dependency_id AND parent.group_id=d.group_id
 WHERE d.group_id=t.group_id AND d.task_id=t.id AND parent.status IS NOT 'accepted') THEN 'dependency_not_accepted'
 ELSE '' END FROM tasks t WHERE t.id=:task AND t.group_id=:grp`

func taskWorkCurrentInTx(ctx context.Context, tx *sql.Tx, f taskWorkFacts) (blocked, active string, err error) {
	d, r := f.d, f.v.r
	err = tx.QueryRowContext(ctx, taskWorkCurrent,
		sql.Named("grp", d.GroupID), sql.Named("task", d.TaskID), sql.Named("topic", d.TopicID), sql.Named("revision", d.TaskRevision),
		sql.Named("mandate", d.MandateID), sql.Named("authority", d.AuthorityGeneration), sql.Named("owner", d.OwnerMemberID),
		sql.Named("binding", r.BindingRevision),
		sql.Named("coordinator", r.CoordinatorMemberID), sql.Named("policy", r.PolicyRevision)).Scan(&active, &blocked)
	if err == nil && blocked == "" {
		var pinned bool
		pinned, err = taskArtifactDependenciesBlockedInTx(ctx, tx, d.GroupID, d.TaskID)
		if pinned {
			blocked = string(CoordinationDependencyNotAccepted)
		}
	}
	return blocked, active, coordinationRoleRelationError(err)
}

func (s *Store) taskWorkReadinessInTx(ctx context.Context, tx *sql.Tx, f taskWorkFacts, turnID string) (blocked, active string, err error) {
	return s.taskWorkReadinessForOwnerInTx(ctx, tx, f, turnID, false)
}

func (s *Store) taskWorkReadinessForOwnerInTx(ctx context.Context, tx *sql.Tx, f taskWorkFacts, turnID string, returnOnly bool) (blocked, active string, err error) {
	d := f.d
	if d.SourceKind != domain.TaskWorkSourceFollowup {
		return "", "", ErrMemberToolDenied
	}
	if turnID == "" {
		handled, err := taskWorkInputHandledInTx(ctx, tx, d.SourceID)
		if err != nil {
			return "", "", err
		}
		if handled {
			return "followup_handled", "", nil
		}
	}
	if !s.taskWorkExecution.Load() {
		return "task_work_execution_disabled", "", nil
	}
	blocked, active, err = taskWorkCurrentInTx(ctx, tx, f)
	if err != nil || blocked != "" {
		return blocked, active, err
	}
	if !returnOnly || turnID == "" {
		_, err = s.taskWorkOwnerChatActionInTx(ctx, tx, f)
		switch {
		case errors.Is(err, errTaskWorkChatScope):
			return "chat_scope_unavailable", active, nil
		case errors.Is(err, ErrTaskInvestigationNotReady):
			return "investigation_scope_stale", active, nil
		case err != nil:
			return "", "", err
		}
	}
	var initialAttempt, initialTurn string
	err = tx.QueryRowContext(ctx, `SELECT attempt_id,turn_id FROM coordination_assignment_attempts WHERE assignment_id=?`, d.AssignmentID).Scan(&initialAttempt, &initialTurn)
	if errors.Is(err, sql.ErrNoRows) {
		// An original that has not bound its Attempt (including a queued
		// original) cannot be overtaken by a typed wake.
		return "original_attempt_unbound", active, nil
	}
	if err != nil {
		return "", "", err
	}
	if f.dispatch.Delivery == nil || initialTurn == turnID {
		return "", "", ErrMemberToolDenied
	}
	original := CoordinationRoleProvenance{Kind: CoordinationRoleWorker, AssignmentID: d.AssignmentID, GroupID: d.GroupID,
		TaskID: d.TaskID, TaskRevision: d.TaskRevision, MemberID: d.OwnerMemberID}
	bound, err := coordinationRoleAttemptInTx(ctx, tx, MemberToolScope{Claim: TurnClaim{TurnID: initialTurn}}, original)
	if err != nil || bound != initialAttempt {
		if err == nil {
			err = ErrMemberToolDenied
		}
		return "", "", err
	}
	own, err := taskWorkBoundAttemptInTx(ctx, tx, d, "")
	if err != nil {
		return "", "", err
	}
	if own.ID != "" && (own.ID != active || own.TurnID != turnID) {
		return "attempt_conflict", active, nil
	}
	if own.ID == "" && active != initialAttempt {
		if active == "" {
			return "attempt_conflict", active, nil
		}
		prior, err := taskWorkBoundAttemptInTx(ctx, tx, d, active)
		if err != nil {
			return "", "", err
		}
		if prior.ID == "" {
			return "attempt_conflict", active, nil
		}
	}
	var unsettled bool
	err = tx.QueryRowContext(ctx, taskWorkUnsettledTurns,
		sql.Named("grp", d.GroupID), sql.Named("task", d.TaskID), sql.Named("turn", turnID),
		sql.Named("coordinator", f.v.coordinatorTurn), sql.Named("original", initialTurn)).Scan(&unsettled)
	if err != nil {
		return "", "", err
	}
	if unsettled {
		return "prior_turn_unsettled", active, nil
	}
	return "", active, nil
}

var errTaskWorkChatScope = errors.New("原任务不属于可继续的文本范围")

func (s *Store) taskWorkOwnerChatActionInTx(ctx context.Context, tx *sql.Tx, f taskWorkFacts) (string, error) {
	d := f.d
	var action, messageID, sourceKind, sourceID string
	// Reuse the original admitted action and trusted source. Neither a model
	// proposal nor the current Task's text can select a new scope.
	err := tx.QueryRowContext(ctx, `SELECT coalesce(json_extract(a.receipt_json,'$.action'),''),c.message_id,e.source_kind,e.source_id
	 FROM chat_assessments a JOIN chat_candidates c ON c.id=a.candidate_id
	 JOIN mandates m ON m.id=:mandate AND m.decision_id=a.decision_id
	 JOIN coordination_triggers origin ON (origin.id,origin.group_id,origin.topic_id,origin.message_id,origin.message_sequence)=
	 (m.origin_trigger_id,c.group_id,c.topic_id,c.message_id,c.generation)
	 JOIN chat_intake_executions e ON e.candidate_id=c.id AND e.turn_id=c.turn_id AND e.purpose='chat_intake/v1'
	 WHERE a.dispatch_request_id=:request AND c.group_id=:grp AND c.topic_id=:topic
	 AND json_extract(a.receipt_json,'$.outcome')='admitted' AND json_type(a.receipt_json,'$.authority_evidence_verified') IS 'true'
	 AND json_extract(a.receipt_json,'$.task_id') IS :task AND json_extract(a.receipt_json,'$.mandate_id') IS :mandate
	 AND json_extract(a.receipt_json,'$.policy_revision') IS c.policy_revision
	 AND json_extract(a.receipt_json,'$.source_kind') IS e.source_kind AND json_extract(a.receipt_json,'$.source_id') IS e.source_id
	 AND NOT EXISTS(SELECT 1 FROM chat_workspace_scopes WHERE task_id=:task)`, sql.Named("request", f.v.r.ID),
		sql.Named("mandate", d.MandateID), sql.Named("grp", d.GroupID), sql.Named("topic", d.TopicID), sql.Named("task", d.TaskID)).Scan(&action, &messageID, &sourceKind, &sourceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errTaskWorkChatScope
	}
	if err != nil {
		return "", err
	}
	switch action {
	case domain.ChatActionTextSubmission:
		kind, id, err := messageSourceInTx(ctx, tx, messageID)
		if err != nil {
			return "", err
		}
		if sourceID == "" || id != sourceID || kind != sourceKind {
			return "", errTaskWorkChatScope
		}
		var current bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM chat_assessments a JOIN chat_candidates c ON c.id=a.candidate_id
		 JOIN chat_policies p ON (p.group_id,p.topic_id,p.revision)=(c.group_id,c.topic_id,c.policy_revision)
		 JOIN chat_policy_revisions r ON (r.group_id,r.topic_id,r.revision)=(p.group_id,p.topic_id,p.revision)
		 WHERE a.dispatch_request_id=? AND r.enabled=1)`, f.v.r.ID).Scan(&current)
		if err == nil && !current {
			err = errTaskWorkChatScope
		}
		return action, err
	case domain.ChatActionReadOnlyInvestigation:
		_, err := s.taskWorkInvestigationInTx(ctx, tx, f)
		return action, err
	default:
		return "", errTaskWorkChatScope
	}
}

func (s *Store) taskWorkInvestigationInTx(ctx context.Context, tx *sql.Tx, f taskWorkFacts) (taskInvestigationEvidence, error) {
	d := f.d
	e, err := taskInvestigationInTx(ctx, tx, d.GroupID, d.TaskID)
	if errors.Is(err, ErrNotFound) {
		return e, ErrTaskInvestigationNotReady
	}
	if err != nil {
		return e, err
	}
	if !e.matchesDispatch(f.v.r) {
		return e, ErrTaskInvestigationNotReady
	}
	return e, s.checkTaskInvestigationReadyInTx(ctx, tx, e, "", false)
}

// Include claims that never reached Attempt binding, starting from each raw
// marker so a missing/mismatched Turn cannot disappear from the history check.
// No recency window can hide an uncertain predecessor.
const taskWorkUnsettledTurns = `WITH related(id,valid) AS (
 SELECT :coordinator,1 UNION ALL SELECT :original,1
 UNION ALL SELECT a.turn_id,1 FROM task_attempts a WHERE a.group_id=:grp AND a.task_id=:task AND a.turn_id IS NOT NULL
 UNION ALL SELECT t.id,coalesce(w.source_kind='followup'
 AND (d.message_id,d.group_id,d.topic_id,d.conversation_id,d.recipient_id)=(w.message_id,w.group_id,w.topic_id,w.conversation_id,w.member_id)
 AND CASE WHEN d.turn_id='' THEN d.state='queued' AND t.id IS NULL
 ELSE t.id=d.turn_id AND (t.group_id,t.conversation_id,t.member_id)=(w.group_id,w.conversation_id,w.member_id)
 AND d.state=` + taskWorkClaimDeliveryState + ` END,0)
 FROM task_work_deliveries w INDEXED BY task_work_deliveries_task
 LEFT JOIN deliveries d ON d.id=w.delivery_id
 LEFT JOIN turns t INDEXED BY turns_coordination_delivery ON t.delivery_id=w.delivery_id
 WHERE w.group_id=:grp AND w.task_id=:task)
 SELECT EXISTS(SELECT 1 FROM related r LEFT JOIN turns h ON h.id=r.id AND h.group_id=:grp
 WHERE CASE
 WHEN NOT r.valid THEN 1
 WHEN r.id IS NULL OR r.id=:turn THEN 0
 WHEN h.state IN ('not_started','succeeded','failed','aborted','truncated')
 AND length(CAST(h.receipt_json AS BLOB))<=1048576 AND json_valid(h.receipt_json)
 AND NOT EXISTS(SELECT 1 FROM resource_claims WHERE turn_id=h.id) THEN NOT (
 json_extract(h.receipt_json,'$.outcome') IS h.state AND json_type(h.receipt_json,'$.delivery_failed') IS 'false'
 AND json_extract(h.receipt_json,'$.reason') IS h.reason AND h.reason<>''
 AND json_extract(h.receipt_json,'$.native_session_id') IS h.native_session_id
 AND CASE WHEN h.state='not_started' THEN h.accepted_at IS NULL AND h.native_session_id='' AND h.candidate_text='' AND h.candidate_terminal=''
 AND json_type(h.receipt_json,'$.terminal') IS NULL AND json_type(h.receipt_json,'$.started') IS 'false'
 AND json_type(h.receipt_json,'$.accepted') IS 'false' AND json_type(h.receipt_json,'$.direct_child_reaped') IS 'false'
 AND json_type(h.receipt_json,'$.output_complete') IS 'false'
 ELSE h.accepted_at IS NOT NULL AND h.native_session_id<>'' AND h.candidate_terminal IN ('',h.state)
 AND json_extract(h.receipt_json,'$.terminal') IS h.state AND json_type(h.receipt_json,'$.started') IS 'true'
 AND json_type(h.receipt_json,'$.accepted') IS 'true' AND json_type(h.receipt_json,'$.direct_child_reaped') IS 'true'
 AND json_type(h.receipt_json,'$.output_complete') IS 'true' END)
 ELSE 1 END)`

// Empty attemptID reads this delivery's binding; a nonempty ID reads a
// predecessor of the same Assignment/Task/scope. Raw absence isn't a binding.
func taskWorkBoundAttemptInTx(ctx context.Context, tx *sql.Tx, d domain.TaskWorkDelivery, attemptID string) (domain.TaskAttempt, error) {
	predicate, id := "delivery_id=?", d.DeliveryID
	if attemptID != "" {
		predicate, id = "attempt_id=?", attemptID
	}
	var boundID, turnID string
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT b.attempt_id,b.turn_id, EXISTS(SELECT 1`+taskWorkContinuationRelations+`
 WHERE w.delivery_id=b.delivery_id AND t.id=b.turn_id AND a.id=b.attempt_id
 AND (w.assignment_id,w.task_id,w.group_id,w.task_revision,w.owner_member_id,w.mandate_id,w.authority_generation,w.topic_id)=(?,?,?,?,?,?,?,?)
 AND NOT EXISTS(SELECT 1 FROM turns other WHERE other.delivery_id=w.delivery_id AND other.id<>t.id))
 FROM task_work_continuations b WHERE b.`+predicate,
		d.AssignmentID, d.TaskID, d.GroupID, d.TaskRevision, d.OwnerMemberID, d.MandateID, d.AuthorityGeneration, d.TopicID, id).Scan(&boundID, &turnID, &valid)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TaskAttempt{}, nil
	}
	if err != nil {
		return domain.TaskAttempt{}, err
	}
	if !valid {
		return domain.TaskAttempt{}, ErrMemberToolDenied
	}
	a, actor, err := taskAttemptInTx(ctx, tx, d.GroupID, d.TaskID, boundID)
	if err != nil {
		return domain.TaskAttempt{}, coordinationRoleRelationError(err)
	}
	if a.TaskRevision != d.TaskRevision || a.TurnID != turnID || actor != d.OwnerMemberID {
		return domain.TaskAttempt{}, ErrMemberToolDenied
	}
	return a, nil
}
