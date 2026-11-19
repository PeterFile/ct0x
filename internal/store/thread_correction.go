package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"agentworkspace/internal/domain"
)

var threadCorrectionPrefix = regexp.MustCompile(`(?i)^(?:(?:这句话|这句|刚才那句话|刚才那句|上一句话|上一句)[ \t]*(?:指的是|是指|说的是)|this (?:sentence|message) (?:refers to|was about|is about)\b)`)

// Recognition is deliberately narrower than chat semantics. A recognized but
// incomplete or mixed instruction is inert, never sent to the chat engine.
func threadCorrectionSubject(body string) (subject string, recognized bool) {
	body = strings.TrimSpace(body)
	prefix := threadCorrectionPrefix.FindStringIndex(body)
	if prefix == nil {
		return "", false
	}
	subject = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(body[prefix[1]:]), ":："))
	subject = strings.TrimSpace(strings.TrimRight(subject, "。.!！"))
	if !validHistoryText(subject, 192) || strings.ContainsAny(subject, "\r\n。！？、，；,;!?\"`“”「」") || strings.Contains(subject, ". ") {
		return "", true
	}
	return subject, true
}

func threadCorrectionInstalledInTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	version := slices.Index(migrations, threadCorrectionSchema) + 1
	var installed, later bool
	var objects, intact int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.schema_migrations WHERE version=:version),
	 :version>0 AND EXISTS(SELECT 1 FROM main.schema_migrations WHERE version>:version),
	 count(*),count(CASE WHEN (type='table' AND sql=:corrections)
	 OR (type='index' AND sql IN (:messages,:attempts,:latest,:incoming))
	 OR (type='trigger' AND sql IN (:insert_guard,:update_guard,:delete_guard)) THEN 1 END)
	 FROM main.sqlite_schema WHERE name IN ('thread_corrections','thread_correction_message_scope','thread_correction_attempt_turn',
	 'thread_corrections_latest','thread_corrections_incoming','thread_corrections_insert','thread_corrections_update','thread_corrections_delete')`,
		sql.Named("version", version), sql.Named("corrections", threadCorrectionTable),
		sql.Named("messages", threadCorrectionMessageIndex), sql.Named("attempts", threadCorrectionAttemptIndex),
		sql.Named("latest", threadCorrectionLatestIndex), sql.Named("incoming", threadCorrectionIncomingIndex),
		sql.Named("insert_guard", threadCorrectionInsertGuard), sql.Named("update_guard", threadCorrectionUpdateGuard),
		sql.Named("delete_guard", threadCorrectionDeleteGuard)).Scan(&installed, &later, &objects, &intact)
	if err != nil {
		return false, err
	}
	if !installed && !later && objects == 0 {
		return false, nil // Legacy v30 and E1a-only installs have no correction footprint.
	}
	if !installed || intact != 8 {
		return false, errors.New("话题纠正迁移记录与数据库结构不一致")
	}
	return true, nil
}

func correctionDestinationInTx(ctx context.Context, tx *sql.Tx, group, subject string) (string, error) {
	if subject == "" {
		return "", nil
	}
	issues := issueTokens(subject)
	if len(issues) == 1 && strings.EqualFold(subject, issues[0]) {
		return threadTopicByKeyInTx(ctx, tx, group, issues[0], true)
	}
	key := threadTitleKey(subject)
	if topic, err := threadTopicByKeyInTx(ctx, tx, group, key, false); err != nil || topic != "" {
		return topic, err
	}
	if len(issues) != 0 {
		return "", nil // An issue embedded in extra prose is not an exact selector.
	}
	match, complete, err := threadTitleEvidenceInTx(ctx, tx, group, "", nil, key)
	if err != nil || !complete || match.conflict {
		return "", err
	}
	return match.topic, nil
}

func resolveThreadCorrectionInTx(ctx context.Context, tx *sql.Tx, r MessageRequest, subject string) (*domain.ThreadCorrection, error) {
	const sourceQuestion = "请引用本群要纠正的那条普通聊天原文，可以吗？"
	id := r.ReplyToMessageID
	if id == "" {
		// Do not skip a peer, a prior correction or a system notice. Only the
		// immediately preceding committed group message can be inferred.
		err := tx.QueryRowContext(ctx, `SELECT message_id FROM main.message_log WHERE group_id=? ORDER BY sequence DESC LIMIT 1`, r.GroupID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return &domain.ThreadCorrection{Question: sourceQuestion}, nil
		}
		if err != nil {
			return nil, err
		}
	}
	kind, _, err := messageSourceInTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if kind != "local_user_chat" && (r.ReplyToMessageID == "" || kind != "member") {
		return &domain.ThreadCorrection{Question: sourceQuestion}, nil
	}
	attr, err := messageThreadAttributionInTx(ctx, tx, r.GroupID, id)
	if err != nil {
		return nil, err
	}
	topic, err := correctionDestinationInTx(ctx, tx, r.GroupID, subject)
	if err != nil {
		return nil, err
	}
	if topic == "" {
		return &domain.ThreadCorrection{Question: "你指的是本群哪个已存在的话题？请用完整名称或故障编号。"}, nil
	}
	return &domain.ThreadCorrection{MessageID: id, OriginalTopicID: attr.Storage.TopicID,
		PreviousTopicID: attr.Effective.TopicID, EffectiveTopicID: topic}, nil
}

func insertThreadCorrectionInTx(ctx context.Context, tx *sql.Tx, m *domain.Message, correction *domain.ThreadCorrection) error {
	var original, previous, effective any
	if correction.MessageID != "" {
		original, previous, effective = correction.OriginalTopicID, correction.PreviousTopicID, correction.EffectiveTopicID
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO main.thread_corrections(group_id,command_message_id,target_message_id,original_topic_id,previous_topic_id,effective_topic_id,question)
	 VALUES(?,?,nullif(?,''),?,?,?,?)`, m.GroupID, m.ID, correction.MessageID, original, previous, effective, correction.Question)
	if err != nil {
		return err
	}
	m.ThreadCorrection = correction
	return nil
}

func threadCorrectionUnavailable() error {
	return fmt.Errorf("%w：当前实例未启用话题纠正，原话没有改动", ErrInvalid)
}
