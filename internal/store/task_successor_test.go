package store

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func withTaskSuccessorSchema(t *testing.T) {
	t.Helper()
	if slices.Contains(migrations, taskSuccessorSchema) {
		return
	}
	original := migrations
	t.Cleanup(func() { migrations = original })
	migrations = append(slices.Clone(original), taskSuccessorSchema)
}

func TestTaskSuccessorAuthorizesOnlyTheExactAcceptedResult(t *testing.T) {
	withTaskSuccessorSchema(t)
	f := chatSetup(t, "user", false)
	s, thread := f.s, f.candidate.ThreadRef
	source := func(text string) domain.Message {
		t.Helper()
		return chatLocalMessage(t, s, MessageRequest{GroupID: f.groupID, TopicID: thread.TopicID,
			ActorID: "user", Content: text, RecipientIDs: []string{}}, nil)
	}
	old := createTestTask(t, s, f.groupID)
	origin := source("Authorize the original result")
	input := requestWorkLink(domain.RequestWorkRequest{TopicID: thread.TopicID, MessageID: origin.ID, Reason: "Deliver v1"}, old)
	if _, err := s.RequestWork(ctx, f.groupID, input, "authorize-original"); err != nil {
		t.Fatal(err)
	}
	attempt, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(old))
	if err != nil {
		t.Fatal(err)
	}
	submission, err := s.SubmitTask(ctx, taskSubmissionInput(attempt))
	if err != nil {
		t.Fatal(err)
	}
	review, err := s.ReviewTask(ctx, impactReviewInput(submission))
	if err != nil {
		t.Fatal(err)
	}
	message := source("Authorize further work on the old result")
	prior := domain.TaskSuccessorInput{TaskID: old.ID, ExpectedRevision: old.Revision, SubmissionID: submission.ID, ReviewID: review.ID}
	request := domain.RequestWorkRequest{
		TopicID: thread.TopicID, MessageID: message.ID, Reason: "Continue the accepted result", Mode: domain.RoutingDecisionCreateNew,
		NewTask: &domain.NewTaskRequest{Title: "Continue work", Acceptance: "Confirm current compatibility"}, Predecessor: &prior,
	}

	for _, tc := range []struct {
		name   string
		change func(*domain.TaskSuccessorInput)
		want   error
	}{
		{"wrong_generation", func(p *domain.TaskSuccessorInput) { p.ExpectedRevision++ }, ErrConflict},
		{"wrong_submission", func(p *domain.TaskSuccessorInput) { p.SubmissionID = "different-result" }, ErrConflict},
		{"wrong_review", func(p *domain.TaskSuccessorInput) { p.ReviewID = "different-review" }, ErrNotFound},
		{"outside_group", func(p *domain.TaskSuccessorInput) { p.TaskID = createTestTask(t, s, f.otherID).ID }, ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := prior
			tc.change(&changed)
			invalid := request
			invalid.Predecessor = &changed
			if _, err := s.RequestWork(ctx, f.groupID, invalid, "authorize-successor"); !errors.Is(err, tc.want) {
				t.Fatalf("admit invalid predecessor: %v", err)
			}
		})
	}
	first, err := s.RequestWork(ctx, f.groupID, request, "authorize-successor")
	if err != nil || first.Predecessor == nil || *first.Predecessor != prior || first.DispatchRequestID == "" {
		t.Fatalf("admit after invalid requests rolled back: %+v %v", first, err)
	}
	var path string
	if err := s.db.QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	if replay, err := reopened.RequestWork(ctx, f.groupID, request, "authorize-successor"); err != nil || !reflect.DeepEqual(replay, first) {
		t.Fatalf("durable successor replay: %+v %v", replay, err)
	}
	changed := prior
	changed.ReviewID = "different-review"
	request.Predecessor = &changed
	if _, err := reopened.RequestWork(ctx, f.groupID, request, "authorize-successor"); !errors.Is(err, ErrConflict) {
		t.Fatalf("idempotency accepted a different predecessor: %v", err)
	}
	if before, err := reopened.GetAdmissionReceipt(ctx, thread, first.Decision.ID); err != nil || !reflect.DeepEqual(before, first) {
		t.Fatalf("read successor receipt: %+v %v", before, err)
	}
}

func TestTaskSuccessorPredecessorUnknownBlocksUntilReconciled(t *testing.T) {
	withTaskSuccessorSchema(t)
	f, owner, submission := chatRepairFixSubmitted(t, "请先交付 v1", "旧结论 v1")
	s, thread := f.s, f.candidate.ThreadRef
	coordinator := taskWorkTestCoordinator(t, s, f.groupID, owner.Provenance.AssignmentID)
	taskWorkTestReceipt(t, s, coordinator, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	review, err := s.ReviewTask(ctx, impactReviewInput(submission))
	if err != nil {
		t.Fatal(err)
	}
	message := chatLocalMessage(t, s, MessageRequest{GroupID: f.groupID, TopicID: thread.TopicID,
		ActorID: "user", Content: "授权接纳后续办", RecipientIDs: []string{}}, nil)
	request := domain.RequestWorkRequest{
		TopicID: thread.TopicID, MessageID: message.ID, Mode: domain.RoutingDecisionCreateNew, Reason: "独立授权新任务",
		NewTask: &domain.NewTaskRequest{Title: "续办", Acceptance: "核对兼容性"},
		Predecessor: &domain.TaskSuccessorInput{TaskID: submission.TaskID, ExpectedRevision: submission.TaskRevision,
			SubmissionID: submission.ID, ReviewID: review.ID},
	}
	queueEvent(t, s, owner.Member.Claim, provider.AcceptedEvent, "", "")
	unknown := queueReceipt(provider.OutcomeUnknown)
	unknown.DirectChildReaped = false
	taskWorkTestReceipt(t, s, owner.Member, unknown, domain.TurnStateUnknown)
	if _, err := s.RequestWork(ctx, f.groupID, request, "reconcile-old-turn"); !errors.Is(err, ErrChatRepairPending) || !errors.Is(err, ErrConflict) {
		t.Fatalf("submitted and accepted result released unresolved predecessor: %v", err)
	}
	taskWorkTestReceipt(t, s, owner.Member, queueReceipt(provider.OutcomeSucceeded), domain.TurnStateSucceeded)
	if got, err := s.RequestWork(ctx, f.groupID, request, "reconcile-old-turn"); err != nil || got.DispatchRequestID == "" {
		t.Fatalf("admit successor after the predecessor reconciled: %+v %v", got, err)
	}
}
