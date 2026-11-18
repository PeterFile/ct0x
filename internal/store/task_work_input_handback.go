package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"agentworkspace/internal/domain"
)

// HandBackWorkInput is the trusted internal, unkeyed scope escalation path.
func (s *Store) HandBackWorkInput(ctx context.Context, scope MemberToolScope, inputID, reason string) (domain.TaskWorkInput, error) {
	return s.handleWorkInput(ctx, scope, workInputReturnOperation, inputID, reason, "")
}

// HandBackWorkInputWithKey records an actor-bound scope escalation, not an
// acknowledgement or grant. It uses the same key rules as AcknowledgeWorkInputWithKey
// in a separate operation namespace. The reason is also part of the exact
// payload. A retry preserves the original reason, disposition, policy basis,
// question, and notice.
func (s *Store) HandBackWorkInputWithKey(ctx context.Context, scope MemberToolScope, inputID, reason, key string) (domain.TaskWorkInput, error) {
	if !memberToolKeyValid(key) {
		return domain.TaskWorkInput{}, ErrInvalid
	}
	return s.handleWorkInput(ctx, scope, workInputReturnOperation, inputID, reason, key)
}

type workInputReturnDecision struct {
	disposition    domain.TaskWorkInputReturnDisposition
	policyRevision int64
	policySourceID string
}

// Reuse the original admission/scope gate after proving the live assignment.
// Nothing from the reason or proposed constraint history is authority evidence.
func (s *Store) decideWorkInputReturnInTx(ctx context.Context, tx *sql.Tx, i domain.TaskWorkInput, v assignmentSnapshot) (workInputReturnDecision, error) {
	policy, err := chatPolicyInTx(ctx, tx, i.ThreadRef, 0)
	if err != nil {
		return workInputReturnDecision{}, err
	}
	d := workInputReturnDecision{disposition: domain.TaskWorkInputReturnOriginalAuthorizationUnavailable,
		policyRevision: policy.Revision, policySourceID: policy.ConfigurationSourceID}
	if policy.Enabled && policy.Action == domain.ChatActionTextSubmission && policy.AuthorizedBy == "user" {
		_, err := s.taskWorkOwnerChatActionInTx(ctx, tx, taskWorkFacts{v: v, d: taskWorkRecord(v, "", "")})
		switch {
		case err == nil:
			d.disposition = domain.TaskWorkInputReturnSeparateAuthorizationRequired
		case errors.Is(err, errTaskWorkChatScope), errors.Is(err, ErrTaskInvestigationNotReady):
		default:
			return workInputReturnDecision{}, err
		}
	}
	return d, nil
}

func workInputReturnNotice(i domain.TaskWorkInput, h *domain.TaskWorkInputReturn) string {
	return fmt.Sprintf("这条后续要求已交回确认，未执行、创建新任务或扩大授权。\n原消息：%s\n执行成员说明（不构成授权）：%q\n采用的聊天策略版本：%d\n%s",
		i.ReplyMessageID, h.Reason, h.PolicyRevision, h.Question)
}

func insertWorkInputReturnInTx(ctx context.Context, tx *sql.Tx, role CoordinationToolScope, i domain.TaskWorkInput, reason string, d workInputReturnDecision) (*domain.TaskWorkInputReturn, error) {
	question := fmt.Sprintf("原任务 %s 的当前授权不可用，是否先核对本话题的授权？", i.TaskID)
	if d.disposition == domain.TaskWorkInputReturnSeparateAuthorizationRequired {
		question = fmt.Sprintf("原任务 %s 仍受原授权约束，是否要另开一项工作并为额外要求单独授权？", i.TaskID)
	}
	h := &domain.TaskWorkInputReturn{MemberID: role.Member.MemberID, TurnID: role.Member.Claim.TurnID, AttemptID: role.AttemptID,
		Reason: reason, Question: question, Disposition: d.disposition, PolicyRevision: d.policyRevision, PolicySourceID: d.policySourceID}
	notice, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: i.GroupID, TopicID: i.TopicID, ActorID: h.MemberID,
		Content: workInputReturnNotice(i, h), RecipientIDs: []string{}}, nil)
	if err != nil {
		return nil, err
	}
	h.NoticeMessageID, h.ReturnedAt = notice.ID, notice.CreatedAt
	if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO task_work_input_returns(input_id,turn_id,attempt_id,member_id,reason,question,notice_message_id,returned_at,
	 disposition,policy_revision,policy_source_id) VALUES(?,?,?,?,?,?,?,?,?,?,nullif(?,''))`, i.ID, h.TurnID, h.AttemptID, h.MemberID, h.Reason, h.Question,
		h.NoticeMessageID, h.ReturnedAt.Format(timestampLayout), h.Disposition, h.PolicyRevision, h.PolicySourceID); err != nil {
		return nil, err
	}
	return h, nil
}

func verifyWorkInputReturnNoticeInTx(ctx context.Context, tx *sql.Tx, i domain.TaskWorkInput) error {
	h := i.Return
	content := workInputReturnNotice(i, h)
	var published bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages n JOIN message_log l
	 ON (l.message_id,l.group_id,l.topic_id)=(n.id,n.group_id,n.topic_id)
	 WHERE (n.id,n.group_id,n.topic_id,n.sender_id,n.content)=(?,?,?,?,?)
	 AND NOT EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=n.id)
	 AND NOT EXISTS(SELECT 1 FROM deliveries WHERE message_id=n.id))`, h.NoticeMessageID, i.GroupID, i.TopicID, h.MemberID, content).Scan(&published)
	if err == nil && !published {
		return ErrConflict
	}
	return err
}
