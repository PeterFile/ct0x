package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestEarliestActiveMemberID(t *testing.T) {
	s, _ := openTestStore(t)
	g, other := group(t, s), group(t, s)
	if id, err := s.EarliestActiveMemberID(ctx, g.ID); err != nil || id != "" {
		t.Fatalf("empty group routing: %q, %v", id, err)
	}
	if id, err := s.EarliestActiveMemberID(ctx, "missing"); !errors.Is(err, ErrNotFound) || id != "" {
		t.Fatalf("missing group routing: %q, %v", id, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// The oldest active member is outside a default newest-first member page.
	for i := range 205 {
		id, groupID, status := fmt.Sprintf("member-%03d", i), g.ID, "active"
		stamp := "2026-01-02T00:00:00.000000000Z"
		switch i {
		case 0:
			status, stamp = "left", "2026-01-01T00:00:00.000000000Z"
		case 1:
			groupID, stamp = other.ID, "2025-01-01T00:00:00.000000000Z"
		case 2, 3:
			stamp = "2026-01-01T00:00:00.000000001Z"
		}
		if _, err := tx.Exec(`INSERT INTO members(id,group_id,name,role,status,created_at) VALUES(?,?,'member','role',?,?)`, id, groupID, status, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if id, err := s.EarliestActiveMemberID(ctx, g.ID); err != nil || id != "member-002" {
		t.Fatalf("timestamp/ID routing or group/status isolation: %q, %v", id, err)
	}
	if _, err := s.db.Exec("UPDATE members SET status='left' WHERE id='member-002'"); err != nil {
		t.Fatal(err)
	}
	if id, err := s.EarliestActiveMemberID(ctx, g.ID); err != nil || id != "member-003" {
		t.Fatalf("departed member remained eligible: %q, %v", id, err)
	}
	if _, err := s.db.Exec("UPDATE members SET status='left' WHERE group_id=?", g.ID); err != nil {
		t.Fatal(err)
	}
	if id, err := s.EarliestActiveMemberID(ctx, g.ID); err != nil || id != "" {
		t.Fatalf("no active members: %q, %v", id, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.EarliestActiveMemberID(canceled, g.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("routing lost cancellation: %v", err)
	}
	assertMessageCounts(t, s, 0, 0, 0, 0)
}
