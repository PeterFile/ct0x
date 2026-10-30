package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"agentworkspace/internal/domain"
)

var ErrTurnBusy = errors.New("轮次、成员、容量或资源正被占用")

const activeTurnPredicate = "state IN ('starting','running','cancel_requested','unknown')"
const turnColumns = `id,group_id,delivery_id,conversation_id,member_id,claim_token,state,native_session_id,
 accepted_at,cancel_requested_at,candidate_text,candidate_terminal,reply_message_id,reason,created_at,updated_at`

type PendingDelivery struct {
	GroupID  string          `json:"group_id"`
	Delivery domain.Delivery `json:"delivery"`
}

type PendingOptions struct {
	Limit int // Zero selects 50; maximum 200.
	After *Cursor
}

type ClaimTurnInput struct {
	GroupID, DeliveryID string
	ResourceKeys        []string // Complete canonical keys from trusted configuration, never model arguments.
	MaxActive           int
}

// ListPendingDeliveries scans FIFO; callers must continue after resource conflicts.
func (s *Store) ListPendingDeliveries(ctx context.Context, options PendingOptions) (Page[PendingDelivery], error) {
	limit, err := listLimit(ListOptions{Limit: options.Limit, Before: options.After})
	if err != nil {
		return Page[PendingDelivery]{}, err
	}
	query := `SELECT d.group_id,d.id,d.message_id,d.conversation_id,d.recipient_id,d.state,d.turn_id,d.error,d.created_at
	 FROM deliveries d JOIN members m ON m.id=d.recipient_id AND m.group_id=d.group_id
	 WHERE d.state='queued' AND d.turn_id='' AND m.status='active'
	 AND NOT EXISTS(SELECT 1 FROM turns WHERE (conversation_id=d.conversation_id OR member_id=d.recipient_id) AND ` + activeTurnPredicate + `)`
	args := []any{}
	if c := options.After; c != nil {
		query += " AND (d.created_at,d.id)>(?,?)"
		args = append(args, c.CreatedAt.UTC().Format(timestampLayout), c.ID)
	}
	query += " ORDER BY d.created_at,d.id LIMIT ?"
	rows, err := s.db.QueryContext(ctx, query, append(args, limit+1)...)
	if err != nil {
		return Page[PendingDelivery]{}, queueError("扫描待投递失败", err)
	}
	defer rows.Close()
	page := Page[PendingDelivery]{Items: make([]PendingDelivery, 0, limit+1)}
	for rows.Next() {
		var pending PendingDelivery
		var stamp string
		d := &pending.Delivery
		err := rows.Scan(&pending.GroupID, &d.ID, &d.MessageID, &d.ConversationID, &d.RecipientID, &d.State, &d.TurnID, &d.Error, &stamp)
		if err == nil {
			d.CreatedAt, err = time.Parse(timestampLayout, stamp)
		}
		if err != nil {
			return Page[PendingDelivery]{}, queueError("扫描待投递失败", err)
		}
		page.Items = append(page.Items, pending)
	}
	if err := rows.Err(); err != nil {
		return Page[PendingDelivery]{}, queueError("扫描待投递失败", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1].Delivery
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func (s *Store) GetTurn(ctx context.Context, groupID, turnID string) (domain.Turn, error) {
	t, err := scanTurn(s.db.QueryRowContext(ctx, "SELECT "+turnColumns+" FROM turns WHERE group_id=? AND id=?", groupID, turnID))
	if err != nil {
		return domain.Turn{}, queueError("读取轮次失败", err)
	}
	return t, nil
}

func scanTurn(row scanner) (t domain.Turn, err error) {
	var created, updated string
	var accepted, cancelled sql.NullString
	err = row.Scan(&t.ID, &t.GroupID, &t.DeliveryID, &t.ConversationID, &t.MemberID, &t.ClaimToken, &t.State, &t.NativeSessionID,
		&accepted, &cancelled, &t.CandidateText, &t.CandidateTerminal, &t.ReplyMessageID, &t.Reason, &created, &updated)
	if err != nil {
		return
	}
	if t.CreatedAt, err = time.Parse(timestampLayout, created); err != nil {
		return
	}
	if t.UpdatedAt, err = time.Parse(timestampLayout, updated); err != nil {
		return
	}
	for _, field := range []struct {
		stamp sql.NullString
		dest  **time.Time
	}{{accepted, &t.AcceptedAt}, {cancelled, &t.CancelRequestedAt}} {
		if field.stamp.Valid {
			var parsed time.Time
			if parsed, err = time.Parse(timestampLayout, field.stamp.String); err != nil {
				return
			}
			*field.dest = &parsed
		}
	}
	return
}

// Queue errors preserve stable categories and cancellation, never raw SQL or payloads.
func queueError(action string, err error) error {
	for _, known := range []error{ErrTurnBusy, ErrInvalid, ErrNotFound, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return fmt.Errorf("%s：%w", action, known)
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s：%w", action, ErrNotFound)
	}
	return errors.New(action)
}
