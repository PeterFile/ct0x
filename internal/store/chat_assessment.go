package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
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
	if err := chatIntakePurposeInTx(ctx, tx, scope, candidateID); err != nil {
		return domain.ChatAssessmentReceipt{}, err
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
		Action: input.Action, CreatedAt: time.Now().UTC(), Purpose: candidate.Purpose, SourceID: candidate.SourceID, SourceKind: candidate.SourceKind, Outcome: input.Disposition}
	decisionID := ""
	wantsWork := input.Disposition == "request_work" || input.Disposition == "prepare_work"
	if wantsWork {
		if err := chatAdmissionGateInTx(ctx, tx, candidate, input); err != nil {
			var denial *chatNonAdmission
			if !errors.As(err, &denial) {
				return domain.ChatAssessmentReceipt{}, admissionError("聊天工作准入结果未确认", err)
			}
			receipt.Outcome, receipt.ReasonCode, receipt.Question = "not_admitted", denial.code, denial.question
			content = "本次未准入，未创建任务或分派。\n原因：" + denial.fact + "\n" + denial.question
			wantsWork = false
		}
	}
	if wantsWork {
		receipt.Outcome = "admitted"
		if input.Disposition == "prepare_work" {
			input.Title, input.Description, input.Acceptance = chatSummaryTask(candidate)
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
		receipt.AuthorityEvidenceVerified = true
		content = fmt.Sprintf("已准入文本任务「%s」，分派已登记，尚未执行完成或接纳。\n依据：%s；聊天策略版本 %d；授权来源 %s。\n任务：%s\n分派：%s\n%s", input.Title, input.Reason,
			receipt.PolicyRevision, receipt.AuthorizationMessageID, receipt.TaskID, receipt.DispatchRequestID, content)
	} else if input.Disposition == "clarify" {
		receipt.Question, content = input.Question, input.Question
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
	case "request_work":
		if !memberToolTextValid(input.Action, 128) || !memberToolTextValid(input.Title, 256) ||
			!memberToolTextValid(input.Acceptance, 4096) || len(input.Description) > 8192 || input.Question != "" {
			return ErrInvalid
		}
	case "prepare_work":
		if !memberToolTextValid(input.Action, 128) || len(input.Title) > 256 || len(input.Description) > 8192 || len(input.Acceptance) > 4096 || input.Question != "" {
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
	if !current.Enabled {
		return chatDeny("policy_disabled", "本话题聊天准入策略未启用。", "是否先在话题策略中启用纯文本准入？")
	}
	if current.Revision != c.Policy.Revision || current.Revision < 1 {
		return chatDeny("stale_policy", "评估绑定的策略版本已经变化。", "是否以当前策略重新评估最新请求？")
	}
	if current.Action != input.Action || input.Action != domain.ChatActionTextSubmission {
		return chatDeny("unsupported_action", "本版策略仅允许纯文本交付，不授予文件、Artifact或外部动作。", "是否只需要在群聊交付纯文本？")
	}
	// Generation is the actual source's committed position, not the chat head.
	// Later user messages supersede it even before capture; member replies do not
	// change user authority. Keep the range anchored to the immutable source.
	var superseded bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
	 SELECT 1 FROM message_log l JOIN message_command_sources s ON s.message_id=l.message_id AND s.kind='local_user_chat'
	 WHERE l.group_id=? AND l.topic_id=? AND l.sequence>?)`,
		c.GroupID, c.TopicID, c.Generation).Scan(&superseded); err != nil {
		return err
	}
	binding, policy, err := chatDispatchRevisionsInTx(ctx, tx, c.GroupID)
	if err != nil {
		return err
	}
	if superseded {
		return chatDeny("stale_source", "已有更新的真实用户聊天命令，本次旧评估未产生新工作。", "是否以最新用户要求为准？")
	}
	if binding != c.BindingRevision || policy != c.DispatchPolicyRevision {
		return chatDeny("stale_dispatch_configuration", "协调员绑定或分派策略已变化。", "是否按当前协调配置重新评估？")
	}
	if binding < 1 || policy < 1 {
		return chatDeny("routing_unavailable", "尚未配置可用的协调员和执行成员，本次没有产生新工作。", "是否先配置协调员及允许执行的成员？")
	}
	configured, err := coordinatorBindingInTx(ctx, tx, c.GroupID, binding)
	if err != nil {
		return err
	}
	coordinatorActive, workerEligible, err := coordinationDispatchMembersInTx(ctx, tx, c.GroupID, configured.MemberID, policy, "", "")
	if err != nil {
		return err
	}
	if !coordinatorActive || !workerEligible {
		return chatDeny("routing_unavailable", "协调员不可用或没有符合当前策略的执行成员，本次没有产生新工作。", "是否先调整协调员及允许执行的成员？")
	}
	if c.SourceID == "" || (c.SourceKind != "local_user_chat" && c.SourceKind != "member") {
		return chatDeny("untrusted_source", "来源没有可核验的聊天命令证明，sender或正文不能构成用户授权。", "请由本机用户说明当前需要落实的文本目标，可以吗？")
	}
	if input.Disposition == "request_work" && c.SourceKind != "local_user_chat" {
		return chatDeny("user_request_required", "成员建议或共识不能替代用户的工作请求。", "你希望将哪项文本目标交付？")
	}
	if input.Disposition == "prepare_work" {
		if !current.PreparationSupported || current.PreauthorizationMessageID == "" || current.AuthorizedBy != "user" {
			return chatDeny("preparation_not_authorized", "未启用受支持的结构化准备范围；旧自由文本规则不被升级为权限。", "是否显式启用本话题纯文本摘要准备范围？")
		}
		if input.Title != "" || input.Description != "" || input.Acceptance != "" {
			return chatDeny("preparation_scope_mismatch", "topic_summary只允许服务端固定的本话题摘要目标和验收，不能改为其他目标。", "是否仅按本话题摘要范围准备？")
		}
	}
	var routed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM coordination_triggers t JOIN routing_decisions d ON d.trigger_id=t.id WHERE t.message_id=?)`, c.MessageID).Scan(&routed); err != nil {
		return err
	}
	if routed {
		return chatDeny("source_already_routed", "此来源已有持久工作路由，本次没有重复准入。", "是否先查看该来源已有的任务？")
	}
	var used int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM chat_candidates c JOIN chat_assessments a ON a.candidate_id=c.id
	 WHERE c.group_id=? AND c.topic_id=? AND c.policy_revision=? AND a.decision_id IS NOT NULL LIMIT ?)`,
		c.GroupID, c.TopicID, current.Revision, current.MaxAdmissions).Scan(&used); err != nil {
		return err
	}
	if used >= current.MaxAdmissions {
		return chatDeny("budget_exhausted", fmt.Sprintf("策略版本%d的准入预算已用完（%d/%d）。", current.Revision, used, current.MaxAdmissions), "是否设置新的准入额度后继续？")
	}
	return nil
}

type chatNonAdmission struct{ code, fact, question string }

func (e *chatNonAdmission) Error() string        { return e.fact }
func chatDeny(code, fact, question string) error { return &chatNonAdmission{code, fact, question} }

func chatSummaryTask(c domain.ChatCandidate) (string, string, string) {
	return "本话题纯文本摘要", fmt.Sprintf("仅依据本群、本话题已经保存的消息整理摘要；绑定来源%s，策略版本%d。不得改为其他目标、附加Artifact或执行文件/外部动作。", c.MessageID, c.Policy.Revision), "提供非空、有效UTF-8且不超过8192字节的本话题纯文本摘要；区分事实、建议和未决问题，引用消息依据；不添加Artifact版本，不声称用户已经接纳。"
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
	if receipt.Purpose == "" {
		receipt.SourceID, receipt.SourceKind = "", "unknown"
	}
	if receipt.Purpose != domain.ChatIntakePurpose || receipt.SourceID == "" {
		receipt.AuthorityEvidenceVerified = false
	}
	if receipt.SourceKind == "" {
		receipt.SourceKind = "unknown"
	}
	return receipt, nil
}
