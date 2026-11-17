package store

import (
	"context"
	"database/sql"
	"fmt"
	"unicode/utf8"
)

func workspaceOperationNoticeText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit] + "…"
}

// The caller must atomically attach this message to its immutable intent or
// receipt. Empty recipients create visibility only, never wakeup or user consent.
func workspaceOperationNoticeInTx(ctx context.Context, tx *sql.Tx, role CoordinationToolScope, o WorkspaceOperation, result *WorkspaceOperationResult) (string, error) {
	if o.GroupID != role.Member.Claim.GroupID || o.MemberID != role.Member.MemberID || o.MemberID == "user" {
		return "", ErrMemberToolDenied
	}
	status := "文件操作意图已保存，实际文件结果尚未确认。"
	if result != nil {
		switch result.Outcome {
		case "done":
			status = "文件写入及同步已确认完成，尚未提交或接纳成果。"
		case "unchanged":
			status = "确认未改动文件；本记录不是成果提交或接纳。"
		case "unknown":
			status = "文件操作结果未确认，须对账；保留资源占用，不自动重试或回滚。"
		case "applied_sync_error":
			status = "文件已应用，但完整同步或无错误状态尚未确认，须对账；保留资源占用，不自动重试或回滚。"
		default:
			return "", ErrInvalid
		}
	}
	content := fmt.Sprintf("平台记录：%s\n任务：%s\n文件操作：%s\n相对路径（展示摘要）：%s\n这是持久操作证据，不是用户许可或 Agent 原话。", status,
		workspaceOperationNoticeText(o.TaskID, 256), workspaceOperationNoticeText(o.ID, 256), workspaceOperationNoticeText(o.Input.RelativePath, 512))
	message, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: o.GroupID, TopicID: role.Member.TopicID,
		ActorID: role.Member.MemberID, Content: content, RecipientIDs: []string{}}, nil)
	if err != nil {
		return "", err
	}
	return message.ID, nil
}
