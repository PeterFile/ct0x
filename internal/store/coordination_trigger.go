package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

const triggerColumns = "id, source_kind, group_id, topic_id, message_id, message_sequence, created_at"

// RecordLocalMessageTrigger is the trusted local Source's persistence seam, not a member capability.
func (s *Store) RecordLocalMessageTrigger(ctx context.Context, thread domain.ThreadRef, messageID string) (domain.Trigger, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Trigger{}, storeError("捕获消息来源失败", err)
	}
	defer tx.Rollback()
	trigger, err := recordLocalMessageTriggerInTx(ctx, tx, thread, messageID)
	if err != nil {
		return domain.Trigger{}, storeError("捕获消息来源失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Trigger{}, storeError("保存消息来源失败", err)
	}
	return trigger, nil
}

// The caller owns the transaction; future member operations must add their own same-tx scope fence.
func recordLocalMessageTriggerInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, messageID string) (domain.Trigger, error) {
	if err := validateTriggerThread(thread); err != nil {
		return domain.Trigger{}, err
	}
	if !validTriggerID(messageID) {
		return domain.Trigger{}, fmt.Errorf("%w：消息标识无效", ErrInvalid)
	}
	var source domain.Trigger
	// Validate the exact immutable source before deduplication; never read mutable source time/content.
	if err := tx.QueryRowContext(ctx, `SELECT message_id,group_id,topic_id,sequence FROM message_log
	 WHERE message_id=? AND group_id=? AND topic_id=?`, messageID, thread.GroupID, thread.TopicID).
		Scan(&source.MessageID, &source.GroupID, &source.TopicID, &source.MessageSequence); err != nil {
		return domain.Trigger{}, err
	}
	trigger, err := scanTrigger(tx.QueryRowContext(ctx,
		"SELECT "+triggerColumns+" FROM coordination_triggers WHERE source_kind=? AND message_id=?", domain.TriggerSourceLocalIM, source.MessageID))
	if !errors.Is(err, sql.ErrNoRows) {
		return trigger, err
	}
	return scanTrigger(tx.QueryRowContext(ctx,
		"INSERT INTO coordination_triggers("+triggerColumns+") VALUES(?,?,?,?,?,?,?) RETURNING "+triggerColumns,
		rand.Text(), domain.TriggerSourceLocalIM, source.GroupID, source.TopicID, source.MessageID, source.MessageSequence, time.Now().UTC().Format(timestampLayout)))
}

func validateTriggerThread(thread domain.ThreadRef) error {
	// TopicID is opaque; SQL matches its exact bytes, including the empty main topic.
	if !validTriggerID(thread.GroupID) {
		return fmt.Errorf("%w：群组标识无效", ErrInvalid)
	}
	return nil
}

func validTriggerID(id string) bool {
	// Bound generated reference IDs; TopicID deliberately does not use this validation.
	return id != "" && len(id) <= 4096 && utf8.ValidString(id) && strings.TrimSpace(id) == id && !strings.ContainsAny(id, "\x00\r\n")
}

func scanTrigger(row scanner) (trigger domain.Trigger, err error) {
	var stamp string
	if err = row.Scan(&trigger.ID, &trigger.SourceKind, &trigger.GroupID, &trigger.TopicID, &trigger.MessageID, &trigger.MessageSequence, &stamp); err == nil {
		trigger.CreatedAt, err = time.Parse(timestampLayout, stamp)
	}
	return
}
