package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

const chatDiscussionContextJSONBytes = 16 * 1024

var ErrChatDiscussionContextTooLarge = errors.New("聊天协作路径的元数据超过 16 KiB；请缩短独立回复要求或改用普通消息")

// ReadChatDiscussionContext returns nil for a valid intake with no handoff.
// It can run before assessment, but only on the claim that captured the intake.
// A successful context fits in 16 KiB of JSON (including newline), leaving
// space for the tool envelope and page. The caller still owns their size limit.
func (s *Store) ReadChatDiscussionContext(ctx context.Context, scope MemberToolScope, candidateID string) (*domain.ChatDiscussionContext, error) {
	const action = "读取聊天协作失败"
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return nil, memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	result, err := readChatDiscussionContextInTx(ctx, tx, scope, candidateID)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return nil, memberToolOperationError(action, err)
	}
	return boundChatDiscussionContext(result)
}

func boundChatDiscussionContext(c *domain.ChatDiscussionContext) (*domain.ChatDiscussionContext, error) {
	if c == nil {
		return nil, nil
	}
	for {
		err := checkChatDiscussionContextSize(c)
		if err == nil {
			return c, nil
		}
		if err != ErrChatDiscussionContextTooLarge || c.ContentOmitted {
			return nil, err
		}
		omitChatDiscussionBodies(c)
	}
}

func omitChatDiscussionBodies(c *domain.ChatDiscussionContext) {
	c.ContentOmitted, c.OriginContent = true, ""
	for i := range c.Events {
		c.Events[i].Message.Content = ""
	}
}

func checkChatDiscussionContextSize(c *domain.ChatDiscussionContext) error {
	data, err := json.Marshal(c)
	if err != nil {
		return errors.New("编码聊天协作上下文失败")
	}
	if len(data)+1 > chatDiscussionContextJSONBytes {
		return ErrChatDiscussionContextTooLarge
	}
	return nil
}

func chatDiscussionJSONStringSize(s string) int {
	data, _ := json.Marshal(s) // Strings always encode.
	return len(data)
}

func widerChatDiscussionRef(a, b string) string {
	if chatDiscussionJSONStringSize(b) > chatDiscussionJSONStringSize(a) {
		return b
	}
	return a
}

// Reserve the body-free context for the admitted prefix, all of its targeted
// returns, and the final resolution. A later push must pass its own check.
// This runs in the publication transaction; the probe is never persisted.
func reserveChatDiscussionReturnContextInTx(ctx context.Context, tx *sql.Tx, chain *chatDiscussionChain, invite domain.ChatDiscussionReceipt) error {
	probe := chain.ChatDiscussionContext
	probe.Events = append(slices.Clone(probe.Events), invite) // At a push, the prefix contains only invites.
	var rootConversation string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM conversations WHERE group_id=? AND member_id=? AND topic_id=?`,
		probe.GroupID, probe.RootInviterID, probe.TopicID).Scan(&rootConversation); err != nil {
		return err
	}
	if !utf8.ValidString(probe.GroupID) || !utf8.ValidString(probe.TopicID) || !utf8.ValidString(probe.RootInviterID) ||
		!utf8.ValidString(rootConversation) || !utf8.ValidString(invite.Message.Deliveries[0].ConversationID) {
		return ErrInvalid // JSON must preserve the exact route.
	}

	// Reuse freshly generated IDs for the widths of future IDs. Reserve the
	// longest UTC timestamp and arrival/head/owner fields of any visible state.
	msg := invite.Message
	msg.CreatedAt = time.Unix(0, 1).UTC() // Nine fractional digits.
	delivery := msg.Deliveries[0]
	delivery.CreatedAt = msg.CreatedAt
	probe.ActiveOwnerID, probe.ClosureReason = probe.RootInviterID, chatDiscussionRecipientLeftReason
	probe.HeadMessageID = msg.ID
	base := domain.ChatDiscussionEvent{OriginMessageID: probe.OriginMessageID, SourceMessageID: msg.ID, MessageID: msg.ID}
	for i := len(probe.Events) - 1; i >= 0; i-- {
		asked := probe.Events[i].Event
		probe.HeadMessageID = widerChatDiscussionRef(probe.HeadMessageID, asked.MessageID)
		probe.ActiveOwnerID = widerChatDiscussionRef(probe.ActiveOwnerID, asked.RecipientID)
		delivery.ConversationID = rootConversation
		if i > 0 {
			delivery.ConversationID = probe.Events[i-1].Message.Deliveries[0].ConversationID
		}
		delivery.RecipientID = asked.ActorID
		msg.SenderID, msg.Deliveries = asked.RecipientID, []domain.Delivery{delivery}
		e := base
		e.Kind, e.ActorID, e.RecipientID = domain.ChatDiscussionReply, msg.SenderID, delivery.RecipientID
		e.DeliveryID, e.InvitationMessageID = delivery.ID, asked.MessageID
		probe.Events = append(probe.Events, domain.ChatDiscussionReceipt{Message: msg, Event: e})
	}
	msg.SenderID, msg.Deliveries = probe.RootInviterID, nil
	base.Kind, base.ActorID, base.InvitationMessageID = domain.ChatDiscussionResolve, msg.SenderID, probe.Events[0].Message.ID
	probe.Events = append(probe.Events, domain.ChatDiscussionReceipt{Message: msg, Event: base})
	probe.IncomingMessageID = widerChatDiscussionRef(probe.OriginMessageID, probe.HeadMessageID)
	probe.ActiveInvitationMessageID = probe.HeadMessageID
	omitChatDiscussionBodies(&probe)
	return checkChatDiscussionContextSize(&probe)
}

func readChatDiscussionContextInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, candidateID string) (*domain.ChatDiscussionContext, error) {
	a, err := chatDiscussionArrivalInTx(ctx, tx, scope, candidateID)
	if err != nil {
		return nil, err
	}
	origin, err := chatDiscussionOriginInTx(ctx, tx, a, scope.MemberID)
	if err != nil || origin == "" {
		return nil, err
	}
	chain, err := chatDiscussionChainInTx(ctx, tx, domain.ThreadRef{GroupID: scope.Claim.GroupID, TopicID: scope.TopicID}, origin)
	if err != nil {
		return nil, err
	}
	if origin == a.messageID && (chain == nil || chain.RootInviterID != scope.MemberID) {
		return nil, nil
	}
	if chain == nil {
		return nil, ErrMemberToolDenied
	}
	chain.IncomingMessageID = a.messageID
	return &chain.ChatDiscussionContext, nil
}

type chatDiscussionArrival struct {
	messageID, deliveryID, senderID, sourceKind string
	hasCommandSource                            bool
}

func chatDiscussionArrivalInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, candidateID string) (chatDiscussionArrival, error) {
	var a chatDiscussionArrival
	if err := chatIntakePurposeInTx(ctx, tx, scope, candidateID); err != nil {
		return a, err
	}
	// Prove the candidate still matches the live delivery, not just its captured
	// purpose. A role receipt disqualifies the turn even if that receipt is damaged.
	var capturedID, commandID, commandKind string
	err := tx.QueryRowContext(ctx, `SELECT c.message_id,d.id,m.sender_id,i.source_kind,i.source_id,coalesce(s.id,''),coalesce(s.kind,'')
	 FROM chat_candidates c JOIN chat_intake_executions i ON i.candidate_id=c.id
	 JOIN turns t ON t.id=c.turn_id AND t.claim_token=?
	 JOIN deliveries d ON d.id=t.delivery_id AND d.turn_id=t.id AND d.message_id=c.message_id
	    AND d.group_id=c.group_id AND d.topic_id=c.topic_id AND d.recipient_id=c.member_id AND d.conversation_id=t.conversation_id
	 JOIN messages m ON m.id=d.message_id AND m.group_id=d.group_id AND m.topic_id=d.topic_id
	 JOIN message_log l ON l.message_id=m.id AND l.sequence=c.generation
	 LEFT JOIN message_command_sources s ON s.message_id=m.id
	 WHERE c.id=? AND ((t.state='starting' AND d.state='queued') OR (t.state='running' AND d.state='accepted'))
	 AND NOT EXISTS(SELECT 1 FROM coordination_dispatch_state WHERE delivery_id=d.id OR message_id=m.id)
	 AND NOT EXISTS(SELECT 1 FROM coordination_assignment_dispatch WHERE delivery_id=d.id OR message_id=m.id)`,
		scope.Claim.Token, candidateID).Scan(&a.messageID, &a.deliveryID, &a.senderID, &a.sourceKind, &capturedID, &commandID, &commandKind)
	if err == sql.ErrNoRows {
		return a, ErrMemberToolDenied
	}
	if err != nil {
		return a, err
	}
	a.hasCommandSource = commandID != ""
	if a.hasCommandSource && (commandID != capturedID || commandKind != a.sourceKind) {
		return a, ErrMemberToolDenied
	}
	if a.sourceKind == "local_user_chat" && (!a.hasCommandSource || a.senderID != "user") {
		return a, ErrMemberToolDenied
	}
	return a, nil
}

func chatDiscussionOriginInTx(ctx context.Context, tx *sql.Tx, a chatDiscussionArrival, memberID string) (string, error) {
	if a.sourceKind == "local_user_chat" {
		return a.messageID, nil
	}
	var origin, delivery, actor, recipient string
	var extraTarget bool
	err := tx.QueryRowContext(ctx, `SELECT origin_message_id,coalesce(delivery_id,''),actor_id,coalesce(recipient_id,''),
	 EXISTS(SELECT 1 FROM deliveries d WHERE d.message_id=e.message_id AND d.id IS NOT e.delivery_id)
	 FROM chat_discussion_events e WHERE message_id=?`, a.messageID).Scan(&origin, &delivery, &actor, &recipient, &extraTarget)
	if err == sql.ErrNoRows {
		return "", nil // A legacy message never implies a structured route.
	}
	if err != nil {
		return "", err
	}
	if extraTarget || !a.hasCommandSource || a.sourceKind != "member" || a.senderID != actor || delivery != a.deliveryID || recipient != memberID {
		return "", ErrMemberToolDenied
	}
	return origin, nil
}

func chatDiscussionChainInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, origin string) (*chatDiscussionChain, error) {
	var root, content string
	var hops, wakes int
	err := tx.QueryRowContext(ctx, `SELECT c.root_inviter_id,m.content,c.max_invite_hops,c.max_targeted_wakes
	 FROM chat_discussions c JOIN messages m ON m.id=c.origin_message_id AND m.sender_id='user'
	 JOIN message_command_sources s ON s.message_id=m.id AND s.kind='local_user_chat'
	 WHERE c.origin_message_id=? AND c.group_id=? AND c.topic_id=?`, origin, thread.GroupID, thread.TopicID).Scan(&root, &content, &hops, &wakes)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c := newChatDiscussionChain(thread, origin, root, hops, wakes)
	c.OriginContent = content
	// Indexed path order; read at most the policy-sized path plus one so an
	// inconsistent overlong path cannot silently masquerade as a valid prefix.
	rows, err := tx.QueryContext(ctx, chatDiscussionReceiptSelect+` WHERE origin_message_id=? ORDER BY step LIMIT ?`, origin, 2*min(hops, wakes/2)+2)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanChatDiscussionReceipt(rows)
		if err != nil {
			return nil, err
		}
		if err := c.advance(r); err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(c.Events) == 0 {
		return nil, ErrMemberToolDenied
	}
	if !c.Closed {
		// The departure trigger records the unclaimed case. After a claim,
		// rely only on the bound turn's terminal state and trusted normalized
		// receipt. Project closure without settling or releasing anything here.
		head := c.Events[len(c.Events)-1].Event
		var departed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deliveries d WHERE d.id=? AND d.state='failed'
		 AND ((d.turn_id='' AND d.error=?) OR EXISTS(
		   SELECT 1 FROM turns t JOIN members m ON m.group_id=t.group_id AND m.id=t.member_id
		   WHERE t.id=d.turn_id AND t.delivery_id=d.id AND t.group_id=d.group_id
		     AND t.conversation_id=d.conversation_id AND t.member_id=d.recipient_id
		     AND t.state='not_started' AND m.status='left' AND t.accepted_at IS NULL AND t.native_session_id=''
		     AND CASE WHEN json_valid(t.receipt_json) THEN
		       json_extract(t.receipt_json,'$.outcome')='not_started'
		       AND json_type(t.receipt_json,'$.started')='false' AND json_type(t.receipt_json,'$.accepted')='false' END
		 )))`, head.DeliveryID, chatDiscussionRecipientLeftReason).Scan(&departed); err != nil {
			return nil, err
		}
		if departed {
			c.Closed, c.ClosureReason = true, chatDiscussionRecipientLeftReason
			c.ActiveInvitationMessageID, c.ActiveOwnerID = "", ""
		}
	}
	return c, nil
}

const chatDiscussionReceiptSelect = `SELECT message_id,origin_message_id,source_message_id,kind,actor_id,coalesce(recipient_id,''),
 coalesce(delivery_id,''),coalesce(related_invitation_id,''),expected_reply,group_id,topic_id,message_json FROM chat_discussion_events`

func scanChatDiscussionReceipt(row scanner) (r domain.ChatDiscussionReceipt, err error) {
	var related, groupID, topicID, data string
	e := &r.Event
	if err = row.Scan(&e.MessageID, &e.OriginMessageID, &e.SourceMessageID, &e.Kind, &e.ActorID, &e.RecipientID,
		&e.DeliveryID, &related, &e.ExpectedReply, &groupID, &topicID, &data); err != nil {
		return
	}
	e.InvitationMessageID = related
	if e.Kind == domain.ChatDiscussionInvite {
		e.InvitationMessageID, e.ParentInvitationMessageID = e.MessageID, related
	}
	if err = json.Unmarshal([]byte(data), &r.Message); err != nil {
		return
	}
	m := r.Message
	if m.ID != e.MessageID || m.SenderID != e.ActorID || m.GroupID != groupID || m.TopicID != topicID || m.SourceKind != "member" || m.SourceID == "" {
		return r, ErrMemberToolDenied
	}
	if e.DeliveryID == "" && len(m.Deliveries) != 0 || e.DeliveryID != "" &&
		(len(m.Deliveries) != 1 || m.Deliveries[0].ID != e.DeliveryID || m.Deliveries[0].RecipientID != e.RecipientID) {
		return r, ErrMemberToolDenied
	}
	return r, nil
}
