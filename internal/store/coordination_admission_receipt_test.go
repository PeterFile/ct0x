package store

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
)

func TestRequestWorkImmutableReplayAfterTaskLifecycle(t *testing.T) {
	for _, mode := range []domain.RoutingDecisionKind{domain.RoutingDecisionCreateNew, domain.RoutingDecisionLinkExisting} {
		for _, outcome := range []domain.ReviewOutcome{domain.ReviewOutcomeAccepted, domain.ReviewOutcomeRejected} {
			t.Run(string(mode)+"/"+string(outcome), func(t *testing.T) {
				s, path, thread, request := requestWorkFixture(t)
				if mode == domain.RoutingDecisionLinkExisting {
					task := createTestTask(t, s, thread.GroupID)
					// An uncoordinated Task can legitimately be open at revision > 1.
					attempt, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(task))
					if err != nil {
						t.Fatal(err)
					}
					submission, err := s.SubmitTask(ctx, taskSubmissionInput(attempt))
					if err != nil {
						t.Fatal(err)
					}
					review := taskReviewInput(submission)
					review.Outcome, review.IdempotencyKey = domain.ReviewOutcomeRejected, "before-admission"
					if _, err := s.ReviewTask(ctx, review); err != nil {
						t.Fatal(err)
					}
					task, err = s.GetTask(ctx, task.GroupID, task.ID)
					if err != nil || task.Revision != 2 {
						t.Fatalf("revision two fixture: %+v, %v", task, err)
					}
					request = requestWorkLink(request, task)
				}
				original, err := s.RequestWork(ctx, thread.GroupID, request, "history")
				if err != nil {
					t.Fatal(err)
				}
				originalJSON, err := json.Marshal(original)
				if err != nil {
					t.Fatal(err)
				}
				check := func(s *Store) {
					t.Helper()
					before := messageSequenceSnapshot(t, s)
					got, err := s.RequestWork(ctx, thread.GroupID, request, "history")
					if err != nil || !reflect.DeepEqual(got, original) {
						t.Fatalf("live Task contaminated historical receipt: %+v, %v", got, err)
					}
					encoded, err := json.Marshal(got)
					if err != nil || !bytes.Equal(encoded, originalJSON) {
						t.Fatalf("historical receipt bytes changed: %s, %v", encoded, err)
					}
					messageSequencePreserved(t, s, before)
				}
				task, err := s.GetTask(ctx, thread.GroupID, original.Task.ID)
				if err != nil {
					t.Fatal(err)
				}
				attempt, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(task))
				if err != nil {
					t.Fatal(err)
				}
				check(s) // Running Tasks are ineligible for a fresh link, but replay is historical.
				submit := taskSubmissionInput(attempt)
				submit.IdempotencyKey = "after-admission"
				submission, err := s.SubmitTask(ctx, submit)
				if err != nil {
					t.Fatal(err)
				}
				check(s)
				review := taskReviewInput(submission)
				review.Outcome, review.IdempotencyKey = outcome, "after-admission"
				if _, err := s.ReviewTask(ctx, review); err != nil {
					t.Fatal(err)
				}
				check(s)
				// Even mutable descriptive fields must not be substituted into a historical response.
				if _, err := s.db.Exec("UPDATE tasks SET title='当前标题',description='当前描述',acceptance='当前验收' WHERE id=?", task.ID); err != nil {
					t.Fatal(err)
				}
				check(s)
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				reopened := queueHandle(t, path, false)
				check(reopened)
				if violations := messageSequenceRows(t, reopened.db, "PRAGMA foreign_key_check"); len(violations) != 0 {
					t.Fatalf("historical revision FK violation: %v", violations)
				}
			})
		}
	}
}
