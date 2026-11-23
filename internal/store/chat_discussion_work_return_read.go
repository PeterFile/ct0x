package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"agentworkspace/internal/domain"
)

// Reread the exact typed B record and its separate immutable non-waking notice.
// The owner/attempt relation is historical; this read never authorizes a write.
func chatDiscussionWorkHandbackInTx(ctx context.Context, tx *sql.Tx, groupID, id string) (h domain.TaskWorkHandback, err error) {
	var stamp, body string
	err = tx.QueryRowContext(ctx, `SELECT `+taskWorkParentColumns+`,p.id,p.author_turn_id,p.author_attempt_id,p.coordinator_member_id,
	 p.summary,p.notice_message_id,p.created_at,m.content
	 FROM task_work_handbacks p JOIN coordination_assignment_dispatch origin ON origin.assignment_id=p.assignment_id
	 JOIN messages m ON (m.id,m.group_id,m.topic_id,m.sender_id)=(p.notice_message_id,p.group_id,p.topic_id,p.owner_member_id)
	 WHERE p.group_id=? AND p.id=? AND EXISTS(`+taskWorkSavedOwnerOrigin+` AND r.coordinator_member_id=p.coordinator_member_id)
	 AND NOT EXISTS(SELECT 1 FROM deliveries WHERE message_id=m.id)
	 AND NOT EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=m.id)
	 AND NOT EXISTS(SELECT 1 FROM task_work_deliveries WHERE message_id=m.id)`, groupID, id).Scan(append(taskWorkParentDest(&h.TaskWorkParent),
		&h.ID, &h.AuthorTurnID, &h.AuthorAttemptID, &h.CoordinatorID, &h.Summary, &h.NoticeMessageID, &stamp, &body)...)
	if err != nil {
		return h, coordinationRoleRelationError(err)
	}
	if !memberToolTextValid(h.Summary, domain.MaxWorkReportBytes) || body != taskWorkHandbackBody(h.ID, h.TaskID, h.CoordinatorID, h.Summary) {
		return h, ErrMemberToolDenied
	}
	h.CreatedAt, err = time.Parse(timestampLayout, stamp)
	return h, err
}

// Read by the immutable result key. An absent marker is different from a
// damaged one, including when the call is a replay or the root is now left.
func chatDiscussionWorkReturnReceiptInTx(ctx context.Context, tx *sql.Tx, h domain.TaskWorkHandback) (*domain.ChatDiscussionWorkReturnReceipt, error) {
	r := domain.ChatDiscussionWorkReturnReceipt{HandbackID: h.ID, HandbackNoticeMessageID: h.NoticeMessageID, SenderID: h.OwnerMemberID}
	var body string
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT w.group_id,w.topic_id,w.origin_message_id,w.invitation_message_id,w.message_id,coalesce(w.delivery_id,''),
	 coalesce(c.root_inviter_id,''), coalesce(d.conversation_id,''), coalesce(m.content,''),
	 coalesce(m.sender_id=:sender,0) AND EXISTS(
	   SELECT 1 FROM messages o JOIN message_command_sources s ON s.message_id=o.id AND s.kind='local_user_chat'
	   JOIN members root ON (root.id,root.group_id)=(c.root_inviter_id,c.group_id)
	   JOIN chat_discussion_events e ON (e.origin_message_id,e.group_id,e.topic_id)=(w.origin_message_id,w.group_id,w.topic_id)
	   JOIN mandates n ON (n.id,n.group_id,n.topic_id,n.task_id)=(:mandate,w.group_id,w.topic_id,:task)
	   JOIN coordination_triggers t ON (t.id,t.group_id,t.topic_id,t.message_id)=(n.origin_trigger_id,e.group_id,e.topic_id,e.message_id)
	   WHERE (o.id,o.group_id,o.topic_id)=(w.origin_message_id,w.group_id,w.topic_id) AND o.sender_id='user'
	   AND e.message_id=w.invitation_message_id AND e.kind='invite'
	   AND NOT EXISTS(SELECT 1 FROM chat_discussion_events later WHERE later.origin_message_id=e.origin_message_id AND later.step>e.step))
	 AND (w.delivery_id IS NULL OR (d.id IS NOT NULL
	   AND (d.state IN ('accepted','failed') OR (d.state='queued' AND d.error='')) AND EXISTS(
	   SELECT 1 FROM conversations v WHERE (v.id,v.group_id,v.topic_id,v.member_id)=(d.conversation_id,d.group_id,d.topic_id,d.recipient_id))
	   AND NOT EXISTS(SELECT 1 FROM turns t WHERE t.delivery_id=d.id AND t.id<>d.turn_id)
	   AND (d.turn_id='' OR EXISTS(SELECT 1 FROM turns t WHERE (t.id,t.delivery_id,t.group_id,t.member_id,t.conversation_id)=
	     (d.turn_id,d.id,d.group_id,d.recipient_id,d.conversation_id)))))
	 AND NOT EXISTS(SELECT 1 FROM deliveries other WHERE other.message_id=w.message_id AND other.id IS NOT w.delivery_id)
	 AND NOT EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=w.message_id)
	 AND NOT EXISTS(SELECT 1 FROM turns WHERE reply_message_id=w.message_id AND reply_message_id<>'')
	 AND NOT EXISTS(SELECT 1 FROM coordination_dispatch_state WHERE delivery_id=d.id OR message_id=w.message_id)
	 AND NOT EXISTS(SELECT 1 FROM coordination_assignment_dispatch WHERE delivery_id=d.id OR message_id=w.message_id)
	 AND NOT EXISTS(SELECT 1 FROM task_work_deliveries WHERE delivery_id=d.id OR message_id=w.message_id)
	 FROM chat_discussion_work_returns w
	 LEFT JOIN chat_discussions c ON (c.origin_message_id,c.group_id,c.topic_id)=(w.origin_message_id,w.group_id,w.topic_id)
	 LEFT JOIN messages m ON (m.id,m.group_id,m.topic_id)=(w.message_id,w.group_id,w.topic_id)
	 LEFT JOIN deliveries d ON (d.id,d.message_id,d.group_id,d.topic_id,d.recipient_id)=
	   (w.delivery_id,w.message_id,w.group_id,w.topic_id,c.root_inviter_id)
	 WHERE w.handback_id=:handback`, sql.Named("sender", h.OwnerMemberID), sql.Named("mandate", h.MandateID),
		sql.Named("task", h.TaskID), sql.Named("handback", h.ID)).Scan(&r.GroupID, &r.TopicID, &r.OriginMessageID, &r.InvitationMessageID,
		&r.MessageID, &r.DeliveryID, &r.RecipientID, &r.ConversationID, &body, &valid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !valid || r.ThreadRef != h.ThreadRef || r.MessageID == h.NoticeMessageID || body != chatDiscussionWorkReturnBody(r.OriginMessageID, h) {
		return nil, ErrMemberToolDenied
	}
	return &r, nil
}

type chatDiscussionWorkReturnReadError struct{ cause error }

func (e *chatDiscussionWorkReturnReadError) Error() string { return "读取讨论工作回传失败" }
func (e *chatDiscussionWorkReturnReadError) Unwrap() error { return e.cause }

// ResolveChatDiscussionWorkReturnDelivery probes immutable routing evidence,
// not readiness. Only ErrNotFound itself means verified absence. For a return,
// the trusted handler must pass this receipt to the explicit queued resolver
// and still fence the actual claim before launch. Generic SourceKind is not
// typed evidence, and a damaged marker is never a fallback to untyped chat.
func (s *Store) ResolveChatDiscussionWorkReturnDelivery(ctx context.Context, groupID, deliveryID string) (domain.ChatDiscussionWorkReturnReceipt, error) {
	var zero domain.ChatDiscussionWorkReturnReceipt
	if !validTriggerID(groupID) || !validTriggerID(deliveryID) {
		return zero, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return zero, &chatDiscussionWorkReturnReadError{err}
	}
	defer tx.Rollback()
	installed, err := s.chatDiscussionWorkReturnInstalledInTx(ctx, tx)
	var r *domain.ChatDiscussionWorkReturnReceipt
	if err == nil && installed {
		r, err = chatDiscussionWorkReturnDeliveryInTx(ctx, tx, groupID, deliveryID)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return zero, &chatDiscussionWorkReturnReadError{err}
	}
	if r == nil {
		return zero, ErrNotFound
	}
	return *r, nil
}

// Read raw markers by either exact unique key before proving any parent. A
// marker with a damaged delivery reference must still be found by message ID.
// The caller must already have verified the schema in this transaction.
func chatDiscussionWorkReturnDeliveryInTx(ctx context.Context, tx *sql.Tx, groupID, deliveryID string) (*domain.ChatDiscussionWorkReturnReceipt, error) {
	var handbackID, group, delivery string
	var markers int
	err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(min(handback_id),''),coalesce(min(group_id),''),coalesce(min(delivery_id),'')
	 FROM main.chat_discussion_work_returns WHERE delivery_id=? OR message_id=(SELECT message_id FROM main.deliveries WHERE id=?)`,
		deliveryID, deliveryID).Scan(&markers, &handbackID, &group, &delivery)
	if err != nil || markers == 0 {
		return nil, err
	}
	if group != groupID || delivery != deliveryID || markers != 1 {
		return nil, ErrMemberToolDenied
	}
	h, err := chatDiscussionWorkHandbackInTx(ctx, tx, groupID, handbackID)
	if err != nil {
		return nil, err
	}
	r, err := chatDiscussionWorkReturnReceiptInTx(ctx, tx, h)
	if err != nil {
		return nil, err
	}
	if r == nil || r.DeliveryID != deliveryID {
		return nil, ErrMemberToolDenied
	}
	return r, nil
}
