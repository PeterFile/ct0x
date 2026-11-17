package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

// New writes honor the actual chat action. Historical same-key receipts bypass
// this gate in the caller; legacy explicit tasks have no chat evidence.
func chatTextSubmissionInTx(ctx context.Context, tx *sql.Tx, input SubmitTaskInput) error {
	var hasChat, hasScope bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='chat_assessments'),EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='chat_policy_scopes')`).Scan(&hasChat, &hasScope); err != nil {
		return err
	}
	if !hasChat {
		return nil
	}
	var action, authority, scope string
	scopeColumn, scopeJoin := "''", ""
	if hasScope {
		scopeColumn = "coalesce(s.scope_kind,'')"
		scopeJoin = " LEFT JOIN chat_policy_scopes s ON s.group_id=c.group_id AND s.topic_id=c.topic_id AND s.revision=c.policy_revision"
	}
	err := tx.QueryRowContext(ctx, `SELECT coalesce(json_extract(a.receipt_json,'$.action'),''),coalesce(json_extract(a.receipt_json,'$.authority'),''),`+scopeColumn+`
	 FROM mandates m INDEXED BY mandates_group_task JOIN chat_assessments a ON a.decision_id=m.decision_id
	 JOIN chat_candidates c ON c.id=a.candidate_id`+scopeJoin+`
	 WHERE m.group_id=? AND m.task_id=?`, input.GroupID, input.TaskID).Scan(&action, &authority, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if action != domain.ChatActionTextSubmission || len(input.ArtifactVersions) != 0 || !utf8.ValidString(input.Summary) {
		return fmt.Errorf("%w：聊天文本交付不允许Artifact版本或非文本结果", ErrInvalid)
	}
	if authority == "written_preauthorization" && scope == domain.ChatPreparationTopicSummary && len(input.Summary) > 8192 {
		return fmt.Errorf("%w：本话题预授权摘要最多8192字节", ErrInvalid)
	}
	return nil
}

// A durable Submission is returned to its chat origin in the same transaction.
// This notice is not a native terminal receipt and never releases claims.
func chatSubmissionNoticeInTx(ctx context.Context, tx *sql.Tx, submission domain.Submission, expectedTopic string) error {
	var topic string
	err := tx.QueryRowContext(ctx, `SELECT m.topic_id FROM mandates m INDEXED BY mandates_group_task
	 JOIN chat_assessments a ON a.decision_id=m.decision_id WHERE m.group_id=? AND m.task_id=?`, submission.GroupID, submission.TaskID).Scan(&topic)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if topic != expectedTopic || !utf8.ValidString(submission.Summary) {
		return ErrConflict
	}
	summary := submission.Summary
	if len(summary) > 8192 {
		end := 8192
		for !utf8.RuneStart(summary[end]) {
			end--
		}
		summary = summary[:end] + "\n（完整成果见此 Submission。）"
	}
	message, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: submission.GroupID, TopicID: topic, ActorID: submission.SubmittedBy,
		RecipientIDs: []string{}, Content: fmt.Sprintf("成果已提交，尚未由本机用户接纳。\n任务：%s\nSubmission：%s\n任务版本：%d\n%s", submission.TaskID, submission.ID, submission.TaskRevision, summary)}, nil)
	if err != nil {
		return err
	}
	return coordinationDispatchWrite(ctx, tx, "INSERT INTO chat_submission_notices(submission_id,message_id) VALUES(?,?)", submission.ID, message.ID)
}
