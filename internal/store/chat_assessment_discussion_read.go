package store

import (
	"context"
	"database/sql"
	"errors"

	"agentworkspace/internal/domain"
)

type chatDiscussionWorkAdmission struct {
	TaskID, MandateID, DispatchRequestID string
	Assignment                           *domain.Assignment
}

type chatDiscussionWorkArrival struct {
	kind, originID, actorID, recipientID, sourceID                      string
	generation, policyRevision, bindingRevision, dispatchPolicyRevision int64
}

// Match the recipient's capture, not the candidate that published the event.
// This proves arrival identity, not live authorization: ApplyChatAssessment
// separately validates the live scope and captured claim in the same transaction.
// Do not use the discussion context/closure projection, which may read this proof.
func chatDiscussionWorkArrivalInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, messageID, candidateID, turnID string) (a chatDiscussionWorkArrival, err error) {
	var validCaptureDigest bool
	err = tx.QueryRowContext(ctx, `SELECT e.kind,e.origin_message_id,e.actor_id,e.recipient_id,i.source_id,
	 c.generation,coalesce(c.policy_revision,0),c.binding_revision,c.dispatch_policy_revision,
	 typeof(i.claim_digest)='blob' AND length(i.claim_digest)=32
	 FROM chat_discussion_events e
	 JOIN chat_discussions chain ON (chain.origin_message_id,chain.group_id,chain.topic_id)=(e.origin_message_id,e.group_id,e.topic_id)
	 JOIN messages o ON (o.id,o.group_id,o.topic_id)=(chain.origin_message_id,chain.group_id,chain.topic_id) AND o.sender_id='user'
	 JOIN message_command_sources os ON os.message_id=o.id AND os.kind='local_user_chat'
	 JOIN chat_discussion_events root ON (root.origin_message_id,root.group_id,root.topic_id)=(e.origin_message_id,e.group_id,e.topic_id)
	    AND root.step=1 AND root.kind='invite' AND root.source_message_id=o.id AND root.actor_id=chain.root_inviter_id
	 JOIN messages m ON (m.id,m.group_id,m.topic_id,m.sender_id)=(e.message_id,e.group_id,e.topic_id,e.actor_id)
	 JOIN deliveries d ON (d.id,d.message_id,d.group_id,d.topic_id,d.recipient_id)=(e.delivery_id,m.id,e.group_id,e.topic_id,e.recipient_id)
	 JOIN conversations v ON (v.id,v.group_id,v.topic_id,v.member_id)=(d.conversation_id,d.group_id,d.topic_id,d.recipient_id)
	 JOIN turns t ON (t.id,t.delivery_id,t.group_id,t.conversation_id,t.member_id)=(d.turn_id,d.id,d.group_id,d.conversation_id,d.recipient_id)
	 JOIN chat_candidates c ON (c.turn_id,c.group_id,c.topic_id,c.member_id,c.message_id)=(t.id,d.group_id,d.topic_id,d.recipient_id,m.id)
	 JOIN message_log l ON (l.message_id,l.group_id,l.topic_id,l.sequence)=(m.id,c.group_id,c.topic_id,c.generation)
	 JOIN chat_intake_executions i ON i.candidate_id=c.id AND i.turn_id=t.id AND i.purpose=? AND i.source_kind='member'
	 JOIN message_command_sources s ON s.message_id=m.id AND s.id=i.source_id AND s.kind=i.source_kind
	 JOIN chat_candidates publisher ON (publisher.id,publisher.turn_id,publisher.group_id,publisher.topic_id,publisher.member_id,publisher.message_id)
	    =(e.candidate_id,s.turn_id,e.group_id,e.topic_id,e.actor_id,e.source_message_id)
	 JOIN chat_intake_executions published ON published.candidate_id=publisher.id AND published.turn_id=publisher.turn_id
	    AND published.purpose=i.purpose AND published.claim_digest=s.claim_digest
	    AND typeof(s.claim_digest)='blob' AND length(s.claim_digest)=32
	 JOIN chat_assessments assessed ON assessed.candidate_id=publisher.id
	 WHERE e.group_id=? AND e.topic_id=? AND e.message_id=? AND c.id=? AND t.id=?
	 AND e.kind IN ('invite','reply') AND e.actor_id<>e.recipient_id
	 AND (e.message_id=root.message_id OR EXISTS(SELECT 1 FROM chat_discussion_events previous
	    WHERE (previous.origin_message_id,previous.group_id,previous.topic_id)=(e.origin_message_id,e.group_id,e.topic_id)
	    AND previous.step=e.step-1 AND previous.message_id=e.source_message_id
	    AND previous.recipient_id=e.actor_id AND previous.delivery_id IS NOT NULL
	    AND (e.kind='reply' OR (previous.kind='invite' AND previous.message_id=e.related_invitation_id))))
	 AND NOT EXISTS(SELECT 1 FROM deliveries extra WHERE extra.message_id=m.id AND extra.id<>d.id)
	 AND NOT EXISTS(SELECT 1 FROM turns extra WHERE extra.delivery_id=d.id AND extra.id<>t.id)
	 AND NOT EXISTS(SELECT 1 FROM coordination_dispatch_state WHERE delivery_id=d.id OR message_id=m.id)
	 AND NOT EXISTS(SELECT 1 FROM coordination_assignment_dispatch WHERE delivery_id=d.id OR message_id=m.id)`,
		domain.ChatIntakePurpose, thread.GroupID, thread.TopicID, messageID, candidateID, turnID).Scan(
		&a.kind, &a.originID, &a.actorID, &a.recipientID, &a.sourceID,
		&a.generation, &a.policyRevision, &a.bindingRevision, &a.dispatchPolicyRevision, &validCaptureDigest)
	if err == nil && !validCaptureDigest {
		err = ErrInvalid
	}
	return a, err
}

// chatDiscussionWorkAdmissionInTx reads a committed admission, even before
// dispatch materializes or the recipient Turn settles. The immutable assessment
// is evidence of the original write's claim check; recovery rotates the live
// token without changing that history. Nil without error means no proved work
// transition for this Turn. Only a separately proved Assignment is returned.
func chatDiscussionWorkAdmissionInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, invitationMessageID, candidateID, turnID string) (*chatDiscussionWorkAdmission, error) {
	a, err := chatDiscussionWorkArrivalInTx(ctx, tx, thread, invitationMessageID, candidateID, turnID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil || a.kind != domain.ChatDiscussionInvite {
		return nil, err
	}
	var id, decisionID, dispatchID, noticeID string
	err = tx.QueryRowContext(ctx, `SELECT a.id,a.decision_id,a.dispatch_request_id,a.notice_message_id FROM chat_assessments a
	 JOIN messages n ON n.id=a.notice_message_id AND n.group_id=? AND n.topic_id=? AND n.sender_id=?
	 WHERE a.candidate_id=? AND a.decision_id IS NOT NULL AND a.dispatch_request_id IS NOT NULL
	 AND NOT EXISTS(SELECT 1 FROM deliveries WHERE message_id=n.id)`, thread.GroupID, thread.TopicID, a.recipientID, candidateID).
		Scan(&id, &decisionID, &dispatchID, &noticeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r, err := chatAssessmentReceiptInTx(ctx, tx, id, candidateID)
	if err != nil {
		return nil, err
	}
	if r.ID != id || r.CandidateID != candidateID || r.TurnID != turnID || r.ThreadRef != thread || r.MessageID != invitationMessageID ||
		r.SourceKind != "member" || r.SourceID != a.sourceID || r.SourceSenderID != a.actorID || r.AssessedBy != a.recipientID ||
		r.CandidateGeneration != a.generation || r.PolicyRevision != a.policyRevision || r.Purpose != domain.ChatIntakePurpose ||
		r.NoticeMessageID != noticeID || r.DispatchRequestID != dispatchID || r.Outcome != "admitted" || r.Disposition != "prepare_work" ||
		r.Authority != "written_preauthorization" || !r.AuthorityEvidenceVerified || r.DiscussionOnly || r.DiscussionOnlyExcerpt != "" {
		return nil, ErrInvalid
	}
	if err := chatDiscussionWorkPolicyProofInTx(ctx, tx, r); err != nil {
		return nil, err
	}
	admission, err := admissionReceiptInTx(ctx, tx, thread.GroupID, decisionID)
	if err != nil {
		return nil, err
	}
	d, m := admission.Decision, admission.Mandate
	if d.Kind != domain.RoutingDecisionCreateNew || d.TopicID != thread.TopicID || m.TopicID != d.TopicID ||
		d.TaskID != r.TaskID || m.TaskID != d.TaskID || m.ID != r.MandateID || m.OriginTriggerID != d.TriggerID ||
		d.TaskRevisionAtAdmission != m.TaskRevisionAtAdmission {
		return nil, ErrInvalid
	}
	// Prove stable Task identity without requiring its live revision or status.
	var proved int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM coordination_triggers t JOIN tasks k ON k.id=? AND k.group_id=t.group_id
	 WHERE t.id=? AND t.source_kind=? AND t.group_id=? AND t.topic_id=? AND t.message_id=? AND t.message_sequence=?`,
		d.TaskID, d.TriggerID, domain.TriggerSourceLocalIM, thread.GroupID, thread.TopicID, invitationMessageID, a.generation).Scan(&proved); err != nil {
		return nil, err
	}
	dispatch, err := coordinationDispatchRequestInTx(ctx, tx, thread.GroupID, dispatchID)
	if err != nil {
		return nil, err
	}
	if dispatch.TopicID != thread.TopicID || dispatch.TaskID != d.TaskID || dispatch.MandateID != m.ID ||
		dispatch.TaskRevision != d.TaskRevisionAtAdmission || dispatch.AuthorityGeneration != 1 ||
		dispatch.BindingRevision != a.bindingRevision || dispatch.PolicyRevision != a.dispatchPolicyRevision {
		return nil, ErrInvalid
	}
	assignment, err := chatDiscussionWorkAssignmentInTx(ctx, tx, dispatch)
	if err != nil {
		return nil, err
	}
	return &chatDiscussionWorkAdmission{TaskID: d.TaskID, MandateID: m.ID, DispatchRequestID: dispatch.ID, Assignment: assignment}, nil
}

func chatDiscussionWorkPolicyProofInTx(ctx context.Context, tx *sql.Tx, r domain.ChatAssessmentReceipt) error {
	// A member handoff can only prepare work under the bound written policy.
	// Read its immutable identity, not current eligibility or a policy view.
	query := `SELECT 1 FROM chat_policy_revisions p JOIN chat_policy_scopes s USING(group_id,topic_id,revision)
	 JOIN message_command_sources src ON src.id=s.command_source_id AND src.kind='local_user_policy'
	 JOIN messages m ON (m.id,m.group_id,m.topic_id)=(src.message_id,p.group_id,p.topic_id) AND m.sender_id='user'
	 WHERE p.group_id=? AND p.topic_id=? AND p.revision=? AND p.enabled=1 AND m.id=?`
	switch r.Action {
	case domain.ChatActionTextSubmission:
		query += ` AND s.scope_kind='topic_summary' AND p.allow_preparation=1 AND p.preauthorization_message_id=m.id`
	case domain.ChatActionReadOnlyInvestigation:
		query += ` AND EXISTS(SELECT 1 FROM chat_investigation_policy_revisions i
		 WHERE (i.group_id,i.topic_id,i.revision)=(p.group_id,p.topic_id,p.revision))`
	default:
		return ErrInvalid
	}
	var proved int
	return tx.QueryRowContext(ctx, query, r.GroupID, r.TopicID, r.PolicyRevision, r.AuthorizationMessageID).Scan(&proved)
}

func chatDiscussionWorkAssignmentInTx(ctx context.Context, tx *sql.Tx, r domain.DispatchRequest) (*domain.Assignment, error) {
	var status domain.DispatchStatus
	var resolved bool
	if err := tx.QueryRowContext(ctx, `SELECT s.status, s.decision_id IS NOT NULL
	 OR EXISTS(SELECT 1 FROM coordination_decisions WHERE request_id=s.request_id)
	 OR EXISTS(SELECT 1 FROM coordination_assignments WHERE request_id=s.request_id)
	 FROM coordination_dispatch_state s WHERE s.request_id=? AND s.group_id=? AND s.task_id=?`, r.ID, r.GroupID, r.TaskID).
		Scan(&status, &resolved); err != nil {
		return nil, err
	}
	switch status {
	case domain.DispatchStatusPending, domain.DispatchStatusEnqueued, domain.DispatchStatusHeld:
		if resolved {
			return nil, ErrInvalid
		}
		return nil, nil
	case domain.DispatchStatusAssigned, domain.DispatchStatusDeclined:
		decision, err := coordinationDecideReceiptInTx(ctx, tx, r.GroupID, r.ID)
		if err != nil || decision.Assignment == nil {
			return nil, err
		}
		a := decision.Assignment
		var bound int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM coordination_assignment_dispatch
		 WHERE assignment_id=? AND group_id=? AND task_id=? AND worker_member_id=?`, a.ID, r.GroupID, r.TaskID, a.WorkerMemberID).Scan(&bound); err != nil {
			return nil, err
		}
		return a, nil
	default:
		return nil, ErrInvalid
	}
}
