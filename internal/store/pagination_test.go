package store

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func TestNewestFirstTimestampAndIDPagination(t *testing.T) {
	s, _ := openTestStore(t)
	base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	groups := make([]domain.Group, 0, 205)
	members := make([]domain.Member, 0, 205)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range 205 {
		// Permute insertion order and tie timestamps independently of IDs.
		n := (i * 73) % 205
		stamp := base.Add(time.Duration(n%5) * time.Nanosecond)
		id := fmt.Sprintf("g-%03d", n)
		if _, err := tx.Exec("INSERT INTO groups(id, name, created_at) VALUES(?, 'fixture', ?)", id, stamp.Format("2006-01-02T15:04:05.000000000Z")); err != nil {
			t.Fatal(err)
		}
		groups = append(groups, domain.Group{ID: id, Name: "fixture", CreatedAt: stamp})
		id = fmt.Sprintf("m-%03d", n)
		if _, err := tx.Exec("INSERT INTO members(id, group_id, name, role, created_at) VALUES(?, 'g-000', 'fixture', 'role', ?)", id, stamp.Format("2006-01-02T15:04:05.000000000Z")); err != nil {
			t.Fatal(err)
		}
		members = append(members, domain.Member{ID: id, GroupID: "g-000", Name: "fixture", Role: "role", Status: domain.MemberStatusActive, CreatedAt: stamp})
	}
	foreignTime := base.Add(time.Hour)
	if _, err := tx.Exec("INSERT INTO members(id, group_id, name, role, created_at) VALUES('foreign-secret', 'g-001', 'foreign', 'role', ?)", foreignTime.Format("2006-01-02T15:04:05.000000000Z")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].CreatedAt.Equal(groups[j].CreatedAt) {
			return groups[i].ID > groups[j].ID
		}
		return groups[i].CreatedAt.After(groups[j].CreatedAt)
	})
	sort.Slice(members, func(i, j int) bool {
		if members[i].CreatedAt.Equal(members[j].CreatedAt) {
			return members[i].ID > members[j].ID
		}
		return members[i].CreatedAt.After(members[j].CreatedAt)
	})
	t.Run("groups", func(t *testing.T) {
		list := func(options ListOptions) (Page[domain.Group], error) { return s.ListGroups(ctx, options) }
		checkPages(t, groups, list, func(g domain.Group) Cursor { return Cursor{g.CreatedAt, g.ID} })
	})
	t.Run("members", func(t *testing.T) {
		list := func(options ListOptions) (Page[domain.Member], error) { return s.ListMembers(ctx, "g-000", options) }
		checkPages(t, members, list, func(m domain.Member) Cursor { return Cursor{m.CreatedAt, m.ID} })
	})
	page, err := s.ListMembers(ctx, "g-000", ListOptions{Before: &Cursor{CreatedAt: foreignTime, ID: "foreign-secret"}})
	if err != nil || !reflect.DeepEqual(page.Items, members[:50]) {
		t.Fatalf("foreign cursor escaped scope: %+v, %v", page, err)
	}
	// A cursor is a position, not a lookup that can reveal another group's row.
	page, err = s.ListMembers(ctx, "g-001", ListOptions{Before: &Cursor{CreatedAt: base, ID: "m-000"}})
	if err != nil || page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil {
		t.Fatalf("foreign anchor leaked members: %+v, %v", page, err)
	}
}

func checkPages[T any](t *testing.T, expected []T, list func(ListOptions) (Page[T], error), position func(T) Cursor) {
	t.Helper()
	for _, requested := range []int{0, 1, 50, 200} {
		limit := requested
		if limit == 0 {
			limit = 50
		}
		var before *Cursor
		for offset := 0; ; {
			page, err := list(ListOptions{Limit: requested, Before: before})
			end := min(offset+limit, len(expected))
			if err != nil || !reflect.DeepEqual(page.Items, expected[offset:end]) {
				t.Fatalf("limit=%d offset=%d: got %d items, %v; want ordered slice [%d:%d]", requested, offset, len(page.Items), err, offset, end)
			}
			if end == len(expected) {
				if page.NextCursor != nil {
					t.Fatalf("exact final page claims a successor: %+v", page.NextCursor)
				}
				break
			}
			want := position(page.Items[len(page.Items)-1])
			if page.NextCursor == nil || !reflect.DeepEqual(*page.NextCursor, want) {
				t.Fatalf("next cursor = %+v, want %+v", page.NextCursor, want)
			}
			before, offset = page.NextCursor, end
		}
	}
}
