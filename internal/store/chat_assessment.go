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
	return s.applyChatAssessment(ctx, scope, candidateID, input, content, key, false, nil)
}

// ApplyChatAssessmentDormant records an official input without waking a native
// Turn. Only trusted callers can choose this mode; it is not a model argument.
func (s *Store) ApplyChatAssessmentDormant(ctx context.Context, scope MemberToolScope, candidateID string, input domain.ChatAssessment, content, key string) (domain.ChatAssessmentReceipt, error) {
	return s.applyChatAssessment(ctx, scope, candidateID, input, content, key, true, nil)
}

func (s *Store) applyChatAssessment(ctx context.Context, scope MemberToolScope, candidateID string, input domain.ChatAssessment, content, key string, dormant bool, clarification *decisionClarificationCapture) (domain.ChatAssessmentReceipt, error) {
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return domain.ChatAssessmentReceipt{}, memberToolOperationError("记录聊天评估失败", err)
	}
	defer tx.Rollback()
	if err := denyTaskWorkFallbackInTx(ctx, tx, scope.Claim.TurnID); err != nil {
		return domain.ChatAssessmentReceipt{}, memberToolOperationError("验证聊天轮次失败", err)
	}
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
	assessmentPayload := mutationPayload("chat_assessment/v1", candidateID, string(encoded), content)
	payload := assessmentPayload
	if clarification != nil {
		if err := checkDecisionClarificationSourceInTx(ctx, tx, scope, candidate); err != nil {
			return domain.ChatAssessmentReceipt{}, decisionRequestError("验证话题待决来源失败", err)
		}
		payload = mutationPayload("chat_decision_clarification/v1", candidateID, string(encoded), content, clarification.body)
	}
	id, replay, err := lookupMutation(ctx, tx, mutation, payload)
	if err != nil {
		return domain.ChatAssessmentReceipt{}, admissionError("读取聊天评估回执失败", err)
	}
	if replay {
		receipt, err := chatAssessmentReceiptInTx(ctx, tx, id, candidateID)
		if errors.Is(err, sql.ErrNoRows) && candidate.SourceKind == "local_user_chat" {
			// A keyed replay from a different candidate may refer to the one
			// immutable follow-up receipt for this trusted source.
			if err := chatIntakePurposeInTx(ctx, tx, scope, candidateID); err != nil {
				return domain.ChatAssessmentReceipt{}, err
			}
			receipt, err = chatFollowupReceiptInTx(ctx, tx, candidate)
			if err == nil && receipt.ID != id {
				err = ErrConflict
			}
		}
		if err != nil {
			return domain.ChatAssessmentReceipt{}, admissionError("读取聊天评估回执失败", err)
		}
		if receipt.WorkInputID != "" {
			if _, err := chatDiscussionArrivalInTx(ctx, tx, scope, candidateID); err != nil {
				return domain.ChatAssessmentReceipt{}, err
			}
		}
		if clarification != nil {
			if err := clarification.loadInTx(ctx, tx, candidate, receipt.ID); err != nil {
				return domain.ChatAssessmentReceipt{}, decisionRequestError("读取话题待决事项失败", err)
			}
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
	var installed bool
	if candidate.SourceKind == "local_user_chat" || input.Disposition == domain.ChatDispositionContinueWork {
		installed, err = workInputSchemaInTx(ctx, tx)
		if err != nil {
			return domain.ChatAssessmentReceipt{}, admissionError("验证工作输入记录失败", err)
		}
		if installed && candidate.SourceKind == "local_user_chat" {
			prior, err := chatFollowupReceiptInTx(ctx, tx, candidate)
			if err == nil {
				if clarification != nil {
					return domain.ChatAssessmentReceipt{}, ErrConflict
				}
				if _, err := chatDiscussionArrivalInTx(ctx, tx, scope, candidateID); err != nil {
					return domain.ChatAssessmentReceipt{}, err
				}
				// Reserve this transport key, not a second evaluation or notice.
				return commitChatAssessmentInTx(ctx, tx, mutation, payload, prior)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return domain.ChatAssessmentReceipt{}, admissionError("读取原工作输入回执失败", err)
			}
		}
	}
	wantsWork := input.Disposition == "request_work" || input.Disposition == "prepare_work"
	receipt := domain.ChatAssessmentReceipt{ID: rand.Text(), CandidateID: candidate.ID, ThreadRef: candidate.ThreadRef,
		MessageID: candidate.MessageID, SourceSenderID: candidate.SourceSenderID, AssessedBy: scope.MemberID, TurnID: scope.Claim.TurnID,
		PolicyRevision: candidate.Policy.Revision, CandidateGeneration: candidate.Generation, Disposition: input.Disposition,
		Action: input.Action, CreatedAt: time.Now().UTC(), Purpose: candidate.Purpose, SourceID: candidate.SourceID, SourceKind: candidate.SourceKind, Outcome: input.Disposition}
	if input.DiscussionOnlyExcerpt != nil {
		excerpt := *input.DiscussionOnlyExcerpt
		if err := verifyChatDiscussionOnlyInTx(ctx, tx, scope, candidate, excerpt); err != nil {
			return domain.ChatAssessmentReceipt{}, memberToolOperationError("验证仅讨论限制失败", err)
		}
		receipt.DiscussionOnly, receipt.DiscussionOnlyExcerpt = true, excerpt
	}
	decisionID := ""
	var workspace *domain.TaskWorkspaceScope
	var repair *domain.ChatRepairSource
	var investigationAuthorization string
	followup, err := s.prepareChatFollowupInTx(ctx, tx, scope, candidate, input, installed)
	if err != nil {
		var denial *chatNonAdmission
		if !errors.As(err, &denial) {
			return domain.ChatAssessmentReceipt{}, admissionError("核对原任务后续输入失败", err)
		}
		receipt.Outcome, receipt.ReasonCode, receipt.Question = "not_admitted", denial.code, denial.question
		content = "未记录原任务输入，未创建新任务或分派。\n原因：" + denial.fact + "\n" + denial.question
		wantsWork = false
	} else if followup != nil {
		if !dormant {
			if !s.taskWorkExecution.Load() {
				return domain.ChatAssessmentReceipt{}, ErrTaskWorkExecutionNotReady
			}
			supported, err := taskWorkSchemaSupportedInTx(ctx, tx)
			if err != nil {
				return domain.ChatAssessmentReceipt{}, admissionError("验证后续输入投递失败", err)
			}
			if !supported {
				return domain.ChatAssessmentReceipt{}, ErrTaskWorkExecutionNotReady
			}
		}
		i := followup.input
		receipt.Outcome, receipt.WorkInputID, receipt.TaskID, receipt.MandateID = "continued", i.ID, i.TaskID, i.MandateID
		receipt.Action, receipt.Authority, receipt.AuthorityEvidenceVerified = followup.action, "existing_mandate", true
		receipt.WorkInputPending = dormant
		content, wantsWork = chatContinuedNotice(i.TaskID, dormant), false
	}
	if wantsWork {
		err := chatAdmissionGateInTx(ctx, tx, candidate, input)
		if err == nil && input.Action == domain.ChatActionReadOnlyInvestigation {
			investigationAuthorization, err = s.chatInvestigationAdmissionInTx(ctx, tx, scope, candidate, input)
		}
		if err == nil && input.Repair != nil {
			repair, err = chatRepairAdmissionInTx(ctx, tx, candidate, input)
		}
		if err == nil && input.Action == domain.ChatActionWorkspaceChange {
			if repair != nil {
				workspace, err = chatRepairWorkspaceInTx(ctx, tx, candidate, *repair)
			} else {
				workspace, err = chatWorkspaceForCandidateInTx(ctx, tx, candidate)
			}
		}
		if err != nil {
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
		receipt.Authority, receipt.AuthorizationMessageID = "explicit_request", candidate.MessageID
		if input.Action == domain.ChatActionReadOnlyInvestigation {
			input.Title, input.Description, input.Acceptance = chatInvestigationTask(candidate)
			input.Reason = "按本机用户的当前只读调查策略准备文本交付。"
			if len(candidate.Policy.Investigation.ClueTerms) > 0 {
				input.Reason = "原消息符合本机用户配置的只读调查线索词组，仅按本版策略范围准备文本交付。"
			}
			receipt.Authority, receipt.AuthorizationMessageID = "written_preauthorization", investigationAuthorization
		} else if input.Disposition == "prepare_work" {
			input.Title, input.Description, input.Acceptance = chatSummaryTask(candidate)
			receipt.Authority, receipt.AuthorizationMessageID = "written_preauthorization", candidate.Policy.PreauthorizationMessageID
		}
		if repair != nil {
			input.Title, input.Description, input.Acceptance = repair.ParentMandate.Scope.Objective, repair.ParentMandate.Scope.Description, repair.ParentMandate.Scope.Acceptance
			receipt.Authority, receipt.AuthorizationMessageID = "explicit_request", ""
			if input.Repair.Authorization == domain.ChatRepairOriginalAgreement && repair.OriginalAuthorization.SourceKind == "local_user_policy" {
				receipt.Authority = "written_preauthorization"
			}
		}
		trigger, err := recordLocalMessageTriggerInTx(ctx, tx, candidate.ThreadRef, candidate.MessageID)
		if err != nil {
			return domain.ChatAssessmentReceipt{}, admissionError("捕获聊天工作来源失败", err)
		}
		decisionID = rand.Text()
		request := domain.RequestWorkRequest{TopicID: candidate.TopicID, MessageID: candidate.MessageID,
			Mode: domain.RoutingDecisionCreateNew, Reason: input.Reason,
			NewTask: &domain.NewTaskRequest{Title: input.Title, Description: input.Description, Acceptance: input.Acceptance}}
		if repair != nil {
			request.NewTask.AssigneeID = repair.ParentMandate.Scope.AssigneeID
			request.NewTask.DependencyIDs = repair.ParentMandate.Scope.DependencyIDs
		}
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
		if repair != nil {
			link, err := insertChatRepairLinkInTx(ctx, tx, candidate, receipt, *repair, *input.Repair)
			if err != nil {
				return domain.ChatAssessmentReceipt{}, admissionError("保存正式修复关联失败", err)
			}
			receipt.RepairLink, receipt.AuthorizationMessageID = link, link.AuthorizationMessageID
		}
		kind := "文本任务"
		if input.Action == domain.ChatActionReadOnlyInvestigation && len(candidate.Policy.Investigation.ClueTerms) > 0 {
			kind = "只读调查"
		}
		if workspace != nil {
			workspace.TaskID, workspace.TaskRevision, workspace.MandateID = admission.Task.ID, admission.Task.RevisionAtAdmission, admission.Mandate.ID
			workspace.AssessmentID, workspace.DispatchRequestID = receipt.ID, dispatch.ID
			if err := insertChatWorkspaceScopeInTx(ctx, tx, *workspace); err != nil {
				return domain.ChatAssessmentReceipt{}, admissionError("绑定目录工作范围失败", err)
			}
			receipt.WorkspaceScope, kind = workspace, "目录任务"
		}
		content = fmt.Sprintf("已准入%s「%s」，分派已登记，尚未执行完成或接纳。\n依据：%s；聊天策略版本 %d；授权来源 %s。\n任务：%s\n分派：%s\n%s", kind, input.Title, input.Reason,
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
	 VALUES(?,?,nullif(?,''),nullif(?,''),?,?,?)`, receipt.ID, candidate.ID, decisionID, receipt.DispatchRequestID, notice.ID, assessmentPayload, string(data)); err != nil {
		return domain.ChatAssessmentReceipt{}, admissionError("保存聊天评估失败", err)
	}
	if clarification != nil {
		if err := clarification.persistInTx(ctx, tx, candidate, receipt); err != nil {
			return domain.ChatAssessmentReceipt{}, decisionRequestError("保存话题待决事项失败", err)
		}
	}
	if err := followup.persistInTx(ctx, tx, receipt); err != nil {
		return domain.ChatAssessmentReceipt{}, admissionError("保存原任务后续输入失败", err)
	}
	if receipt.WorkInputID != "" && !dormant {
		i := followup.input
		wake, err := taskWorkFollowupWakeInTx(ctx, tx, scope, *i, receipt.ID)
		if err != nil {
			return domain.ChatAssessmentReceipt{}, admissionError("保存原任务后续输入投递失败", err)
		}
		receipt.WorkDeliveryID, receipt.WorkWakeMessageID = wake.DeliveryID, wake.MessageID
	}
	return commitChatAssessmentInTx(ctx, tx, mutation, payload, receipt)
}

func commitChatAssessmentInTx(ctx context.Context, tx *sql.Tx, mutation mutationScope, payload []byte, receipt domain.ChatAssessmentReceipt) (domain.ChatAssessmentReceipt, error) {
	if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO mutation_idempotency(operation,actor_id,group_id,idempotency_key,payload,record_id) VALUES(?,?,?,?,?,?)`,
		mutation.operation, mutation.actorID, mutation.groupID, mutation.key, payload, receipt.ID); err != nil {
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
	if input.DiscussionOnlyExcerpt != nil && (input.Disposition != "discuss" || !memberToolTextValid(*input.DiscussionOnlyExcerpt, chatDiscussionOnlyExcerptBytes)) {
		return ErrInvalid
	}
	if f := input.Followup; f != nil {
		if (input.Disposition != domain.ChatDispositionContinueWork && input.Disposition != "request_work") || input.Repair != nil ||
			(f.TaskID != "" && !validTriggerID(f.TaskID)) || (f.Constraint != "" && !memberToolTextValid(f.Constraint, 2048)) {
			return ErrInvalid
		}
	}
	if input.Repair != nil {
		if input.Disposition != "request_work" || !validTriggerID(input.Repair.ReviewID) {
			return ErrInvalid
		}
		switch input.Repair.Authorization {
		case domain.ChatRepairReviewRequest, domain.ChatRepairOriginalAgreement, domain.ChatRepairChatRequest:
		default:
			return ErrInvalid
		}
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
	case domain.ChatDispositionContinueWork:
		if len(input.Action) > 128 || len(input.Title) > 256 || len(input.Description) > 8192 || len(input.Acceptance) > 4096 || input.Question != "" {
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
		question := "是否先在话题策略中启用纯文本准入？"
		if input.Action == domain.ChatActionWorkspaceChange {
			question = "是否先在本话题策略中启用目录工作准入？"
		}
		return chatDeny("policy_disabled", "本话题聊天准入策略未启用。", question)
	}
	if current.Revision != c.Policy.Revision || current.Revision < 1 {
		return chatDeny("stale_policy", "评估绑定的策略版本已经变化。", "是否以当前策略重新评估最新请求？")
	}
	if current.Action != domain.ChatActionTextSubmission || (input.Action != domain.ChatActionTextSubmission && input.Action != domain.ChatActionWorkspaceChange && input.Action != domain.ChatActionReadOnlyInvestigation) {
		return chatDeny("unsupported_action", "本版策略不支持所提动作。", "是否只需要在群聊交付纯文本？")
	}
	if input.Action == domain.ChatActionWorkspaceChange && !current.AllowWorkspaceChanges {
		return chatDeny("workspace_changes_disabled", "本话题未显式允许目录工作。", "是否在本话题策略中允许已有授权目录的工作？")
	}
	// New work uses the actual source's committed position, not the chat head.
	// Supplements do not grant new work and can be recorded out of source order.
	if input.Disposition != domain.ChatDispositionContinueWork {
		superseded, err := chatSourceSupersededInTx(ctx, tx, c.ThreadRef, c.Generation)
		if err != nil {
			return err
		}
		if superseded {
			return chatStaleSource()
		}
	}
	binding, policy, err := chatDispatchRevisionsInTx(ctx, tx, c.GroupID)
	if err != nil {
		return err
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
	if input.Repair == nil && (c.SourceID == "" || (c.SourceKind != "local_user_chat" && c.SourceKind != "member")) {
		question := "请由本机用户说明当前需要落实的文本目标，可以吗？"
		if input.Action == domain.ChatActionWorkspaceChange {
			question = "请由本机用户说明当前需要落实的目录工作目标，可以吗？"
		}
		return chatDeny("untrusted_source", "来源没有可核验的聊天命令证明，sender或正文不能构成用户授权。", question)
	}
	if input.Repair == nil && input.Disposition == "request_work" && c.SourceKind != "local_user_chat" {
		question := "你希望将哪项文本目标交付？"
		if input.Action == domain.ChatActionWorkspaceChange {
			question = "你希望在已授权目录落实哪项工作？"
		}
		return chatDeny("user_request_required", "成员建议或共识不能替代用户的工作请求。", question)
	}
	if input.Disposition == "prepare_work" && input.Action != domain.ChatActionReadOnlyInvestigation {
		if !current.PreparationSupported || current.PreauthorizationMessageID == "" || current.AuthorizedBy != "user" {
			return chatDeny("preparation_not_authorized", "未启用受支持的结构化准备范围；旧自由文本规则不被升级为权限。", "是否显式启用本话题纯文本摘要准备范围？")
		}
		if input.Action != domain.ChatActionTextSubmission || input.Title != "" || input.Description != "" || input.Acceptance != "" {
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
	if err := chatDiscussionWorkGateInTx(ctx, tx, c); err != nil {
		return err
	}
	if p := current.Investigation; p != nil && len(p.ClueTerms) > 0 && (c.SourceKind == "local_user_chat" || c.SourceKind == "member") {
		// Omitting the typed excerpt or proposing another action cannot turn
		// an explicitly discussion-only clue into work.
		body, err := chatInvestigationClueBodyInTx(ctx, tx, c, input.Disposition != domain.ChatDispositionContinueWork)
		if err != nil {
			return err
		}
		if input.Disposition == "prepare_work" && input.Action == domain.ChatActionReadOnlyInvestigation && !p.MatchesClue(body) {
			return chatDeny("preparation_scope_mismatch",
				fmt.Sprintf("消息 %s（来源 %s）的原文未包含本话题策略版本%d配置的全部只读调查线索词组。", c.MessageID, c.SourceID, current.Revision),
				"是否先继续讨论，并由本机用户确认是否需要调整本话题调查规则？")
		}
	}
	if input.Disposition == domain.ChatDispositionContinueWork {
		// An existing binding does not spend another admission slot.
		return nil
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
	if receipt.WorkInputID != "" && !receipt.WorkInputPending {
		// A dangling wake is a broken proof, not permission to create another.
		var deliveryID string
		if err := tx.QueryRowContext(ctx, `SELECT delivery_id FROM task_work_deliveries WHERE source_kind='followup' AND source_id=?`, receipt.WorkInputID).Scan(&deliveryID); err != nil {
			return domain.ChatAssessmentReceipt{}, coordinationRoleRelationError(err)
		}
		f, err := lookupTaskWorkDeliveryInTx(ctx, tx, receipt.GroupID, deliveryID)
		if err != nil {
			return domain.ChatAssessmentReceipt{}, coordinationRoleRelationError(err)
		}
		if f.assessmentID != receipt.ID || f.sourceTurnID != receipt.TurnID || f.d.SourceID != receipt.WorkInputID {
			return domain.ChatAssessmentReceipt{}, ErrMemberToolDenied
		}
		receipt.WorkDeliveryID, receipt.WorkWakeMessageID = f.d.DeliveryID, f.d.MessageID
	}
	return receipt, nil
}
