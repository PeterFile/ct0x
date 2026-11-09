package store

import (
	"context"
	"fmt"
	"time"
)

// DispatchDueCursor is the original, exclusive scan key, never a display cursor.
type DispatchDueCursor struct {
	NextAttemptAt time.Time
	RequestID     string
}

type DispatchDueCandidate struct {
	RequestID     string
	Generation    int64
	NextAttemptAt time.Time
}

type DispatchDueOptions struct {
	Limit int // Zero selects 50; explicit limits are 1 through 50.
	After *DispatchDueCursor
}

type DispatchDuePage struct {
	Items      []DispatchDueCandidate
	NextCursor *DispatchDueCursor // An exactly full page continues; the next page may be empty.
}

const coordinationDispatchDueSQL = `SELECT request_id,generation,next_attempt_at FROM coordination_dispatch_state
 WHERE status='pending' AND next_attempt_at<=?`

// ListDueDispatchRequests captures only metadata. It is not a production pass or fairness policy.
// No lookahead row is loaded: a full page conservatively returns its last original key.
func (s *Store) ListDueDispatchRequests(ctx context.Context, at time.Time, options DispatchDueOptions) (out DispatchDuePage, err error) {
	defer func() { err = storeError("扫描到期分派请求失败", err) }()
	limit := options.Limit
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 50 {
		return out, fmt.Errorf("%w：分派扫描条数必须为 1 到 50", ErrInvalid)
	}
	stamp := at.UTC().Format(timestampLayout)
	if _, err := coordinationDispatchTime(stamp); err != nil {
		return out, err
	}
	query, args := coordinationDispatchDueSQL, []any{stamp}
	if c := options.After; c != nil {
		cursorStamp, err := coordinationDispatchInput(c.RequestID, 1, c.NextAttemptAt)
		if err != nil {
			return out, err
		}
		query += " AND (next_attempt_at,request_id)>(?,?)"
		args = append(args, cursorStamp, c.RequestID)
	}
	query += " ORDER BY next_attempt_at,request_id LIMIT ?"
	rows, err := s.db.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	page := DispatchDuePage{Items: make([]DispatchDueCandidate, 0, limit)}
	for rows.Next() {
		var item DispatchDueCandidate
		var next string
		if err := rows.Scan(&item.RequestID, &item.Generation, &next); err != nil {
			return out, err
		}
		item.NextAttemptAt, err = coordinationDispatchTime(next)
		if err != nil {
			return out, err
		}
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	if len(page.Items) == limit {
		last := page.Items[len(page.Items)-1]
		page.NextCursor = &DispatchDueCursor{NextAttemptAt: last.NextAttemptAt, RequestID: last.RequestID}
	}
	return page, nil
}
