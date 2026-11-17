package store

import (
	"context"
	"database/sql"
	"time"

	"agentworkspace/internal/domain"
)

// GetChatRepairLink is trusted historical evidence, never a live capability.
func (s *Store) GetChatRepairLink(ctx context.Context, groupID, repairTaskID string) (domain.ChatRepairLink, error) {
	if !validTriggerID(groupID) || !validTriggerID(repairTaskID) {
		return domain.ChatRepairLink{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.ChatRepairLink{}, admissionError("读取修复关联失败", err)
	}
	defer tx.Rollback()
	if err := requireMessageActor(ctx, tx, groupID, "user"); err != nil {
		return domain.ChatRepairLink{}, admissionError("读取修复关联失败", err)
	}
	link, err := chatRepairLinkInTx(ctx, tx, groupID, repairTaskID)
	if err != nil {
		return domain.ChatRepairLink{}, admissionError("读取修复关联失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.ChatRepairLink{}, admissionError("读取修复关联失败", err)
	}
	return link, nil
}

func chatRepairLinkInTx(ctx context.Context, tx *sql.Tx, groupID, repairTaskID string) (domain.ChatRepairLink, error) {
	supported, err := chatRepairSupportedInTx(ctx, tx)
	if err != nil {
		return domain.ChatRepairLink{}, err
	}
	if !supported {
		return domain.ChatRepairLink{}, ErrNotFound
	}
	var link domain.ChatRepairLink
	var stamp string
	err = tx.QueryRowContext(ctx, `SELECT id,repair_task_id,parent_task_id,parent_submission_id,parent_review_id,reviewed_task_revision,
	 parent_mandate_id,group_id,topic_id,candidate_id,assessment_id,mandate_id,dispatch_request_id,policy_revision,
	 authorization,authorization_source_kind,authorization_source_id,coalesce(authorization_message_id,''),authorization_policy_revision,created_at
	 FROM chat_repair_links WHERE group_id=? AND repair_task_id=?`, groupID, repairTaskID).Scan(
		&link.ID, &link.RepairTaskID, &link.ParentTaskID, &link.ParentSubmissionID, &link.ParentReviewID, &link.ReviewedTaskRevision,
		&link.ParentMandateID, &link.GroupID, &link.TopicID, &link.CandidateID, &link.AssessmentID, &link.MandateID, &link.DispatchRequestID, &link.PolicyRevision,
		&link.Authorization, &link.AuthorizationSourceKind, &link.AuthorizationSourceID, &link.AuthorizationMessageID, &link.AuthorizationPolicyRevision, &stamp)
	if err != nil {
		return domain.ChatRepairLink{}, err
	}
	link.CreatedAt, err = time.Parse(timestampLayout, stamp)
	if err != nil {
		return domain.ChatRepairLink{}, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT artifact_id,version_id FROM chat_repair_artifact_versions WHERE repair_task_id=? ORDER BY artifact_id,version_id", repairTaskID)
	if err != nil {
		return domain.ChatRepairLink{}, err
	}
	defer rows.Close()
	link.ArtifactVersions = []domain.ArtifactVersionRef{}
	for rows.Next() {
		var ref domain.ArtifactVersionRef
		if err := rows.Scan(&ref.ArtifactID, &ref.VersionID); err != nil {
			return domain.ChatRepairLink{}, err
		}
		link.ArtifactVersions = append(link.ArtifactVersions, ref)
	}
	if err := rows.Err(); err != nil {
		return domain.ChatRepairLink{}, err
	}
	return link, nil
}
