package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"

	"agentworkspace/internal/domain"
)

func chatRepairAdmissionInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate, input domain.ChatAssessment) (*domain.ChatRepairSource, error) {
	supported, err := chatRepairSupportedInTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if !supported {
		return nil, chatDeny("repair_unavailable", "本实例尚不支持正式修复关联。", "是否先保留拒收记录，待修复链可用后再继续？")
	}
	proposal := input.Repair
	if c.RepairSource != nil && c.RepairSource.ParentReviewID != proposal.ReviewID {
		return nil, chatDeny("repair_source_mismatch", "评估引用不是本轮绑定的正式拒收。", "是否针对本轮原拒收版本说明需要处理的内容？")
	}
	source, err := chatRepairSourceInTx(ctx, tx, c.ThreadRef, proposal.ReviewID)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
		return nil, chatDeny("repair_source_unavailable", "拒收来源不存在、不属于本话题或已被新的任务状态取代。", "是否以当前正式提交和评审记录为准？")
	}
	if err != nil {
		return nil, err
	}
	if input.Action != source.ParentAdmission.Action {
		return nil, chatDeny("repair_scope_mismatch", "修复不能改变原任务的动作范围。", "是否仅按原任务范围处理此拒收？")
	}
	var linked bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM chat_repair_links WHERE parent_review_id=?)", source.ParentReviewID).Scan(&linked); err != nil {
		return nil, err
	}
	if linked {
		return nil, chatDeny("repair_already_linked", "此正式拒收已经关联修复任务，不重复开工。", "是否先查看已有修复任务的进展？")
	}
	if err := chatRepairParentReadyInTx(ctx, tx, c.GroupID, source.ParentTaskID); err != nil {
		return nil, err
	}
	if proposal.Authorization == domain.ChatRepairChatRequest {
		if c.SourceKind != "local_user_chat" || c.SourceID == "" {
			return nil, chatDeny("user_request_required", "普通成员或平台通知不能冒充新的用户聊天请求。", "你希望在原范围内处理哪项拒收内容？")
		}
	} else {
		if c.RepairSource == nil || c.SourceKind != "platform" || c.SourceID != "review:"+source.ParentReviewID {
			return nil, chatDeny("repair_source_mismatch", "此轮没有绑定真实用户评审的受信来源。", "是否由本机用户说明当前希望处理的原版本？")
		}
		if proposal.Authorization == domain.ChatRepairOriginalAgreement && source.OriginalAuthorization.ScopeKind != "" {
			return nil, chatDeny("repair_agreement_unavailable", "原规范预授权仅允许话题摘要准备，不能升级为拒收后的继续执行许可。", "是否需要在原任务范围继续处理此拒收？")
		}
	}
	return &source, nil
}

func chatRepairWorkspaceInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate, source domain.ChatRepairSource) (*domain.TaskWorkspaceScope, error) {
	parent, err := taskWorkspaceScopeInTx(ctx, tx, c.GroupID, source.ParentTaskID)
	if err != nil {
		return nil, err
	}
	scope := &domain.TaskWorkspaceScope{ThreadRef: c.ThreadRef, CandidateID: c.ID, PolicyRevision: c.Policy.Revision,
		ResourceKey: parent.ResourceKey, NodeID: parent.NodeID, Grants: []domain.TaskWorkspaceGrant{}}
	for _, grant := range parent.Grants {
		current, err := taskWorkspaceGrantInTx(ctx, tx, c.GroupID, source.ParentTaskID, grant.MemberID)
		if errors.Is(err, ErrMemberToolDenied) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var eligible bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM group_dispatch_policy_revisions p
		 JOIN json_each(p.allowed_worker_ids) w ON w.type='text' AND w.value=?
		 JOIN group_coordinator_binding_revisions b ON b.group_id=p.group_id AND b.revision=?
		 WHERE p.group_id=? AND p.revision=? AND b.member_id<>?)`, current.MemberID, c.BindingRevision, c.GroupID, c.DispatchPolicyRevision, current.MemberID).Scan(&eligible); err != nil {
			return nil, err
		}
		if eligible {
			scope.Grants = append(scope.Grants, current)
		}
	}
	if len(scope.Grants) == 0 {
		return nil, chatDeny("repair_workspace_unavailable", "原物理范围内没有仍获当前策略允许的授权执行成员。", "是否先恢复原目录范围内的可用执行授权？")
	}
	return scope, nil
}

func insertChatRepairLinkInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate, receipt domain.ChatAssessmentReceipt, source domain.ChatRepairSource, proposal domain.ChatRepairAssessment) (*domain.ChatRepairLink, error) {
	proof := source.OriginalAuthorization
	switch proposal.Authorization {
	case domain.ChatRepairReviewRequest:
		proof = domain.ChatRepairAuthorization{SourceKind: "local_user_review", SourceID: source.ParentReviewID, PolicyRevision: c.Policy.Revision}
	case domain.ChatRepairChatRequest:
		proof = domain.ChatRepairAuthorization{SourceKind: "local_user_chat", SourceID: c.SourceID, MessageID: c.MessageID, PolicyRevision: c.Policy.Revision}
	}
	link := domain.ChatRepairLink{ID: rand.Text(), ThreadRef: c.ThreadRef, RepairTaskID: receipt.TaskID,
		ParentTaskID: source.ParentTaskID, ParentSubmissionID: source.ParentSubmissionID, ParentReviewID: source.ParentReviewID,
		ReviewedTaskRevision: source.ReviewedTaskRevision, ParentMandateID: source.ParentMandate.ID,
		CandidateID: c.ID, AssessmentID: receipt.ID, MandateID: receipt.MandateID, DispatchRequestID: receipt.DispatchRequestID,
		PolicyRevision: c.Policy.Revision, Authorization: proposal.Authorization, AuthorizationSourceKind: proof.SourceKind,
		AuthorizationSourceID: proof.SourceID, AuthorizationMessageID: proof.MessageID, AuthorizationPolicyRevision: proof.PolicyRevision,
		ArtifactVersions: source.ArtifactVersions, CreatedAt: receipt.CreatedAt}
	if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO chat_repair_links(id,repair_task_id,parent_task_id,parent_submission_id,parent_review_id,
	 reviewed_task_revision,parent_mandate_id,group_id,topic_id,candidate_id,assessment_id,mandate_id,dispatch_request_id,policy_revision,
	 authorization,authorization_source_kind,authorization_source_id,authorization_message_id,authorization_policy_revision,created_at)
	 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,nullif(?,''),?,?)`, link.ID, link.RepairTaskID, link.ParentTaskID, link.ParentSubmissionID, link.ParentReviewID,
		link.ReviewedTaskRevision, link.ParentMandateID, link.GroupID, link.TopicID, link.CandidateID, link.AssessmentID, link.MandateID, link.DispatchRequestID, link.PolicyRevision,
		link.Authorization, link.AuthorizationSourceKind, link.AuthorizationSourceID, link.AuthorizationMessageID, link.AuthorizationPolicyRevision, link.CreatedAt.Format(timestampLayout)); err != nil {
		return nil, err
	}
	for _, ref := range link.ArtifactVersions {
		if err := coordinationDispatchWrite(ctx, tx, "INSERT INTO chat_repair_artifact_versions(repair_task_id,artifact_id,version_id) VALUES(?,?,?)", link.RepairTaskID, ref.ArtifactID, ref.VersionID); err != nil {
			return nil, err
		}
	}
	return &link, nil
}
