package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func taskReviewInput(submission domain.Submission) ReviewTaskInput {
	return ReviewTaskInput{GroupID: submission.GroupID, TaskID: submission.TaskID, SubmissionID: submission.ID, ExpectedRevision: submission.TaskRevision, Outcome: domain.ReviewOutcomeAccepted, Reason: "通过。 ", ActorID: "user", IdempotencyKey: "review"}
}

func taskReviewSubmitted(t *testing.T) (*Store, string, domain.Submission, ReviewTaskInput) {
	t.Helper()
	s, path, _, _, input := taskSubmissionHuman(t)
	submission, err := s.SubmitTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	return s, path, submission, taskReviewInput(submission)
}

func taskReviewCounts(t *testing.T, s *Store, rows, receipts int) {
	t.Helper()
	for query, want := range map[string]int{"SELECT count(*) FROM task_review_decisions": rows, "SELECT count(*) FROM mutation_idempotency WHERE operation='review_task'": receipts} {
		var got int
		if err := s.db.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %d, %v; want %d", query, got, err, want)
		}
	}
}

func TestTaskReviewOnlyUserBeforeDisclosureAndJSONHidesActor(t *testing.T) {
	s, _, submission, input := taskReviewSubmitted(t)
	m := member(t, s, submission.GroupID)
	actors := []string{"", m.ID, "USER", "user "}
	for _, actor := range actors {
		bad := input
		bad.ActorID = actor
		_, err := s.ReviewTask(ctx, bad)
		requireMemberToolDenied(t, err)
	}
	taskReviewCounts(t, s, 0, 0)
	var unbound ReviewTaskInput
	if err := json.Unmarshal([]byte(`{"ActorID":"user","actor_id":"user"}`), &unbound); err != nil || unbound.ActorID != "" {
		t.Fatalf("JSON selected reviewer authority: %+v, %v", unbound, err)
	}
	encoded, err := json.Marshal(input)
	if err != nil || strings.Contains(string(encoded), "ActorID") || strings.Contains(string(encoded), "actor_id") {
		t.Fatalf("actor escaped request: %s, %v", encoded, err)
	}
	if err := json.Unmarshal([]byte(`{"ActorID":"forged","actor_id":"forged","ReviewedBy":"forged","ArtifactVersions":[{"ArtifactID":"fake","VersionID":"fake"}],"Submission":{"Summary":"replacement"}}`), &input); err != nil || input.ActorID != "user" {
		t.Fatalf("JSON replaced trusted review identity: %+v, %v", input, err)
	}
	decision, err := s.ReviewTask(ctx, input)
	if err != nil || decision.ReviewedBy != "user" || decision.SubmissionID != submission.ID || decision.TaskID != submission.TaskID || decision.GroupID != submission.GroupID || decision.TaskRevision != submission.TaskRevision || decision.ArtifactVersions == nil || len(decision.ArtifactVersions) != 0 {
		t.Fatalf("review substituted request evidence: %+v, %v", decision, err)
	}
	for _, actor := range actors {
		for _, changed := range []bool{false, true} {
			bad := input
			bad.ActorID = actor
			if changed {
				bad.TaskID = "missing"
				bad.ExpectedRevision = 0
				bad.Outcome = "invalid"
			}
			_, err := s.ReviewTask(ctx, bad)
			requireMemberToolDenied(t, err)
		}
	}
	taskReviewCounts(t, s, 1, 1)
}

func TestTaskReviewInvalidOutcomeRollbackAndPayloadConflict(t *testing.T) {
	s, _, submission, input := taskReviewSubmitted(t)
	before, err := s.GetTask(ctx, submission.GroupID, submission.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	bad := input
	bad.Outcome = "invalid"
	if got, err := s.ReviewTask(ctx, bad); !errors.Is(err, ErrInvalid) || !errors.Is(err, domain.ErrTaskInvalid) || got.ID != "" {
		t.Fatalf("invalid outcome accepted: %+v, %v", got, err)
	}
	taskReviewCounts(t, s, 0, 0)
	if current, err := s.GetTask(ctx, submission.GroupID, submission.TaskID); err != nil || !reflect.DeepEqual(current, before) {
		t.Fatalf("invalid review changed task: %+v, %v", current, err)
	}
	original, err := s.ReviewTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ReviewTaskInput){
		func(i *ReviewTaskInput) { i.Reason += " " }, func(i *ReviewTaskInput) { i.Outcome = domain.ReviewOutcomeRejected },
		func(i *ReviewTaskInput) { i.Outcome = "invalid" }, func(i *ReviewTaskInput) { i.ExpectedRevision = 0 },
		func(i *ReviewTaskInput) { i.SubmissionID = "" }, func(i *ReviewTaskInput) { i.TaskID = "missing" },
	} {
		changed := input
		change(&changed)
		if _, err := s.ReviewTask(ctx, changed); !errors.Is(err, ErrConflict) {
			t.Fatalf("changed key did not precede validation: %v", err)
		}
	}
	if replay, err := s.ReviewTask(ctx, input); err != nil || !reflect.DeepEqual(replay, original) {
		t.Fatalf("accepted replay changed evidence: %+v, %v", replay, err)
	}
	taskReviewCounts(t, s, 1, 1)
}

func TestTaskReviewRejectsForeignTaskAndSubmission(t *testing.T) {
	s, _, submission, input := taskReviewSubmitted(t)
	otherGroup := group(t, s)
	foreignTask := createTestTask(t, s, otherGroup.ID)
	foreignAttempt, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(foreignTask))
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.SubmitTask(ctx, taskSubmissionInput(foreignAttempt))
	if err != nil {
		t.Fatal(err)
	}
	otherTask := createTestTask(t, s, submission.GroupID)
	for _, change := range []func(*ReviewTaskInput){
		func(i *ReviewTaskInput) { i.GroupID = otherGroup.ID }, func(i *ReviewTaskInput) { i.GroupID = "missing" },
		func(i *ReviewTaskInput) { i.TaskID = otherTask.ID }, func(i *ReviewTaskInput) { i.SubmissionID = foreign.ID },
		func(i *ReviewTaskInput) { i.SubmissionID = "missing" },
	} {
		bad := input
		change(&bad)
		if _, err := s.ReviewTask(ctx, bad); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign review read accepted: %v", err)
		}
	}
	taskReviewCounts(t, s, 0, 0)
	if _, err := s.ReviewTask(ctx, input); err != nil {
		t.Fatal(err)
	}
}

func TestTaskReviewCASMismatchRollsBackRowAndReceipt(t *testing.T) {
	s, _, submission, input := taskReviewSubmitted(t)
	before, err := s.GetTask(ctx, submission.GroupID, submission.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	// Change status/pointers within the same revision after immutable row insertion.
	if _, err := s.db.Exec(`CREATE TRIGGER force_review_cas_miss AFTER INSERT ON task_review_decisions
 BEGIN UPDATE tasks SET status='open',active_attempt_id=NULL,current_submission_id=NULL WHERE id=NEW.task_id; END`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReviewTask(ctx, input); !errors.Is(err, ErrConflict) || got.ID != "" {
		t.Fatalf("SQL CAS missed same-generation change: %+v, %v", got, err)
	}
	taskReviewCounts(t, s, 0, 0)
	if after, err := s.GetTask(ctx, submission.GroupID, submission.TaskID); err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("failed CAS changed task: %+v, %v", after, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER force_review_cas_miss"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReviewTask(ctx, input); err != nil {
		t.Fatal(err)
	}
	taskReviewCounts(t, s, 1, 1)
}

func TestTaskReviewConcurrentReplayAndOppositeOutcomes(t *testing.T) {
	for _, sameKey := range []bool{true, false} {
		t.Run(fmt.Sprintf("same-key=%t", sameKey), func(t *testing.T) {
			s, path, submission, input := taskReviewSubmitted(t)
			handles := []*Store{s, queueHandle(t, path, false)}
			type result struct {
				decision domain.ReviewDecision
				err      error
			}
			results, start := make(chan result, 2), make(chan struct{})
			for i, handle := range handles {
				go func() {
					<-start
					request := input
					if !sameKey {
						request.IdempotencyKey = fmt.Sprint(i)
						if i == 1 {
							request.Outcome = domain.ReviewOutcomeRejected
						}
					}
					value, err := handle.ReviewTask(ctx, request)
					results <- result{value, err}
				}()
			}
			close(start)
			var winner domain.ReviewDecision
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
				if winner.ID != "" && !reflect.DeepEqual(winner, r.decision) {
					t.Fatal("same-key review changed evidence")
				}
				winner = r.decision
				success++
			}
			if sameKey {
				if success != 2 || conflicts != 0 {
					t.Fatalf("replay outcomes: %d/%d", success, conflicts)
				}
			} else if success != 1 || conflicts != 1 {
				t.Fatalf("opposite outcomes did not fence: %d/%d", success, conflicts)
			}
			current, err := s.GetTask(ctx, submission.GroupID, submission.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			if winner.Outcome == domain.ReviewOutcomeAccepted {
				if current.Status != domain.TaskStatusAccepted || current.Revision != 1 || current.CurrentSubmissionID != submission.ID || current.ActiveAttemptID != "" {
					t.Fatalf("accepted race result: %+v", current)
				}
			} else if current.Status != domain.TaskStatusOpen || current.Revision != 2 || current.ActiveAttemptID != "" || current.CurrentSubmissionID != "" {
				t.Fatalf("rejected race result: %+v", current)
			}
			taskReviewCounts(t, s, 1, 1)
		})
	}
}
