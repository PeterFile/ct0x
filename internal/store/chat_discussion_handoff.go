package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"agentworkspace/internal/domain"
)

const (
	defaultDiscussionInviteHops          = 2
	defaultDiscussionTargetedWakes       = 4
	chatDiscussionExpectedReplyJSONBytes = 1024
)

// ApplyChatDiscussion publishes an assessed handoff step using only the bound
// intake and the current position in the path. It grants no work authority.
func (s *Store) ApplyChatDiscussion(ctx context.Context, scope MemberToolScope, candidateID string, input domain.ChatDiscussionAction, key string) (domain.ChatDiscussionReceipt, error) {
	const action = "保存聊天协作失败"
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return domain.ChatDiscussionReceipt{}, memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	r, err := applyChatDiscussionInTx(ctx, tx, scope, candidateID, input, key)
	if err == nil {
		err = tx.Commit()
	}
	if err == ErrChatDiscussionContextTooLarge {
		return domain.ChatDiscussionReceipt{}, err
	}
	if err != nil {
		return domain.ChatDiscussionReceipt{}, memberToolOperationError(action, err)
	}
	return r, nil
}

func applyChatDiscussionInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, candidateID string, input domain.ChatDiscussionAction, key string) (domain.ChatDiscussionReceipt, error) {
	var r domain.ChatDiscussionReceipt
	a, err := chatDiscussionArrivalInTx(ctx, tx, scope, candidateID)
	if err != nil {
		return r, err
	}
	if !memberToolKeyValid(key) {
		return r, ErrInvalid
	}
	var assessed bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM chat_assessments WHERE candidate_id=?)", candidateID).Scan(&assessed); err != nil {
		return r, err
	}
	if !assessed {
		return r, ErrMemberToolDenied
	}
	m := mutationScope{operation: "create_message", actorID: scope.MemberID, groupID: scope.Claim.GroupID, key: key}
	payload := mutationPayload("chat_discussion/v1", candidateID, input.Kind, input.Content, input.RecipientID, input.ExpectedReply)
	id, replay, err := lookupMutation(ctx, tx, m, payload)
	if err != nil {
		return r, err
	}
	origin, err := chatDiscussionOriginInTx(ctx, tx, a, scope.MemberID)
	if err != nil {
		return r, err
	}
	if origin == "" {
		return r, ErrMemberToolDenied
	}
	if replay {
		// Authorization and the assessment still apply, but neither later path
		// progress nor mutable delivery status changes an original receipt.
		return scanChatDiscussionReceipt(tx.QueryRowContext(ctx, chatDiscussionReceiptSelect+`
		 WHERE message_id=? AND candidate_id=? AND origin_message_id=?`, id, candidateID, origin))
	}
	if !validChatDiscussionAction(input) {
		return r, ErrInvalid
	}
	thread := domain.ThreadRef{GroupID: scope.Claim.GroupID, TopicID: scope.TopicID}
	chain, err := chatDiscussionChainInTx(ctx, tx, thread, origin)
	if err != nil {
		return r, err
	}
	if chain == nil {
		if origin != a.messageID {
			return r, ErrMemberToolDenied
		}
		chain = newChatDiscussionChain(thread, origin, scope.MemberID, defaultDiscussionInviteHops, defaultDiscussionTargetedWakes)
	}
	e, err := chain.plan(a.messageID, scope.MemberID, input)
	if err != nil {
		return r, err
	}
	targets := []string{}
	if e.RecipientID != "" {
		var status domain.MemberStatus
		err := tx.QueryRowContext(ctx, "SELECT status FROM members WHERE group_id=? AND id=?", thread.GroupID, e.RecipientID).Scan(&status)
		if err == sql.ErrNoRows {
			return r, ErrMemberToolDenied
		}
		if err != nil {
			return r, err
		}
		switch {
		case status == domain.MemberStatusActive:
			targets = append(targets, e.RecipientID)
		case status == domain.MemberStatusLeft && e.Kind == domain.ChatDiscussionReply:
			// A departed inviter is not replaced. The public result ends the path.
		default:
			return r, ErrMemberToolDenied
		}
	}
	if err := chatDiscussionMessageSlotInTx(ctx, tx, scope.Claim.TurnID, e.RecipientID != ""); err != nil {
		return r, err
	}
	if len(chain.Events) == 0 {
		if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO chat_discussions(origin_message_id,group_id,topic_id,root_inviter_id,max_invite_hops,max_targeted_wakes)
		 VALUES(?,?,?,?,?,?)`, origin, thread.GroupID, thread.TopicID, scope.MemberID, chain.maxInviteHops, chain.maxTargetedWakes); err != nil {
			return r, err
		}
	}
	r, err = publishChatDiscussionInTx(ctx, tx, scope, candidateID, input.Content, m, payload, len(chain.Events)+1, e, targets)
	if err == nil && e.Kind == domain.ChatDiscussionInvite {
		// A push must leave room for its entire unwind. Never apply a new
		// admission check to a reply, resolution, or an original replay.
		err = reserveChatDiscussionReturnContextInTx(ctx, tx, chain, r)
	}
	return r, err
}

func publishChatDiscussionInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, candidateID, content string, mutation mutationScope,
	payload []byte, step int, e domain.ChatDiscussionEvent, targets []string) (domain.ChatDiscussionReceipt, error) {
	var r domain.ChatDiscussionReceipt
	msg, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: mutation.groupID, TopicID: scope.TopicID,
		ActorID: scope.MemberID, Content: content, RecipientIDs: targets}, targets)
	if err != nil {
		return r, err
	}
	if len(msg.Deliveries) != len(targets) {
		return r, ErrMemberToolDenied // Missing storage is not a public-only return.
	}
	msg.SourceID, err = messageCommandSourceInTx(ctx, tx, msg.ID, "member", payload, mutation.key, &scope)
	if err != nil {
		return r, err
	}
	msg.SourceKind, e.MessageID = "member", msg.ID
	if len(msg.Deliveries) == 1 {
		e.DeliveryID = msg.Deliveries[0].ID
	}
	related := e.InvitationMessageID
	if e.Kind == domain.ChatDiscussionInvite {
		related, e.InvitationMessageID = e.ParentInvitationMessageID, msg.ID
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return r, err
	}
	if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO chat_discussion_events(message_id,origin_message_id,group_id,topic_id,step,source_message_id,
	 candidate_id,kind,actor_id,recipient_id,delivery_id,related_invitation_id,expected_reply,message_json)
	 VALUES(?,?,?,?,?,?,?,?,?,nullif(?,''),nullif(?,''),nullif(?,''),?,?)`, msg.ID, e.OriginMessageID, msg.GroupID, msg.TopicID,
		step, e.SourceMessageID, candidateID, e.Kind, e.ActorID, e.RecipientID, e.DeliveryID, related, e.ExpectedReply, string(data)); err != nil {
		return r, err
	}
	if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO mutation_idempotency(operation,actor_id,group_id,idempotency_key,payload,record_id)
	 VALUES(?,?,?,?,?,?)`, mutation.operation, mutation.actorID, mutation.groupID, mutation.key, payload, msg.ID); err != nil {
		return r, err
	}
	return domain.ChatDiscussionReceipt{Message: msg, Event: e}, nil
}

func validChatDiscussionAction(input domain.ChatDiscussionAction) bool {
	if !memberToolTextValid(input.Content, 16384) {
		return false
	}
	switch input.Kind {
	case domain.ChatDiscussionInvite:
		return validTriggerID(input.RecipientID) && (input.ExpectedReply == "" ||
			memberToolTextValid(input.ExpectedReply, chatDiscussionExpectedReplyJSONBytes) &&
				chatDiscussionJSONStringSize(input.ExpectedReply) <= chatDiscussionExpectedReplyJSONBytes)
	case domain.ChatDiscussionReply, domain.ChatDiscussionResolve:
		return input.RecipientID == "" && input.ExpectedReply == ""
	default:
		return false
	}
}
