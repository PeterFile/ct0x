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

func TestTaskAttemptHistoryScopesBoundsAndTies(t *testing.T) {
	s, _ := openTestStore(t)
	g, foreign := group(t, s), group(t, s)
	task, other, empty, foreignTask := createTestTask(t, s, g.ID), createTestTask(t, s, g.ID), createTestTask(t, s, g.ID), createTestTask(t, s, foreign.ID)
	page, err := s.ListTaskAttempts(ctx, g.ID, empty.ID, ListOptions{})
	if err != nil || page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil {
		t.Fatalf("empty history: %+v, %v", page, err)
	}
	base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	want := make([]domain.TaskAttempt, 0, 205)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	insert := `INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,created_at) VALUES(?,?,?,1,'user',?)`
	for i := range 205 {
		n := (i * 73) % 205
		stamp, id := base.Add(time.Duration(n%5)*time.Nanosecond), fmt.Sprintf("attempt-%03d", n)
		if _, err := tx.Exec(insert, id, task.ID, g.ID, stamp.Format(timestampLayout)); err != nil {
			t.Fatal(err)
		}
		want = append(want, domain.TaskAttempt{ID: id, TaskID: task.ID, GroupID: g.ID, TaskRevision: 1, CreatedAt: stamp})
	}
	for _, v := range []struct {
		id   string
		task domain.Task
	}{{"other-task", other}, {"foreign", foreignTask}} {
		if _, err := tx.Exec(insert, v.id, v.task.ID, v.task.GroupID, base.Add(time.Hour).Format(timestampLayout)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	sort.Slice(want, func(i, j int) bool {
		if want[i].CreatedAt.Equal(want[j].CreatedAt) {
			return want[i].ID > want[j].ID
		}
		return want[i].CreatedAt.After(want[j].CreatedAt)
	})
	checkPages(t, want, func(q ListOptions) (Page[domain.TaskAttempt], error) {
		return s.ListTaskAttempts(ctx, g.ID, task.ID, q)
	}, func(a domain.TaskAttempt) Cursor { return Cursor{CreatedAt: a.CreatedAt, ID: a.ID} })
	cursor := &Cursor{CreatedAt: base.Add(time.Hour), ID: "foreign"}
	page, err = s.ListTaskAttempts(ctx, g.ID, task.ID, ListOptions{Before: cursor})
	if err != nil || !reflect.DeepEqual(page.Items, want[:50]) {
		t.Fatalf("cursor escaped task/group: %+v, %v", page, err)
	}
	last := want[len(want)-1]
	page, err = s.ListTaskAttempts(ctx, g.ID, task.ID, ListOptions{Before: &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}})
	if err != nil || page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil {
		t.Fatalf("empty history tail: %+v, %v", page, err)
	}
	for _, pair := range [][2]string{{g.ID, "missing"}, {foreign.ID, task.ID}, {g.ID, foreignTask.ID}} {
		if _, err := s.ListTaskAttempts(ctx, pair[0], pair[1], ListOptions{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing/cross-group task list: %v", err)
		}
	}
	for _, ids := range [][3]string{{g.ID, other.ID, want[0].ID}, {foreign.ID, task.ID, want[0].ID}, {g.ID, task.ID, "missing"}} {
		if _, err := s.GetTaskAttempt(ctx, ids[0], ids[1], ids[2]); !errors.Is(err, ErrNotFound) {
			t.Errorf("unscoped history get: %v", err)
		}
	}
	for _, q := range []ListOptions{{Limit: -1}, {Limit: 201}, {Before: &Cursor{ID: "id"}}, {Before: &Cursor{CreatedAt: base}}} {
		if _, err := s.ListTaskAttempts(ctx, g.ID, task.ID, q); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid cursor/limit %+v: %v", q, err)
		}
	}
}
