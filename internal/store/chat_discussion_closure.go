package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

const (
	chatDiscussionNotStartedReason   = "接收成员的讨论轮未启动"
	chatDiscussionNoActionReason     = "讨论已中断：接收成员已完成本轮，但未提交结构化讨论动作"
	chatDiscussionAbnormalExitReason = "讨论已中断：接收成员的轮次异常结束，未提交结构化讨论动作"
)

const chatDiscussionClosureEligible = `((d.state='failed' AND d.turn_id='' AND d.error='` + chatDiscussionRecipientLeftReason + `'
     AND NOT EXISTS(SELECT 1 FROM turns WHERE delivery_id=d.id))
 OR (d.state='failed' AND t.state='not_started' AND d.error=t.reason AND t.accepted_at IS NULL
     AND t.native_session_id='' AND t.candidate_text='' AND t.reply_message_id='')
 OR (d.state='accepted' AND t.state IN ('succeeded','failed','aborted','truncated') AND d.error=''
     AND t.accepted_at IS NOT NULL AND t.native_session_id<>''))`

const chatDiscussionAmbiguousReceiver = `EXISTS(SELECT 1 FROM turns WHERE delivery_id=d.id LIMIT 1 OFFSET 1)`

// Bind the current event to its receiving Turn, never to the Turn that
// published it. The indexed assessment join loads that candidate's unique
// receipt for B's proof; it does not prove admission or require a live Turn.
const chatDiscussionHeadSelect = `SELECT e.message_id, e.topic_id, coalesce(t.id,''), coalesce(a.candidate_id,''), a.receipt_json,
 coalesce(` + chatDiscussionClosureEligible + `,0), ` + chatDiscussionAmbiguousReceiver + `,
 r.status, coalesce(t.state,''), coalesce(t.native_session_id,''),
 t.accepted_at, coalesce(t.candidate_terminal,''), coalesce(t.reason,''),
 CASE WHEN a.candidate_id IS NULL AND length(CAST(t.receipt_json AS BLOB))<=1048576 THEN t.receipt_json ELSE '' END
 FROM chat_discussion_events e
 JOIN chat_discussions c ON c.origin_message_id=e.origin_message_id AND c.group_id=e.group_id AND c.topic_id=e.topic_id
 JOIN messages o ON o.id=c.origin_message_id AND o.group_id=c.group_id AND o.topic_id=c.topic_id AND o.sender_id='user'
 JOIN messages m ON m.id=e.message_id AND m.group_id=e.group_id AND m.topic_id=e.topic_id AND m.sender_id=e.actor_id
 JOIN deliveries d ON d.id=e.delivery_id AND d.message_id=m.id AND d.group_id=m.group_id AND d.topic_id=m.topic_id AND d.recipient_id=e.recipient_id
 JOIN conversations v ON v.id=d.conversation_id AND v.group_id=d.group_id AND v.topic_id=d.topic_id AND v.member_id=d.recipient_id
 JOIN members r ON r.id=d.recipient_id AND r.group_id=d.group_id
 LEFT JOIN turns t ON t.id=d.turn_id AND d.turn_id<>'' AND t.delivery_id=d.id AND t.group_id=d.group_id
    AND t.conversation_id=d.conversation_id AND t.member_id=d.recipient_id
 LEFT JOIN chat_candidates candidate ON e.kind='invite' AND candidate.turn_id=t.id
    AND (candidate.group_id,candidate.topic_id,candidate.message_id,candidate.member_id)=(d.group_id,d.topic_id,m.id,d.recipient_id)
 LEFT JOIN chat_assessments a ON a.candidate_id=candidate.id AND a.decision_id IS NOT NULL AND a.dispatch_request_id IS NOT NULL
 WHERE e.kind IN ('invite','reply')
 AND EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=o.id AND kind='local_user_chat')
 AND EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=m.id AND kind='member')
 AND NOT EXISTS(SELECT 1 FROM chat_discussion_events n WHERE n.origin_message_id=e.origin_message_id AND n.step>e.step)
 AND NOT EXISTS(SELECT 1 FROM deliveries extra WHERE extra.message_id=m.id AND extra.id<>d.id)
 AND (a.candidate_id IS NOT NULL OR ` + chatDiscussionClosureEligible + ` OR ` + chatDiscussionAmbiguousReceiver + `)`

type chatDiscussionHeadState struct {
	work          *domain.ChatDiscussionWork
	closureReason string
	pendingProof  bool
}

type chatDiscussionAdmissionProbe struct {
	messageID, topicID, turnID, candidateID string
}

// chatDiscussionHeadsInTx projects proved admission, a head pending proof,
// or interruption for an already authorized, bounded set of message IDs.
// Topics are pinned by the message and delivery joins. Use the caller's
// snapshot, not publication receipts, to read this evidence.
func chatDiscussionHeadsInTx(ctx context.Context, tx *sql.Tx, groupID string, messageIDs ...string) (map[string]chatDiscussionHeadState, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	// Skip only legacy storage. Do not treat a missing registered table (or
	// evidence of partial installation) as a legacy schema.
	var available bool
	if err := tx.QueryRowContext(ctx, `SELECT CASE WHEN EXISTS(SELECT 1 FROM schema_migrations WHERE version>=24) THEN 1
	 ELSE EXISTS(SELECT 1 FROM sqlite_schema WHERE name IN ('chat_discussions','chat_discussion_events')) END`).Scan(&available); err != nil || !available {
		return nil, err
	}
	args := make([]any, 1, len(messageIDs)+1)
	args[0] = groupID
	for _, id := range messageIDs {
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, chatDiscussionHeadSelect+` AND e.group_id=? AND e.message_id IN (?`+strings.Repeat(",?", len(messageIDs)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var heads map[string]chatDiscussionHeadState
	var probes []chatDiscussionAdmissionProbe
	for rows.Next() {
		var probe chatDiscussionAdmissionProbe
		var data string
		var assessmentJSON []byte
		var closureEligible, ambiguousReceiver bool
		var status domain.MemberStatus
		var turn domain.Turn
		var accepted sql.NullString
		if err := rows.Scan(&probe.messageID, &probe.topicID, &probe.turnID, &probe.candidateID, &assessmentJSON,
			&closureEligible, &ambiguousReceiver, &status, &turn.State, &turn.NativeSessionID,
			&accepted, &turn.CandidateTerminal, &turn.Reason, &data); err != nil {
			return nil, err
		}
		if !ambiguousReceiver && probe.candidateID != "" {
			// Prove a typed decode failure on the exact joined receipt before
			// calling B, so errors from its other reads cannot stand in for it.
			var receipt domain.ChatAssessmentReceipt
			err := json.Unmarshal(assessmentJSON, &receipt)
			if err == nil {
				probes = append(probes, probe)
				continue
			}
			if !json.Valid(assessmentJSON) {
				return nil, err // Invalid JSON violates the table CHECK.
			}
		}
		if ambiguousReceiver || probe.candidateID != "" {
			// An ambiguous Turn or a malformed typed receipt proves neither
			// work nor interruption and cannot reopen the ordinary path.
			if heads == nil {
				heads = make(map[string]chatDiscussionHeadState)
			}
			heads[probe.messageID] = chatDiscussionHeadState{pendingProof: true}
			continue
		}
		if !closureEligible {
			continue
		}
		if accepted.Valid {
			stamp, err := time.Parse(timestampLayout, accepted.String)
			if err != nil {
				continue
			}
			turn.AcceptedAt = &stamp
		}
		if turn.State != "" {
			// The SQL filters candidate states; require the unchanged canonical
			// receipt from normalizeTurnReceipt, not just terminal-looking JSON.
			receipt, err := decodeNativeReceipt(turn, data)
			if err != nil || receipt.Outcome != provider.Outcome(turn.State) || receipt.Reason != turn.Reason || receipt.NativeSessionID != turn.NativeSessionID {
				continue
			}
		}
		var reason string
		switch turn.State {
		case "": // The explicitly marked unclaimed departure.
			reason = chatDiscussionRecipientLeftReason
		case domain.TurnStateNotStarted:
			reason = chatDiscussionNotStartedReason
			if status == domain.MemberStatusLeft {
				reason = chatDiscussionRecipientLeftReason
			}
		case domain.TurnStateSucceeded:
			reason = chatDiscussionNoActionReason
		case domain.TurnStateFailed, domain.TurnStateAborted, domain.TurnStateTruncated:
			reason = chatDiscussionAbnormalExitReason
		default:
			continue
		}
		if heads == nil {
			heads = make(map[string]chatDiscussionHeadState)
		}
		heads[probe.messageID] = chatDiscussionHeadState{closureReason: reason}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, probe := range probes {
		thread := domain.ThreadRef{GroupID: groupID, TopicID: probe.topicID}
		admission, err := chatDiscussionWorkAdmissionInTx(ctx, tx, thread, probe.messageID, probe.candidateID, probe.turnID)
		if err != nil && !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrNotFound) && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if heads == nil {
			heads = make(map[string]chatDiscussionHeadState)
		}
		if err != nil || admission == nil {
			// The exact receiving candidate has a persisted decision and dispatch
			// reference, but not enough proof to project work or reopen the path.
			heads[probe.messageID] = chatDiscussionHeadState{pendingProof: true}
			continue
		}
		work := &domain.ChatDiscussionWork{TaskID: admission.TaskID, MandateID: admission.MandateID, DispatchRequestID: admission.DispatchRequestID}
		if a := admission.Assignment; a != nil {
			work.AssignmentID, work.WorkerMemberID = a.ID, a.WorkerMemberID
		}
		heads[probe.messageID] = chatDiscussionHeadState{work: work}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return heads, nil
}
