package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"strings"

	"agentworkspace/internal/domain"
)

// CreateLocalUserMessage is a trusted command seam, not a member operation.
// Only a new command creates proof; replaying legacy data never upgrades it.
func (s *Store) CreateLocalUserMessage(ctx context.Context, r MessageRequest, recipients []string) (domain.Message, error) {
	return s.createLocalUserMessage(ctx, r, recipients, false)
}

// PostLocalUserMessage is the normal local chat entry. A caller supplying
// authoritative deliveries directly continues to use CreateLocalUserMessage.
func (s *Store) PostLocalUserMessage(ctx context.Context, r MessageRequest) (domain.Message, error) {
	return s.createLocalUserMessage(ctx, r, r.RecipientIDs, true)
}

func (s *Store) createLocalUserMessage(ctx context.Context, r MessageRequest, recipients []string, normalPost bool) (domain.Message, error) {
	if r.ActorID != "user" {
		return domain.Message{}, ErrMemberToolDenied
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Message{}, storeError("创建用户消息失败", err)
	}
	defer tx.Rollback()
	if err := requireMessageActor(ctx, tx, r.GroupID, "user"); err != nil {
		return domain.Message{}, storeError("验证用户消息失败", err)
	}
	payload := r.payload()
	reservation := payload
	bindSelector := normalPost && r.TopicID == "" && r.IdempotencyKey != ""
	if bindSelector {
		reservation = r.topicSelectionPayload()
	}
	id, replay, err := reserveMutation(ctx, tx, r.scope(), reservation)
	if bindSelector && errors.Is(err, ErrConflict) {
		// Exact legacy reservations are still replayable; this only reads
		// and cannot create an unversioned key or rewrite its source proof.
		id, replay, err = lookupMutation(ctx, tx, r.scope(), payload)
	}
	if errors.Is(err, ErrConflict) && r.hasLegacyQuotePayload() {
		id, replay, err = lookupMutation(ctx, tx, r.scope(), r.encodePayload("", true))
	}
	if err != nil {
		return domain.Message{}, storeError("读取用户消息回执失败", err)
	}
	var message domain.Message
	if replay {
		message, err = localMessageReceiptInTx(ctx, tx, r, id, normalPost)
	} else {
		message, err = publishLocalMessageInTx(ctx, tx, r, id, payload, recipients, normalPost)
	}
	if err != nil {
		return domain.Message{}, storeError("创建用户消息失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Message{}, storeError("保存用户消息失败", err)
	}
	return message, nil
}

func messageCommandSourceInTx(ctx context.Context, tx *sql.Tx, messageID, kind string, payload []byte, key string, scope *MemberToolScope) (string, error) {
	id := rand.Text()
	var turn, digest any
	if scope != nil {
		turn = scope.Claim.TurnID
		sum := sha256.Sum256([]byte(scope.Claim.Token))
		digest = sum[:]
	}
	err := coordinationDispatchWrite(ctx, tx, `INSERT INTO message_command_sources(id,message_id,kind,payload,idempotency_key,turn_id,claim_digest) VALUES(?,?,?,?,?,?,?)`, id, messageID, kind, payload, key, turn, digest)
	return id, err
}

// Materializer/terminal relationships are proof of platform/member provenance,
// not local-user consent. Missing legacy evidence stays unknown.
func messageSourceInTx(ctx context.Context, tx *sql.Tx, messageID string) (kind, id string, err error) {
	messages := []domain.Message{{ID: messageID}}
	if err := messageSourcesInTx(ctx, tx, messages); err != nil {
		return "", "", err
	}
	if messages[0].SourceKind == "" {
		messages[0].SourceKind = "unknown"
	}
	return messages[0].SourceKind, messages[0].SourceID, nil
}

// One bounded, indexed query per page, never one provenance query per message.
func messageSourcesInTx(ctx context.Context, tx *sql.Tx, messages []domain.Message) error {
	if len(messages) == 0 {
		return nil
	}
	var available, hasReviewNotices, hasWorkspaceNotices, hasTaskWork, hasWorkInputs, hasWorkReports bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=17),
	 EXISTS(SELECT 1 FROM schema_migrations WHERE version=18),
	 EXISTS(SELECT 1 FROM schema_migrations WHERE version=21),
	 EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='task_work_deliveries'),
	 EXISTS(SELECT 1 FROM schema_migrations WHERE version=?),`+workInputSchemaExists, taskWorkReportVersion, taskWorkInputVersion).
		Scan(&available, &hasReviewNotices, &hasWorkspaceNotices, &hasTaskWork, &hasWorkReports, &hasWorkInputs); err != nil {
		return err
	}
	if !available && !hasWorkspaceNotices {
		return nil
	} // Older schemas have no verified source; broken v21 storage must fail.
	args := make([]any, len(messages))
	positions := make(map[string]int, len(messages))
	for i, m := range messages {
		args[i] = m.ID
		positions[m.ID] = i
	}
	reviewKind, reviewSource, reviewJoin := "", "", ""
	if hasReviewNotices {
		reviewKind = "CASE WHEN rn.review_id IS NOT NULL THEN 'platform' END,"
		reviewSource = "CASE WHEN rn.review_id IS NOT NULL THEN 'review:'||rn.review_id END,"
		reviewJoin = " LEFT JOIN chat_review_notices rn ON rn.message_id=m.id"
	}
	workspaceKind, workspaceSource, workspaceJoin := "", "", ""
	if hasWorkspaceNotices {
		workspaceKind = "CASE WHEN wb.id IS NOT NULL OR wo.id IS NOT NULL THEN 'platform' END,"
		workspaceSource = "CASE WHEN wb.id IS NOT NULL THEN 'workspace_begin:'||wb.id WHEN wo.id IS NOT NULL THEN 'workspace_finish:'||wo.id END,"
		workspaceJoin = ` LEFT JOIN workspace_operations wb ON wb.notice_message_id=m.id AND wb.group_id=m.group_id
	 LEFT JOIN workspace_operation_receipts wf ON wf.notice_message_id=m.id
	 LEFT JOIN workspace_operations wo ON wo.id=wf.operation_id AND wo.group_id=m.group_id`
	}
	workKind, workSource, workJoin := "", "", ""
	if hasTaskWork {
		workKind = "CASE WHEN tw.delivery_id IS NOT NULL THEN 'platform' END,"
		workSource = "CASE WHEN tw.delivery_id IS NOT NULL THEN 'work:'||tw.source_kind||':'||tw.source_id END,"
		workJoin = ` LEFT JOIN task_work_deliveries tw ON tw.message_id=m.id AND tw.group_id=m.group_id
		 AND tw.topic_id=m.topic_id AND tw.sender_member_id=m.sender_id`
	}
	if hasWorkReports {
		workKind += "CASE WHEN hb.id IS NOT NULL THEN 'platform' END,"
		workSource += "CASE WHEN hb.id IS NOT NULL THEN 'work:handback:'||hb.id END,"
		workJoin += ` LEFT JOIN task_work_handbacks hb ON hb.notice_message_id=m.id AND hb.group_id=m.group_id
		 AND hb.topic_id=m.topic_id AND hb.owner_member_id=m.sender_id`
	}
	returnKind, returnSource, returnJoin := "", "", ""
	if hasWorkInputs {
		returnKind = "CASE WHEN handback.input_id IS NOT NULL THEN 'platform' END,"
		returnSource = "CASE WHEN handback.input_id IS NOT NULL THEN 'work_input_return:'||handback.input_id END,"
		returnJoin = " LEFT JOIN task_work_input_returns handback ON handback.notice_message_id=m.id AND handback.member_id=m.sender_id"
	}
	query := `SELECT m.id,
	 coalesce(` + workKind + returnKind + reviewKind + workspaceKind + `s.kind,CASE WHEN d.request_id IS NOT NULL OR a.assignment_id IS NOT NULL OR n.id IS NOT NULL OR sn.submission_id IS NOT NULL THEN 'platform' WHEN t.id IS NOT NULL THEN 'member' ELSE 'unknown' END),
	 coalesce(` + workSource + returnSource + reviewSource + workspaceSource + `s.id,CASE WHEN d.request_id IS NOT NULL THEN 'dispatch:'||d.request_id WHEN a.assignment_id IS NOT NULL THEN 'assignment:'||a.assignment_id WHEN n.id IS NOT NULL THEN 'assessment:'||n.id WHEN sn.submission_id IS NOT NULL THEN 'submission:'||sn.submission_id WHEN t.id IS NOT NULL THEN 'turn:'||t.id ELSE '' END)
	 FROM messages m LEFT JOIN message_command_sources s ON s.message_id=m.id
	 LEFT JOIN coordination_dispatch_state d ON d.message_id=m.id
	 LEFT JOIN coordination_assignment_dispatch a ON a.message_id=m.id
	 LEFT JOIN chat_assessments n ON n.notice_message_id=m.id
	 LEFT JOIN chat_submission_notices sn ON sn.message_id=m.id` + reviewJoin + workspaceJoin + workJoin + returnJoin + `
	 LEFT JOIN turns t ON t.reply_message_id=m.id AND t.reply_message_id<>''
	 WHERE m.id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(messages)), ",") + ")"
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind, source string
		if err := rows.Scan(&id, &kind, &source); err != nil {
			return err
		}
		i := positions[id]
		messages[i].SourceKind, messages[i].SourceID = kind, source
	}
	return rows.Err()
}
