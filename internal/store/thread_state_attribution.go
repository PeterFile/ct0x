package store

import (
	"context"
	"database/sql"

	"agentworkspace/internal/domain"
)

const (
	noCorrectionLedger = -1
	// Bound both the coverage verification and the concurrent-claim check.
	// An incomplete proof may never narrow history or admit a stale prompt.
	correctionProofLimit = 128
)

// The committed correction ledger has an immutable integer primary key. Its
// tip is a cheap cursor, not a grant or a message-log coverage position.
func correctionTipInTx(ctx context.Context, tx *sql.Tx) (int64, error) {
	installed, err := threadCorrectionInstalledInTx(ctx, tx)
	if err != nil || !installed {
		return noCorrectionLedger, err
	}
	var tip int64
	err = tx.QueryRowContext(ctx, `SELECT coalesce(max(sequence),0) FROM main.thread_corrections`).Scan(&tip)
	return tip, err
}

// The caller already verified the storage evidence (and E1b installation).
// A true move after confirmation invalidates that proof even if the final
// effective topic is the original one. No-op/clarification receipts do not.
func ordinaryEvidenceCurrentInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, messageID string, confirmedAtTip int64) (bool, error) {
	var current bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.messages m
	 WHERE m.group_id=?1 AND m.id=?2 AND `+notCorrectionCommandSQL+` AND `+effectiveHistoryTopicSQL+`=?3
	 AND NOT EXISTS(SELECT 1 FROM main.thread_corrections c WHERE c.group_id=m.group_id AND c.target_message_id=m.id
	   AND c.previous_topic_id<>c.effective_topic_id AND c.sequence>?4))`, thread.GroupID, messageID, thread.TopicID, confirmedAtTip).Scan(&current)
	return current, err
}

// Covering through a message-log position cannot cover a later move into the
// topic, even when that move's target is an older message. Inspect only a
// bounded page from E1b's exact destination index (including returns). When
// every incoming command precedes or meets the mark, its target also precedes
// the mark, by E1b's append guard. Thus an indexed storage-log seek suffices.
func ordinaryCoverageCurrentInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, coverage *domain.ThreadStateCoverage, tip int64) (bool, error) {
	current, err := ordinaryEvidenceCurrentInTx(ctx, tx, thread, coverage.MessageID, tip)
	if err != nil || !current {
		return current, err
	}
	var moves, logged int
	var lastPosition int64
	err = tx.QueryRowContext(ctx, `SELECT count(*),count(l.sequence),coalesce(max(l.sequence),0) FROM (
	 SELECT command_message_id FROM main.thread_corrections INDEXED BY thread_corrections_incoming
	 WHERE group_id=?1 AND effective_topic_id=?2 AND previous_topic_id<>effective_topic_id LIMIT ?3
	 ) c LEFT JOIN main.message_log l ON l.message_id=c.command_message_id AND l.group_id=?1 AND l.topic_id=''`,
		thread.GroupID, thread.TopicID, correctionProofLimit+1).Scan(&moves, &logged, &lastPosition)
	return moves <= correctionProofLimit && moves == logged && lastPosition <= coverage.MessageLogPosition, err
}

// Only the commit tail since Prepare matters at Claim. Match actual moves in
// the bound group touching this topic; comparing just the latest topic would
// miss A->B->A. Too long a tail is conservatively retried rather than scanned.
func correctionTailTouchesThreadInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, since int64) (bool, error) {
	var stale bool
	err := tx.QueryRowContext(ctx, `SELECT count(*)>?4 OR coalesce(max(group_id=?2 AND previous_topic_id<>effective_topic_id
	 AND (original_topic_id=?3 OR previous_topic_id=?3 OR effective_topic_id=?3)),0) FROM (
	 SELECT group_id,original_topic_id,previous_topic_id,effective_topic_id FROM main.thread_corrections
	 WHERE sequence>?1 ORDER BY sequence LIMIT ?5
	 )`, since, thread.GroupID, thread.TopicID, correctionProofLimit, correctionProofLimit+1).Scan(&stale)
	return stale, err
}
