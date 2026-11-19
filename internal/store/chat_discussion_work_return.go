package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"agentworkspace/internal/domain"
)

var chatDiscussionWorkReturnVersion = slices.Index(migrations, chatDiscussionWorkReturnSchema) + 1

var chatDiscussionWorkReturnDDL = func() string {
	// In immutable v31, column-one CREATEs start statements and column-one
	// comments separate them. SQLite stores only the statement, without ';'.
	ddl := strings.Split(chatDiscussionWorkReturnSchema, "\nCREATE ")[1:]
	for i, body := range ddl {
		body, _, _ = strings.Cut(body, "\n--")
		ddl[i] = "CREATE " + strings.TrimSuffix(strings.TrimSpace(body), ";")
	}
	encoded, _ := json.Marshal(ddl) // A slice of strings always marshals.
	return string(encoded)
}()

// A missing registration is legacy only if the return footprint is absent.
// With a marker, require the original table (including composite foreign
// keys) and every v31 trigger body, including triggers on other tables.
func chatDiscussionWorkReturnInstalledInTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var state int
	err := tx.QueryRowContext(ctx, `WITH footprint AS (
	 SELECT type,sql FROM main.sqlite_schema WHERE name='chat_discussion_work_returns' COLLATE NOCASE
	 OR (type='trigger' AND lower(name) GLOB 'chat_discussion_work_return[s_]*')
	)
	SELECT CASE
	 WHEN EXISTS(SELECT 1 FROM main.schema_migrations WHERE version=?1) THEN
	   CASE WHEN EXISTS(
	     SELECT value FROM json_each(?2)
	     EXCEPT SELECT sql FROM footprint WHERE type IN ('table','trigger')
	   ) THEN -1 ELSE 1 END
	 WHEN (?1>0 AND EXISTS(SELECT 1 FROM main.schema_migrations WHERE version>?1))
	   OR EXISTS(SELECT 1 FROM footprint) THEN -1
	 ELSE 0 END`, chatDiscussionWorkReturnVersion, chatDiscussionWorkReturnDDL).Scan(&state)
	if err != nil {
		return false, err
	}
	if state == -1 {
		return false, fmt.Errorf("%w：讨论工作回传迁移记录与数据库结构不一致", ErrConflict)
	}
	return state == 1, nil
}

// chatDiscussionWorkReturnInTx is called only from HandbackWork's fenced
// owner transaction, after B has persisted and verified its own receipt.
// Propagate errors to the caller's rollback. Do not replace B's receipt or
// invoke this on a different transaction. replay is B's mutation lookup
// result, not an input option: old receipts must never wake retroactively.
func chatDiscussionWorkReturnInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, handback domain.TaskWorkHandback, replay bool) (*domain.ChatDiscussionWorkReturnReceipt, error) {
	installed, err := chatDiscussionWorkReturnInstalledInTx(ctx, tx)
	if err != nil || !installed {
		return nil, err
	}
	f, attempt, err := taskWorkOwnerInTx(ctx, tx, scope)
	if err != nil {
		return nil, err
	}
	if handback.TaskWorkParent != taskWorkParent(f) || handback.AuthorTurnID != f.turnID || handback.AuthorAttemptID != attempt ||
		handback.CoordinatorID != f.v.r.CoordinatorMemberID {
		return nil, ErrMemberToolDenied
	}
	saved, err := chatDiscussionWorkHandbackInTx(ctx, tx, handback.GroupID, handback.ID)
	if err != nil {
		return nil, err
	}
	if saved != handback {
		return nil, ErrMemberToolDenied
	}
	prior, err := chatDiscussionWorkReturnReceiptInTx(ctx, tx, saved)
	if err != nil || prior != nil || replay {
		return prior, err // Independent of current member status and live delivery state.
	}

	route, status, err := chatDiscussionWorkReturnRouteInTx(ctx, tx, f.v)
	if err != nil || route == nil {
		return nil, err
	}
	targets := []string{}
	if status == domain.MemberStatusActive {
		targets = append(targets, route.RecipientID)
	}
	body := chatDiscussionWorkReturnBody(route.OriginMessageID, saved)
	if !memberToolTextValid(body, 16<<10) {
		return nil, ErrInvalid
	}
	// Keep it an ordinary delivery with no command proof or Work role marker.
	// The typed A relation, not a chat command or the body, identifies the return.
	m, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: route.GroupID, TopicID: route.TopicID,
		ActorID: saved.OwnerMemberID, Content: body, RecipientIDs: targets}, targets)
	if err != nil {
		return nil, err
	}
	if len(m.Deliveries) != len(targets) {
		return nil, ErrConflict
	}
	route.HandbackID, route.HandbackNoticeMessageID = saved.ID, saved.NoticeMessageID
	route.MessageID, route.SenderID = m.ID, saved.OwnerMemberID
	if len(targets) == 1 {
		d := m.Deliveries[0]
		if d.RecipientID != route.RecipientID || d.State != domain.DeliveryStateQueued || d.TurnID != "" || d.Error != "" {
			return nil, ErrConflict
		}
		route.DeliveryID, route.ConversationID = d.ID, d.ConversationID
	}
	if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO chat_discussion_work_returns
	 (origin_message_id,group_id,topic_id,invitation_message_id,handback_id,message_id,delivery_id) VALUES(?,?,?,?,?,?,nullif(?,''))`,
		route.OriginMessageID, route.GroupID, route.TopicID, route.InvitationMessageID, route.HandbackID, route.MessageID, route.DeliveryID); err != nil {
		return nil, err
	}
	published, err := chatDiscussionWorkReturnReceiptInTx(ctx, tx, saved)
	if err == nil && (published == nil || *published != *route) {
		return nil, ErrConflict
	}
	return published, err
}

// Follow the Assignment's exact Mandate origin, never a Task hint, newer
// same-topic message, or ordinary reply. The admission belongs to the Turn
// that received the invitation, not the Turn that published it or the worker.
func chatDiscussionWorkReturnRouteInTx(ctx context.Context, tx *sql.Tx, v assignmentSnapshot) (*domain.ChatDiscussionWorkReturnReceipt, domain.MemberStatus, error) {
	r := v.r
	thread := domain.ThreadRef{GroupID: r.GroupID, TopicID: r.TopicID}
	var invitation, origin, kind, eventGroup, eventTopic, candidate, turn string
	var invitationSequence int64
	err := tx.QueryRowContext(ctx, `SELECT t.message_id,t.message_sequence,coalesce(e.origin_message_id,''),coalesce(e.kind,''),
	 coalesce(e.group_id,''),coalesce(e.topic_id,''),coalesce(c.id,''),coalesce(c.turn_id,'')
	 FROM mandates m JOIN coordination_triggers t ON (t.id,t.group_id,t.topic_id)=(m.origin_trigger_id,m.group_id,m.topic_id)
	 LEFT JOIN chat_discussion_events e ON e.message_id=t.message_id
	 LEFT JOIN chat_assessments a ON a.dispatch_request_id=? AND a.decision_id=m.decision_id
	 LEFT JOIN chat_candidates c ON c.id=a.candidate_id
	 WHERE (m.id,m.task_id,m.group_id,m.topic_id)=(?,?,?,?)`, r.ID, r.MandateID, r.TaskID, r.GroupID, r.TopicID).
		Scan(&invitation, &invitationSequence, &origin, &kind, &eventGroup, &eventTopic, &candidate, &turn)
	if err != nil {
		return nil, "", coordinationRoleRelationError(err)
	}
	if kind != domain.ChatDiscussionInvite {
		return nil, "", nil // No structured invitation origin for this B result.
	}
	if eventGroup != r.GroupID || eventTopic != r.TopicID {
		return nil, "", ErrMemberToolDenied
	}
	admission, err := chatDiscussionWorkAdmissionInTx(ctx, tx, thread, invitation, candidate, turn)
	if err != nil {
		return nil, "", coordinationRoleRelationError(err)
	}
	if admission == nil || admission.TaskID != r.TaskID || admission.MandateID != r.MandateID || admission.DispatchRequestID != r.ID ||
		admission.Assignment == nil || *admission.Assignment != v.a {
		return nil, "", ErrMemberToolDenied
	}

	// A5 closes ordinary moves once work is admitted. Use the structural
	// head, not the active owner, to find the still-reserved return slot.
	chain, err := chatDiscussionChainInTx(ctx, tx, thread, origin)
	if err != nil {
		return nil, "", err
	}
	if chain == nil {
		return nil, "", ErrMemberToolDenied
	}
	if chain.HeadMessageID != invitation {
		return nil, "", nil // An ordinary step already consumed this head; keep B's notice only.
	}
	// A current invite means the bounded path contains only pushes: its unwind
	// was reserved at publication. Prove the actual publishers and the first
	// member's immutable local-user intake, not just the stored root ID.
	messages := make([]domain.Message, 0, len(chain.Events))
	for _, step := range chain.Events {
		messages = append(messages, domain.Message{ID: step.Event.MessageID})
	}
	if err := messageDiscussionsInTx(ctx, tx, r.GroupID, messages); err != nil {
		return nil, "", err
	}
	for _, m := range messages {
		if m.Discussion == nil || m.Discussion.Kind != domain.ChatDiscussionInvite || m.Discussion.RootMessageID != origin {
			return nil, "", ErrMemberToolDenied
		}
	}

	var status domain.MemberStatus
	err = tx.QueryRowContext(ctx, `SELECT root.status FROM chat_discussion_events first
	 JOIN members root ON (root.id,root.group_id)=(first.actor_id,first.group_id)
	 JOIN chat_candidates c ON c.id=first.candidate_id JOIN turns t ON t.id=c.turn_id
	 JOIN message_log l ON (l.message_id,l.group_id,l.topic_id)=(first.origin_message_id,first.group_id,first.topic_id)
	 WHERE first.origin_message_id=? AND first.step=1 AND root.id=? AND l.sequence<?
	 AND NOT EXISTS(SELECT 1 FROM turns other WHERE other.delivery_id=t.delivery_id AND other.id<>t.id)`,
		origin, chain.RootInviterID, invitationSequence).Scan(&status)
	if err != nil {
		return nil, "", coordinationRoleRelationError(err)
	}
	if status != domain.MemberStatusActive && status != domain.MemberStatusLeft {
		return nil, "", ErrMemberToolDenied
	}
	head := chain.Events[len(chain.Events)-1].Event
	if head.RecipientID == chain.RootInviterID {
		return nil, "", ErrMemberToolDenied
	}
	if err := chatDiscussionWorkReturnIntakeInTx(ctx, tx, turn, head.DeliveryID, candidate); err != nil {
		return nil, "", err
	}
	// HandbackWork already checked current authority. Later user chat may
	// supersede a prompt for new work, but does not retarget this result.
	return &domain.ChatDiscussionWorkReturnReceipt{ThreadRef: thread, OriginMessageID: origin,
		InvitationMessageID: invitation, RecipientID: chain.RootInviterID}, status, nil
}

// Call only after proving admission for this exact receiving Turn, in the
// Worker's serialized write transaction. Known running intake is not terminal:
// the result wakes the distinct root, without settling or releasing intake.
func chatDiscussionWorkReturnIntakeInTx(ctx context.Context, tx *sql.Tx, turnID, deliveryID, candidateID string) error {
	var t domain.Turn
	var accepted, receiptJSON, claim string
	var capturedDigest []byte
	err := tx.QueryRowContext(ctx, `SELECT h.state,h.native_session_id,h.candidate_terminal,h.reason,h.accepted_at,
	 CASE WHEN length(CAST(h.receipt_json AS BLOB))<=1048576 THEN h.receipt_json ELSE '' END,
	 CASE WHEN h.state='running' THEN h.claim_token ELSE '' END,
	 CASE WHEN h.state='running' THEN i.claim_digest END
	 FROM turns h JOIN deliveries d ON d.id=h.delivery_id AND d.turn_id=h.id
	 JOIN chat_intake_executions i ON i.turn_id=h.id AND i.candidate_id=? AND i.purpose=?
	 WHERE h.id=? AND d.id=? AND d.state='accepted' AND d.error='' AND h.accepted_at IS NOT NULL
	 AND NOT EXISTS(SELECT 1 FROM turns other WHERE other.delivery_id=d.id AND other.id<>h.id)
	 AND ((h.state='running' AND h.cancel_requested_at IS NULL AND h.candidate_terminal=''
	   AND h.reason='' AND h.receipt_json='' AND h.reply_message_id=''
	   AND EXISTS(SELECT 1 FROM members m WHERE (m.id,m.group_id)=(h.member_id,h.group_id) AND m.status='active')
	   AND NOT EXISTS(SELECT 1 FROM task_work_deliveries w WHERE w.delivery_id=d.id OR w.message_id=d.message_id))
	  OR (h.state IN ('succeeded','failed','aborted','truncated') AND NOT EXISTS(SELECT 1 FROM resource_claims WHERE turn_id=h.id)))`,
		candidateID, domain.ChatIntakePurpose, turnID, deliveryID).
		Scan(&t.State, &t.NativeSessionID, &t.CandidateTerminal, &t.Reason, &accepted, &receiptJSON, &claim, &capturedDigest)
	if err != nil {
		return coordinationRoleRelationError(err)
	}
	stamp, err := time.Parse(timestampLayout, accepted)
	if err != nil || stamp.Format(timestampLayout) != accepted || !validNativeSession(t.NativeSessionID) {
		return ErrMemberToolDenied
	}
	if t.State == domain.TurnStateRunning {
		digest := sha256.Sum256([]byte(claim))
		if !memberToolIDsValid(claim) || !bytes.Equal(capturedDigest, digest[:]) {
			return ErrMemberToolDenied
		}
		return nil
	}
	// Only the explicit terminal branch uses a receipt. Recovery can rotate
	// the intake token without invalidating its immutable admission.
	t.AcceptedAt = &stamp
	r, err := decodeNativeReceipt(t, receiptJSON)
	if err != nil || string(r.Outcome) != string(t.State) || r.Reason != t.Reason || r.NativeSessionID != t.NativeSessionID {
		return ErrMemberToolDenied
	}
	return nil
}

// Keep the v1 body stable; the receipt reader uses it to verify the message.
func chatDiscussionWorkReturnBody(origin string, h domain.TaskWorkHandback) string {
	return fmt.Sprintf("平台回传：原讨论关联的工作说明。\n原讨论消息：%s\n任务：%s\n工作移交：%s（通知 %s）\n本消息不代表执行完成、成果接纳或新授权。\n\n负责成员说明：\n%s",
		origin, h.TaskID, h.ID, h.NoticeMessageID, h.Summary)
}
