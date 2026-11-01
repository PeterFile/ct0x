package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestTaskReviewHumanRejectRetryAcceptAndReopen(t *testing.T) {
	s, path, first, input := taskReviewSubmitted(t)
	create := taskInput(first.GroupID)
	create.DependencyIDs = []string{first.TaskID}
	dependent, err := s.CreateTask(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	dependencyBlocked := func() {
		t.Helper()
		if _, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(dependent)); !errors.Is(err, ErrConflict) {
			t.Fatalf("unaccepted dependency unlocked work: %v", err)
		}
	}
	dependencyBlocked()
	input.Outcome, input.Reason, input.IdempotencyKey = domain.ReviewOutcomeRejected, "需要补充依据。 ", "reject-1"
	rejected, err := s.ReviewTask(ctx, input)
	if err != nil || rejected.ID == "" || rejected.ID == first.ID || rejected.SubmissionID != first.ID || rejected.TaskID != first.TaskID || rejected.GroupID != first.GroupID || rejected.TaskRevision != 1 || rejected.Outcome != domain.ReviewOutcomeRejected || rejected.Reason != input.Reason || rejected.ReviewedBy != "user" || rejected.CreatedAt.IsZero() || rejected.CreatedAt.Location() != time.UTC || rejected.ArtifactVersions == nil || len(rejected.ArtifactVersions) != 0 {
		t.Fatalf("rejected decision: %+v, %v", rejected, err)
	}
	current, err := s.GetTask(ctx, first.GroupID, first.TaskID)
	if err != nil || current.Status != domain.TaskStatusOpen || current.Revision != 2 || current.ActiveAttemptID != "" || current.CurrentSubmissionID != "" {
		t.Fatalf("rejection did not fence generation: %+v, %v", current, err)
	}
	dependencyBlocked()
	attempt, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(current))
	if err != nil || attempt.ID == first.AttemptID || attempt.TaskRevision != 2 || attempt.TurnID != "" {
		t.Fatalf("revision two attempt: %+v, %v", attempt, err)
	}
	submit := taskSubmissionInput(attempt)
	submit.IdempotencyKey, submit.Summary = "submit-2", "补充后的交付文本。"
	second, err := s.SubmitTask(ctx, submit)
	if err != nil {
		t.Fatal(err)
	}
	dependencyBlocked()
	before, err := s.GetTask(ctx, first.GroupID, first.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	stale := input
	stale.IdempotencyKey = "stale"
	if _, err := s.ReviewTask(ctx, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision reviewed current work: %v", err)
	}
	stale.ExpectedRevision = 2
	if _, err := s.ReviewTask(ctx, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("old submission reviewed new generation: %v", err)
	}
	stale = taskReviewInput(second)
	stale.ExpectedRevision, stale.IdempotencyKey = 1, "stale"
	if _, err := s.ReviewTask(ctx, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong revision reviewed current submission: %v", err)
	}
	if replay, err := s.ReviewTask(ctx, input); err != nil || !reflect.DeepEqual(replay, rejected) {
		t.Fatalf("historical decision replay changed: %+v, %v", replay, err)
	}
	if after, err := s.GetTask(ctx, first.GroupID, first.TaskID); err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("stale review/replay touched new generation: %+v, %v", after, err)
	}
	accept := taskReviewInput(second)
	accept.IdempotencyKey, accept.Reason = "accept-2", "通过。"
	accepted, err := s.ReviewTask(ctx, accept)
	if err != nil || accepted.Outcome != domain.ReviewOutcomeAccepted || accepted.SubmissionID != second.ID || accepted.TaskRevision != 2 {
		t.Fatalf("explicit acceptance: %+v, %v", accepted, err)
	}
	if current, err := s.GetTask(ctx, second.GroupID, second.TaskID); err != nil || current.Status != domain.TaskStatusAccepted || current.Revision != 2 || current.ActiveAttemptID != "" || current.CurrentSubmissionID != second.ID {
		t.Fatalf("accepted pointers: %+v, %v", current, err)
	}
	if _, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(dependent)); err != nil {
		t.Fatalf("accepted dependency stayed blocked: %v", err)
	}
	duplicate := accept
	duplicate.IdempotencyKey = ""
	if _, err := s.ReviewTask(ctx, duplicate); !errors.Is(err, ErrConflict) {
		t.Fatalf("unkeyed duplicate decision accepted: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, want := range []domain.Submission{first, second} {
		got, err := reopened.GetTaskSubmission(ctx, want.GroupID, want.TaskID, want.ID)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("durable evidence changed: %+v, %v", got, err)
		}
		attempt, err := reopened.GetTaskAttempt(ctx, want.GroupID, want.TaskID, want.AttemptID)
		if err != nil || attempt.TurnID != "" {
			t.Fatalf("human history manufactured Turn: %+v, %v", attempt, err)
		}
	}
	for _, want := range []domain.ReviewDecision{rejected, accepted} {
		got, err := reopened.GetTaskReview(ctx, want.GroupID, want.TaskID, want.ID)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("durable review changed: %+v, %v", got, err)
		}
		data, err := json.Marshal(got)
		if err != nil || !strings.Contains(string(data), `"artifact_versions":[]`) {
			t.Fatalf("nil review refs: %s, %v", data, err)
		}
	}
	page, err := reopened.ListTaskReviews(ctx, first.GroupID, first.TaskID, ListOptions{})
	if err != nil || len(page.Items) != 2 || page.NextCursor != nil {
		t.Fatalf("review history: %+v, %v", page, err)
	}
	if replay, err := reopened.ReviewTask(ctx, input); err != nil || !reflect.DeepEqual(replay, rejected) {
		t.Fatalf("old generation replay after acceptance: %+v, %v", replay, err)
	}
	if current, err := reopened.GetTask(ctx, second.GroupID, second.TaskID); err != nil || current.Status != domain.TaskStatusAccepted || current.Revision != 2 || current.CurrentSubmissionID != second.ID {
		t.Fatalf("old receipt reopened accepted task: %+v, %v", current, err)
	}
	taskReviewCounts(t, reopened, 2, 2)
	queueCounts(t, reopened, 0, 0, 0)
}

func TestTaskReviewAcceptUnknownExecutionKeepsClaim(t *testing.T) {
	f, scope, task, _, submit := taskSubmissionAgent(t)
	submission, err := f.s.SubmitTask(ctx, submit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}); err != nil {
		t.Fatal(err)
	}
	before := queueTurn(t, f.s, scope.Claim)
	if before.State != domain.TurnStateUnknown {
		t.Fatalf("expected unresolved execution: %+v", before)
	}
	decision, err := f.s.ReviewTask(ctx, taskReviewInput(submission))
	if err != nil || decision.Outcome != domain.ReviewOutcomeAccepted {
		t.Fatalf("human cannot accept submitted evidence: %+v, %v", decision, err)
	}
	if current, err := f.s.GetTask(ctx, task.GroupID, task.ID); err != nil || current.Status != domain.TaskStatusAccepted {
		t.Fatalf("task was not explicitly accepted: %+v, %v", current, err)
	}
	if after := queueTurn(t, f.s, scope.Claim); !reflect.DeepEqual(after, before) {
		t.Fatal("acceptance reconciled or released unknown execution")
	}
	queueCounts(t, f.s, 1, 1, 1)
	var heldBy string
	if err := f.s.db.QueryRow("SELECT turn_id FROM resource_claims WHERE resource_key='shared'").Scan(&heldBy); err != nil || heldBy != scope.Claim.TurnID {
		t.Fatalf("unknown execution lost resource: %q, %v", heldBy, err)
	}
}
