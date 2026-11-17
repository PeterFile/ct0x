package store

import (
	"strings"
	"testing"
)

// Formatting/message-transaction test only. It rolls back every call and does
// not insert a workspace scope, operation, publication or authorization receipt.
func TestWorkspaceOperationNoticeFactsAreBoundedAndPublicOnly(t *testing.T) {
	f, _, role := assignmentBoundWorker(t)
	for _, tc := range []struct{ outcome, contains string }{{"pending", "尚未确认"}, {"done", "写入及同步已确认完成"}, {"unchanged", "确认未改动"}, {"unknown", "结果未确认"}, {"applied_sync_error", "已应用"}} {
		t.Run(tc.outcome, func(t *testing.T) {
			tx, err := f.s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			o := WorkspaceOperation{ID: "notice-operation", TaskID: role.Provenance.TaskID, GroupID: f.groupID, MemberID: f.worker, Input: WorkspaceOperationInput{RelativePath: strings.Repeat("目录", 1000)}}
			var result *WorkspaceOperationResult
			if tc.outcome != "pending" {
				result = &WorkspaceOperationResult{Outcome: tc.outcome}
			}
			id, err := workspaceOperationNoticeInTx(ctx, tx, role, o, result)
			if err != nil || id == "" {
				t.Fatalf("platform fact message: %q,%v", id, err)
			}
			var group, topic, sender, content string
			var deliveries, commands int
			err = tx.QueryRow(`SELECT m.group_id,m.topic_id,m.sender_id,m.content,
			 (SELECT count(*) FROM deliveries WHERE message_id=m.id),
			 (SELECT count(*) FROM message_command_sources WHERE message_id=m.id) FROM messages m WHERE m.id=?`, id).Scan(&group, &topic, &sender, &content, &deliveries, &commands)
			if err != nil || group != f.groupID || topic != role.Member.TopicID || sender != f.worker || deliveries != 0 || commands != 0 || len(content) > 2048 || !strings.Contains(content, tc.contains) {
				t.Fatalf("unbounded/fabricated/routed progress: %q,%q,%q,%q,%d,%d,%v", group, topic, sender, content, deliveries, commands, err)
			}
			if tc.outcome == "unknown" && strings.Contains(content, "已确认完成") {
				t.Fatal("unknown progress fabricated success")
			}
		})
	}
}
