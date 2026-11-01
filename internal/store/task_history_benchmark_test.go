package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

// Seed complete human-only generations in one transaction, outside benchmark timers.
func taskHistoryFixture(tb testing.TB, count int) (*Store, string, string, *Cursor) {
	tb.Helper()
	s, err := Open(filepath.Join(tb.TempDir(), "history.sqlite"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { s.Close() })
	g, err := s.CreateGroup(ctx, CreateGroupInput{Name: "history"})
	if err != nil {
		tb.Fatal(err)
	}
	task, err := s.CreateTask(ctx, taskInput(g.ID))
	if err != nil {
		tb.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		tb.Fatal(err)
	}
	defer tx.Rollback()
	base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	for i := range count {
		n := (i * 73) % count
		id, stamp, revision := fmt.Sprintf("history-%04d", n), base.Add(time.Duration(n%5)*time.Nanosecond).Format(timestampLayout), i+1
		for _, statement := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,created_at) VALUES(?,?,?,?,'user',?)`, []any{id, task.ID, g.ID, revision, stamp}},
			{`UPDATE tasks SET active_attempt_id=? WHERE id=?`, []any{id, task.ID}},
			{`INSERT INTO task_submissions(id,task_id,group_id,task_revision,attempt_id,summary,submitted_by,created_at) VALUES(?,?,?,?,?,'history','user',?)`, []any{id, task.ID, g.ID, revision, id, stamp}},
			{`UPDATE tasks SET status='submitted',current_submission_id=? WHERE id=?`, []any{id, task.ID}},
			{`INSERT INTO task_review_decisions(id,submission_id,task_id,group_id,task_revision,outcome,reviewed_by,created_at) VALUES(?,?,?,?,?,'rejected','user',?)`, []any{id, id, task.ID, g.ID, revision, stamp}},
			{`UPDATE tasks SET status='open',revision=revision+1,active_attempt_id=NULL,current_submission_id=NULL WHERE id=?`, []any{task.ID}},
		} {
			if _, err := tx.Exec(statement.sql, statement.args...); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	return s, g.ID, task.ID, &Cursor{CreatedAt: base.Add(2 * time.Nanosecond), ID: fmt.Sprintf("history-%04d", count/2)}
}

func BenchmarkTaskHistory(b *testing.B) {
	s, groupID, taskID, before := taskHistoryFixture(b, 1024)
	for _, kind := range []string{"attempts", "submissions", "reviews"} {
		for _, cursor := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/before=%t", kind, cursor), func(b *testing.B) {
				options := ListOptions{}
				if cursor {
					options.Before = before
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					var count int
					var err error
					switch kind {
					case "attempts":
						var page Page[domain.TaskAttempt]
						page, err = s.ListTaskAttempts(ctx, groupID, taskID, options)
						count = len(page.Items)
					case "submissions":
						var page Page[domain.Submission]
						page, err = s.ListTaskSubmissions(ctx, groupID, taskID, options)
						count = len(page.Items)
					case "reviews":
						var page Page[domain.ReviewDecision]
						page, err = s.ListTaskReviews(ctx, groupID, taskID, options)
						count = len(page.Items)
					}
					if err != nil || count != 50 {
						b.Fatalf("bounded history: %d, %v", count, err)
					}
				}
			})
		}
	}
}
