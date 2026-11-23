package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"time"

	"agentworkspace/internal/domain"
)

// ApplyChatDecisionClarification is the trusted opt-in to a human-needed
// question, not a reinterpretation of ordinary clarify or a routing default.
// The caller supplies the real native assessment; there is no standalone
// Thread write accepting a proposed sender, source, recipient or relay target.
func (s *Store) ApplyChatDecisionClarification(ctx context.Context, scope MemberToolScope, candidateID string, input domain.ChatAssessment, details domain.DecisionContext, content, key string) (domain.DecisionRequest, error) {
	if input.Disposition != "clarify" {
		return domain.DecisionRequest{}, ErrInvalid
	}
	body, err := decisionQuestionJSON(domain.DecisionQuestion{DecisionContext: details, Question: input.Question}, domain.DecisionRequestClarification)
	if err != nil {
		return domain.DecisionRequest{}, decisionRequestError("记录话题待决事项失败", err)
	}
	capture := decisionClarificationCapture{body: body}
	if _, err := s.applyChatAssessment(ctx, scope, candidateID, input, content, key, false, &capture); err != nil {
		return domain.DecisionRequest{}, err
	}
	return capture.request, nil
}

type decisionClarificationCapture struct {
	body    string
	request domain.DecisionRequest
}

func checkDecisionClarificationSourceInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, c domain.ChatCandidate) error {
	if err := requireDecisionRequestsInTx(ctx, tx); err != nil {
		return err
	}
	if c.SourceKind != "local_user_chat" || c.SourceID == "" || c.SourceSenderID != "user" {
		return ErrMemberToolDenied
	}
	if _, err := chatDiscussionArrivalInTx(ctx, tx, scope, c.ID); err != nil {
		return err
	}
	// Admit only a directly addressed single recipient. The stored original
	// command distinguishes explicit addressing from a default delivery.
	const maxSourceBytes = 64 << 10
	r := MessageRequest{GroupID: c.GroupID, TopicID: c.TopicID, RecipientIDs: []string{c.MemberID}}
	var proof []byte
	err := tx.QueryRowContext(ctx, `SELECT m.content,s.payload FROM message_command_sources s
	 JOIN messages m ON (m.id,m.group_id,m.topic_id,m.sender_id)=(s.message_id,?,?, 'user')
	 WHERE (s.id,s.message_id,s.kind)=(?,?,'local_user_chat') AND length(CAST(m.content AS BLOB))<=?
	 AND length(s.payload)<=?`, c.GroupID, c.TopicID, c.SourceID, c.MessageID, maxSourceBytes, maxSourceBytes+4096).Scan(&r.Content, &proof)
	if err != nil {
		return coordinationRoleRelationError(err)
	}
	if !bytes.Equal(proof, r.payload()) {
		return ErrMemberToolDenied
	}
	stale, err := chatSourceSupersededInTx(ctx, tx, c.ThreadRef, c.Generation)
	if err == nil && stale {
		err = ErrConflict
	}
	return err
}

func (capture *decisionClarificationCapture) persistInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate, receipt domain.ChatAssessmentReceipt) error {
	if receipt.Outcome != "clarify" || receipt.ID == "" || receipt.TaskID != "" || receipt.WorkInputID != "" {
		return ErrConflict
	}
	id := rand.Text()
	stamp := receipt.CreatedAt.Format(timestampLayout)
	// Recipient comes from the same proved local-user source, not the assessor.
	if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO decision_requests(id,group_id,topic_id,recipient_id,kind,relay,
	 chat_source_id,chat_assessment_id,source_message_id,source_sequence,created_at)
	 SELECT ?,m.group_id,m.topic_id,m.sender_id,'clarification','origin_thread',s.id,?,m.id,?,?
	 FROM message_command_sources s JOIN messages m ON m.id=s.message_id
	 WHERE (s.id,s.message_id,s.kind)=(?,?,'local_user_chat') AND (m.group_id,m.topic_id,m.sender_id)=(?,?,'user')`,
		id, receipt.ID, c.Generation, stamp, c.SourceID, c.MessageID, c.GroupID, c.TopicID); err != nil {
		return err
	}
	if err := appendDecisionVersionInTx(ctx, tx, id, 1, capture.body, c.MemberID, c.TurnID, "", stamp, ""); err != nil {
		return err
	}
	return capture.loadInTx(ctx, tx, c, receipt.ID)
}

func (capture *decisionClarificationCapture) loadInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate, assessmentID string) error {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM decision_requests WHERE chat_source_id=? AND chat_assessment_id=?`, c.SourceID, assessmentID).Scan(&id)
	if err == nil {
		capture.request, err = pendingDecisionRequestInTx(ctx, tx, c.ThreadRef, id, 1, time.Now().UTC())
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	}
	return err
}
