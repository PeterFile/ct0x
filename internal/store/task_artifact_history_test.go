package store

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func TestTaskArtifactBatchHistoriesPreserveCanonicalScopedRefs(t *testing.T) {
	s, _, task, _, _ := taskSubmissionHuman(t)
	base := mustPublishArtifact(t, s, artifactPublishInput(task.GroupID))
	next := taskArtifactNext(t, s, base, base.ID)
	both := []domain.ArtifactVersionRef{{ArtifactID: base.ArtifactID, VersionID: base.ID}, {ArtifactID: base.ArtifactID, VersionID: next.ID}}
	slices.SortFunc(both, func(a, b domain.ArtifactVersionRef) int { return strings.Compare(a.VersionID, b.VersionID) })
	stamp := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	var submissions []domain.Submission
	var reviews []domain.ReviewDecision
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 205 {
		n := (i * 73) % 205
		id, created, revision := fmt.Sprintf("batch-%03d", n), stamp.Add(time.Duration(n%5)*time.Nanosecond), int64(i+1)
		refs := slices.Clone(both)
		if n%2 == 1 {
			refs = refs[:1]
		}
		exec(`INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,created_at) VALUES(?,?,?,?,'user',?)`, id, task.ID, task.GroupID, revision, created.Format(timestampLayout))
		exec(`UPDATE tasks SET active_attempt_id=? WHERE id=?`, id, task.ID)
		exec(`INSERT INTO task_submissions(id,task_id,group_id,task_revision,attempt_id,summary,submitted_by,created_at) VALUES(?,?,?,?,?,'batch','user',?)`, id, task.ID, task.GroupID, revision, id, created.Format(timestampLayout))
		for _, ref := range refs {
			exec(`INSERT INTO task_submission_artifacts(submission_id,task_id,group_id,task_revision,artifact_id,version_id) VALUES(?,?,?,?,?,?)`, id, task.ID, task.GroupID, revision, ref.ArtifactID, ref.VersionID)
		}
		exec(`UPDATE tasks SET status='submitted',current_submission_id=? WHERE id=?`, id, task.ID)
		exec(`INSERT INTO task_review_decisions(id,submission_id,task_id,group_id,task_revision,outcome,reviewed_by,created_at) VALUES(?,?,?,?,?,'rejected','user',?)`, "review-"+id, id, task.ID, task.GroupID, revision, created.Format(timestampLayout))
		exec(`UPDATE tasks SET status='open',revision=revision+1,active_attempt_id=NULL,current_submission_id=NULL WHERE id=?`, task.ID)
		submissions = append(submissions, domain.Submission{ID: id, TaskID: task.ID, GroupID: task.GroupID, TaskRevision: revision, AttemptID: id, Summary: "batch", SubmittedBy: "user", CreatedAt: created, ArtifactVersions: refs})
		reviews = append(reviews, domain.ReviewDecision{ID: "review-" + id, SubmissionID: id, TaskID: task.ID, GroupID: task.GroupID, TaskRevision: revision, Outcome: domain.ReviewOutcomeRejected, ReviewedBy: "user", CreatedAt: created, ArtifactVersions: refs})
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	foreignGroup := group(t, s)
	for _, groupID := range []string{task.GroupID, foreignGroup.ID} {
		other := createTestTask(t, s, groupID)
		attempt, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(other))
		if err != nil {
			t.Fatal(err)
		}
		version := mustPublishArtifact(t, s, artifactPublishInput(groupID))
		input := taskSubmissionInput(attempt)
		input.ArtifactVersions = []domain.ArtifactVersionRef{{ArtifactID: version.ArtifactID, VersionID: version.ID}}
		submission, err := s.SubmitTask(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		decision, err := s.ReviewTask(ctx, taskReviewInput(submission))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetTaskSubmission(ctx, task.GroupID, task.ID, submission.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign parent leaked submission refs: %v", err)
		}
		if _, err := s.GetTaskReview(ctx, task.GroupID, task.ID, decision.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign parent leaked review refs: %v", err)
		}
	}
	taskArtifactNext(t, s, base, next.ID)
	less := func(a time.Time, aid string, b time.Time, bid string) bool {
		if a.Equal(b) {
			return aid > bid
		}
		return a.After(b)
	}
	sort.Slice(submissions, func(i, j int) bool {
		return less(submissions[i].CreatedAt, submissions[i].ID, submissions[j].CreatedAt, submissions[j].ID)
	})
	sort.Slice(reviews, func(i, j int) bool {
		return less(reviews[i].CreatedAt, reviews[i].ID, reviews[j].CreatedAt, reviews[j].ID)
	})
	checkPages(t, submissions, func(q ListOptions) (Page[domain.Submission], error) {
		return s.ListTaskSubmissions(ctx, task.GroupID, task.ID, q)
	}, func(v domain.Submission) Cursor { return Cursor{CreatedAt: v.CreatedAt, ID: v.ID} })
	checkPages(t, reviews, func(q ListOptions) (Page[domain.ReviewDecision], error) {
		return s.ListTaskReviews(ctx, task.GroupID, task.ID, q)
	}, func(v domain.ReviewDecision) Cursor { return Cursor{CreatedAt: v.CreatedAt, ID: v.ID} })
	got, err := s.GetTaskSubmission(ctx, task.GroupID, task.ID, submissions[0].ID)
	if err != nil || !reflect.DeepEqual(got, submissions[0]) {
		t.Fatalf("single read refs: %+v, %v", got, err)
	}
	taskArtifactIntegrity(t, s)
}
