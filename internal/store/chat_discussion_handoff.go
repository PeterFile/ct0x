package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"

	"agentworkspace/internal/domain"
)

const chatDiscussionExpectedReplyJSONBytes = 1024

// ErrChatDiscussionMutationUnknown takes precedence over any business cause it wraps.
var ErrChatDiscussionMutationUnknown = errors.New("讨论接力写入结果未确认；请保留原幂等键和原请求内容用于对账")

type chatDiscussionTransactionUnknown struct {
	cause  error
	commit bool
}

func (e *chatDiscussionTransactionUnknown) Error() string {
	return ErrChatDiscussionMutationUnknown.Error()
}
func (e *chatDiscussionTransactionUnknown) Unwrap() error { return e.cause }
func (e *chatDiscussionTransactionUnknown) Is(target error) bool {
	return target == ErrChatDiscussionMutationUnknown
}
func (e *chatDiscussionTransactionUnknown) CommitOutcomeUnknown() bool { return e.commit }
func (e *chatDiscussionTransactionUnknown) RollbackConfirmed() bool    { return false }

// ApplyChatDiscussion publishes an assessed handoff step using only the bound
// intake and the current position in the path. It never creates a member or
// grants work authority.
func (s *Store) ApplyChatDiscussion(ctx context.Context, scope MemberToolScope, candidateID string, input domain.ChatDiscussionAction, key string) (domain.ChatDiscussionReceipt, error) {
	return s.applyChatDiscussion(ctx, scope, candidateID, input, key, nil)
}

// ApplyChatDiscussionWithMember accepts only the create_member invitation form.
// The caller must have checked the live intake's send_message and create_member
// grants and supply the same trusted policy as standalone create_member.
func (s *Store) ApplyChatDiscussionWithMember(ctx context.Context, scope MemberToolScope, candidateID string, input domain.ChatDiscussionAction, key string, policy MemberToolCreatePolicy) (domain.ChatDiscussionReceipt, error) {
	return s.applyChatDiscussion(ctx, scope, candidateID, input, key, &policy)
}

func (s *Store) applyChatDiscussion(ctx context.Context, scope MemberToolScope, candidateID string, input domain.ChatDiscussionAction, key string, policy *MemberToolCreatePolicy) (_ domain.ChatDiscussionReceipt, err error) {
	const action = "保存聊天协作失败"
	// The live-scope fence itself writes, so it must be inside the boundary
	// whose rollback we check, rather than inside beginMemberToolTx.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ChatDiscussionReceipt{}, memberToolOperationError(action, err)
	}
	commit := false
	defer func() {
		rollback := tx.Rollback()
		if err == nil {
			return
		}
		if commit || rollback != nil {
			// ErrTxDone after Commit tells us nothing about its outcome.
			if rollback != nil && !(commit && rollback == sql.ErrTxDone) {
				err = errors.Join(err, rollback)
			}
			err = &chatDiscussionTransactionUnknown{cause: err, commit: commit}
		} else if err != ErrChatDiscussionContextTooLarge {
			err = memberToolOperationError(action, err)
		}
	}()
	if err := memberToolScopeInTx(ctx, tx, scope); err != nil {
		return domain.ChatDiscussionReceipt{}, err
	}
	r, err := applyChatDiscussionInTx(ctx, tx, scope, candidateID, input, key, policy)
	if err != nil {
		return domain.ChatDiscussionReceipt{}, err
	}
	commit = true
	if err := tx.Commit(); err != nil {
		return domain.ChatDiscussionReceipt{}, err
	}
	return r, nil
}

func applyChatDiscussionInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, candidateID string, input domain.ChatDiscussionAction, key string, policy *MemberToolCreatePolicy) (domain.ChatDiscussionReceipt, error) {
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
	create := input.CreateMember
	if (create == nil) != (policy == nil) || (policy != nil && !policy.valid()) {
		return r, ErrInvalid
	}
	m := mutationScope{operation: "create_message", actorID: scope.MemberID, groupID: scope.Claim.GroupID, key: key}
	fields := []string{"chat_discussion/v1", candidateID, input.Kind, input.Content, input.RecipientID, input.ExpectedReply}
	if create != nil {
		// Bind the intended provider/model, not the mutable capacity or the selected member.
		fields = append(fields, "create_member", create.Name, create.Role, create.Reason, policy.ProviderID, policy.Model)
	}
	payload := mutationPayload(fields...)
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
		limits, err := currentChatDiscussionPolicyInTx(ctx, tx, thread)
		if err != nil {
			return r, err
		}
		// Later steps use the persisted caps, not edits to the topic policy.
		chain = newChatDiscussionChain(thread, origin, scope.MemberID, limits.MaxInviteHops, limits.MaxTargetedWakes)
	}
	var newMember bool
	if create != nil {
		input.RecipientID, newMember, err = planChatDiscussionMemberInTx(ctx, tx, chain, create.Role, *policy)
		if err != nil {
			return r, err
		}
		input.CreateMember = nil // Plan the same bound route as an explicit invitation.
	}
	e, err := chain.plan(a.messageID, scope.MemberID, input)
	if err != nil {
		return r, err
	}
	targets := []string{}
	switch {
	case newMember:
		targets = append(targets, e.RecipientID)
	case e.RecipientID != "":
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
	if newMember {
		member, err := insertAutomaticMemberInTx(ctx, tx, e.RecipientID, scope, MemberToolCreateInput(*create), *policy)
		if err != nil {
			return r, err
		}
		// The public join is visible without a wake; only the invitation is delivered.
		if _, err := publishAutomaticMemberJoinInTx(ctx, tx, scope, member.Name, create.Reason, []string{}); err != nil {
			return r, err
		}
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

// Pick a route without writing anything. The chain and publication slot are
// checked against that route before an unmatched request can insert a member.
func planChatDiscussionMemberInTx(ctx context.Context, tx *sql.Tx, chain *chatDiscussionChain, role string, policy MemberToolCreatePolicy) (id string, newMember bool, err error) {
	visited := []string{chain.RootInviterID}
	for _, receipt := range chain.Events {
		if receipt.Event.Kind == domain.ChatDiscussionInvite {
			visited = append(visited, receipt.Event.RecipientID)
		}
	}
	member, err := reusableAutomaticMemberInTx(ctx, tx, chain.GroupID, role, policy, visited)
	if errors.Is(err, sql.ErrNoRows) {
		return rand.Text(), true, nil
	}
	return member.ID, false, err
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
		if input.ExpectedReply != "" && (!memberToolTextValid(input.ExpectedReply, chatDiscussionExpectedReplyJSONBytes) ||
			chatDiscussionJSONStringSize(input.ExpectedReply) > chatDiscussionExpectedReplyJSONBytes) {
			return false
		}
		if input.CreateMember != nil {
			return input.RecipientID == "" && MemberToolCreateInput(*input.CreateMember).valid()
		}
		return validTriggerID(input.RecipientID)
	case domain.ChatDiscussionReply, domain.ChatDiscussionResolve:
		return input.RecipientID == "" && input.ExpectedReply == "" && input.CreateMember == nil
	default:
		return false
	}
}
