package store

import (
	"context"
	"database/sql"

	"agentworkspace/internal/domain"
)

// GetChatRepairSource reads official Review context, not current execution
// authority. Live intake must first fence its scope and use the same-tx helper.
func (s *Store) GetChatRepairSource(ctx context.Context, thread domain.ThreadRef, reviewID string) (domain.ChatRepairSource, error) {
	if err := validateTriggerThread(thread); err != nil {
		return domain.ChatRepairSource{}, err
	}
	if !validTriggerID(reviewID) {
		return domain.ChatRepairSource{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.ChatRepairSource{}, admissionError("读取修复来源失败", err)
	}
	defer tx.Rollback()
	if err := requireMessageActor(ctx, tx, thread.GroupID, "user"); err != nil {
		return domain.ChatRepairSource{}, admissionError("验证修复来源范围失败", err)
	}
	source, err := chatRepairSourceInTx(ctx, tx, thread, reviewID)
	if err != nil {
		return domain.ChatRepairSource{}, admissionError("读取修复来源失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.ChatRepairSource{}, admissionError("读取修复来源失败", err)
	}
	return source, nil
}

// Callers supply trusted identities, not notice text or model-selected scope.
// The source is eligible context only: unknown execution and resource claims
// are deliberately not reconciled, released or converted to permissions here.
func chatRepairSourceInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, reviewID string) (domain.ChatRepairSource, error) {
	return chatRepairSourceEvidenceInTx(ctx, tx, thread, reviewID, true)
}

// Historical context remains available for discussion after supersession; only
// admission requires the untouched post-rejection Task generation.
func chatRepairSourceEvidenceInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, reviewID string, requireCurrent bool) (domain.ChatRepairSource, error) {
	var taskID, decisionID, assessmentID string
	var c domain.ChatCandidate
	c.ThreadRef = thread
	err := tx.QueryRowContext(ctx, `SELECT r.task_id,m.decision_id,a.id,c.id,c.message_id,c.member_id,c.turn_id,
	 c.generation,coalesce(c.policy_revision,0),e.purpose,e.source_id,e.source_kind
	 FROM task_review_decisions r
	 JOIN mandates m INDEXED BY mandates_group_task ON m.group_id=r.group_id AND m.task_id=r.task_id
	 JOIN routing_decisions d ON d.id=m.decision_id AND d.mandate_id=m.id AND d.trigger_id=m.origin_trigger_id
	 AND d.group_id=m.group_id AND d.topic_id=m.topic_id AND d.task_id=m.task_id
	 AND d.task_revision_at_admission=m.task_revision_at_admission
	 JOIN chat_assessments a ON a.decision_id=d.id
	 JOIN chat_candidates c ON c.id=a.candidate_id AND c.group_id=m.group_id AND c.topic_id=m.topic_id
	 JOIN chat_intake_executions e ON e.candidate_id=c.id AND e.turn_id=c.turn_id
	 WHERE r.id=? AND r.group_id=? AND m.topic_id=?`, reviewID, thread.GroupID, thread.TopicID).Scan(
		&taskID, &decisionID, &assessmentID, &c.ID, &c.MessageID, &c.MemberID, &c.TurnID,
		&c.Generation, &c.Policy.Revision, &c.Purpose, &c.SourceID, &c.SourceKind)
	if err != nil {
		return domain.ChatRepairSource{}, err
	}
	review, err := taskReviewInTx(ctx, tx, thread.GroupID, taskID, reviewID)
	if err != nil {
		return domain.ChatRepairSource{}, err
	}
	if review.ReviewedBy != "user" || review.Outcome != domain.ReviewOutcomeRejected {
		return domain.ChatRepairSource{}, ErrConflict
	}
	task, err := taskInTx(ctx, tx, thread.GroupID, taskID)
	if err != nil {
		return domain.ChatRepairSource{}, err
	}
	// Subtraction avoids overflowing an exhausted reviewed revision.
	if requireCurrent && (task.Status != domain.TaskStatusOpen || task.Revision-1 != review.TaskRevision || task.ActiveAttemptID != "" || task.CurrentSubmissionID != "") {
		return domain.ChatRepairSource{}, ErrConflict
	}
	submission, err := taskSubmissionInTx(ctx, tx, thread.GroupID, taskID, review.SubmissionID)
	if err != nil {
		return domain.ChatRepairSource{}, err
	}
	if submission.TaskRevision != review.TaskRevision {
		return domain.ChatRepairSource{}, ErrConflict
	}
	admission, err := admissionReceiptInTx(ctx, tx, thread.GroupID, decisionID)
	if err != nil {
		return domain.ChatRepairSource{}, err
	}
	parent, err := chatAssessmentReceiptInTx(ctx, tx, assessmentID, c.ID)
	if err != nil {
		return domain.ChatRepairSource{}, err
	}
	if parent.ID != assessmentID || parent.CandidateID != c.ID || parent.ThreadRef != thread || parent.TaskID != taskID ||
		parent.MandateID != admission.Mandate.ID || parent.MessageID != c.MessageID || parent.AssessedBy != c.MemberID ||
		parent.TurnID != c.TurnID || parent.CandidateGeneration != c.Generation || parent.PolicyRevision != c.Policy.Revision ||
		parent.Purpose != domain.ChatIntakePurpose || parent.Purpose != c.Purpose || parent.SourceID == "" ||
		parent.SourceID != c.SourceID || parent.SourceKind != c.SourceKind || !parent.AuthorityEvidenceVerified || parent.Outcome != "admitted" {
		return domain.ChatRepairSource{}, ErrConflict
	}
	kind, id, err := messageSourceInTx(ctx, tx, c.MessageID)
	if err != nil {
		return domain.ChatRepairSource{}, err
	}
	if kind != c.SourceKind || id != c.SourceID {
		return domain.ChatRepairSource{}, ErrConflict
	}
	policy, err := chatPolicyInTx(ctx, tx, thread, c.Policy.Revision)
	if err != nil {
		return domain.ChatRepairSource{}, err
	}
	proof, err := chatRepairOriginalAuthorizationInTx(ctx, tx, c, parent, policy)
	if err != nil {
		return domain.ChatRepairSource{}, err
	}
	return domain.ChatRepairSource{
		ThreadRef: thread, ParentTaskID: taskID, ParentSubmissionID: submission.ID, ParentReviewID: review.ID,
		ReviewedTaskRevision: review.TaskRevision, ParentMandate: admission.Mandate, ParentAdmission: parent,
		ParentSubmissionSummary: submission.Summary, ReviewReason: review.Reason,
		ArtifactVersions: submission.ArtifactVersions, OriginalAuthorization: proof,
	}, nil
}

// The original command, not the later Review notice, anchors inherited consent.
// Any newer real user chat requires a fresh semantic request; member discussion
// cannot withdraw or renew user authority.
func chatRepairOriginalAgreementCurrentInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate, proof domain.ChatRepairAuthorization) (bool, error) {
	var generation int64
	var err error
	switch proof.SourceKind {
	case "local_user_chat", "local_user_policy":
		err = tx.QueryRowContext(ctx, `SELECT l.sequence FROM message_log l
		 JOIN message_command_sources s ON s.message_id=l.message_id
		 WHERE l.group_id=? AND l.topic_id=? AND l.message_id=? AND s.kind=? AND s.id=?`,
			c.GroupID, c.TopicID, proof.MessageID, proof.SourceKind, proof.SourceID).Scan(&generation)
	case "local_user_review":
		err = tx.QueryRowContext(ctx, `SELECT l.sequence FROM message_log l
		 JOIN chat_review_notices rn ON rn.message_id=l.message_id
		 JOIN task_review_decisions r ON r.id=rn.review_id AND r.group_id=l.group_id
		 WHERE l.group_id=? AND l.topic_id=? AND r.id=? AND r.reviewed_by='user' AND r.outcome='rejected'`,
			c.GroupID, c.TopicID, proof.SourceID).Scan(&generation)
	default:
		return false, ErrConflict
	}
	if err != nil {
		return false, err
	}
	var superseded bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
	 SELECT 1 FROM message_log l JOIN message_command_sources s ON s.message_id=l.message_id AND s.kind='local_user_chat'
	 WHERE l.group_id=? AND l.topic_id=? AND l.sequence>?)`,
		c.GroupID, c.TopicID, generation).Scan(&superseded); err != nil {
		return false, err
	}
	return generation > 0 && generation <= c.Generation && !superseded, nil
}

func chatRepairOriginalAuthorizationInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate, parent domain.ChatAssessmentReceipt, policy domain.ChatPolicy) (domain.ChatRepairAuthorization, error) {
	proof := domain.ChatRepairAuthorization{MessageID: parent.AuthorizationMessageID, PolicyRevision: policy.Revision}
	if parent.RepairLink != nil {
		link, err := chatRepairLinkInTx(ctx, tx, c.GroupID, parent.TaskID)
		if err != nil {
			return domain.ChatRepairAuthorization{}, err
		}
		if link.ID != parent.RepairLink.ID || link.AssessmentID != parent.ID || link.CandidateID != c.ID || link.MandateID != parent.MandateID || link.AuthorizationMessageID != parent.AuthorizationMessageID {
			return domain.ChatRepairAuthorization{}, ErrConflict
		}
		proof.SourceKind, proof.SourceID, proof.MessageID, proof.PolicyRevision = link.AuthorizationSourceKind, link.AuthorizationSourceID, link.AuthorizationMessageID, link.AuthorizationPolicyRevision
		if proof.SourceKind == "local_user_review" {
			if parent.Authority != "explicit_request" || proof.MessageID != "" {
				return domain.ChatRepairAuthorization{}, ErrConflict
			}
			if err := tx.QueryRowContext(ctx, `SELECT r.reason FROM task_review_decisions r JOIN mandates m INDEXED BY mandates_group_task
			 ON m.group_id=r.group_id AND m.task_id=r.task_id AND m.topic_id=?
			 WHERE r.id=? AND r.group_id=? AND r.reviewed_by='user' AND r.outcome='rejected'`, c.TopicID, proof.SourceID, c.GroupID).Scan(&proof.Content); err != nil {
				return domain.ChatRepairAuthorization{}, err
			}
			return proof, nil
		}
	} else {
		switch parent.Authority {
		case "explicit_request":
			if c.SourceKind != "local_user_chat" || parent.AuthorizationMessageID != c.MessageID {
				return domain.ChatRepairAuthorization{}, ErrConflict
			}
			proof.SourceKind, proof.SourceID = "local_user_chat", c.SourceID
		case "written_preauthorization":
			if !policy.PreparationSupported || policy.AuthorizedBy != "user" || parent.AuthorizationMessageID != policy.PreauthorizationMessageID {
				return domain.ChatRepairAuthorization{}, ErrConflict
			}
			proof.SourceKind, proof.SourceID = "local_user_policy", policy.ConfigurationSourceID
		default:
			return domain.ChatRepairAuthorization{}, ErrConflict
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT msg.content FROM message_command_sources s
	 JOIN messages msg ON msg.id=s.message_id AND msg.sender_id='user'
	 WHERE s.id=? AND s.kind=? AND msg.id=? AND msg.group_id=? AND msg.topic_id=?`, proof.SourceID, proof.SourceKind, proof.MessageID, c.GroupID, c.TopicID).Scan(&proof.Content); err != nil {
		return domain.ChatRepairAuthorization{}, err
	}
	if proof.SourceKind == "local_user_policy" {
		originalPolicy, err := chatPolicyInTx(ctx, tx, c.ThreadRef, proof.PolicyRevision)
		if err != nil {
			return domain.ChatRepairAuthorization{}, err
		}
		if !originalPolicy.PreparationSupported || originalPolicy.ConfigurationSourceID != proof.SourceID || originalPolicy.PreauthorizationMessageID != proof.MessageID {
			return domain.ChatRepairAuthorization{}, ErrConflict
		}
		proof.ScopeKind = originalPolicy.PreparationScopeKind
	}
	return proof, nil
}
