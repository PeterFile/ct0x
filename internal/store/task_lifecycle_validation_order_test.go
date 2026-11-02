package store

import (
	"errors"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
)

func TestTaskLifecycleValidationAttemptRevision(t *testing.T) {
	for _, tc := range []struct {
		name        string
		revision    int64
		missingTask bool
	}{
		{name: "zero", revision: 0},
		{name: "negative-before-task-lookup", revision: -1, missingTask: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTestStore(t)
			g := group(t, s)
			task := createTestTask(t, s, g.ID)
			input := taskAttemptInput(task)
			input.IdempotencyKey = "retry"
			bad := input
			bad.ExpectedRevision = tc.revision
			if tc.missingTask {
				bad.TaskID = "missing"
			}
			if got, err := s.ActivateTaskAttempt(ctx, bad); !errors.Is(err, ErrInvalid) || got.ID != "" {
				t.Fatalf("fresh invalid revision: %+v, %v; want ErrInvalid", got, err)
			}
			taskAttemptCounts(t, s, 0, 0)
			if current, err := s.GetTask(ctx, task.GroupID, task.ID); err != nil || !reflect.DeepEqual(current, task) {
				t.Fatalf("invalid activation changed task: %+v, %v", current, err)
			}
			if _, err := s.ActivateTaskAttempt(ctx, input); err != nil {
				t.Fatalf("corrected request could not reuse key: %v", err)
			}
			if _, err := s.ActivateTaskAttempt(ctx, bad); !errors.Is(err, ErrConflict) {
				t.Fatalf("used key must conflict before validation: %v", err)
			}
			taskAttemptCounts(t, s, 1, 1)
		})
	}
}

func TestTaskLifecycleValidationSubmissionRevision(t *testing.T) {
	for _, tc := range []struct {
		name        string
		revision    int64
		missingTask bool
	}{
		{name: "zero", revision: 0},
		{name: "negative-before-task-lookup", revision: -1, missingTask: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, task, _, input := taskSubmissionHuman(t)
			before, err := s.GetTask(ctx, task.GroupID, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			bad := input
			bad.ExpectedRevision = tc.revision
			if tc.missingTask {
				bad.TaskID = "missing"
			}
			if got, err := s.SubmitTask(ctx, bad); !errors.Is(err, ErrInvalid) || got.ID != "" {
				t.Fatalf("fresh invalid revision: %+v, %v; want ErrInvalid", got, err)
			}
			taskSubmissionCounts(t, s, 0, 0)
			if current, err := s.GetTask(ctx, task.GroupID, task.ID); err != nil || !reflect.DeepEqual(current, before) {
				t.Fatalf("invalid submission changed task: %+v, %v", current, err)
			}
			if _, err := s.SubmitTask(ctx, input); err != nil {
				t.Fatalf("corrected request could not reuse key: %v", err)
			}
			if _, err := s.SubmitTask(ctx, bad); !errors.Is(err, ErrConflict) {
				t.Fatalf("used key must conflict before validation: %v", err)
			}
			taskSubmissionCounts(t, s, 1, 1)
		})
	}
}

func TestTaskLifecycleValidationReviewSemantics(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ReviewTaskInput)
	}{
		{name: "zero-revision", change: func(i *ReviewTaskInput) { i.ExpectedRevision = 0 }},
		{name: "negative-before-task-lookup", change: func(i *ReviewTaskInput) {
			i.ExpectedRevision, i.TaskID = -1, "missing"
		}},
		{name: "invalid-outcome-before-task-lookup", change: func(i *ReviewTaskInput) {
			i.Outcome, i.TaskID = "invalid", "missing"
		}},
		{name: "non-exact-outcome-before-submission-lookup", change: func(i *ReviewTaskInput) {
			i.Outcome, i.SubmissionID = "accepted ", "missing"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, submission, input := taskReviewSubmitted(t)
			before, err := s.GetTask(ctx, submission.GroupID, submission.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			bad := input
			tc.change(&bad)
			if got, err := s.ReviewTask(ctx, bad); !errors.Is(err, ErrInvalid) || !errors.Is(err, domain.ErrTaskInvalid) || got.ID != "" {
				t.Fatalf("fresh invalid review: %+v, %v; want ErrInvalid and ErrTaskInvalid", got, err)
			}
			taskReviewCounts(t, s, 0, 0)
			if current, err := s.GetTask(ctx, submission.GroupID, submission.TaskID); err != nil || !reflect.DeepEqual(current, before) {
				t.Fatalf("invalid review changed task: %+v, %v", current, err)
			}
			if _, err := s.ReviewTask(ctx, input); err != nil {
				t.Fatalf("corrected request could not reuse key: %v", err)
			}
			if _, err := s.ReviewTask(ctx, bad); !errors.Is(err, ErrConflict) {
				t.Fatalf("used key must conflict before validation: %v", err)
			}
			taskReviewCounts(t, s, 1, 1)
		})
	}
}

func TestTaskLifecycleValidationHistoricalReplayAfterAdvanceAndReopen(t *testing.T) {
	s, path := openTestStore(t)
	g := group(t, s)
	task := createTestTask(t, s, g.ID)
	activate := taskAttemptInput(task)
	activate.IdempotencyKey = "activate"
	attempt, err := s.ActivateTaskAttempt(ctx, activate)
	if err != nil {
		t.Fatal(err)
	}
	submit := taskSubmissionInput(attempt)
	submission, err := s.SubmitTask(ctx, submit)
	if err != nil {
		t.Fatal(err)
	}
	review := taskReviewInput(submission)
	review.Outcome = domain.ReviewOutcomeRejected
	decision, err := s.ReviewTask(ctx, review)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.GetTask(ctx, task.GroupID, task.ID)
	if err != nil || before.Status != domain.TaskStatusOpen || before.Revision != 2 || before.ActiveAttemptID != "" || before.CurrentSubmissionID != "" {
		t.Fatalf("rejection did not advance task: %+v, %v", before, err)
	}
	check := func(s *Store) {
		t.Helper()
		// A fresh invalid outcome must precede even a stale generation's state fence.
		bad := review
		bad.IdempotencyKey, bad.Outcome = "invalid-after-advance", "invalid"
		if got, err := s.ReviewTask(ctx, bad); !errors.Is(err, ErrInvalid) || got.ID != "" {
			t.Fatalf("fresh invalid outcome after advance: %+v, %v; want ErrInvalid", got, err)
		}
		if got, err := s.ActivateTaskAttempt(ctx, activate); err != nil || !reflect.DeepEqual(got, attempt) {
			t.Fatalf("historical attempt replay changed: %+v, %v", got, err)
		}
		if got, err := s.SubmitTask(ctx, submit); err != nil || !reflect.DeepEqual(got, submission) {
			t.Fatalf("historical submission replay changed: %+v, %v", got, err)
		}
		if got, err := s.ReviewTask(ctx, review); err != nil || !reflect.DeepEqual(got, decision) {
			t.Fatalf("historical review replay changed: %+v, %v", got, err)
		}
		if current, err := s.GetTask(ctx, task.GroupID, task.ID); err != nil || !reflect.DeepEqual(current, before) {
			t.Fatalf("invalid request or historical replay changed task: %+v, %v", current, err)
		}
		taskAttemptCounts(t, s, 1, 1)
		taskSubmissionCounts(t, s, 1, 1)
		taskReviewCounts(t, s, 1, 1)
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
}
