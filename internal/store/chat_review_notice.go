package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

// Only a new Review emits a notice. Historical receipts and pre-v18 schemas
// never backfill one; current chat policy is not authority for a user's Review.
func chatReviewNoticeInTx(ctx context.Context, tx *sql.Tx, decision domain.ReviewDecision) error {
	var available bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='chat_review_notices')").Scan(&available); err != nil {
		return err
	}
	if !available {
		return nil
	}
	var topic string
	err := tx.QueryRowContext(ctx, `SELECT c.topic_id FROM mandates m INDEXED BY mandates_group_task
	 JOIN chat_assessments a ON a.decision_id=m.decision_id
	 JOIN chat_candidates c ON c.id=a.candidate_id AND c.group_id=m.group_id AND c.topic_id=m.topic_id
	 WHERE m.group_id=? AND m.task_id=?`, decision.GroupID, decision.TaskID).Scan(&topic)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	// Bound only the display; preserve the full original reason in the Review.
	reason := strings.ToValidUTF8(decision.Reason, "�")
	if strings.TrimSpace(reason) == "" {
		reason = "（未提供理由。）"
	}
	const reasonBytes = 2048
	if len(reason) > reasonBytes {
		end := reasonBytes
		for !utf8.RuneStart(reason[end]) {
			end--
		}
		reason = reason[:end] + "…（已截断，完整理由见 Review。）"
	}
	result, boundary := "已接纳（accepted）", ""
	if decision.Outcome == domain.ReviewOutcomeRejected {
		result = "已拒收（rejected）"
		boundary = "拒收仅针对以上原版本：保留原版本和拒收记录；后续修复仍需适用授权，本通知不创建任务或触发执行。\n"
	}
	content := fmt.Sprintf("平台记录：本机用户%s此版本成果。\n任务：%s\nSubmission：%s\nReview：%s\n审查任务版本：%d\n这不是 Agent 共识，也不是用户聊天原话；不代表原生执行成功。\n%s用户理由（展示摘要，完整内容见 Review）：\n%s",
		result, decision.TaskID, decision.SubmissionID, decision.ID, decision.TaskRevision, boundary, reason)
	// Explicitly public-only: no routing, deliveries, turns, or resource changes.
	message, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: decision.GroupID, TopicID: topic,
		ActorID: "user", RecipientIDs: []string{}, Content: content}, nil)
	if err != nil {
		return err
	}
	return coordinationDispatchWrite(ctx, tx, "INSERT INTO chat_review_notices(review_id,message_id) VALUES(?,?)", decision.ID, message.ID)
}
