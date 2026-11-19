package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"agentworkspace/internal/domain"
)

// PendingThreadDecisions selects only the typed lane, never ordinary chat.
// A runtime must not poll this lane until it can perform the typed continuation.
const PendingThreadDecisions = "thread_decision"

// ThreadDecisionWake is a server-only continuation plan. It is not an intake
// candidate, a Task, a new user command, or proof of handling.
type ThreadDecisionWake struct {
	Receipt                                    domain.DecisionAnswerReceipt
	AskerID, AskerTurnID, ConversationID       string
	SourceMessageID, NoticeMessageID, Question string
}

// Reserved message/delivery IDs can only exclude legacy paths. The immutable
// ledger and its exact relations (not the prefix) prove a typed launch.
func isThreadDecisionRef(id string) bool { return strings.HasPrefix(id, threadDecisionAnswerPrefix) }

func (s *Store) threadDecisionPendingFilter(ctx context.Context, lane string) (string, []any, error) {
	if lane != PendingThreadDecisions {
		return ` AND instr(d.id,'` + threadDecisionAnswerPrefix + `')<>1 AND instr(d.message_id,'` + threadDecisionAnswerPrefix + `')<>1`, nil, nil
	}
	installed, err := decisionThreadAnswerInstalled(ctx, s.db)
	if err != nil {
		return "", nil, err
	}
	if !installed {
		return ` AND 0`, nil, nil
	}
	return ` AND EXISTS(SELECT 1 FROM decision_thread_answers a
	 JOIN decision_thread_answerable r ON (r.request_id,r.version)=(a.request_id,a.version)
	 WHERE (a.delivery_id,a.reply_message_id,r.group_id,r.topic_id,r.conversation_id,r.asker_id)=
	    (d.id,d.message_id,d.group_id,d.topic_id,d.conversation_id,d.recipient_id)
	 AND (r.expires_at IS NULL OR r.expires_at>?))`, []any{time.Now().UTC().Format(timestampLayout)}, nil
}

// Used by both answer readback and launch. A historical receipt by itself
// never permits a wake: the original version/source/asker must still match.
func decisionThreadWakeInTx(ctx context.Context, tx *sql.Tx, groupID, deliveryID string) (wake ThreadDecisionWake, state domain.DeliveryState, turnID string, err error) {
	var requestID, topicID string
	var version int64
	err = tx.QueryRowContext(ctx, `SELECT r.request_id,r.topic_id,r.version,r.asker_id,r.asker_turn_id,r.conversation_id,
	 r.source_message_id,r.notice_message_id,r.question,d.state,d.turn_id
	 FROM decision_thread_answers ans JOIN decision_thread_answerable r ON (r.request_id,r.version)=(ans.request_id,ans.version)
	 JOIN deliveries d ON (d.id,d.message_id,d.group_id,d.topic_id,d.conversation_id,d.recipient_id)=
	    (ans.delivery_id,ans.reply_message_id,r.group_id,r.topic_id,r.conversation_id,r.asker_id)
	 WHERE (r.group_id,d.id)=(?,?) AND (r.expires_at IS NULL OR r.expires_at>?)
	 AND (d.turn_id='' OR EXISTS(SELECT 1 FROM turns t WHERE (t.id,t.delivery_id,t.group_id,t.conversation_id,t.member_id)=
	    (d.turn_id,d.id,d.group_id,d.conversation_id,d.recipient_id)))
	 AND NOT EXISTS(SELECT 1 FROM turns t WHERE t.delivery_id=d.id AND t.id<>d.turn_id)
	 AND NOT EXISTS(SELECT 1 FROM deliveries extra WHERE extra.message_id=d.message_id AND extra.id<>d.id)
	 AND NOT EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=d.message_id)
	 AND NOT EXISTS(SELECT 1 FROM coordination_triggers WHERE message_id=d.message_id)`,
		groupID, deliveryID, time.Now().UTC().Format(timestampLayout)).Scan(&requestID, &topicID, &version, &wake.AskerID,
		&wake.AskerTurnID, &wake.ConversationID, &wake.SourceMessageID, &wake.NoticeMessageID, &wake.Question, &state, &turnID)
	if err == nil {
		wake.Receipt, err = decisionThreadAnswerReceiptInTx(ctx, tx, domain.ThreadRef{GroupID: groupID, TopicID: topicID}, requestID, version)
	}
	return
}

// ResolveThreadDecisionWake is a preclaim peek for the trusted native adapter.
// ErrNotFound means skip, never try ordinary preparation for a typed-lane row.
func (s *Store) ResolveThreadDecisionWake(ctx context.Context, groupID, deliveryID string) (ThreadDecisionWake, error) {
	const action = "读取话题答复投递失败"
	if !validTriggerID(groupID) || !validTriggerID(deliveryID) {
		return ThreadDecisionWake{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ThreadDecisionWake{}, queueError(action, err)
	}
	defer tx.Rollback()
	installed, err := decisionThreadAnswerInstalled(ctx, tx)
	if err != nil {
		return ThreadDecisionWake{}, queueError(action, err)
	}
	if !installed {
		return ThreadDecisionWake{}, ErrNotFound
	}
	wake, state, turnID, err := decisionThreadWakeInTx(ctx, tx, groupID, deliveryID)
	if err != nil {
		return ThreadDecisionWake{}, queueError(action, err)
	}
	if state != domain.DeliveryStateQueued || turnID != "" {
		return ThreadDecisionWake{}, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return ThreadDecisionWake{}, queueError(action, err)
	}
	return wake, nil
}

func threadDecisionClaimInTx(ctx context.Context, tx *sql.Tx, turn domain.Turn, messageID, answerID string) error {
	marked := isThreadDecisionRef(turn.DeliveryID) || isThreadDecisionRef(messageID)
	if !marked && answerID == "" {
		return nil
	}
	if !marked || !isThreadDecisionRef(answerID) {
		return ErrMemberToolDenied
	}
	installed, err := decisionThreadAnswerInstalled(ctx, tx)
	if err != nil {
		return err
	}
	if !installed {
		return ErrMemberToolDenied
	}
	wake, err := claimedThreadDecisionWakeInTx(ctx, tx, turn)
	if errors.Is(err, ErrMemberToolDenied) {
		return ErrTurnBusy // The typed plan went stale after scheduling, not a fallback.
	}
	if err == nil && wake.Receipt.ID != answerID {
		return ErrMemberToolDenied
	}
	return err
}

func claimedThreadDecisionWakeInTx(ctx context.Context, tx *sql.Tx, turn domain.Turn) (ThreadDecisionWake, error) {
	wake, state, turnID, err := decisionThreadWakeInTx(ctx, tx, turn.GroupID, turn.DeliveryID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (turnID != turn.ID || state != domain.DeliveryStateQueued ||
		wake.AskerID != turn.MemberID || wake.ConversationID != turn.ConversationID) {
		return ThreadDecisionWake{}, ErrMemberToolDenied
	}
	return wake, err
}

// CommitThreadDecisionLaunch is the last fence before invoking a non-granting
// same-conversation continuation. admit must spend the native wrapper's
// existing one-shot owner in memory, under its registry lock (no I/O). Invoke
// only after commit succeeds. This is not evidence of launch or consumption.
func (s *Store) CommitThreadDecisionLaunch(ctx context.Context, claim TurnClaim, expected ThreadDecisionWake, admit func() bool) error {
	const action = "提交话题答复续接启动失败"
	if admit == nil || expected.Receipt.ID == "" {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return queueError(action, err)
	}
	defer tx.Rollback()
	// First write fences policy, cancellation and recovery across connections.
	turn, err := lockTurnClaim(ctx, tx, claim)
	if err != nil {
		return queueError(action, err)
	}
	if !nativeFreshTurn(turn) || turn.DeliveryID != expected.Receipt.WakeDeliveryID {
		return ErrMemberToolDenied
	}
	wake, err := claimedThreadDecisionWakeInTx(ctx, tx, turn)
	if err != nil {
		return queueError(action, err)
	}
	// Re-read the live claim after the lock statement, including any effects
	// of triggers. The native runtime owns its provider and resource checks.
	var fresh bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM turns WHERE
	 (id,claim_token,group_id,delivery_id,member_id,conversation_id)=(?,?,?,?,?,?)
	 AND state='starting' AND cancel_requested_at IS NULL AND accepted_at IS NULL AND native_session_id=''
	 AND candidate_text='' AND candidate_terminal='' AND receipt_json='')`,
		turn.ID, claim.Token, turn.GroupID, turn.DeliveryID, turn.MemberID, turn.ConversationID).Scan(&fresh)
	if err != nil {
		return queueError(action, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !fresh || wake != expected || !admit() {
		return ErrMemberToolDenied
	}
	if err := tx.Commit(); err != nil {
		return queueError(action, err)
	}
	return nil
}
