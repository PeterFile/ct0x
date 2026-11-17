package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"agentworkspace/internal/domain"
)

func chatRepairSupportedInTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var supported bool
	err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=22)").Scan(&supported)
	return supported, err
}

// Only a NEW actual Review command reaches this hook. Missing routing leaves a
// valid Review untouched; it is not permission to reinterpret notice text.
func chatRepairReviewIntakeInTx(ctx context.Context, tx *sql.Tx, decision domain.ReviewDecision) error {
	if decision.Outcome != domain.ReviewOutcomeRejected || decision.ReviewedBy != "user" {
		return nil
	}
	supported, err := chatRepairSupportedInTx(ctx, tx)
	if err != nil || !supported {
		return err
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM chat_repair_review_intakes WHERE review_id=?)", decision.ID).Scan(&exists); err != nil || exists {
		return err
	}
	var messageID, topicID, memberID string
	var binding, policy int64
	err = tx.QueryRowContext(ctx, `SELECT rn.message_id,c.topic_id,b.member_id,b.revision,cp.revision
	 FROM chat_review_notices rn JOIN mandates m INDEXED BY mandates_group_task ON m.group_id=? AND m.task_id=?
	 JOIN chat_assessments a ON a.decision_id=m.decision_id
	 JOIN chat_candidates c ON c.id=a.candidate_id AND c.group_id=m.group_id AND c.topic_id=m.topic_id
	 JOIN group_coordinator_bindings cb ON cb.group_id=m.group_id
	 JOIN group_coordinator_binding_revisions b ON b.group_id=cb.group_id AND b.revision=cb.revision
	 JOIN members member ON member.id=b.member_id AND member.group_id=b.group_id AND member.status='active'
	 JOIN chat_policies cp ON cp.group_id=c.group_id AND cp.topic_id=c.topic_id
	 JOIN chat_policy_revisions p ON p.group_id=cp.group_id AND p.topic_id=cp.topic_id AND p.revision=cp.revision AND p.enabled=1
	 WHERE rn.review_id=?`, decision.GroupID, decision.TaskID, decision.ID).Scan(&messageID, &topicID, &memberID, &binding, &policy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	conversation, err := conversationInTx(ctx, tx, decision.GroupID, memberID, topicID)
	if err != nil {
		return err
	}
	deliveryID := rand.Text()
	if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO deliveries(id,message_id,conversation_id,recipient_id,group_id,topic_id,created_at)
	 VALUES(?,?,?,?,?,?,?)`, deliveryID, messageID, conversation.ID, memberID, decision.GroupID, topicID, time.Now().UTC().Format(timestampLayout)); err != nil {
		return err
	}
	return coordinationDispatchWrite(ctx, tx, `INSERT INTO chat_repair_review_intakes(review_id,message_id,delivery_id,group_id,topic_id,member_id,binding_revision,policy_revision)
	 VALUES(?,?,?,?,?,?,?,?)`, decision.ID, messageID, deliveryID, decision.GroupID, topicID, memberID, binding, policy)
}

func captureChatRepairCandidateInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, candidateID string) error {
	supported, err := chatRepairSupportedInTx(ctx, tx)
	if err != nil || !supported {
		return err
	}
	var reviewID string
	err = tx.QueryRowContext(ctx, `SELECT i.review_id FROM turns t JOIN chat_repair_review_intakes i ON i.delivery_id=t.delivery_id
	 WHERE t.id=? AND i.group_id=? AND i.topic_id=? AND i.member_id=?`, scope.Claim.TurnID, scope.Claim.GroupID, scope.TopicID, scope.MemberID).Scan(&reviewID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	source, err := chatRepairSourceEvidenceInTx(ctx, tx, domain.ThreadRef{GroupID: scope.Claim.GroupID, TopicID: scope.TopicID}, reviewID, false)
	if err != nil {
		return err
	}
	data, err := json.Marshal(source)
	if err != nil {
		return err
	}
	return coordinationDispatchWrite(ctx, tx, "INSERT INTO chat_repair_candidates(candidate_id,review_id,source_json) VALUES(?,?,?)", candidateID, reviewID, string(data))
}

func chatRepairCandidateInTx(ctx context.Context, tx *sql.Tx, candidateID string) (*domain.ChatRepairSource, error) {
	supported, err := chatRepairSupportedInTx(ctx, tx)
	if err != nil || !supported {
		return nil, err
	}
	var data string
	err = tx.QueryRowContext(ctx, "SELECT source_json FROM chat_repair_candidates WHERE candidate_id=?", candidateID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var source domain.ChatRepairSource
	if err := json.Unmarshal([]byte(data), &source); err != nil {
		return nil, err
	}
	return &source, nil
}
