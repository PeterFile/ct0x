package store

import (
	"context"
	"database/sql"
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

// A local user source may have multiple recipient Turns. A structured handoff
// inherits the same source limit, but never the origin's work authority.
func chatDiscussionWorkGateInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate) error {
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
		if c.SourceKind != "local_user_chat" {
			return nil // Ordinary member messages do not inherit a limit.
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
	// A newer handoff cannot make the root current again after a user command.
	stale, err := chatSourceSupersededInTx(ctx, tx, c.ThreadRef, source.sequence)
	if err != nil {
		return err
	}
	if stale {
		return chatStaleSource()
	}
	return chatDiscussionRootLimitInTx(ctx, tx, c.ThreadRef, origin, source)
}

func chatDiscussionRootLimitInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, origin string, source chatDiscussionRootSource) error {
	// Start at the root's deliveries, not the topic's assessment history. Read
	// at most two claims: multiple rows fail closed without choosing a quote.
	// Historical proof does not depend on the assessor's live claim or status.
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.notice_message_id,c.id,c.member_id,c.turn_id,coalesce(c.policy_revision,0),
	 coalesce((c.group_id,c.topic_id,c.message_id,c.member_id,c.generation)=(d.group_id,d.topic_id,d.message_id,d.recipient_id,?)
	 AND (i.turn_id,i.purpose,i.source_id,i.source_kind)=(c.turn_id,?,?,'local_user_chat')
	 AND a.decision_id IS NULL AND a.dispatch_request_id IS NULL,0)
	 FROM deliveries d
	 JOIN turns t ON (t.delivery_id,t.group_id,t.conversation_id,t.member_id)=(d.id,d.group_id,d.conversation_id,d.recipient_id)
	 JOIN chat_candidates c ON c.turn_id=t.id
	 JOIN chat_assessments a ON a.candidate_id=c.id
	 LEFT JOIN chat_intake_executions i ON i.candidate_id=c.id
	 WHERE d.message_id=? AND d.group_id=? AND d.topic_id=?
	 AND (json_type(a.receipt_json,'$.discussion_only') IS NOT NULL OR json_type(a.receipt_json,'$.discussion_only_excerpt') IS NOT NULL)
	 LIMIT 2`, source.sequence, domain.ChatIntakePurpose, source.id, origin, thread.GroupID, thread.TopicID)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		return rows.Err()
	}
	var receiptID, candidateID, memberID, turnID string
	var noticeID sql.NullString
	var revision int64
	var bound bool
	if err := rows.Scan(&receiptID, &noticeID, &candidateID, &memberID, &turnID, &revision, &bound); err != nil {
		return err
	}
	if !bound || rows.Next() {
		return ErrMemberToolDenied
	}
	if err := rows.Err(); err != nil {
		return err
	}
	r, err := chatAssessmentReceiptInTx(ctx, tx, receiptID, candidateID)
	if err != nil {
		return err
	}
	if !r.DiscussionOnly || r.ID != receiptID || r.CandidateID != candidateID || r.ThreadRef != thread || r.MessageID != origin ||
		r.SourceKind != "local_user_chat" || r.SourceID != source.id || r.SourceSenderID != "user" || r.CandidateGeneration != source.sequence ||
		r.AssessedBy != memberID || r.TurnID != turnID || r.Purpose != domain.ChatIntakePurpose || r.PolicyRevision != revision ||
		r.NoticeMessageID == "" || r.NoticeMessageID != noticeID.String || r.Disposition != "discuss" || r.Outcome != "discuss" ||
		r.Authority != "" || r.AuthorityEvidenceVerified || r.TaskID != "" || r.MandateID != "" || r.DispatchRequestID != "" ||
		!memberToolTextValid(r.DiscussionOnlyExcerpt, chatDiscussionOnlyExcerptBytes) || !strings.Contains(source.content, r.DiscussionOnlyExcerpt) {
		return ErrMemberToolDenied
	}
	return chatDeny("discussion_only", fmt.Sprintf("本机用户在消息 %s（来源 %s）中明确限制：「%s」。", origin, r.SourceID, r.DiscussionOnlyExcerpt),
		"是否发出一条新消息，明确现在需要开展哪项工作？")
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
