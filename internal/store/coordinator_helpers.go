package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// This helper returns an open transaction; callers read saved evidence before committing.
func (s *Store) beginCoordinatorConfig(ctx context.Context, groupID string, ids []string, expected int64, key string, policy bool) (tx *sql.Tx, revision int64, err error) {
	if !validTriggerID(groupID) || strings.TrimSpace(key) == "" || expected < 0 || expected == math.MaxInt64 {
		return nil, 0, fmt.Errorf("%w：群组、幂等键或预期版本无效", ErrInvalid)
	}
	for _, id := range ids {
		if !validTriggerID(id) {
			return nil, 0, fmt.Errorf("%w：成员标识无效", ErrInvalid)
		}
	}
	current, versions, column, operation := "group_coordinator_bindings", "group_coordinator_binding_revisions", "member_id", "set_coordinator"
	overlapQuery := `SELECT EXISTS(SELECT 1 FROM group_dispatch_policies p
	 JOIN group_dispatch_policy_revisions r USING(group_id,revision), json_each(r.allowed_worker_ids) w
	 WHERE p.group_id=? AND w.value=?)`
	if policy {
		current, versions, column, operation = "group_dispatch_policies", "group_dispatch_policy_revisions", "allowed_worker_ids", "set_dispatch_policy"
		overlapQuery = `SELECT EXISTS(SELECT 1 FROM group_coordinator_bindings b
		 JOIN group_coordinator_binding_revisions r USING(group_id,revision) WHERE b.group_id=? AND r.member_id=?)`
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
			tx, revision = nil, 0
		}
	}()
	// This must be the first SQL, including when replaying an old revision.
	if err = requireMessageActor(ctx, tx, groupID, "user"); err != nil {
		return
	}
	payload := mutationPayload(append([]string{groupID, strconv.FormatInt(expected, 10)}, ids...)...)
	scope := mutationScope{operation: operation, actorID: "user", groupID: groupID, key: key}
	var recordID string
	var replay bool
	recordID, replay, err = lookupMutation(ctx, tx, scope, payload)
	if err != nil {
		return
	}
	if replay {
		revision, err = strconv.ParseInt(recordID, 10, 64)
		if err == nil && revision < 1 {
			err = fmt.Errorf("%w：协调配置回执版本无效", ErrConflict)
		}
		return
	}
	var actual int64
	err = tx.QueryRowContext(ctx, "SELECT revision FROM "+current+" WHERE group_id=?", groupID).Scan(&actual)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return
	}
	if actual != expected {
		err = fmt.Errorf("%w：协调配置版本已变化", ErrConflict)
		return
	}
	for _, id := range ids {
		var active int
		if err = tx.QueryRowContext(ctx, "SELECT 1 FROM members WHERE group_id=? AND id=? AND status='active'", groupID, id).Scan(&active); err != nil {
			return
		}
		var overlap bool
		if err = tx.QueryRowContext(ctx, overlapQuery, groupID, id).Scan(&overlap); err != nil {
			return
		}
		if overlap {
			err = fmt.Errorf("%w：协调员不能同时是允许执行成员", ErrConflict)
			return
		}
	}
	var value string
	if policy {
		var encoded []byte
		encoded, err = json.Marshal(ids)
		if err != nil {
			return
		}
		value = string(encoded)
	} else {
		value = ids[0]
	}
	revision = expected + 1
	if err = coordinatorWrite(ctx, tx, "INSERT INTO "+versions+"(group_id,revision,"+column+") VALUES(?,?,?)", groupID, revision, value); err != nil {
		return
	}
	if expected == 0 {
		err = coordinatorWrite(ctx, tx, "INSERT INTO "+current+"(group_id,revision) VALUES(?,?)", groupID, revision)
	} else {
		err = coordinatorWrite(ctx, tx, "UPDATE "+current+" SET revision=? WHERE group_id=? AND revision=?", revision, groupID, expected)
	}
	if err != nil {
		return
	}
	// RETURNING detects RAISE(IGNORE), unlike the shared generic reservation helper.
	err = tx.QueryRowContext(ctx, `INSERT INTO mutation_idempotency(operation,actor_id,group_id,idempotency_key,payload,record_id)
	 VALUES(?,?,?,?,?,?) RETURNING record_id`, operation, "user", groupID, key, payload, strconv.FormatInt(revision, 10)).Scan(&recordID)
	if errors.Is(err, sql.ErrNoRows) {
		err = fmt.Errorf("%w：协调配置回执未写入", ErrConflict)
	}
	return
}

func coordinatorWrite(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w：协调配置未写入", ErrConflict)
	}
	return nil
}
