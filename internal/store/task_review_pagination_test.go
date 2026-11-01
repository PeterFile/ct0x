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

func TestTaskReviewHistoryScopesBoundsAndTies(t *testing.T) {
	s, _ := openTestStore(t)
	g, foreign := group(t, s), group(t, s)
	task, other, empty, foreignTask := createTestTask(t, s, g.ID), createTestTask(t, s, g.ID), createTestTask(t, s, g.ID), createTestTask(t, s, foreign.ID)
	page, err := s.ListTaskReviews(ctx, g.ID, empty.ID, ListOptions{})
	if err != nil || page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil {
		t.Fatalf("empty reviews: %+v, %v", page, err)
	}
	base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	want := make([]domain.ReviewDecision, 0, 205)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	insert := func(task domain.Task, id string, stamp time.Time, outcome domain.ReviewOutcome) domain.ReviewDecision {
		t.Helper()
		attemptID, subID := "attempt-"+id, "submission-"+id
		if _, err := tx.Exec(`INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,created_at) VALUES(?,?,?,1,'user',?)`, attemptID, task.ID, task.GroupID, stamp.Format(timestampLayout)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO task_submissions(id,task_id,group_id,task_revision,attempt_id,summary,submitted_by,created_at) VALUES(?,?,?,1,?,'history','user',?)`, subID, task.ID, task.GroupID, attemptID, stamp.Format(timestampLayout)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO task_review_decisions(id,submission_id,task_id,group_id,task_revision,outcome,reviewed_by,reason,created_at) VALUES(?,?,?,?,1,?,'user','history reason',?)`, id, subID, task.ID, task.GroupID, outcome, stamp.Format(timestampLayout)); err != nil {
			t.Fatal(err)
		}
		return domain.ReviewDecision{ID: id, SubmissionID: subID, TaskID: task.ID, GroupID: task.GroupID, TaskRevision: 1, Outcome: outcome, ReviewedBy: "user", Reason: "history reason", CreatedAt: stamp, ArtifactVersions: []domain.ArtifactVersionRef{}}
	}
	for i := range 205 {
		n := (i * 73) % 205
		stamp, id := base.Add(time.Duration(n%5)*time.Nanosecond), fmt.Sprintf("review-%03d", n)
		outcome := domain.ReviewOutcomeAccepted
		if n%2 == 1 {
			outcome = domain.ReviewOutcomeRejected
		}
		want = append(want, insert(task, id, stamp, outcome))
	}
	insert(other, "other-task", base.Add(time.Hour), domain.ReviewOutcomeAccepted)
	insert(foreignTask, "foreign", base.Add(time.Hour), domain.ReviewOutcomeRejected)
	if staged, err := taskReviewInTx(ctx, tx, g.ID, task.ID, want[0].ID); err != nil || !reflect.DeepEqual(staged, want[0]) {
		t.Fatalf("supplied transaction lost staged decision: %+v, %v", staged, err)
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
	checkPages(t, want, func(q ListOptions) (Page[domain.ReviewDecision], error) {
		return s.ListTaskReviews(ctx, g.ID, task.ID, q)
	}, func(d domain.ReviewDecision) Cursor { return Cursor{CreatedAt: d.CreatedAt, ID: d.ID} })
	cursor := &Cursor{CreatedAt: base.Add(time.Hour), ID: "foreign"}
	page, err = s.ListTaskReviews(ctx, g.ID, task.ID, ListOptions{Before: cursor})
	if err != nil || !reflect.DeepEqual(page.Items, want[:50]) {
		t.Fatalf("cursor escaped task/group: %+v, %v", page, err)
	}
	last := want[len(want)-1]
	page, err = s.ListTaskReviews(ctx, g.ID, task.ID, ListOptions{Before: &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}})
	if err != nil || page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil {
		t.Fatalf("empty review tail: %+v, %v", page, err)
	}
	for _, pair := range [][2]string{{g.ID, "missing"}, {foreign.ID, task.ID}, {g.ID, foreignTask.ID}} {
		if _, err := s.ListTaskReviews(ctx, pair[0], pair[1], ListOptions{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing/cross-group task list: %v", err)
		}
	}
	for _, ids := range [][3]string{{g.ID, other.ID, want[0].ID}, {foreign.ID, task.ID, want[0].ID}, {g.ID, task.ID, "missing"}} {
		if _, err := s.GetTaskReview(ctx, ids[0], ids[1], ids[2]); !errors.Is(err, ErrNotFound) {
			t.Errorf("unscoped decision get: %v", err)
		}
	}
	for _, q := range []ListOptions{{Limit: -1}, {Limit: 201}, {Before: &Cursor{ID: "id"}}, {Before: &Cursor{CreatedAt: base}}} {
		if _, err := s.ListTaskReviews(ctx, g.ID, task.ID, q); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid cursor/limit %+v: %v", q, err)
		}
	}
}
