package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"slices"
	"strconv"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

// Keep the default and ceiling at the v24 context reservation bound.
const maxChatDiscussionInviteHops = 2

// The registry assigns this migration's version. A missing marker is legacy
// only if neither the policy table nor a later migration is present.
var chatDiscussionPolicyVersion = slices.Index(migrations, chatDiscussionPolicySchema) + 1

// ChatDiscussionPolicyAvailable reports whether the settings migration has been applied.
func (s *Store) ChatDiscussionPolicyAvailable(ctx context.Context) (bool, error) {
	return chatDiscussionPolicyAvailable(ctx, s.db)
}

func chatDiscussionPolicyAvailable(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (bool, error) {
	if chatDiscussionPolicyVersion == 0 {
		return false, nil
	}
	var state int
	err := q.QueryRowContext(ctx, `SELECT CASE
	 WHEN EXISTS(SELECT 1 FROM schema_migrations WHERE version=?1) THEN 1
	 WHEN EXISTS(SELECT 1 FROM schema_migrations WHERE version>?1)
	   OR EXISTS(SELECT 1 FROM main.sqlite_schema WHERE type='table' AND name='chat_discussion_policy_revisions') THEN -1
	 ELSE 0 END`, chatDiscussionPolicyVersion).Scan(&state)
	if err != nil {
		return false, err
	}
	if state == -1 {
		return false, errors.New("讨论策略迁移记录缺失")
	}
	return state == 1, nil
}

func (s *Store) GetChatDiscussionPolicy(ctx context.Context, thread domain.ThreadRef) (domain.ChatDiscussionPolicy, error) {
	const action = "读取讨论策略失败"
	if !validTriggerID(thread.GroupID) || !utf8.ValidString(thread.TopicID) {
		return domain.ChatDiscussionPolicy{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.ChatDiscussionPolicy{}, storeError(action, err)
	}
	defer tx.Rollback()
	if err := requireMessageActor(ctx, tx, thread.GroupID, "user"); err != nil {
		return domain.ChatDiscussionPolicy{}, storeError(action, err)
	}
	policy, err := currentChatDiscussionPolicyInTx(ctx, tx, thread)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return domain.ChatDiscussionPolicy{}, storeError(action, err)
	}
	return policy, nil
}

// SetChatDiscussionPolicy is a local-user configuration command. No caller
// identity, recipients or authorities are accepted as input.
func (s *Store) SetChatDiscussionPolicy(ctx context.Context, thread domain.ThreadRef, maxInviteHops int, expected int64, key string) (domain.ChatDiscussionPolicy, error) {
	const action = "保存讨论策略失败"
	if !validTriggerID(thread.GroupID) || !utf8.ValidString(thread.TopicID) || !memberToolKeyValid(key) ||
		maxInviteHops < 0 || maxInviteHops > maxChatDiscussionInviteHops || expected < 0 || expected == math.MaxInt64 {
		return domain.ChatDiscussionPolicy{}, ErrInvalid
	}
	// Store's immediate transaction serializes policy writes and new chain publication.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ChatDiscussionPolicy{}, storeError(action, err)
	}
	defer tx.Rollback()
	policy, err := setChatDiscussionPolicyInTx(ctx, tx, thread, maxInviteHops, expected, key)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return domain.ChatDiscussionPolicy{}, storeError(action, err)
	}
	return policy, nil
}

func setChatDiscussionPolicyInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, hops int, expected int64, key string) (domain.ChatDiscussionPolicy, error) {
	if err := requireMessageActor(ctx, tx, thread.GroupID, "user"); err != nil {
		return domain.ChatDiscussionPolicy{}, err
	}
	available, err := chatDiscussionPolicyAvailable(ctx, tx)
	if err != nil {
		return domain.ChatDiscussionPolicy{}, err
	}
	if !available {
		return domain.ChatDiscussionPolicy{}, ErrNotFound
	}
	scope := mutationScope{operation: "set_chat_discussion_policy", actorID: "user", groupID: thread.GroupID, key: key}
	payload := mutationPayload(thread.TopicID, strconv.FormatInt(expected, 10), strconv.Itoa(hops))
	id, replay, err := lookupMutation(ctx, tx, scope, payload)
	if err != nil {
		return domain.ChatDiscussionPolicy{}, err
	}
	revision := expected + 1
	recordID := strconv.FormatInt(revision, 10)
	if replay {
		if id != recordID {
			return domain.ChatDiscussionPolicy{}, errors.New("讨论策略回执版本不符")
		}
	} else {
		current, err := readChatDiscussionPolicyInTx(ctx, tx, thread, 0)
		if err != nil {
			return domain.ChatDiscussionPolicy{}, err
		}
		if current.Revision != expected {
			return domain.ChatDiscussionPolicy{}, ErrConflict
		}
		if err := coordinatorWrite(ctx, tx, `INSERT INTO chat_discussion_policy_revisions(group_id,topic_id,revision,max_invite_hops)
		 VALUES(?,?,?,?)`, thread.GroupID, thread.TopicID, revision, hops); err != nil {
			return domain.ChatDiscussionPolicy{}, err
		}
		if err := coordinatorWrite(ctx, tx, `INSERT INTO mutation_idempotency(operation,actor_id,group_id,idempotency_key,payload,record_id)
		 VALUES(?,?,?,?,?,?)`, scope.operation, scope.actorID, scope.groupID, scope.key, payload, recordID); err != nil {
			return domain.ChatDiscussionPolicy{}, err
		}
	}
	policy, err := readChatDiscussionPolicyInTx(ctx, tx, thread, revision)
	if err == nil && policy.MaxInviteHops != hops {
		return domain.ChatDiscussionPolicy{}, errors.New("讨论策略回执额度不符")
	}
	return policy, err
}

func defaultChatDiscussionPolicy(thread domain.ThreadRef) domain.ChatDiscussionPolicy {
	return domain.ChatDiscussionPolicy{ThreadRef: thread, MaxInviteHops: maxChatDiscussionInviteHops, MaxTargetedWakes: 2 * maxChatDiscussionInviteHops}
}

func currentChatDiscussionPolicyInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef) (domain.ChatDiscussionPolicy, error) {
	available, err := chatDiscussionPolicyAvailable(ctx, tx)
	if err != nil {
		return domain.ChatDiscussionPolicy{}, err
	}
	if !available {
		return defaultChatDiscussionPolicy(thread), nil
	}
	return readChatDiscussionPolicyInTx(ctx, tx, thread, 0)
}

// Revision zero selects the latest revision of this exact topic using the PK.
func readChatDiscussionPolicyInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, revision int64) (domain.ChatDiscussionPolicy, error) {
	const query = `SELECT revision,max_invite_hops FROM chat_discussion_policy_revisions WHERE group_id=? AND topic_id=?`
	policy := domain.ChatDiscussionPolicy{ThreadRef: thread}
	var row *sql.Row
	if revision == 0 {
		row = tx.QueryRowContext(ctx, query+` ORDER BY revision DESC LIMIT 1`, thread.GroupID, thread.TopicID)
	} else {
		row = tx.QueryRowContext(ctx, query+` AND revision=?`, thread.GroupID, thread.TopicID, revision)
	}
	if err := row.Scan(&policy.Revision, &policy.MaxInviteHops); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if revision == 0 {
				return defaultChatDiscussionPolicy(thread), nil
			}
			return domain.ChatDiscussionPolicy{}, errors.New("讨论策略回执的历史版本缺失")
		}
		return domain.ChatDiscussionPolicy{}, err
	}
	if policy.Revision <= 0 || policy.MaxInviteHops < 0 || policy.MaxInviteHops > maxChatDiscussionInviteHops {
		return domain.ChatDiscussionPolicy{}, errors.New("讨论策略版本或额度无效")
	}
	policy.MaxTargetedWakes = 2 * policy.MaxInviteHops
	return policy, nil
}
