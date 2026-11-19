package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

const threadStateTaskRelation = ` FROM tasks t JOIN task_coordination link ON (link.task_id,link.group_id)=(t.id,t.group_id)
 JOIN mandates m ON (m.id,m.task_id,m.group_id,m.topic_id)=(link.current_mandate_id,t.id,t.group_id,link.topic_id)`

const threadStateSlotCount, threadStateContentBytes = 8, 1024

var ErrThreadStateConflict = errors.New("话题状态、任务版本或来源已变更")

// ThreadStateUpdate is an explicit local-user command, not a model operation.
// Content is an exact excerpt of the confirmed source (the reason for invalidate).
type ThreadStateUpdate struct {
	ExpectedRevision               int64
	Slot                           int
	Kind                           domain.ThreadStateKind
	Action, Content, SourceMessage string
}

func validThreadStateScope(scope domain.ThreadStateScope) bool {
	// TopicID is opaque, including an empty main topic. Do not trim it.
	return validTriggerID(scope.GroupID) && utf8.ValidString(scope.TopicID) &&
		((scope.TaskID == "" && scope.TaskRevision == 0) || (validTriggerID(scope.TaskID) && scope.TaskRevision > 0))
}

func validThreadStateContent(content string) bool {
	return len(content) <= threadStateContentBytes && strings.TrimSpace(content) != "" && utf8.ValidString(content) && !strings.ContainsRune(content, 0)
}

func validThreadStateKind(kind domain.ThreadStateKind) bool {
	switch kind {
	case domain.ThreadStateGoal, domain.ThreadStateConstraint, domain.ThreadStateDecision, domain.ThreadStatePlan:
		return true
	default:
		return false
	}
}

// This is E2's compatibility Task↔Thread relation, not a disclosure grant.
// When E1 supplies authoritative links, replace this relation (and its insert
// guard via a forward migration) rather than creating a second link table.
func verifyThreadStateTaskInTx(ctx context.Context, tx *sql.Tx, scope domain.ThreadStateScope, historical bool) error {
	if scope.TaskID == "" {
		return nil
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT t.revision`+threadStateTaskRelation+`
	 WHERE t.id=? AND t.group_id=? AND link.topic_id=?`, scope.TaskID, scope.GroupID, scope.TopicID).Scan(&revision); err != nil {
		return err
	}
	if scope.TaskRevision != revision && (!historical || scope.TaskRevision > revision) {
		return ErrThreadStateConflict
	}
	return nil
}

// SetLocalThreadState verifies authority before replay and commits one immutable
// revision and the idempotency receipt in the same immediate transaction.
func (s *Store) SetLocalThreadState(ctx context.Context, scope domain.ThreadStateScope, update ThreadStateUpdate, key string) (domain.ThreadStateSnapshot, error) {
	const action = "保存话题状态失败"
	if !validThreadStateScope(scope) || !memberToolKeyValid(key) || !validTriggerID(update.SourceMessage) ||
		update.ExpectedRevision < 0 || update.ExpectedRevision == math.MaxInt64 || update.Slot < 0 || update.Slot >= threadStateSlotCount ||
		!validThreadStateKind(update.Kind) || !validThreadStateContent(update.Content) || (update.Action != "set" && update.Action != "invalidate") {
		return domain.ThreadStateSnapshot{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ThreadStateSnapshot{}, storeError(action, err)
	}
	defer tx.Rollback()
	snapshot, err := setLocalThreadStateInTx(ctx, tx, scope, update, key)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return domain.ThreadStateSnapshot{}, storeError(action, err)
	}
	return snapshot, nil
}

func setLocalThreadStateInTx(ctx context.Context, tx *sql.Tx, scope domain.ThreadStateScope, update ThreadStateUpdate, key string) (domain.ThreadStateSnapshot, error) {
	var empty domain.ThreadStateSnapshot
	if err := requireLocalThreadStateInTx(ctx, tx, scope, false); err != nil {
		return empty, err
	}
	var sourceID string
	var position int64
	var excerpt bool
	if err := tx.QueryRowContext(ctx, `SELECT s.id,l.sequence,instr(m.content,?)>0
	 FROM message_log l JOIN messages m ON (m.id,m.group_id,m.topic_id)=(l.message_id,l.group_id,l.topic_id)
	 JOIN message_command_sources s ON s.message_id=m.id AND s.kind='local_user_chat'
	 WHERE l.message_id=? AND l.group_id=? AND l.topic_id=? AND m.sender_id='user'`,
		update.Content, update.SourceMessage, scope.GroupID, scope.TopicID).Scan(&sourceID, &position, &excerpt); err != nil {
		return empty, err
	}
	mutation := mutationScope{operation: "set_thread_state", actorID: "user", groupID: scope.GroupID, key: key}
	payload := mutationPayload(scope.TopicID, scope.TaskID, strconv.FormatInt(scope.TaskRevision, 10), strconv.FormatInt(update.ExpectedRevision, 10),
		strconv.Itoa(update.Slot), string(update.Kind), update.Action, update.Content, update.SourceMessage)
	revision := update.ExpectedRevision + 1
	recordID := strconv.FormatInt(revision, 10)
	id, replay, err := lookupMutation(ctx, tx, mutation, payload)
	if err != nil {
		return empty, err
	}
	if replay {
		if id != recordID || !excerpt {
			return empty, errThreadStateIntegrity
		}
		snapshot, err := readThreadStateInTx(ctx, tx, scope, revision)
		if errors.Is(err, sql.ErrNoRows) {
			return empty, errThreadStateIntegrity
		}
		return snapshot, err
	}
	if !excerpt {
		return empty, ErrInvalid
	}
	current, err := readThreadStateInTx(ctx, tx, scope, 0)
	if err != nil {
		return empty, err
	}
	if current.Revision != update.ExpectedRevision {
		return empty, ErrThreadStateConflict
	}
	var invalidates any
	for _, previous := range current.Records {
		if previous.Slot != update.Slot {
			continue
		}
		last := previous.Source.MessageLogPosition
		if !previous.Active {
			last = previous.InvalidatedBy.Source.MessageLogPosition
		}
		if previous.Kind != update.Kind || position <= last || (previous.Active && previous.Kind == domain.ThreadStatePlan && update.Action == "set") {
			return empty, ErrThreadStateConflict
		}
		if previous.Active && update.Action == "invalidate" {
			invalidates = previous.Revision
		}
	}
	if update.Action == "invalidate" && invalidates == nil {
		return empty, ErrThreadStateConflict
	}
	if err := coordinatorWrite(ctx, tx, `INSERT INTO thread_state_revisions
	 (group_id,topic_id,task_id,task_revision,revision,slot,kind,content,source_id,source_position,confirmed_at,invalidates_revision)
	 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, scope.GroupID, scope.TopicID, scope.TaskID, scope.TaskRevision, revision, update.Slot, update.Kind,
		update.Content, sourceID, position, time.Now().UTC().Format(timestampLayout), invalidates); err != nil {
		return empty, err
	}
	if err := coordinatorWrite(ctx, tx, `INSERT INTO mutation_idempotency(operation,actor_id,group_id,idempotency_key,payload,record_id)
	 VALUES(?,?,?,?,?,?)`, mutation.operation, mutation.actorID, mutation.groupID, mutation.key, payload, recordID); err != nil {
		return empty, err
	}
	return readThreadStateInTx(ctx, tx, scope, revision)
}

// GetLocalThreadState names one exact Thread (and optional Task version). A
// positive state revision explicitly requests history, never a broader search.
func (s *Store) GetLocalThreadState(ctx context.Context, scope domain.ThreadStateScope, revision int64) (domain.ThreadStateSnapshot, error) {
	const action = "读取话题状态失败"
	if !validThreadStateScope(scope) || revision < 0 {
		return domain.ThreadStateSnapshot{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.ThreadStateSnapshot{}, storeError(action, err)
	}
	defer tx.Rollback()
	if err := requireLocalThreadStateInTx(ctx, tx, scope, revision > 0); err != nil {
		return domain.ThreadStateSnapshot{}, storeError(action, err)
	}
	var found bool
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM message_log INDEXED BY message_log_topic_sequence
	 WHERE group_id=? AND topic_id=? LIMIT 1`, scope.GroupID, scope.TopicID).Scan(&found); err != nil {
		return domain.ThreadStateSnapshot{}, storeError(action, err)
	}
	snapshot, err := readThreadStateInTx(ctx, tx, scope, revision)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return domain.ThreadStateSnapshot{}, storeError(action, err)
	}
	return snapshot, nil
}

func requireLocalThreadStateInTx(ctx context.Context, tx *sql.Tx, scope domain.ThreadStateScope, historical bool) error {
	if err := requireMessageActor(ctx, tx, scope.GroupID, "user"); err != nil {
		return err
	}
	installed, err := threadStateInstalledInTx(ctx, tx)
	if err != nil {
		return err
	}
	if !installed {
		return ErrThreadStateUnavailable
	}
	return verifyThreadStateTaskInTx(ctx, tx, scope, historical)
}
