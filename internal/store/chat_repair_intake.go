package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

// ErrChatRepairSourceEncoding blocks assessment without rewriting source bytes.
var ErrChatRepairSourceEncoding = errors.New("完整修复来源含非法 UTF-8，无法无损评估")

func chatRepairSupportedInTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var supported bool
	err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=22)").Scan(&supported)
	return supported, err
}

// Only a NEW actual Review command reaches this hook. Missing routing leaves a
// valid Review untouched; it is not permission to reinterpret notice text.
// Investigation Policy authorizes preparation only, not repair intake.
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
	 WHERE rn.review_id=? AND json_extract(a.receipt_json,'$.action') IS NOT ?`,
		decision.GroupID, decision.TaskID, decision.ID, domain.ChatActionReadOnlyInvestigation).Scan(&messageID, &topicID, &memberID, &binding, &policy)
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
	if err := validateChatRepairSourceUTF8(reflect.ValueOf(source), "repair_source"); err != nil {
		return err
	}
	data, err := json.Marshal(source)
	if err != nil {
		return err
	}
	return coordinationDispatchWrite(ctx, tx, "INSERT INTO chat_repair_candidates(candidate_id,review_id,source_json) VALUES(?,?,?)", candidateID, reviewID, string(data))
}

// Check every exported string in the acyclic source DTO before encoding/json
// can silently replace invalid bytes, including nested historical evidence.
func validateChatRepairSourceUTF8(value reflect.Value, path string) error {
	switch value.Kind() {
	case reflect.String:
		if !utf8.ValidString(value.String()) {
			return fmt.Errorf("%w：%w：%s", ErrInvalid, ErrChatRepairSourceEncoding, path)
		}
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			field := value.Type().Field(i)
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if field.PkgPath != "" || name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			if err := validateChatRepairSourceUTF8(value.Field(i), path+"."+name); err != nil {
				return err
			}
		}
	case reflect.Pointer, reflect.Interface:
		if !value.IsNil() {
			return validateChatRepairSourceUTF8(value.Elem(), path)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			if err := validateChatRepairSourceUTF8(value.Index(i), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
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
