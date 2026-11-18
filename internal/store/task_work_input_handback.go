package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"agentworkspace/internal/domain"
)

// HandBackWorkInput is the trusted internal, unkeyed scope escalation path.
func (s *Store) HandBackWorkInput(ctx context.Context, scope MemberToolScope, inputID, reason string) (domain.TaskWorkInput, error) {
	return s.handleWorkInput(ctx, scope, workInputReturnOperation, inputID, reason, "")
}

// HandBackWorkInputWithKey records an actor-bound scope escalation, not an
// acknowledgement or grant. It uses the same key rules as AcknowledgeWorkInputWithKey
// in a separate operation namespace. The reason is also part of the exact
// payload. A retry preserves the original reason, question, and notice.
func (s *Store) HandBackWorkInputWithKey(ctx context.Context, scope MemberToolScope, inputID, reason, key string) (domain.TaskWorkInput, error) {
	if !memberToolKeyValid(key) {
		return domain.TaskWorkInput{}, ErrInvalid
	}
	return s.handleWorkInput(ctx, scope, workInputReturnOperation, inputID, reason, key)
}

func workInputReturnNotice(i domain.TaskWorkInput, reason string) (question, content string) {
	question = fmt.Sprintf("请确认：原任务 %s 在现有授权内应如何处理这条后续要求？", i.TaskID)
	content = fmt.Sprintf("原任务收到一条超出当前授权的后续要求，已交回确认，未执行或扩大授权。\n原消息：%s\n交回依据：%s\n%s", i.ReplyMessageID, reason, question)
	return
}

func insertWorkInputReturnInTx(ctx context.Context, tx *sql.Tx, role CoordinationToolScope, i domain.TaskWorkInput, reason string) (*domain.TaskWorkInputReturn, error) {
	question, content := workInputReturnNotice(i, reason)
	notice, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: i.GroupID, TopicID: i.TopicID, ActorID: role.Member.MemberID, Content: content, RecipientIDs: []string{}}, nil)
	if err != nil {
		return nil, err
	}
	h := &domain.TaskWorkInputReturn{MemberID: role.Member.MemberID, TurnID: role.Member.Claim.TurnID, AttemptID: role.AttemptID,
		Reason: reason, Question: question, NoticeMessageID: notice.ID, ReturnedAt: time.Now().UTC()}
	if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO task_work_input_returns(input_id,turn_id,attempt_id,member_id,reason,question,notice_message_id,returned_at)
	 VALUES(?,?,?,?,?,?,?,?)`, i.ID, h.TurnID, h.AttemptID, h.MemberID, h.Reason, h.Question, h.NoticeMessageID, h.ReturnedAt.Format(timestampLayout)); err != nil {
		return nil, err
	}
	return h, nil
}

func verifyWorkInputReturnNoticeInTx(ctx context.Context, tx *sql.Tx, i domain.TaskWorkInput) error {
	h := i.Return
	question, content := workInputReturnNotice(i, h.Reason)
	var published bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages n JOIN message_log l
	 ON (l.message_id,l.group_id,l.topic_id)=(n.id,n.group_id,n.topic_id)
	 WHERE (n.id,n.group_id,n.topic_id,n.sender_id,n.content)=(?,?,?,?,?)
	 AND NOT EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=n.id)
	 AND NOT EXISTS(SELECT 1 FROM deliveries WHERE message_id=n.id))`, h.NoticeMessageID, i.GroupID, i.TopicID, h.MemberID, content).Scan(&published)
	if err == nil && (!published || h.Question != question) {
		return ErrConflict
	}
	return err
}
