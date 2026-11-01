package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func taskSubmissionInput(attempt domain.TaskAttempt) SubmitTaskInput {
	return SubmitTaskInput{GroupID: attempt.GroupID, TaskID: attempt.TaskID, AttemptID: attempt.ID, ExpectedRevision: attempt.TaskRevision, Summary: "交付文本。 ", ActorID: "user", IdempotencyKey: "submit"}
}

func taskSubmissionHuman(t *testing.T) (*Store, string, domain.Task, domain.TaskAttempt, SubmitTaskInput) {
	t.Helper()
	s, path := openTestStore(t)
	g := group(t, s)
	task := createTestTask(t, s, g.ID)
	activate := taskAttemptInput(task)
	activate.IdempotencyKey = "submit"
	attempt, err := s.ActivateTaskAttempt(ctx, activate)
	if err != nil {
		t.Fatal(err)
	}
	return s, path, task, attempt, taskSubmissionInput(attempt)
}

func taskSubmissionCounts(t *testing.T, s *Store, rows, receipts int) {
	t.Helper()
	for query, want := range map[string]int{"SELECT count(*) FROM task_submissions": rows, "SELECT count(*) FROM mutation_idempotency WHERE operation='submit_task'": receipts} {
		var got int
		if err := s.db.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %d, %v; want %d", query, got, err, want)
		}
	}
}

// Advance the real stored history without implementing the next Review API slice.
func taskSubmissionRejectFixture(t *testing.T, s *Store, submission domain.Submission) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO task_review_decisions(id,submission_id,task_id,group_id,task_revision,outcome,reviewed_by,created_at)
 VALUES(?,?,?,?,?,'rejected','user',?)`, "fixture-review-"+submission.ID, submission.ID, submission.TaskID, submission.GroupID, submission.TaskRevision, submission.CreatedAt.Add(time.Minute).Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET status='open',revision=revision+1,active_attempt_id=NULL,current_submission_id=NULL WHERE id=? AND group_id=?`, submission.TaskID, submission.GroupID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestTaskSubmissionHumanPersistsAndReplaysAfterRevision(t *testing.T) {
	s, path, task, attempt, input := taskSubmissionHuman(t)
	submission, err := s.SubmitTask(ctx, input)
	if err != nil || submission.ID == "" || submission.ID == attempt.ID || submission.TaskID != task.ID || submission.GroupID != task.GroupID || submission.AttemptID != attempt.ID || submission.TaskRevision != 1 || submission.Summary != input.Summary || submission.SubmittedBy != "user" || submission.ArtifactVersions == nil || len(submission.ArtifactVersions) != 0 || submission.CreatedAt.IsZero() || submission.CreatedAt.Location() != time.UTC {
		t.Fatalf("manual text submission: %+v, %v", submission, err)
	}
	check := func(s *Store) {
		t.Helper()
		got, err := s.GetTaskSubmission(ctx, task.GroupID, task.ID, submission.ID)
		if err != nil || !reflect.DeepEqual(got, submission) {
			t.Fatalf("stored evidence changed: %+v, %v", got, err)
		}
		got, err = s.SubmitTask(ctx, input)
		if err != nil || !reflect.DeepEqual(got, submission) {
			t.Fatalf("immutable replay: %+v, %v", got, err)
		}
		current, err := s.GetTask(ctx, task.GroupID, task.ID)
		if err != nil || current.Status != domain.TaskStatusSubmitted || current.Revision != 1 || current.ActiveAttemptID != attempt.ID || current.CurrentSubmissionID != submission.ID {
			t.Fatalf("submission implied acceptance: %+v, %v", current, err)
		}
		data, err := json.Marshal(got)
		if err != nil || !strings.Contains(string(data), `"artifact_versions":[]`) {
			t.Fatalf("nil evidence refs: %s, %v", data, err)
		}
		queueCounts(t, s, 0, 0, 0)
		taskSubmissionCounts(t, s, 1, 1)
	}
	check(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	check(reopened)
	taskSubmissionRejectFixture(t, reopened, submission)
	before, err := reopened.GetTask(ctx, task.GroupID, task.ID)
	if err != nil || before.Revision != 2 {
		t.Fatalf("history fixture: %+v, %v", before, err)
	}
	if replay, err := reopened.SubmitTask(ctx, input); err != nil || !reflect.DeepEqual(replay, submission) {
		t.Fatalf("advanced task lost receipt: %+v, %v", replay, err)
	}
	if historical, err := reopened.GetTaskSubmission(ctx, task.GroupID, task.ID, submission.ID); err != nil || !reflect.DeepEqual(historical, submission) {
		t.Fatalf("rejected evidence changed: %+v, %v", historical, err)
	}
	if after, err := reopened.GetTask(ctx, task.GroupID, task.ID); err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("receipt reopened or rebound task: %+v, %v", after, err)
	}
}

func TestTaskSubmissionInvalidInputDoesNotBurnReceipt(t *testing.T) {
	s, _, task, _, input := taskSubmissionHuman(t)
	for _, change := range []func(*SubmitTaskInput){
		func(i *SubmitTaskInput) { i.Summary = " \t" },
		// Complete missing refs now use real scoped lookup; TaskArtifact tests cover ErrNotFound.
		func(i *SubmitTaskInput) {
			i.ArtifactVersions = []domain.ArtifactVersionRef{{ArtifactID: "fake", VersionID: ""}}
		},
		func(i *SubmitTaskInput) { i.ArtifactVersions = []domain.ArtifactVersionRef{{}} },
	} {
		bad := input
		change(&bad)
		if got, err := s.SubmitTask(ctx, bad); !errors.Is(err, ErrInvalid) || got.ID != "" {
			t.Fatalf("invalid evidence accepted: %+v, %v", got, err)
		}
		taskSubmissionCounts(t, s, 0, 0)
	}
	if current, err := s.GetTask(ctx, task.GroupID, task.ID); err != nil || current.Status != domain.TaskStatusOpen || current.CurrentSubmissionID != "" {
		t.Fatalf("invalid request advanced task: %+v, %v", current, err)
	}
	if _, err := s.SubmitTask(ctx, input); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*SubmitTaskInput){
		func(i *SubmitTaskInput) { i.Summary += " " }, func(i *SubmitTaskInput) { i.Summary = "" },
		func(i *SubmitTaskInput) { i.TaskID = "missing" }, func(i *SubmitTaskInput) { i.AttemptID = "missing" },
		func(i *SubmitTaskInput) { i.ExpectedRevision = 0 },
		func(i *SubmitTaskInput) { i.ArtifactVersions = []domain.ArtifactVersionRef{{}} },
	} {
		bad := input
		change(&bad)
		if _, err := s.SubmitTask(ctx, bad); !errors.Is(err, ErrConflict) {
			t.Fatalf("changed key did not precede validation: %v", err)
		}
	}
	taskSubmissionCounts(t, s, 1, 1)
}

func TestTaskSubmissionRejectsForeignAndStaleAttempts(t *testing.T) {
	s, _, task, attempt, input := taskSubmissionHuman(t)
	otherGroup := group(t, s)
	foreignTask := createTestTask(t, s, otherGroup.ID)
	foreign, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(foreignTask))
	if err != nil {
		t.Fatal(err)
	}
	otherTask := createTestTask(t, s, task.GroupID)
	other, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(otherTask))
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*SubmitTaskInput){
		func(i *SubmitTaskInput) { i.GroupID = otherGroup.ID }, func(i *SubmitTaskInput) { i.TaskID = otherTask.ID },
		func(i *SubmitTaskInput) { i.AttemptID = other.ID }, func(i *SubmitTaskInput) { i.AttemptID = foreign.ID },
		func(i *SubmitTaskInput) { i.ExpectedRevision++ },
	} {
		bad := input
		change(&bad)
		if _, err := s.SubmitTask(ctx, bad); !errors.Is(err, ErrConflict) && !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign/stale reference accepted: %v", err)
		}
	}
	next := taskAttemptInput(task)
	next.ExpectedActiveAttemptID = attempt.ID
	fresh, err := s.ActivateTaskAttempt(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitTask(ctx, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("superseded attempt submitted: %v", err)
	}
	taskSubmissionCounts(t, s, 0, 0)
	input.AttemptID = fresh.ID
	if _, err := s.SubmitTask(ctx, input); err != nil {
		t.Fatal(err)
	}
}

func TestTaskSubmissionCASFailureRollsBack(t *testing.T) {
	s, _, task, _, input := taskSubmissionHuman(t)
	before, err := s.GetTask(ctx, task.GroupID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER force_submission_cas_miss AFTER INSERT ON task_submissions
 BEGIN UPDATE tasks SET active_attempt_id=NULL WHERE id=NEW.task_id; END`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.SubmitTask(ctx, input); !errors.Is(err, ErrConflict) || got.ID != "" {
		t.Fatalf("SQL CAS miss returned success: %+v, %v", got, err)
	}
	taskSubmissionCounts(t, s, 0, 0)
	if current, err := s.GetTask(ctx, task.GroupID, task.ID); err != nil || !reflect.DeepEqual(current, before) {
		t.Fatalf("partial submission changed task: %+v, %v", current, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER force_submission_cas_miss"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitTask(ctx, input); err != nil {
		t.Fatal(err)
	}
	taskSubmissionCounts(t, s, 1, 1)
}

func TestTaskSubmissionConcurrentReplayAndCompetition(t *testing.T) {
	for _, mode := range []string{"same-key", "different-keys", "no-key"} {
		t.Run(mode, func(t *testing.T) {
			s, path, _, _, input := taskSubmissionHuman(t)
			handles := []*Store{s, queueHandle(t, path, false)}
			type result struct {
				submission domain.Submission
				err        error
			}
			results, start := make(chan result, 2), make(chan struct{})
			for i, handle := range handles {
				go func() {
					<-start
					request := input
					if mode == "different-keys" {
						request.IdempotencyKey = fmt.Sprint(i)
					} else if mode == "no-key" {
						request.IdempotencyKey = ""
					}
					value, err := handle.SubmitTask(ctx, request)
					results <- result{value, err}
				}()
			}
			close(start)
			var winner domain.Submission
			success, conflicts := 0, 0
			for range 2 {
				r := <-results
				if errors.Is(r.err, ErrConflict) {
					conflicts++
					continue
				}
				if r.err != nil {
					t.Fatal(r.err)
				}
				if winner.ID != "" && !reflect.DeepEqual(winner, r.submission) {
					t.Fatal("same-key replay changed evidence")
				}
				winner = r.submission
				success++
			}
			if mode == "same-key" {
				if success != 2 || conflicts != 0 {
					t.Fatalf("replay outcomes: %d/%d", success, conflicts)
				}
			} else if success != 1 || conflicts != 1 {
				t.Fatalf("competing outcomes: %d/%d", success, conflicts)
			}
			keys := 1
			if mode == "no-key" {
				keys = 0
			}
			taskSubmissionCounts(t, s, 1, keys)
		})
	}
}

func TestTaskSubmissionPayloadCanonicalizesExactRefPairs(t *testing.T) {
	input := SubmitTaskInput{GroupID: "group", TaskID: "task", AttemptID: "attempt", ExpectedRevision: 1, Summary: "text", ArtifactVersions: []domain.ArtifactVersionRef{{ArtifactID: "b", VersionID: "blue"}, {ArtifactID: "a", VersionID: "one"}, {ArtifactID: "b", VersionID: "blue"}, {ArtifactID: "a", VersionID: "two"}}}
	before := slices.Clone(input.ArtifactVersions)
	payload := taskSubmissionPayload(input)
	other := input
	other.ActorID, other.IdempotencyKey = "different-scope", "different-key"
	other.ArtifactVersions = []domain.ArtifactVersionRef{{ArtifactID: "a", VersionID: "two"}, {ArtifactID: "b", VersionID: "blue"}, {ArtifactID: "a", VersionID: "one"}}
	if !bytes.Equal(payload, taskSubmissionPayload(other)) || !slices.Equal(input.ArtifactVersions, before) {
		t.Fatal("payload changed on pair ordering/duplicates or mutated caller")
	}
	other.ArtifactVersions = []domain.ArtifactVersionRef{{ArtifactID: "a", VersionID: "one"}, {ArtifactID: "b", VersionID: "blue"}}
	if bytes.Equal(payload, taskSubmissionPayload(other)) {
		t.Fatal("different versions of one artifact were collapsed")
	}
}
