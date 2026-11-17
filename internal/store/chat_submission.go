package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

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
