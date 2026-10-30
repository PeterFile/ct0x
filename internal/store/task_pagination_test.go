package store

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func TestTaskBoundedHistoryAndEmptyArrays(t *testing.T) {
	s, _ := openTestStore(t)
	g, other := group(t, s), group(t, s)
	empty, err := s.ListTasks(ctx, g.ID, ListOptions{})
	if err != nil || empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != nil {
		t.Fatalf("empty task page: %+v, %v", empty, err)
	}
	base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	want := make([]domain.Task, 0, 205)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range 205 {
		n := (i * 73) % 205
		stamp, id := base.Add(time.Duration(n%5)*time.Nanosecond), fmt.Sprintf("task-%03d", n)
		if _, err := tx.Exec(`INSERT INTO tasks(id,group_id,title,acceptance,created_at) VALUES(?,?,'fixture','criteria',?)`, id, g.ID, stamp.Format(timestampLayout)); err != nil {
			t.Fatal(err)
		}
		want = append(want, domain.Task{ID: id, GroupID: g.ID, Title: "fixture", Acceptance: "criteria", Status: domain.TaskStatusOpen, Revision: 1, DependencyIDs: []string{}, CreatedAt: stamp})
	}
	if _, err := tx.Exec(`INSERT INTO tasks(id,group_id,title,acceptance,created_at) VALUES('foreign',?,'secret','criteria',?)`, other.ID, base.Add(time.Hour).Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
	// Dependencies are loaded after the bounded parent result closes.
	if _, err := tx.Exec(`INSERT INTO task_dependencies(group_id,task_id,dependency_id) VALUES(?,'task-204','task-000')`, g.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if want[i].ID == "task-204" {
			want[i].DependencyIDs = []string{"task-000"}
		}
	}
	sort.Slice(want, func(i, j int) bool {
		if want[i].CreatedAt.Equal(want[j].CreatedAt) {
			return want[i].ID > want[j].ID
		}
		return want[i].CreatedAt.After(want[j].CreatedAt)
	})
	checkPages(t, want, func(q ListOptions) (Page[domain.Task], error) { return s.ListTasks(ctx, g.ID, q) }, func(v domain.Task) Cursor { return Cursor{v.CreatedAt, v.ID} })
	foreign := &Cursor{CreatedAt: base.Add(time.Hour), ID: "foreign"}
	page, err := s.ListTasks(ctx, g.ID, ListOptions{Before: foreign})
	if err != nil || !reflect.DeepEqual(page.Items, want[:50]) {
		t.Fatalf("foreign cursor escaped group: %+v, %v", page, err)
	}
	end := want[len(want)-1]
	page, err = s.ListTasks(ctx, g.ID, ListOptions{Before: &Cursor{CreatedAt: end.CreatedAt, ID: end.ID}})
	if err != nil || page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil {
		t.Fatalf("empty tail: %+v, %v", page, err)
	}
	if _, err := s.ListTasks(ctx, "missing", ListOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing group: %v", err)
	}
	for _, q := range []ListOptions{{Limit: -1}, {Limit: 201}, {Before: &Cursor{ID: "id"}}, {Before: &Cursor{CreatedAt: base}}} {
		if _, err := s.ListTasks(ctx, g.ID, q); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid bounds %+v: %v", q, err)
		}
	}
}
