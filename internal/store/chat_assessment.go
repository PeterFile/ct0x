package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"agentworkspace/internal/domain"
)

// ApplyChatAssessment is a bound assessor command, not a local-user API. The
// semantic proposal cannot select source, caller, policy, grants or task owner.
func (s *Store) ApplyChatAssessment(ctx context.Context, scope MemberToolScope, candidateID string, input domain.ChatAssessment, content, key string) (domain.ChatAssessmentReceipt, error) {
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return domain.ChatAssessmentReceipt{}, memberToolOperationError("记录聊天评估失败", err)
	}
	defer tx.Rollback()
	candidate, err := chatCandidateInTx(ctx, tx, scope, candidateID)
	if err != nil {
		return domain.ChatAssessmentReceipt{}, memberToolOperationError("验证聊天候选失败", err)
	}
	if !memberToolKeyValid(key) {
		return domain.ChatAssessmentReceipt{}, ErrInvalid
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return domain.ChatAssessmentReceipt{}, err
	}
	// Share the send_message namespace: adding/removing an assessment under the
	// same transport key is a payload conflict, not a second independent write.
	mutation := mutationScope{operation: "create_message", actorID: scope.MemberID, groupID: scope.Claim.GroupID, key: key}
	payload := mutationPayload("chat_assessment/v1", candidateID, string(encoded), content)
	id, replay, err := lookupMutation(ctx, tx, mutation, payload)
	if err != nil {
		return domain.ChatAssessmentReceipt{}, admissionError("读取聊天评估回执失败", err)
	}
	if replay {
		receipt, err := chatAssessmentReceiptInTx(ctx, tx, id, candidateID)
		if err != nil {
			return domain.ChatAssessmentReceipt{}, admissionError("读取聊天评估回执失败", err)
		}
		if err := tx.Commit(); err != nil {
			return domain.ChatAssessmentReceipt{}, admissionError("读取聊天评估回执失败", err)
		}
		return receipt, nil
	}
	if err := validateChatAssessment(input, content); err != nil {
		return domain.ChatAssessmentReceipt{}, err
	}
	var evaluated bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM chat_assessments WHERE candidate_id=?)", candidateID).Scan(&evaluated); err != nil {
		return domain.ChatAssessmentReceipt{}, err
	}
	if evaluated {
		return domain.ChatAssessmentReceipt{}, ErrConflict
	}
	receipt := domain.ChatAssessmentReceipt{ID: rand.Text(), CandidateID: candidate.ID, ThreadRef: candidate.ThreadRef,
		MessageID: candidate.MessageID, SourceSenderID: candidate.SourceSenderID, AssessedBy: scope.MemberID, TurnID: scope.Claim.TurnID,
		PolicyRevision: candidate.Policy.Revision, CandidateGeneration: candidate.Generation, Disposition: input.Disposition,
		Action: input.Action, CreatedAt: time.Now().UTC()}
	decisionID := ""
	if input.Disposition == "request_work" || input.Disposition == "prepare_work" {
		if err := chatAdmissionGateInTx(ctx, tx, candidate, input); err != nil {
			return domain.ChatAssessmentReceipt{}, admissionError("聊天工作未获准入", err)
		}
		receipt.Authority, receipt.AuthorizationMessageID = "explicit_request", candidate.MessageID
		if input.Disposition == "prepare_work" {
			receipt.Authority, receipt.AuthorizationMessageID = "written_preauthorization", candidate.Policy.PreauthorizationMessageID
		}
		trigger, err := recordLocalMessageTriggerInTx(ctx, tx, candidate.ThreadRef, candidate.MessageID)
		if err != nil {
			return domain.ChatAssessmentReceipt{}, admissionError("捕获聊天工作来源失败", err)
		}
		decisionID = rand.Text()
		request := domain.RequestWorkRequest{TopicID: candidate.TopicID, MessageID: candidate.MessageID,
			Mode: domain.RoutingDecisionCreateNew, Reason: input.Reason,
			NewTask: &domain.NewTaskRequest{Title: input.Title, Description: input.Description, Acceptance: input.Acceptance}}
		// The fixed legacy gate records explicit local-user authority: either the
		// actual request or their explicitly enabled written policy. The precise
		// distinction and assessor remain in this immutable chat receipt.
		if err := admitWorkInTx(ctx, tx, trigger, request, decisionID); err != nil {
			return domain.ChatAssessmentReceipt{}, admissionError("授权聊天工作失败", err)
		}
		admission, err := admissionReceiptInTx(ctx, tx, candidate.GroupID, decisionID)
		if err != nil {
			return domain.ChatAssessmentReceipt{}, admissionError("读取聊天工作准入失败", err)
		}
		dispatch, err := coordinationDispatchCreateInTx(ctx, tx, candidate.GroupID, admission.Task.ID, admission.Task.RevisionAtAdmission, 1, input.Reason)
		if err != nil {
			return domain.ChatAssessmentReceipt{}, admissionError("自动登记聊天工作分派失败", err)
		}
		receipt.TaskID, receipt.MandateID, receipt.DispatchRequestID = admission.Task.ID, admission.Mandate.ID, dispatch.ID
		content = fmt.Sprintf("已准入文本任务「%s」，分派已登记，尚未执行完成或接纳。\n依据：%s；聊天策略版本 %d；授权来源 %s。\n任务：%s\n分派：%s\n%s", input.Title, input.Reason,
			receipt.PolicyRevision, receipt.AuthorizationMessageID, receipt.TaskID, receipt.DispatchRequestID, content)
	} else if input.Disposition == "clarify" {
		content = input.Question
	}
	notice, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: candidate.GroupID, TopicID: candidate.TopicID, ActorID: scope.MemberID, Content: content, RecipientIDs: []string{}}, nil)
	if err != nil {
		return domain.ChatAssessmentReceipt{}, admissionError("保存聊天行动说明失败", err)
	}
	receipt.NoticeMessageID, receipt.CreatedAt = notice.ID, notice.CreatedAt
	data, err := json.Marshal(receipt)
	if err != nil {
		return domain.ChatAssessmentReceipt{}, err
	}
	if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO chat_assessments(id,candidate_id,decision_id,dispatch_request_id,notice_message_id,payload,receipt_json)
	 VALUES(?,?,nullif(?,''),nullif(?,''),?,?,?)`, receipt.ID, candidate.ID, decisionID, receipt.DispatchRequestID, notice.ID, payload, string(data)); err != nil {
		return domain.ChatAssessmentReceipt{}, admissionError("保存聊天评估失败", err)
	}
	if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO mutation_idempotency(operation,actor_id,group_id,idempotency_key,payload,record_id) VALUES(?,?,?,?,?,?)`,
		mutation.operation, mutation.actorID, mutation.groupID, key, payload, receipt.ID); err != nil {
		return domain.ChatAssessmentReceipt{}, admissionError("保存聊天评估回执失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.ChatAssessmentReceipt{}, admissionError("保存聊天准入失败", err)
	}
	return receipt, nil
}

func validateChatAssessment(input domain.ChatAssessment, content string) error {
	if !memberToolTextValid(input.Reason, 4096) || !memberToolTextValid(content, 16384) {
		return ErrInvalid
	}
	switch input.Disposition {
	case "request_work", "prepare_work":
		if input.Action != domain.ChatActionTextSubmission || !memberToolTextValid(input.Title, 256) ||
			!memberToolTextValid(input.Acceptance, 4096) || len(input.Description) > 8192 || input.Question != "" {
			return ErrInvalid
		}
	case "discuss", "clarify":
		if input.Action != "" || input.Title != "" || input.Description != "" || input.Acceptance != "" {
			return ErrInvalid
		}
		if input.Disposition == "clarify" && !memberToolTextValid(input.Question, 2048) || input.Disposition == "discuss" && input.Question != "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func chatAdmissionGateInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate, input domain.ChatAssessment) error {
	current, err := chatPolicyInTx(ctx, tx, c.ThreadRef, 0)
	if err != nil {
		return err
	}
	if !current.Enabled || current.Revision != c.Policy.Revision || current.Revision < 1 || current.Action != input.Action {
		return ErrConflict
	}
	// Generation is the actual source's committed position, not the chat head.
	// Later user messages supersede it even before capture; member replies do not
	// change user authority. Keep the range anchored to the immutable source.
	var superseded bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
	 SELECT 1 FROM message_log l JOIN messages m ON m.id=l.message_id
	 WHERE l.group_id=? AND l.topic_id=? AND l.sequence>? AND m.sender_id='user')`,
		c.GroupID, c.TopicID, c.Generation).Scan(&superseded); err != nil {
		return err
	}
	binding, policy, err := chatDispatchRevisionsInTx(ctx, tx, c.GroupID)
	if err != nil {
		return err
	}
	if superseded || binding != c.BindingRevision || policy != c.DispatchPolicyRevision {
		return ErrConflict
	}
	if input.Disposition == "request_work" && c.SourceSenderID != "user" {
		return ErrConflict
	}
	if input.Disposition == "prepare_work" && (!current.AllowPreparation || current.PreauthorizationMessageID == "" || current.AuthorizedBy != "user") {
		return ErrConflict
	}
	var used int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM chat_candidates c JOIN chat_assessments a ON a.candidate_id=c.id
	 WHERE c.group_id=? AND c.topic_id=? AND c.policy_revision=? AND a.decision_id IS NOT NULL LIMIT ?)`,
		c.GroupID, c.TopicID, current.Revision, current.MaxAdmissions).Scan(&used); err != nil {
		return err
	}
	if used >= current.MaxAdmissions {
		return fmt.Errorf("%w：本版聊天策略的工作预算已用完", ErrConflict)
	}
	return nil
}

func chatAssessmentReceiptInTx(ctx context.Context, tx *sql.Tx, id, candidateID string) (domain.ChatAssessmentReceipt, error) {
	var data string
	if err := tx.QueryRowContext(ctx, "SELECT receipt_json FROM chat_assessments WHERE id=? AND candidate_id=?", id, candidateID).Scan(&data); err != nil {
		return domain.ChatAssessmentReceipt{}, err
	}
	var receipt domain.ChatAssessmentReceipt
	if err := json.Unmarshal([]byte(data), &receipt); err != nil {
		return domain.ChatAssessmentReceipt{}, err
	}
	return receipt, nil
}
