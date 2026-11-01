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

func TestTaskSubmissionHistoryScopesBoundsAndTies(t *testing.T) {
	s, _ := openTestStore(t)
	g, foreign := group(t, s), group(t, s)
	task, other, empty, foreignTask := createTestTask(t, s, g.ID), createTestTask(t, s, g.ID), createTestTask(t, s, g.ID), createTestTask(t, s, foreign.ID)
	page, err := s.ListTaskSubmissions(ctx, g.ID, empty.ID, ListOptions{})
	if err != nil || page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil {
		t.Fatalf("empty evidence: %+v, %v", page, err)
	}
	base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	want := make([]domain.Submission, 0, 205)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	insert := func(task domain.Task, id string, stamp time.Time) {
		t.Helper()
		attemptID := "attempt-" + id
		if _, err := tx.Exec(`INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,created_at) VALUES(?,?,?,1,'user',?)`, attemptID, task.ID, task.GroupID, stamp.Format(timestampLayout)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO task_submissions(id,task_id,group_id,task_revision,attempt_id,summary,submitted_by,created_at) VALUES(?,?,?,1,?,'history','user',?)`, id, task.ID, task.GroupID, attemptID, stamp.Format(timestampLayout)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 205 {
		n := (i * 73) % 205
		stamp, id := base.Add(time.Duration(n%5)*time.Nanosecond), fmt.Sprintf("submission-%03d", n)
		insert(task, id, stamp)
		want = append(want, domain.Submission{ID: id, TaskID: task.ID, GroupID: g.ID, TaskRevision: 1, AttemptID: "attempt-" + id, Summary: "history", SubmittedBy: "user", CreatedAt: stamp, ArtifactVersions: []domain.ArtifactVersionRef{}})
	}
	insert(other, "other-task", base.Add(time.Hour))
	insert(foreignTask, "foreign", base.Add(time.Hour))
	if staged, err := taskSubmissionInTx(ctx, tx, g.ID, task.ID, want[0].ID); err != nil || !reflect.DeepEqual(staged, want[0]) {
		t.Fatalf("supplied transaction lost staged evidence: %+v, %v", staged, err)
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
	checkPages(t, want, func(q ListOptions) (Page[domain.Submission], error) {
		return s.ListTaskSubmissions(ctx, g.ID, task.ID, q)
	}, func(s domain.Submission) Cursor { return Cursor{CreatedAt: s.CreatedAt, ID: s.ID} })
	cursor := &Cursor{CreatedAt: base.Add(time.Hour), ID: "foreign"}
	page, err = s.ListTaskSubmissions(ctx, g.ID, task.ID, ListOptions{Before: cursor})
	if err != nil || !reflect.DeepEqual(page.Items, want[:50]) {
		t.Fatalf("cursor escaped task/group: %+v, %v", page, err)
	}
	for _, pair := range [][2]string{{g.ID, "missing"}, {foreign.ID, task.ID}, {g.ID, foreignTask.ID}} {
		if _, err := s.ListTaskSubmissions(ctx, pair[0], pair[1], ListOptions{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing/cross-group task list: %v", err)
		}
	}
	for _, ids := range [][3]string{{g.ID, other.ID, want[0].ID}, {foreign.ID, task.ID, want[0].ID}, {g.ID, task.ID, "missing"}} {
		if _, err := s.GetTaskSubmission(ctx, ids[0], ids[1], ids[2]); !errors.Is(err, ErrNotFound) {
			t.Errorf("unscoped evidence get: %v", err)
		}
	}
	for _, q := range []ListOptions{{Limit: -1}, {Limit: 201}, {Before: &Cursor{ID: "id"}}, {Before: &Cursor{CreatedAt: base}}} {
		if _, err := s.ListTaskSubmissions(ctx, g.ID, task.ID, q); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid cursor/limit %+v: %v", q, err)
		}
	}
}
