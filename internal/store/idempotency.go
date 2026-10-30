package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

type mutationScope struct{ operation, actorID, groupID, key string }

// Length-prefixed fields preserve exact bytes, including empty and invalid UTF-8 strings.
func mutationPayload(fields ...string) []byte {
	var payload []byte
	for _, field := range fields {
		payload = binary.BigEndian.AppendUint64(payload, uint64(len(field)))
		payload = append(payload, field...)
	}
	return payload
}

// The DSN's immediate transaction acquires the write lock before this lookup.
// Callers must create/read the result through tx and commit it with the reservation.
func (s *Store) beginMutation(ctx context.Context, scope mutationScope, payload []byte) (tx *sql.Tx, id string, replay bool, err error) {
	if scope.key != "" && strings.TrimSpace(scope.actorID) == "" {
		return nil, "", false, fmt.Errorf("%w：使用幂等键时调用者身份不能为空", ErrInvalid)
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			tx.Rollback()
			tx, id, replay = nil, "", false
		}
	}()
	id = rand.Text()
	if scope.key == "" {
		return
	}
	var saved []byte
	err = tx.QueryRowContext(ctx,
		`SELECT payload, record_id FROM mutation_idempotency
		 WHERE operation = ? AND actor_id = ? AND group_id = ? AND idempotency_key = ?`,
		scope.operation, scope.actorID, scope.groupID, scope.key).Scan(&saved, &id)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO mutation_idempotency(operation, actor_id, group_id, idempotency_key, payload, record_id)
			 VALUES(?, ?, ?, ?, ?, ?)`, scope.operation, scope.actorID, scope.groupID, scope.key, payload, id)
	} else if err == nil {
		if !bytes.Equal(saved, payload) {
			err = ErrConflict
		} else {
			replay = true
		}
	}
	return
}
