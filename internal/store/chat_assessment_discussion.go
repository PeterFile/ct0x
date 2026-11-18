package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agentworkspace/internal/domain"
)

const chatDiscussionOnlyExcerptBytes = 4096

type chatDiscussionRootSource struct {
	id, content string
	sequence    int64
}

func chatDiscussionRootSourceInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate) (root chatDiscussionRootSource, err error) {
	err = tx.QueryRowContext(ctx, `SELECT s.id,m.content,l.sequence FROM messages m
	 JOIN message_log l ON (l.message_id,l.group_id,l.topic_id)=(m.id,m.group_id,m.topic_id)
	 JOIN message_command_sources s ON s.message_id=m.id AND s.kind='local_user_chat'
	 WHERE m.id=? AND m.group_id=? AND m.topic_id=? AND m.sender_id='user' AND s.id=? AND l.sequence=?`,
		c.MessageID, c.GroupID, c.TopicID, c.SourceID, c.Generation).Scan(&root.id, &root.content, &root.sequence)
	if err == sql.ErrNoRows {
		return root, ErrMemberToolDenied
	}
	return root, err
}

// The proposal supplies a quote, not its origin. Bind the quote to the live
// delivery and the exact immutable local-user command before recording it.
func verifyChatDiscussionOnlyInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, c domain.ChatCandidate, excerpt string) error {
	if c.SourceKind != "local_user_chat" || c.SourceSenderID != "user" || c.SourceID == "" {
		return ErrInvalid
	}
	if _, err := chatDiscussionArrivalInTx(ctx, tx, scope, c.ID); err != nil {
		return err
	}
	root, err := chatDiscussionRootSourceInTx(ctx, tx, c)
	if err != nil {
		return err
	}
	if !strings.Contains(root.content, excerpt) {
		return ErrInvalid
	}
	stale, err := chatSourceSupersededInTx(ctx, tx, c.ThreadRef, c.Generation)
	if err != nil {
		return err
	}
	if stale {
		return ErrConflict
	}
	return nil
}

func chatDiscussionWorkGateInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate) error {
	return chatDiscussionSourceGateInTx(ctx, tx, c, true)
}

// Relays inherit the same source limit, not the origin's work authority.
// Recency constrains new admissions, not execution of already admitted work.
func chatDiscussionSourceGateInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate, checkRecency bool) error {
	var available, structured bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version>=24)
	 OR EXISTS(SELECT 1 FROM sqlite_schema WHERE name IN ('chat_discussions','chat_discussion_events'))`).Scan(&available); err != nil {
		return err
	}
	if available {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM chat_discussion_events WHERE message_id=?)`, c.MessageID).
			Scan(&structured); err != nil {
			return err
		}
	}
	if !structured {
		if c.SourceKind == "member" {
			return chatDiscussionLegacyWorkGateInTx(ctx, tx, c, checkRecency)
		}
		if c.SourceKind != "local_user_chat" {
			return nil
		}
		source, err := chatDiscussionRootSourceInTx(ctx, tx, c)
		if err != nil {
			return err
		}
		return chatDiscussionRootLimitInTx(ctx, tx, c.ThreadRef, c.MessageID, source)
	}
	arrival, err := chatDiscussionWorkArrivalInTx(ctx, tx, c.ThreadRef, c.MessageID, c.ID, c.TurnID)
	if err == sql.ErrNoRows {
		return ErrMemberToolDenied // A broken event must not fall back to legacy chat.
	}
	if err != nil {
		return err
	}
	origin := arrival.originID
	// Prove the first event's captured root. Its publisher need not be the
	// recipient who recorded the limitation.
	var source chatDiscussionRootSource
	err = tx.QueryRowContext(ctx, `SELECT s.id,m.content,l.sequence FROM chat_discussion_events e
	 JOIN chat_candidates c ON c.id=e.candidate_id AND c.message_id=e.source_message_id AND c.member_id=e.actor_id
	    AND c.group_id=e.group_id AND c.topic_id=e.topic_id
	 JOIN chat_intake_executions i ON i.candidate_id=c.id AND i.turn_id=c.turn_id AND i.purpose=?
	 JOIN messages m ON m.id=c.message_id AND m.group_id=c.group_id AND m.topic_id=c.topic_id AND m.sender_id='user'
	 JOIN message_log l ON l.message_id=m.id AND l.group_id=m.group_id AND l.topic_id=m.topic_id AND l.sequence=c.generation
	 JOIN message_command_sources s ON s.message_id=m.id AND s.kind='local_user_chat' AND s.id=i.source_id AND s.kind=i.source_kind
	 JOIN chat_assessments a ON a.candidate_id=c.id
	 WHERE e.origin_message_id=? AND e.group_id=? AND e.topic_id=? AND e.step=1 AND e.source_message_id=e.origin_message_id`,
		domain.ChatIntakePurpose, origin, c.GroupID, c.TopicID).Scan(&source.id, &source.content, &source.sequence)
	if err == sql.ErrNoRows {
		return ErrMemberToolDenied
	}
	if err != nil {
		return err
	}
	if checkRecency {
		// A newer handoff cannot make the root current again after a user command.
		stale, err := chatSourceSupersededInTx(ctx, tx, c.ThreadRef, source.sequence)
		if err != nil {
			return err
		}
		if stale {
			return chatStaleSource()
		}
	}
	return chatDiscussionRootLimitInTx(ctx, tx, c.ThreadRef, origin, source)
}

// Generic sends can relay an arrival without capturing an intake on every hop.
// Each message has at most one publishing Turn, and its input must be older.
// Bound the indexed walk; an unproved prefix must not count as unrelated chat.
func chatDiscussionLegacyWorkGateInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate, checkRecency bool) error {
	const maxSourceHops = 16
	for hops := 0; c.SourceKind == "member"; hops++ {
		if hops == maxSourceHops {
			return ErrMemberToolDenied
		}
		parent, err := chatDiscussionMemberParentInTx(ctx, tx, c)
		if err != nil {
			return err
		}
		c = parent
	}
	if c.SourceKind != "local_user_chat" {
		return nil // No associated user chat command, regardless of sender labels or text.
	}
	root, err := chatDiscussionRootSourceInTx(ctx, tx, c)
	if err != nil {
		return err
	}
	limitErr := chatDiscussionRootLimitInTx(ctx, tx, c.ThreadRef, c.MessageID, root)
	var limit *chatNonAdmission
	if !checkRecency || !errors.As(limitErr, &limit) {
		return limitErr
	}
	// A newer command can make the verified root stale, but cannot authorize its relay.
	stale, err := chatSourceSupersededInTx(ctx, tx, c.ThreadRef, root.sequence)
	if err != nil {
		return err
	}
	if stale {
		return chatStaleSource()
	}
	return limitErr
}

// Resolve the actual publisher before following its incoming delivery. A
// captured intake must agree with that history, but generic send need not have
// one. Never depend on the publisher's current claim token or terminal state.
func chatDiscussionMemberParentInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate) (p domain.ChatCandidate, err error) {
	p.ThreadRef = c.ThreadRef
	var capturedKind, capturedID string
	err = tx.QueryRowContext(ctx, `SELECT prior.id,prior.sender_id,pl.sequence,t.id,t.member_id,
	 coalesce(i.source_kind,''),coalesce(i.source_id,'')
	 FROM messages m JOIN message_log l ON (l.message_id,l.group_id,l.topic_id)=(m.id,m.group_id,m.topic_id)
	 LEFT JOIN message_command_sources s ON s.message_id=m.id
	 JOIN turns t ON (t.id,t.group_id,t.member_id)=(coalesce(s.turn_id,substr(:source,6)),m.group_id,m.sender_id)
	 JOIN deliveries d ON (d.id,d.turn_id,d.group_id,d.topic_id,d.conversation_id,d.recipient_id)
	    =(t.delivery_id,t.id,t.group_id,m.topic_id,t.conversation_id,t.member_id)
	 JOIN conversations v ON (v.id,v.group_id,v.topic_id,v.member_id)=(d.conversation_id,d.group_id,d.topic_id,d.recipient_id)
	 JOIN messages prior ON (prior.id,prior.group_id,prior.topic_id)=(d.message_id,d.group_id,d.topic_id)
	 JOIN message_log pl ON (pl.message_id,pl.group_id,pl.topic_id)=(prior.id,prior.group_id,prior.topic_id) AND pl.sequence<l.sequence
	 LEFT JOIN chat_candidates publisher ON publisher.turn_id=t.id
	 LEFT JOIN chat_intake_executions i ON i.candidate_id=publisher.id
	 WHERE (m.id,m.group_id,m.topic_id,l.sequence)=(:message,:group,:topic,:sequence)
	 AND EXISTS(SELECT 1 FROM turns incoming JOIN deliveries delivered ON delivered.id=incoming.delivery_id
	    WHERE (incoming.id,incoming.group_id,incoming.member_id)=(:turn,m.group_id,:recipient)
	    AND (delivered.turn_id,delivered.message_id,delivered.group_id,delivered.topic_id,delivered.conversation_id,delivered.recipient_id)
	    =(incoming.id,m.id,m.group_id,m.topic_id,incoming.conversation_id,incoming.member_id)
	    AND NOT EXISTS(SELECT 1 FROM turns other WHERE other.delivery_id=delivered.id AND other.id<>incoming.id))
	 AND ((s.id=:source AND s.kind='member' AND typeof(s.claim_digest)='blob' AND length(s.claim_digest)=32)
	    OR (s.id IS NULL AND :source='turn:'||t.id AND t.reply_message_id=m.id))
	 AND NOT EXISTS(SELECT 1 FROM turns other WHERE other.delivery_id=d.id AND other.id<>t.id)
	 AND NOT EXISTS(SELECT 1 FROM turns other WHERE other.reply_message_id=m.id AND other.reply_message_id<>'' AND (s.id IS NOT NULL OR other.id<>t.id))
	 AND (publisher.id IS NULL OR (publisher.group_id,publisher.topic_id,publisher.message_id,publisher.member_id,publisher.generation)
	    =(prior.group_id,prior.topic_id,prior.id,t.member_id,pl.sequence))
	 AND (i.candidate_id IS NULL OR (i.turn_id=t.id AND i.purpose=:purpose AND (s.id IS NULL OR i.claim_digest=s.claim_digest)))`,
		sql.Named("source", c.SourceID), sql.Named("message", c.MessageID), sql.Named("group", c.GroupID), sql.Named("topic", c.TopicID),
		sql.Named("sequence", c.Generation), sql.Named("turn", c.TurnID), sql.Named("recipient", c.MemberID), sql.Named("purpose", domain.ChatIntakePurpose)).Scan(
		&p.MessageID, &p.SourceSenderID, &p.Generation, &p.TurnID, &p.MemberID, &capturedKind, &capturedID)
	if err == sql.ErrNoRows {
		return p, ErrMemberToolDenied
	}
	if err != nil {
		return p, err
	}
	p.SourceKind, p.SourceID, err = messageSourceInTx(ctx, tx, p.MessageID)
	if err == nil && capturedKind != "" && (capturedKind != p.SourceKind || capturedID != p.SourceID) {
		err = ErrMemberToolDenied
	}
	return p, err
}

// Prove the historical receive identity for candidate c and its Turn rt and
// delivery rd. Neither recovery nor terminal settlement invalidates a capture.
const chatDiscussionReceiveMatches = `(rt.delivery_id,rt.id,rt.group_id,rt.conversation_id,rt.member_id)
	 =(rd.id,rd.turn_id,rd.group_id,rd.conversation_id,rd.recipient_id)
	 AND (c.message_id,c.group_id,c.topic_id,c.member_id)=(rd.message_id,rd.group_id,rd.topic_id,rd.recipient_id)
	 AND EXISTS(SELECT 1 FROM conversations v WHERE (v.id,v.group_id,v.topic_id,v.member_id)
	    =(rd.conversation_id,rd.group_id,rd.topic_id,rd.recipient_id))
	 AND EXISTS(SELECT 1 FROM messages m JOIN message_log l ON (l.message_id,l.group_id,l.topic_id)=(m.id,m.group_id,m.topic_id)
	    WHERE (m.id,m.group_id,m.topic_id,l.sequence)=(c.message_id,c.group_id,c.topic_id,c.generation))
	 AND NOT EXISTS(SELECT 1 FROM turns other WHERE other.delivery_id=rd.id AND other.id<>rt.id)
	 AND NOT EXISTS(SELECT 1 FROM deliveries other WHERE other.message_id=rd.message_id AND other.turn_id=rt.id AND other.id<>rd.id)`

const chatDiscussionReceiptSuspect = `CASE WHEN json_valid(a.receipt_json) THEN
	 json_type(a.receipt_json,'$.discussion_only') IS NOT NULL OR json_type(a.receipt_json,'$.discussion_only_excerpt') IS NOT NULL
	 ELSE 1 END`

func chatDiscussionRootLimitInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, origin string, source chatDiscussionRootSource) error {
	// Consider only the root's indexed deliveries. Either pointer can locate a
	// suspected restriction, but both must agree to prove it. The BLOB marker
	// can also occur in plain text, so stream the audience without a raw hit cap.
	const maxProofBytes = 192 << 10
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.notice_message_id,c.id,c.member_id,c.turn_id,coalesce(c.policy_revision,0),
	 CASE WHEN typeof(a.payload)='blob' AND length(a.payload)<=:max_bytes THEN a.payload END,
	 substr(CAST(a.receipt_json AS BLOB),1,:max_bytes+1), `+chatDiscussionReceiptSuspect+`,
	 coalesce((c.group_id,c.topic_id,c.message_id,c.generation)=(:group,:topic,:origin,:sequence)
	 AND (i.turn_id,i.purpose,i.source_id,i.source_kind)=(c.turn_id,:purpose,:source,'local_user_chat')
	 AND typeof(i.claim_digest)='blob' AND length(i.claim_digest)=32
	 AND a.decision_id IS NULL AND a.dispatch_request_id IS NULL
	 AND EXISTS(SELECT 1 FROM messages n WHERE (n.id,n.group_id,n.topic_id,n.sender_id)=(a.notice_message_id,c.group_id,c.topic_id,c.member_id))
	 AND `+chatDiscussionReceiveMatches+`,0)
	 FROM deliveries rd
	 CROSS JOIN turns rt ON rt.delivery_id=rd.id OR rt.id=rd.turn_id
	 CROSS JOIN chat_candidates c ON c.turn_id=rt.id
	 CROSS JOIN chat_assessments a ON a.candidate_id=c.id
	 LEFT JOIN chat_intake_executions i ON i.candidate_id=c.id
	 WHERE rd.message_id=:origin
	 AND (`+chatDiscussionReceiptSuspect+` OR instr(CAST(a.payload AS BLOB),CAST('"discussion_only_excerpt"' AS BLOB))>0)`,
		sql.Named("origin", origin), sql.Named("group", thread.GroupID), sql.Named("topic", thread.TopicID),
		sql.Named("sequence", source.sequence), sql.Named("purpose", domain.ChatIntakePurpose), sql.Named("source", source.id),
		sql.Named("max_bytes", maxProofBytes))
	if err != nil {
		return err
	}
	defer rows.Close()
	var quote string
	for rows.Next() {
		var receiptID, candidateID, memberID, turnID string
		var noticeID sql.NullString
		var revision int64
		var payload, data []byte
		var suspect, bound bool
		if err := rows.Scan(&receiptID, &noticeID, &candidateID, &memberID, &turnID, &revision, &payload, &data, &suspect, &bound); err != nil {
			return err
		}
		excerpt, err := chatDiscussionLimitExcerpt(payload, candidateID)
		if err != nil {
			return err
		}
		if excerpt == nil && !suspect {
			continue // A text-only marker does not count as a restriction.
		}
		if excerpt == nil || !suspect || !bound || quote != "" || len(data) > maxProofBytes {
			return ErrMemberToolDenied
		}
		// Decode the immutable receipt, not the live follow-up projection.
		var r domain.ChatAssessmentReceipt
		if !decodeChatDiscussionProof(data, &r) || !r.DiscussionOnly || r.DiscussionOnlyExcerpt != *excerpt ||
			r.ID != receiptID || r.CandidateID != candidateID || r.ThreadRef != thread || r.MessageID != origin ||
			r.SourceKind != "local_user_chat" || r.SourceID != source.id || r.SourceSenderID != "user" || r.CandidateGeneration != source.sequence ||
			r.AssessedBy != memberID || r.TurnID != turnID || r.Purpose != domain.ChatIntakePurpose || r.PolicyRevision != revision ||
			r.NoticeMessageID == "" || r.NoticeMessageID != noticeID.String || r.Disposition != "discuss" || r.Outcome != "discuss" ||
			r.Action != "" || r.Authority != "" || r.AuthorizationMessageID != "" || r.AuthorityEvidenceVerified ||
			r.TaskID != "" || r.MandateID != "" || r.DispatchRequestID != "" || r.WorkInputID != "" || !strings.Contains(source.content, *excerpt) {
			return ErrMemberToolDenied
		}
		quote = *excerpt
	}
	if err := rows.Err(); err != nil || quote == "" {
		return err
	}
	return chatDeny("discussion_only", fmt.Sprintf("本机用户在消息 %s（来源 %s）中明确限制：「%s」。", origin, source.id, quote),
		"是否发出一条新消息，明确现在需要开展哪项工作？")
}

func chatDiscussionLimitExcerpt(payload []byte, candidateID string) (*string, error) {
	// mutationPayload writes four byte strings, each prefixed by a uint64 length.
	var fields [4][]byte
	for i := range fields {
		if len(payload) < 8 {
			return nil, ErrMemberToolDenied
		}
		n := binary.BigEndian.Uint64(payload[:8])
		payload = payload[8:]
		if n > uint64(len(payload)) {
			return nil, ErrMemberToolDenied
		}
		fields[i], payload = payload[:n], payload[n:]
	}
	var input domain.ChatAssessment
	if len(payload) != 0 || string(fields[0]) != "chat_assessment/v1" || string(fields[1]) != candidateID ||
		!decodeChatDiscussionProof(fields[2], &input) || validateChatAssessment(input, string(fields[3])) != nil {
		return nil, ErrMemberToolDenied
	}
	return input.DiscussionOnlyExcerpt, nil
}

func decodeChatDiscussionProof(data []byte, target any) bool {
	if err := json.Unmarshal(data, target); err != nil {
		return false
	}
	// Store-generated proofs use the typed encoder, with no duplicate keys.
	encoded, err := json.Marshal(target)
	return err == nil && bytes.Equal(data, encoded)
}

func chatSourceSupersededInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, generation int64) (bool, error) {
	var superseded bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
	 SELECT 1 FROM message_log l JOIN message_command_sources s ON s.message_id=l.message_id AND s.kind='local_user_chat'
	 WHERE l.group_id=? AND l.topic_id=? AND l.sequence>?)`, thread.GroupID, thread.TopicID, generation).Scan(&superseded)
	return superseded, err
}

func chatStaleSource() error {
	return chatDeny("stale_source", "已有更新的真实用户聊天命令，本次旧评估未产生新工作。", "是否以最新用户要求为准？")
}
