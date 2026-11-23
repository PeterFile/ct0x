package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

// ApplyTurnReceipt accepts trusted internal adapter evidence, never model/tool input.
// Native terminal evidence alone cannot publish a reply or release execution claims.
func (s *Store) ApplyTurnReceipt(ctx context.Context, claim TurnClaim, receipt provider.ExecutionReceipt) (domain.Turn, error) {
	const action = "结算轮次失败"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Turn{}, queueError(action, err)
	}
	defer tx.Rollback()
	turn, err := lockTurnClaim(ctx, tx, claim)
	if err != nil {
		return domain.Turn{}, queueError(action, err)
	}
	if receipt.NativePreflightFailure {
		// A native startup refusal is not a new execution observation. Its
		// first-evidence guard runs under the same claim lock as normal Apply.
		turn, _, err = nativePreflightFailureInTx(ctx, tx, turn, nil)
	} else {
		normalized := normalizeTurnReceipt(turn, receipt)
		if normalized.Outcome == provider.OutcomeNotStarted && normalized.NativeRef == nil && nativeFreshTurn(turn) && turn.ReplyMessageID == "" {
			turn, err = applyDecisionAnswerRefusalInTx(ctx, tx, turn, normalized)
		} else {
			turn, err = applyTurnReceiptInTx(ctx, tx, turn, normalized)
		}
	}
	if err != nil {
		return domain.Turn{}, queueError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Turn{}, queueError(action, err)
	}
	return turn, nil
}

// applyTurnReceiptInTx shares the existing settlement policy with the native
// first-evidence path. Callers hold the exact claim lock and pass a normalized receipt.
func applyTurnReceiptInTx(ctx context.Context, tx *sql.Tx, turn domain.Turn, receipt provider.ExecutionReceipt) (domain.Turn, error) {
	data, err := json.Marshal(receipt)
	if err != nil {
		return domain.Turn{}, err
	}
	claim := TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	if !activeTurn(turn.State) {
		var previous string
		if err := tx.QueryRowContext(ctx, "SELECT receipt_json FROM turns WHERE group_id=? AND id=? AND claim_token=?", claim.GroupID, claim.TurnID, claim.Token).Scan(&previous); err != nil {
			return domain.Turn{}, err
		}
		if previous != string(data) {
			return domain.Turn{}, ErrConflict
		}
		if err := applyNativeBindingInTx(ctx, tx, turn, receipt, true); err != nil {
			return domain.Turn{}, err
		}
		return turn, nil
	}
	// A callback may have failed to persist acceptance even though the adapter
	// observed it. Unknown/DeliveryFailed does not erase that matching fact.
	if receipt.Started && validNativeSession(receipt.NativeSessionID) && (turn.NativeSessionID == "" || turn.NativeSessionID == receipt.NativeSessionID) {
		turn.NativeSessionID = receipt.NativeSessionID
		if receipt.Accepted {
			acceptTurn(&turn)
		}
	}
	turn.State, turn.Reason = domain.TurnState(receipt.Outcome), receipt.Reason
	if terminalOutcome(receipt.Outcome) && strings.TrimSpace(turn.CandidateText) != "" {
		if err := publishSettledTurnReply(ctx, tx, &turn); err != nil {
			return domain.Turn{}, err
		}
	}
	if err := writeClaimedTurn(ctx, tx, &turn); err != nil {
		return domain.Turn{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE turns SET receipt_json=? WHERE group_id=? AND id=? AND claim_token=?", string(data), claim.GroupID, claim.TurnID, claim.Token); err != nil {
		return domain.Turn{}, err
	}
	if err := applyNativeBindingInTx(ctx, tx, turn, receipt, false); err != nil {
		return domain.Turn{}, err
	}
	if !activeTurn(turn.State) {
		if turn.AcceptedAt == nil {
			if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET state='failed',error=?
			 WHERE group_id=? AND id=? AND turn_id=? AND state<>'accepted'
			 AND EXISTS(SELECT 1 FROM turns WHERE group_id=? AND id=? AND claim_token=?)`,
				turn.Reason, turn.GroupID, turn.DeliveryID, turn.ID, claim.GroupID, claim.TurnID, claim.Token); err != nil {
				return domain.Turn{}, err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM resource_claims WHERE turn_id=?
		 AND EXISTS(SELECT 1 FROM turns WHERE group_id=? AND id=? AND claim_token=?)`, turn.ID, claim.GroupID, claim.TurnID, claim.Token); err != nil {
			return domain.Turn{}, err
		}
	}
	return turn, nil
}

func normalizeTurnReceipt(turn domain.Turn, receipt provider.ExecutionReceipt) provider.ExecutionReceipt {
	outcome := provider.OutcomeUnknown
	bound := validNativeSession(receipt.NativeSessionID) && (turn.NativeSessionID == "" || turn.NativeSessionID == receipt.NativeSessionID)
	if terminalOutcome(receipt.Outcome) && receipt.Terminal == receipt.Outcome && receipt.Started && receipt.Accepted && bound &&
		receipt.OutputComplete && receipt.DirectChildReaped && !receipt.DeliveryFailed &&
		(turn.CandidateTerminal == "" || turn.CandidateTerminal == domain.TurnState(receipt.Terminal)) {
		// Outcome is adapter-normalized. In particular, trustworthy failed turns
		// may have nonzero exit codes; storage must not reinterpret CLI semantics.
		outcome = receipt.Outcome
	}
	if receipt.Outcome == provider.OutcomeNotStarted && !receipt.Started && !receipt.Accepted && receipt.NativeSessionID == "" &&
		receipt.Terminal == "" && !receipt.DeliveryFailed && !receipt.DirectChildReaped && !receipt.OutputComplete &&
		turn.AcceptedAt == nil && turn.NativeSessionID == "" && turn.CandidateText == "" && turn.CandidateTerminal == "" &&
		(turn.State == domain.TurnStateStarting || turn.State == domain.TurnStateCancelRequested || turn.State == domain.TurnStateNotStarted) {
		// Unknown/recovered executions cannot be disproved by a fresh invocation
		// that did not start. NotStarted is included only for identical replay.
		outcome = provider.OutcomeNotStarted
	}
	receipt.Outcome = outcome
	if !terminalOutcome(receipt.Terminal) {
		receipt.Terminal = ""
	}
	if !validNativeSession(receipt.NativeSessionID) {
		receipt.NativeSessionID = ""
	}
	receipt.StdoutPath, receipt.StderrPath = receiptLogPath(receipt.StdoutPath), receiptLogPath(receipt.StderrPath)
	receipt.Reason = map[provider.Outcome]string{
		provider.OutcomeUnknown: "执行结果未知，需对账后再处理", provider.OutcomeNotStarted: "执行未启动",
		provider.OutcomeSucceeded: "执行完成", provider.OutcomeFailed: "模型执行失败",
		provider.OutcomeAborted: "模型执行已中止", provider.OutcomeTruncated: "模型输出被截断",
	}[outcome]
	return receipt
}

func receiptLogPath(path string) string {
	if len(path) > 4096 || !utf8.ValidString(path) || strings.ContainsAny(path, "\x00\r\n") {
		return ""
	}
	return path
}

const turnReplyConversation = ` FROM conversations v WHERE v.group_id=? AND v.id=? AND v.member_id=?`

func publishSettledTurnReply(ctx context.Context, tx *sql.Tx, turn *domain.Turn) error {
	if turn.State != domain.TurnStateSucceeded {
		return publishTurnReply(ctx, tx, turn)
	}
	// Fold the indexed candidate/resolve probe into the topic lookup. Match
	// the recorded publication and intake digests: recovery can rotate the live claim.
	var topicID string
	var duplicate bool
	if err := tx.QueryRowContext(ctx, `SELECT v.topic_id, EXISTS(
	 SELECT 1 FROM chat_candidates c
	 CROSS JOIN chat_discussion_events e ON e.candidate_id=c.id
	    AND e.group_id=c.group_id AND e.topic_id=c.topic_id AND e.source_message_id=c.message_id AND e.actor_id=c.member_id
	 CROSS JOIN chat_intake_executions i ON i.candidate_id=c.id AND i.turn_id=c.turn_id AND i.purpose='`+domain.ChatIntakePurpose+`'
	 CROSS JOIN message_command_sources s ON s.message_id=e.message_id
	    AND s.kind='member' AND s.turn_id=c.turn_id AND s.claim_digest=i.claim_digest
	 CROSS JOIN message_command_sources source ON source.message_id=c.message_id AND source.id=i.source_id AND source.kind=i.source_kind
	 CROSS JOIN deliveries incoming ON incoming.id=? AND incoming.turn_id=c.turn_id AND incoming.conversation_id=v.id
	    AND incoming.message_id=c.message_id AND incoming.group_id=c.group_id AND incoming.topic_id=c.topic_id AND incoming.recipient_id=c.member_id
	 CROSS JOIN messages m ON m.id=e.message_id AND m.group_id=e.group_id AND m.topic_id=e.topic_id AND m.sender_id=e.actor_id
	 WHERE c.turn_id=? AND c.group_id=v.group_id AND c.topic_id=v.topic_id AND c.member_id=v.member_id
	   AND typeof(i.claim_digest)='blob' AND length(i.claim_digest)=32 AND source.kind IN ('local_user_chat','member')
	   AND e.kind='`+domain.ChatDiscussionResolve+`' AND e.recipient_id IS NULL AND e.delivery_id IS NULL AND e.expected_reply=''
	   AND m.content COLLATE BINARY=?
	   AND NOT EXISTS(SELECT 1 FROM deliveries d WHERE d.message_id=e.message_id)
	)`+turnReplyConversation, turn.DeliveryID, turn.ID, turn.CandidateText, turn.GroupID, turn.ConversationID, turn.MemberID).Scan(&topicID, &duplicate); err != nil {
		return err
	}
	if duplicate {
		return nil // The structured publication keeps its own provenance, not a native reply link.
	}
	if turn.AcceptedAt != nil {
		duplicate, err := hasMatchingOrdinaryTurnSend(ctx, tx, turn, topicID)
		if err != nil {
			return err
		}
		if duplicate {
			return nil // Keep the explicit send without linking it as a native reply.
		}
		if duplicate, err := hasMatchingDiscussNotice(ctx, tx, turn, topicID); err != nil || duplicate {
			return err // An assessment notice keeps its own provenance, too.
		}
	}
	return insertTurnReply(ctx, tx, turn, topicID)
}

// Start with sources for this Turn only; every other lookup uses a message key
// or the exact mutation receipt key. Any delivery row vetoes a public-only match.
const ordinaryTurnReplyMatch = `SELECT EXISTS(
 SELECT 1 FROM message_command_sources s INDEXED BY message_command_sources_turn
 CROSS JOIN messages m ON m.id=s.message_id
 WHERE s.turn_id=? AND s.kind='member' AND s.claim_digest=?
   AND m.group_id=? AND m.sender_id=? AND m.topic_id=? AND m.content COLLATE BINARY=?
   AND s.payload IN (?,?)
   AND NOT EXISTS(SELECT 1 FROM chat_discussion_events e WHERE e.message_id=m.id)
   AND NOT EXISTS(SELECT 1 FROM deliveries d WHERE d.message_id=m.id)
   AND EXISTS(SELECT 1 FROM mutation_idempotency r WHERE r.operation=? AND r.actor_id=m.sender_id
     AND r.group_id=m.group_id AND r.idempotency_key=s.idempotency_key AND r.record_id=m.id AND r.payload=s.payload)
)`

func hasMatchingOrdinaryTurnSend(ctx context.Context, tx *sql.Tx, turn *domain.Turn, topicID string) (bool, error) {
	// Unlike structured resolves, ordinary sends must prove the current claim and
	// their exact send_message fingerprint (omitted or explicitly empty recipients).
	digest := sha256.Sum256([]byte(turn.ClaimToken))
	r := MessageRequest{GroupID: turn.GroupID, TopicID: topicID, ActorID: turn.MemberID, Content: turn.CandidateText}
	omitted := r.payload()
	r.RecipientIDs = []string{}
	var duplicate bool
	err := tx.QueryRowContext(ctx, ordinaryTurnReplyMatch, turn.ID, digest[:], r.GroupID, r.ActorID, r.TopicID, r.Content,
		omitted, r.payload(), r.scope().operation).Scan(&duplicate)
	if err != nil || !duplicate {
		return false, err
	}

	// Typed TaskWork deliveries keep their native reply separate from member
	// commands. WorkReport uses the delegated Turn's actual reply as evidence.
	supported, err := taskWorkSchemaSupportedInTx(ctx, tx)
	if err != nil {
		return false, err
	}
	if !supported {
		return true, nil // A valid older schema has no TaskWork delivery markers.
	}
	var untyped bool
	err = tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM task_work_deliveries WHERE delivery_id=?)`, turn.DeliveryID).Scan(&untyped)
	return untyped, err
}

// Start from the unique candidate for this Turn and follow the assessment's
// notice link. The assessment was fenced against the original intake claim;
// a recovered Turn's current token need not match that historical claim.
// Legacy input without a command row must stay unknown with an empty source
// ID. Only that path can match a receipt with its source ID absent or null.
const discussNoticeTurnReplyMatch = `SELECT EXISTS(
 SELECT 1 FROM chat_candidates c
 CROSS JOIN chat_assessments a ON a.candidate_id=c.id
 CROSS JOIN chat_intake_executions i ON i.candidate_id=c.id AND i.turn_id=c.turn_id AND i.purpose='` + domain.ChatIntakePurpose + `'
 LEFT JOIN message_command_sources source ON source.message_id=c.message_id
 CROSS JOIN deliveries incoming ON incoming.id=? AND incoming.turn_id=c.turn_id AND incoming.conversation_id=?
    AND (incoming.message_id,incoming.group_id,incoming.topic_id,incoming.recipient_id)=(c.message_id,c.group_id,c.topic_id,c.member_id)
 CROSS JOIN messages m ON m.id=a.notice_message_id AND m.group_id=c.group_id AND m.topic_id=c.topic_id AND m.sender_id=c.member_id
 WHERE c.turn_id=? AND c.group_id=? AND c.topic_id=? AND c.member_id=?
   AND incoming.state='accepted'
   AND ((source.message_id IS NULL AND i.source_kind='unknown' AND i.source_id='')
     OR (i.source_id<>'' AND source.kind IN ('local_user_chat','member') AND source.id=i.source_id AND source.kind=i.source_kind))
   AND typeof(i.claim_digest)='blob' AND length(i.claim_digest)=32
   AND m.content COLLATE BINARY=?
   AND a.decision_id IS NULL AND a.dispatch_request_id IS NULL
   AND json_extract(a.receipt_json,'$.disposition') IS 'discuss'
   AND json_extract(a.receipt_json,'$.outcome') IS 'discuss'
   AND json_extract(a.receipt_json,'$.id') IS a.id
   AND json_extract(a.receipt_json,'$.notice_message_id') IS m.id
   AND json_extract(a.receipt_json,'$.candidate_id') IS c.id
   AND json_extract(a.receipt_json,'$.turn_id') IS c.turn_id
   AND json_extract(a.receipt_json,'$.group_id') IS c.group_id
   AND json_extract(a.receipt_json,'$.topic_id') IS c.topic_id
   AND json_extract(a.receipt_json,'$.assessed_by') IS c.member_id
   AND json_extract(a.receipt_json,'$.message_id') IS c.message_id
   AND json_extract(a.receipt_json,'$.purpose') IS i.purpose
   AND coalesce(json_extract(a.receipt_json,'$.source_id'),'') IS i.source_id
   AND json_extract(a.receipt_json,'$.source_kind') IS i.source_kind
   AND NOT EXISTS(SELECT 1 FROM deliveries d WHERE d.message_id=m.id)
   AND NOT EXISTS(SELECT 1 FROM coordination_dispatch_state WHERE delivery_id=incoming.id)
   AND NOT EXISTS(SELECT 1 FROM coordination_assignment_dispatch WHERE delivery_id=incoming.id)
)`

func hasMatchingDiscussNotice(ctx context.Context, tx *sql.Tx, turn *domain.Turn, topicID string) (bool, error) {
	var duplicate bool
	err := tx.QueryRowContext(ctx, discussNoticeTurnReplyMatch, turn.DeliveryID, turn.ConversationID,
		turn.ID, turn.GroupID, topicID, turn.MemberID, turn.CandidateText).Scan(&duplicate)
	if err != nil || !duplicate {
		return false, err
	}
	// Even a damaged typed marker vetoes suppression. The shared check also
	// handles schemas installed before typed work deliveries existed.
	err = denyTaskWorkFallbackInTx(ctx, tx, turn.ID)
	if errors.Is(err, ErrMemberToolDenied) {
		return false, nil
	}
	return err == nil, err
}

func publishTurnReply(ctx context.Context, tx *sql.Tx, turn *domain.Turn) error {
	var topicID string
	if err := tx.QueryRowContext(ctx, "SELECT v.topic_id"+turnReplyConversation,
		turn.GroupID, turn.ConversationID, turn.MemberID).Scan(&topicID); err != nil {
		return err
	}
	return insertTurnReply(ctx, tx, turn, topicID)
}

func insertTurnReply(ctx context.Context, tx *sql.Tx, turn *domain.Turn, topicID string) error {
	message := domain.Message{
		ID: rand.Text(), GroupID: turn.GroupID, TopicID: topicID, SenderID: turn.MemberID,
		Content: turn.CandidateText, CreatedAt: time.Now().UTC(), Deliveries: []domain.Delivery{},
	}
	if err := insertMessageRow(ctx, tx, message); err != nil {
		return err
	}
	turn.ReplyMessageID = message.ID
	return nil
}
