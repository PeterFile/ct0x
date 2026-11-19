package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"agentworkspace/internal/domain"
)

var ErrTaskInvestigationNotReady = errors.New("只读调查执行未启用或任务、授权、分派配置已过时")

type taskInvestigationEvidence struct {
	domain.TaskInvestigationScope
	dispatchRequestID string
	assessmentID      string
	scopeJSON         string
}

func (scope taskInvestigationEvidence) matchesDispatch(r domain.DispatchRequest) bool {
	return scope.GroupID == r.GroupID && scope.TopicID == r.TopicID && scope.TaskID == r.TaskID &&
		scope.TaskRevision == r.TaskRevision && scope.MandateID == r.MandateID && scope.dispatchRequestID == r.ID
}

// GetTaskInvestigationScope is a trusted historical lookup. ErrNotFound means
// no retained investigation admission; a changed or disabled current policy is not absence.
func (s *Store) GetTaskInvestigationScope(ctx context.Context, groupID, taskID string) (domain.TaskInvestigationScope, error) {
	evidence, err := s.readTaskInvestigation(ctx, groupID, taskID, false)
	return evidence.TaskInvestigationScope, err
}

// CheckTaskInvestigationReady rechecks startup opt-in and live authority, not a
// provider or claim. The runtime must also require its read-only role profile
// and apply the existing dispatch and claim fences before starting a Turn.
func (s *Store) CheckTaskInvestigationReady(ctx context.Context, groupID, taskID string) error {
	_, err := s.readTaskInvestigation(ctx, groupID, taskID, true)
	return err
}

func (s *Store) readTaskInvestigation(ctx context.Context, groupID, taskID string, requireReady bool) (taskInvestigationEvidence, error) {
	const action = "读取任务只读调查范围失败"
	if !validTriggerID(groupID) || !validTriggerID(taskID) {
		return taskInvestigationEvidence{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return taskInvestigationEvidence{}, admissionError(action, err)
	}
	defer tx.Rollback()
	if err := requireMessageActor(ctx, tx, groupID, "user"); err != nil {
		return taskInvestigationEvidence{}, admissionError(action, err)
	}
	evidence, err := taskInvestigationInTx(ctx, tx, groupID, taskID)
	if err == nil && requireReady {
		err = s.checkTaskInvestigationReadyInTx(ctx, tx, evidence, "", false)
	}
	if err != nil {
		return taskInvestigationEvidence{}, admissionError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return taskInvestigationEvidence{}, admissionError(action, err)
	}
	return evidence, nil
}

// The admitted action is immutable assessment evidence. Both config and source
// come from its bound revision, never from the current Policy pointer.
func taskInvestigationInTx(ctx context.Context, tx *sql.Tx, groupID, taskID string) (taskInvestigationEvidence, error) {
	supported, err := chatInvestigationSupportedInTx(ctx, tx)
	if err != nil {
		return taskInvestigationEvidence{}, err
	}
	if !supported {
		// The catalog can look legacy even though this Task retained its
		// investigation admission. That receipt proves restriction, not readiness.
		var admitted bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		 SELECT 1 FROM main.mandates m INDEXED BY mandates_group_task
		 CROSS JOIN main.chat_assessments a ON a.decision_id=m.decision_id
		 WHERE m.group_id=? AND m.task_id=? AND json_extract(a.receipt_json,'$.action')=?)`,
			groupID, taskID, domain.ChatActionReadOnlyInvestigation).Scan(&admitted); err != nil {
			return taskInvestigationEvidence{}, err
		}
		if admitted {
			return taskInvestigationEvidence{}, ErrTaskInvestigationNotReady
		}
		return taskInvestigationEvidence{}, ErrNotFound
	}
	var out taskInvestigationEvidence
	var revision sql.NullInt64
	var source, data sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT m.group_id,m.topic_id,m.task_id,m.task_revision_at_admission,m.id,
	 c.policy_revision,s.command_source_id,i.scope_json,a.dispatch_request_id,a.id
	 FROM mandates m INDEXED BY mandates_group_task JOIN chat_assessments a ON a.decision_id=m.decision_id
	 LEFT JOIN chat_candidates c ON c.id=a.candidate_id AND c.group_id=m.group_id AND c.topic_id=m.topic_id
	 LEFT JOIN chat_policy_scopes s ON s.group_id=m.group_id AND s.topic_id=m.topic_id AND s.revision=c.policy_revision
	 LEFT JOIN chat_investigation_policy_revisions i ON i.group_id=s.group_id AND i.topic_id=s.topic_id AND i.revision=s.revision
	 WHERE m.group_id=? AND m.task_id=? AND json_extract(a.receipt_json,'$.action')=?`, groupID, taskID, domain.ChatActionReadOnlyInvestigation).Scan(
		&out.GroupID, &out.TopicID, &out.TaskID, &out.TaskRevision, &out.MandateID, &revision, &source, &data,
		&out.dispatchRequestID, &out.assessmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return taskInvestigationEvidence{}, ErrNotFound
	}
	if err != nil {
		return taskInvestigationEvidence{}, err
	}
	// Damaged investigation evidence is not the absence of an investigation.
	if !revision.Valid || !source.Valid || source.String == "" || !data.Valid {
		return taskInvestigationEvidence{}, ErrTaskInvestigationNotReady
	}
	out.PolicyRevision, out.ConfigurationSourceID, out.scopeJSON = revision.Int64, source.String, data.String
	if err := json.Unmarshal([]byte(out.scopeJSON), &out.ChatInvestigationPolicy); err != nil {
		return taskInvestigationEvidence{}, err
	}
	if err := validateChatInvestigationPolicy(out.ChatInvestigationPolicy); err != nil {
		return taskInvestigationEvidence{}, err
	}
	return out, nil
}

// A new result uses the same live investigation fence as a read, after the
// shared submit helper has reserved the key and checked the Task and same Turn.
func (s *Store) chatInvestigationSubmissionInTx(ctx context.Context, tx *sql.Tx, input SubmitTaskInput) (taskInvestigationEvidence, error) {
	if len(input.ArtifactVersions) != 0 || !memberToolTextValid(input.Summary, investigationSubmissionMaxBytes) {
		return taskInvestigationEvidence{}, fmt.Errorf("%w：只读调查仅允许非空且最多%d字节的有效UTF-8文本，不允许附带成果版本", ErrInvalid, investigationSubmissionMaxBytes)
	}
	p, work, err := investigationSubmissionProvenanceInTx(ctx, tx, input)
	if err != nil {
		return taskInvestigationEvidence{}, err
	}
	if work.d.DeliveryID != "" {
		return s.investigationForFollowupInTx(ctx, tx, work, *input.MemberScope, input.AttemptID)
	}
	return s.investigationForProvenanceInTx(ctx, tx, *input.MemberScope, p, input.AttemptID)
}

func investigationSubmissionProvenanceInTx(ctx context.Context, tx *sql.Tx, input SubmitTaskInput) (p CoordinationRoleProvenance, work taskWorkFacts, err error) {
	if input.MemberScope == nil {
		return p, work, ErrMemberToolDenied
	}
	member := *input.MemberScope
	if err := coordinationDecideLiveInTx(ctx, tx, member); err != nil {
		return p, work, err
	}
	work, err = lookupTaskWorkExecutionInTx(ctx, tx, member)
	if err == nil {
		if !work.d.SourceKind.IsOwnerContinuation() {
			return p, work, ErrMemberToolDenied
		}
		p = taskWorkOwnerProvenance(work)
	} else if errors.Is(err, ErrNotFound) {
		p, err = coordinationClaimProvenanceInTx(ctx, tx, member)
	}
	if err != nil {
		return p, work, err
	}
	if p.Kind != CoordinationRoleWorker || p.GroupID != input.GroupID || p.TaskID != input.TaskID || p.TaskRevision != input.ExpectedRevision || p.MemberID != input.ActorID {
		return p, work, ErrMemberToolDenied
	}
	return p, work, nil
}

// Both submission entries recheck after the Task and any notice were written.
// The bound worker must still own the exact resulting Task pointer.
func (s *Store) checkInvestigationSubmissionWriteInTx(ctx context.Context, tx *sql.Tx, evidence taskInvestigationEvidence, input SubmitTaskInput, submissionID string) error {
	p, work, err := investigationSubmissionProvenanceInTx(ctx, tx, input)
	if err != nil {
		return err
	}
	if work.d.DeliveryID != "" {
		attempt, err := taskWorkBoundAttemptInTx(ctx, tx, work.d, "")
		if err != nil {
			return err
		}
		if !s.taskWorkExecution.Load() || !evidence.matchesDispatch(work.v.r) || attempt.ID == "" || attempt.ID != input.AttemptID || attempt.TurnID != input.MemberScope.Claim.TurnID {
			return ErrMemberToolDenied
		}
		task, err := taskInTx(ctx, tx, p.GroupID, p.TaskID)
		if err != nil {
			return err
		}
		if task.ActiveAttemptID != attempt.ID || (task.AssigneeID != "" && task.AssigneeID != p.MemberID) {
			return ErrMemberToolDenied
		}
		return investigationToolError(s.checkTaskInvestigationReadyInTx(ctx, tx, evidence, submissionID, false))
	}
	role := CoordinationToolScope{Member: *input.MemberScope, Provenance: p, AttemptID: input.AttemptID}
	v, view, err := coordinationAssignmentWorkerInTx(ctx, tx, role)
	if err != nil {
		return err
	}
	if !evidence.matchesDispatch(v.r) || !coordinationAssignmentBound(role, view.Binding) || view.Task.ActiveAttemptID != input.AttemptID ||
		(view.Task.AssigneeID != "" && view.Task.AssigneeID != p.MemberID) {
		return ErrMemberToolDenied
	}
	return investigationToolError(s.checkTaskInvestigationReadyInTx(ctx, tx, evidence, submissionID, false))
}

// Only a genuine absence falls through to generic work. Scope does not itself
// prove a worker capability. Only a new coordinator decline may allow no worker.
func (s *Store) investigationDispatchInTx(ctx context.Context, tx *sql.Tx, r domain.DispatchRequest, allowNoEligibleWorker bool) (bool, error) {
	evidence, err := taskInvestigationInTx(ctx, tx, r.GroupID, r.TaskID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, investigationToolError(err)
	}
	if !evidence.matchesDispatch(r) {
		return false, ErrMemberToolDenied
	}
	return true, investigationToolError(s.checkTaskInvestigationReadyInTx(ctx, tx, evidence, "", allowNoEligibleWorker))
}

func investigationToolError(err error) error {
	if errors.Is(err, ErrTaskInvestigationNotReady) {
		return ErrMemberToolDenied
	}
	return err
}

// submissionID is only set for the read-back of a fresh, exact submission.
func (s *Store) checkTaskInvestigationReadyInTx(ctx context.Context, tx *sql.Tx, scope taskInvestigationEvidence, submissionID string, allowNoEligibleWorker bool) error {
	if !s.investigationExecution.Load() {
		return ErrTaskInvestigationNotReady
	}
	return checkTaskInvestigationAuthorizationInTx(ctx, tx, scope, submissionID, allowNoEligibleWorker)
}

// This is the durable half of the execution fence. It can explain a handback,
// but by itself never authorizes investigation execution or tools.
func checkTaskInvestigationAuthorizationInTx(ctx context.Context, tx *sql.Tx, scope taskInvestigationEvidence, submissionID string, allowNoEligibleWorker bool) error {
	status := domain.TaskStatusOpen
	if submissionID != "" {
		status = domain.TaskStatusSubmitted
	}
	c := domain.ChatCandidate{ThreadRef: scope.ThreadRef}
	var coordinator, taskAssignee, mandateAssignee string
	var dispatchPolicy int64
	err := tx.QueryRowContext(ctx, `SELECT r.coordinator_member_id,r.policy_revision,coalesce(t.assignee_id,''),m.assignee_id,
	 c.id,c.message_id,c.member_id,c.turn_id,c.generation,e.source_kind,e.source_id
	 FROM coordination_dispatch_requests r JOIN tasks t ON t.id=r.task_id AND t.group_id=r.group_id
	 JOIN groups g ON g.id=r.group_id
	 JOIN task_coordination tc ON tc.task_id=t.id AND tc.group_id=t.group_id AND tc.topic_id=r.topic_id
	 JOIN mandates m ON m.id=r.mandate_id AND m.task_id=t.id AND m.group_id=t.group_id AND m.topic_id=r.topic_id AND m.task_revision_at_admission=r.task_revision
	 JOIN group_coordinator_bindings cb ON cb.group_id=r.group_id AND cb.revision=r.binding_revision
	 JOIN group_dispatch_policies dp ON dp.group_id=r.group_id AND dp.revision=r.policy_revision
	 JOIN chat_assessments a ON a.decision_id=m.decision_id AND a.dispatch_request_id=r.id
	 JOIN chat_candidates c ON c.id=a.candidate_id AND c.group_id=r.group_id AND c.topic_id=r.topic_id
	  AND c.binding_revision=r.binding_revision AND c.dispatch_policy_revision=r.policy_revision
	 JOIN chat_intake_executions e ON e.candidate_id=c.id AND e.turn_id=c.turn_id AND e.purpose=?
	  AND typeof(e.claim_digest)='blob' AND length(e.claim_digest)=32 AND e.source_kind IN ('local_user_chat','member')
	 JOIN turns rt ON rt.id=c.turn_id
	 JOIN deliveries rd ON rd.id=rt.delivery_id
	 JOIN coordination_triggers origin ON origin.id=m.origin_trigger_id AND origin.message_id=c.message_id
	  AND origin.group_id=r.group_id AND origin.topic_id=r.topic_id AND origin.message_sequence=c.generation
	 JOIN chat_policies p ON p.group_id=c.group_id AND p.topic_id=c.topic_id AND p.revision=c.policy_revision
	 JOIN chat_policy_revisions pr ON pr.group_id=p.group_id AND pr.topic_id=p.topic_id AND pr.revision=p.revision
	 JOIN chat_policy_scopes ps ON ps.group_id=p.group_id AND ps.topic_id=p.topic_id AND ps.revision=p.revision
	 JOIN chat_investigation_policy_revisions i ON i.group_id=p.group_id AND i.topic_id=p.topic_id AND i.revision=p.revision
	 JOIN message_command_sources cmd ON cmd.id=ps.command_source_id AND cmd.kind='local_user_policy'
	 JOIN messages config ON config.id=cmd.message_id AND config.group_id=p.group_id AND config.topic_id=p.topic_id AND config.sender_id='user'
	 WHERE r.id=? AND t.group_id=? AND t.id=? AND t.status=? AND t.current_submission_id IS nullif(?,'')
	 AND r.topic_id=? AND r.mandate_id=? AND tc.current_mandate_id=r.mandate_id AND tc.authority_generation=r.authority_generation
	 AND r.task_revision=? AND t.revision=r.task_revision AND pr.enabled=1 AND pr.authorized_by='user'
	 AND a.id=? AND p.revision=? AND cmd.id=? AND i.scope_json=?
	 AND `+chatDiscussionReceiveMatches+`
	 AND json_extract(a.receipt_json,'$.action')=? AND json_extract(a.receipt_json,'$.policy_revision') IS c.policy_revision
	 AND json_extract(a.receipt_json,'$.authorization_message_id') IS cmd.message_id
	 AND json_extract(a.receipt_json,'$.source_kind') IS e.source_kind AND json_extract(a.receipt_json,'$.source_id') IS e.source_id`,
		domain.ChatIntakePurpose, scope.dispatchRequestID, scope.GroupID, scope.TaskID, status, submissionID, scope.TopicID, scope.MandateID,
		scope.TaskRevision, scope.assessmentID, scope.PolicyRevision, scope.ConfigurationSourceID, scope.scopeJSON, domain.ChatActionReadOnlyInvestigation).Scan(
		&coordinator, &dispatchPolicy, &taskAssignee, &mandateAssignee, &c.ID, &c.MessageID, &c.MemberID, &c.TurnID, &c.Generation, &c.SourceKind, &c.SourceID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTaskInvestigationNotReady
	}
	if err != nil {
		return err
	}
	kind, id, err := messageSourceInTx(ctx, tx, c.MessageID)
	if err != nil {
		return err
	}
	if c.SourceID == "" || kind != c.SourceKind || id != c.SourceID {
		return ErrTaskInvestigationNotReady
	}
	// A competing intake may have recorded a same-source limit after admission.
	if err := chatDiscussionSourceGateInTx(ctx, tx, c, false); err != nil {
		var limit *chatNonAdmission
		if errors.As(err, &limit) || errors.Is(err, ErrMemberToolDenied) || errors.Is(err, ErrInvalid) {
			return ErrTaskInvestigationNotReady
		}
		return err
	}
	if err := chatInvestigationVersionsInTx(ctx, tx, scope.GroupID, scope.ArtifactVersions); err != nil {
		if errors.Is(err, ErrNotFound) {
			err = ErrTaskInvestigationNotReady
		}
		return err
	}
	active, eligible, err := coordinationDispatchMembersInTx(ctx, tx, scope.GroupID, coordinator, dispatchPolicy, taskAssignee, mandateAssignee)
	if err == nil && (!active || (!eligible && !allowNoEligibleWorker)) {
		return ErrTaskInvestigationNotReady
	}
	return err
}
