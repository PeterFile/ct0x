package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"agentworkspace/internal/domain"
)

// HandbackWork records a bounded escalation to the current bound coordinator.
// It is intentionally a notice, not a decision, new assignment or Turn.
func (s *Store) HandbackWork(ctx context.Context, scope MemberToolScope, input domain.HandbackWorkInput, key string) (domain.TaskWorkHandback, error) {
	if !memberToolKeyValid(key) || !memberToolTextValid(input.Summary, domain.MaxWorkReportBytes) {
		return domain.TaskWorkHandback{}, ErrInvalid
	}
	return workMemberTx(ctx, s, scope, "记录任务移交失败", func(tx *sql.Tx) (out domain.TaskWorkHandback, err error) {
		f, attempt, err := taskWorkOwnerInTx(ctx, tx, scope)
		if err != nil {
			return out, err
		}
		p, coordinator := taskWorkParent(f), f.v.r.CoordinatorMemberID
		mutation := mutationScope{operation: "handback_work", groupID: p.GroupID, actorID: p.OwnerMemberID, key: key}
		payload := mutationPayload(p.AssignmentID, f.turnID, attempt, input.Summary)
		id, err := taskWorkMutationInTx(ctx, tx, mutation, payload, func(id string, replay bool) error {
			if replay {
				return nil
			}
			if _, err := s.taskWorkOwnerReadyInTx(ctx, tx, f, attempt); err != nil {
				return err
			}
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_work_handbacks WHERE group_id=? AND task_id=?)`, p.GroupID, p.TaskID).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return ErrConflict
			}
			body := taskWorkHandbackBody(id, p.TaskID, coordinator, input.Summary)
			m, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: p.GroupID, TopicID: p.TopicID, ActorID: p.OwnerMemberID, Content: body}, nil)
			if err != nil {
				return err
			}
			return coordinationDispatchWrite(ctx, tx, `INSERT INTO task_work_handbacks
	 (id,group_id,topic_id,task_id,task_revision,assignment_id,mandate_id,authority_generation,owner_member_id,
	 author_turn_id,author_attempt_id,coordinator_member_id,summary,notice_message_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				id, p.GroupID, p.TopicID, p.TaskID, p.TaskRevision, p.AssignmentID, p.MandateID, p.AuthorityGeneration, p.OwnerMemberID,
				f.turnID, attempt, coordinator, input.Summary, m.ID, time.Now().UTC().Format(timestampLayout))
		})
		if err != nil {
			return out, err
		}
		var stamp string
		var valid bool
		o := &out
		err = tx.QueryRowContext(ctx, `SELECT `+taskWorkParentColumns+`,p.id,p.author_turn_id,p.author_attempt_id,p.coordinator_member_id,
 p.summary,p.notice_message_id,p.created_at,
 m.content=? AND m.sender_id=p.owner_member_id AND NOT EXISTS(SELECT 1 FROM deliveries WHERE message_id=m.id)
 AND NOT EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=m.id)
 AND NOT EXISTS(SELECT 1 FROM task_work_deliveries WHERE message_id=m.id)
 FROM task_work_handbacks p JOIN coordination_assignment_dispatch origin ON origin.assignment_id=p.assignment_id
 JOIN messages m ON (m.id,m.group_id,m.topic_id)=(p.notice_message_id,p.group_id,p.topic_id)
 WHERE p.group_id=? AND p.id=?`, taskWorkHandbackBody(id, p.TaskID, coordinator, input.Summary), p.GroupID, id).Scan(append(taskWorkParentDest(&o.TaskWorkParent),
			&o.ID, &o.AuthorTurnID, &o.AuthorAttemptID, &o.CoordinatorID, &o.Summary, &o.NoticeMessageID, &stamp, &valid)...)
		if err != nil {
			return out, err
		}
		if !valid || out.TaskWorkParent != p || out.AuthorTurnID != f.turnID || out.AuthorAttemptID != attempt ||
			out.CoordinatorID != coordinator || out.HandbackWorkInput != input {
			return out, ErrConflict
		}
		o.CreatedAt, err = time.Parse(timestampLayout, stamp)
		return out, err
	})
}

func taskWorkHandbackBody(id, taskID, coordinator, summary string) string {
	return fmt.Sprintf("平台记录：任务协作问题已提交给当前协调员查阅。\n任务：%s\n移交：%s\n协调员：%s\n本通知不触发执行、变更分派或提交成果。\n\n负责成员说明：\n%s",
		taskID, id, coordinator, summary)
}
