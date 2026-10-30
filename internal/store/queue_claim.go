package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"agentworkspace/internal/domain"
)

func (s *Store) ClaimTurn(ctx context.Context, input ClaimTurnInput) (domain.Turn, error) {
	const action = "认领轮次失败"
	if strings.TrimSpace(input.GroupID) == "" || strings.TrimSpace(input.DeliveryID) == "" || input.MaxActive < 1 {
		return domain.Turn{}, queueError(action, ErrInvalid)
	}
	keys := slices.Clone(input.ResourceKeys)
	for _, key := range keys {
		if key == "" || strings.TrimSpace(key) != key || strings.ContainsAny(key, "\x00\r\n") {
			return domain.Turn{}, queueError(action, ErrInvalid)
		}
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Turn{}, queueError(action, err)
	}
	defer tx.Rollback()
	t := domain.Turn{ID: rand.Text(), GroupID: input.GroupID, DeliveryID: input.DeliveryID, ClaimToken: rand.Text(), State: domain.TurnStateStarting}
	// The first statement is a conditional write, even with a deferred transaction.
	// Reserving the binding before reads prevents competing read-to-write upgrades.
	err = tx.QueryRowContext(ctx, `UPDATE deliveries SET turn_id=?
	 WHERE id=? AND group_id=? AND state='queued' AND turn_id=''
	 AND EXISTS(SELECT 1 FROM members WHERE id=deliveries.recipient_id AND group_id=deliveries.group_id AND status='active')
	 RETURNING conversation_id,recipient_id`, t.ID, input.DeliveryID, input.GroupID).Scan(&t.ConversationID, &t.MemberID)
	if errors.Is(err, sql.ErrNoRows) {
		var found int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM deliveries d JOIN members m ON m.id=d.recipient_id AND m.group_id=d.group_id
		 WHERE d.id=? AND d.group_id=? AND m.status='active'`, input.DeliveryID, input.GroupID).Scan(&found)
		if err == nil {
			err = ErrTurnBusy
		}
	}
	if err != nil {
		return domain.Turn{}, queueError(action, err)
	}
	var active, occupied int
	err = tx.QueryRowContext(ctx, `SELECT count(*),coalesce(max(conversation_id=? OR member_id=?),0)
	 FROM turns WHERE `+activeTurnPredicate, t.ConversationID, t.MemberID).Scan(&active, &occupied)
	if err != nil {
		return domain.Turn{}, queueError(action, err)
	}
	if active >= input.MaxActive || occupied != 0 {
		return domain.Turn{}, queueError(action, ErrTurnBusy)
	}
	t.CreatedAt = time.Now().UTC()
	t.UpdatedAt = t.CreatedAt
	stamp := t.CreatedAt.Format(timestampLayout)
	if _, err := tx.ExecContext(ctx, `INSERT INTO turns(id,group_id,delivery_id,conversation_id,member_id,claim_token,state,created_at,updated_at)
	 VALUES(?,?,?,?,?,?,?,?,?)`, t.ID, t.GroupID, t.DeliveryID, t.ConversationID, t.MemberID, t.ClaimToken, t.State, stamp, stamp); err != nil {
		return domain.Turn{}, queueError(action, err)
	}
	for _, key := range keys {
		result, err := tx.ExecContext(ctx, `INSERT INTO resource_claims(resource_key,turn_id) VALUES(?,?) ON CONFLICT(resource_key) DO NOTHING`, key, t.ID)
		if err != nil {
			return domain.Turn{}, queueError(action, err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return domain.Turn{}, queueError(action, err)
		}
		if n != 1 {
			return domain.Turn{}, queueError(action, ErrTurnBusy)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Turn{}, queueError(action, err)
	}
	return t, nil
}

// RecoverInFlight is an explicit singleton-startup operation, never an Open hook.
// Unknown work keeps every resource and capacity claim until later reconciliation.
func (s *Store) RecoverInFlight(ctx context.Context) (int, error) {
	const action = "恢复轮次失败"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, queueError(action, err)
	}
	defer tx.Rollback()
	// Acquire write serialization before selecting the claims to fence.
	if _, err := tx.ExecContext(ctx, "UPDATE turns SET claim_token=claim_token WHERE "+activeTurnPredicate); err != nil {
		return 0, queueError(action, err)
	}
	rows, err := tx.QueryContext(ctx, "SELECT id FROM turns WHERE "+activeTurnPredicate)
	if err != nil {
		return 0, queueError(action, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, queueError(action, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, queueError(action, err)
	}
	if err := rows.Close(); err != nil {
		return 0, queueError(action, err)
	}
	stamp := time.Now().UTC().Format(timestampLayout)
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "UPDATE turns SET state='unknown',claim_token=?,updated_at=? WHERE id=?", rand.Text(), stamp, id); err != nil {
			return 0, queueError(action, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, queueError(action, err)
	}
	return len(ids), nil
}
