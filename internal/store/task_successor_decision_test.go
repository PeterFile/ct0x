package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestTaskSuccessorReturnedInputCanBeAnsweredAndAcknowledged(t *testing.T) {
	withTaskSuccessorSchema(t)
	f := chatSetup(t, "user", false)
	s := f.s
	owner, admission := admitSuccessorForDecisionTest(t, f)
	s.EnableTaskWorkExecution()
	refusal := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}
	taskWorkTestReceipt(t, s, owner.Member, refusal, domain.TurnStateNotStarted)

	followup, delivery := taskWorkTestFollowup(t, f, "请把续办结论发布到外部目录。")
	returnOwner := taskWorkTestClaim(t, s, delivery)
	returnAttempt, err := s.BindTaskWorkContinuation(ctx, returnOwner)
	if err != nil || returnAttempt.TaskID != admission.Task.ID {
		t.Fatalf("the owner could not start the successor followup: %+v %v", returnAttempt, err)
	}
	const reason, returnKey = "原续办仅授权在原话题给出文本结论。", "return-successor-input"
	returned, err := s.HandBackWorkInputWithKey(ctx, returnOwner, followup.WorkInputID, reason, returnKey)
	if err != nil || returned.Return == nil || returned.Return.Disposition != domain.TaskWorkInputReturnSeparateAuthorizationRequired || returned.Consumption != nil {
		t.Fatalf("the successor owner could not return a pending decision: %+v %v", returned, err)
	}
	page, err := s.ListPendingTaskDecisions(ctx, ListOptions{})
	if err != nil || len(page.Items) != 1 || page.NextCursor != nil {
		t.Fatalf("the exact successor return was not listed as a pending decision: %+v %v", page, err)
	}
	q := page.Items[0]
	if !strings.HasPrefix(q.ID, workInputDecisionPrefix) || q.Task == nil || q.Task.WorkInputID != returned.ID ||
		q.Task.TaskID != admission.Task.ID || q.Task.MandateID != admission.Mandate.ID || q.Task.AssignmentID != owner.Provenance.AssignmentID ||
		q.Task.OwnerMemberID != owner.Member.MemberID || q.AuthorTurnID != returnOwner.Claim.TurnID || q.AuthorAttemptID != returnAttempt.ID ||
		q.Task.OriginTriggerID != returned.TriggerID || q.SourceMessageID != returned.ReplyMessageID || q.SourceSequence != returned.SourceSequence ||
		q.ThreadRef != f.candidate.ThreadRef || q.Kind != domain.DecisionRequestAuthorization || q.Facts != reason {
		t.Fatalf("the pending decision has the wrong successor or authority: %+v", q)
	}
	if again, err := s.HandBackWorkInputWithKey(ctx, returnOwner, returned.ID, reason, returnKey); err != nil || !reflect.DeepEqual(again, returned) {
		t.Fatalf("handback replay changed the durable receipt: %+v %v", again, err)
	}
	if replayed, err := s.ListPendingTaskDecisions(ctx, ListOptions{}); err != nil || !reflect.DeepEqual(replayed, page) {
		t.Fatalf("handback replay changed the pending decision: %+v %v", replayed, err)
	}
	const answerKey = "answer-successor-input"
	response := domain.DecisionAnswerInput{Version: q.Version, Text: "继续原文本工作，外部发布另行决定。"}
	foreign := domain.ThreadRef{GroupID: f.otherID, TopicID: q.TopicID}
	if got, err := s.AnswerDecisionRequest(ctx, "user", foreign, q.ID, response, answerKey); !errors.Is(err, ErrConflict) || got.ID != "" {
		t.Fatalf("a different group could answer the successor decision: %+v %v", got, err)
	}
	answer, err := s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, response, answerKey)
	if err != nil || answer.ID == "" || answer.RequestID != q.ID || answer.TaskID != admission.Task.ID || answer.WakeDeliveryID == "" || answer.Text != response.Text {
		t.Fatalf("the successor decision could not be answered: %+v %v", answer, err)
	}
	if page, err := s.ListPendingTaskDecisions(ctx, ListOptions{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("the answered successor decision remained pending: %+v %v", page, err)
	}
	taskWorkTestReceipt(t, s, returnOwner, refusal, domain.TurnStateNotStarted)
	wake, err := s.ResolveTaskWorkDelivery(ctx, f.groupID, answer.WakeDeliveryID)
	if err != nil || wake.Blocked != "" || wake.SourceKind != domain.TaskWorkSourceFollowup || wake.SourceID != answer.ID ||
		wake.TaskID != admission.Task.ID || wake.MandateID != admission.Mandate.ID || wake.MemberID != owner.Member.MemberID || wake.AssignmentID != owner.Provenance.AssignmentID {
		t.Fatalf("the answer was not routed to the successor owner: %+v %v", wake, err)
	}
	firstWake := taskWorkTestClaim(t, s, wake)
	taskWorkTestReceipt(t, s, firstWake, refusal, domain.TurnStateNotStarted)
	pending, err := s.ListPendingDeliveries(ctx, PendingOptions{Lane: "work"})
	if err != nil || len(pending.Items) != 1 || pending.Items[0].Delivery.ID == wake.DeliveryID {
		t.Fatalf("the refused answer did not retain one replacement: %+v %v", pending, err)
	}
	replacement, err := s.ResolveTaskWorkDelivery(ctx, f.groupID, pending.Items[0].Delivery.ID)
	wake.DeliveryID, wake.MessageID = pending.Items[0].Delivery.ID, pending.Items[0].Delivery.MessageID
	if err != nil || replacement != wake {
		t.Fatalf("the replacement changed the logical answer: %+v %v", replacement, err)
	}
	answerOwner := taskWorkTestClaim(t, s, replacement)
	answerAttempt, err := s.BindTaskWorkContinuation(ctx, answerOwner)
	if err != nil || answerAttempt.TaskID != admission.Task.ID || answerAttempt.ID == returnAttempt.ID {
		t.Fatalf("the answer did not bind a new successor Attempt: %+v %v", answerAttempt, err)
	}
	ack, err := s.AcknowledgeWorkInputWithKey(ctx, answerOwner, answer.ID, "ack-successor-answer")
	if err != nil || ack.Consumption == nil || ack.Consumption.TurnID != answerOwner.Claim.TurnID || ack.Consumption.AttemptID != answerAttempt.ID ||
		ack.ReplyMessageID != answer.ReplyMessageID || !strings.HasSuffix(ack.Content, response.Text) || ack.AfterConstraintRevision != 0 {
		t.Fatalf("the successor owner could not acknowledge the exact answer: %+v %v", ack, err)
	}
	assertDecisionAnswerView(t, s, answer, ack.Consumption)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, f.path, false)
	reopened.EnableTaskWorkExecution()
	if again, err := reopened.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, response, answerKey); err != nil || again != answer {
		t.Fatalf("the answer receipt changed on reopen: %+v %v", again, err)
	}
	if again, err := reopened.AcknowledgeWorkInputWithKey(ctx, answerOwner, answer.ID, "ack-successor-answer"); err != nil || !reflect.DeepEqual(again, ack) {
		t.Fatalf("the ACK receipt changed on reopen: %+v %v", again, err)
	}
	assertDecisionAnswerView(t, reopened, answer, ack.Consumption)
	taskWorkTestReceipt(t, reopened, answerOwner, refusal, domain.TurnStateNotStarted)
	dispatchAssertFK(t, reopened)
}

func admitSuccessorForDecisionTest(t *testing.T, f chatFixture) (CoordinationToolScope, domain.AdmissionReceipt) {
	t.Helper()
	s, thread := f.s, f.candidate.ThreadRef
	refusal := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}
	taskWorkTestReceipt(t, s, f.scope, refusal, domain.TurnStateNotStarted)
	message := func(content string) domain.Message {
		t.Helper()
		return chatLocalMessage(t, s, MessageRequest{GroupID: f.groupID, TopicID: thread.TopicID, ActorID: "user", Content: content, RecipientIDs: []string{}}, nil)
	}
	old := createTestTask(t, s, f.groupID)
	priorWork := requestWorkLink(domain.RequestWorkRequest{TopicID: thread.TopicID, MessageID: message("授权文本原任务。").ID, Reason: "完成旧成果"}, old)
	if _, err := s.RequestWork(ctx, f.groupID, priorWork, "original-for-successor-decision"); err != nil {
		t.Fatal(err)
	}
	priorAttempt, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(old))
	if err != nil {
		t.Fatal(err)
	}
	priorSubmission, err := s.SubmitTask(ctx, taskSubmissionInput(priorAttempt))
	if err != nil {
		t.Fatal(err)
	}
	review, err := s.ReviewTask(ctx, impactReviewInput(priorSubmission))
	if err != nil {
		t.Fatal(err)
	}
	admission, err := s.RequestWork(ctx, f.groupID, domain.RequestWorkRequest{
		TopicID: thread.TopicID, MessageID: message("授权续办旧成果，并在原话题提供文本结论。").ID,
		Mode: domain.RoutingDecisionCreateNew, Reason: "明确授权文本续办",
		NewTask:     &domain.NewTaskRequest{Title: "续办旧成果", Acceptance: "在原话题交付文本结论"},
		Predecessor: &domain.TaskSuccessorInput{TaskID: old.ID, ExpectedRevision: old.Revision, SubmissionID: priorSubmission.ID, ReviewID: review.ID},
	}, "successor-for-decision")
	if err != nil || admission.DispatchRequestID == "" {
		t.Fatalf("the exact accepted result could not be continued: %+v %v", admission, err)
	}
	dispatch, err := s.GetDispatchRequest(ctx, f.groupID, admission.DispatchRequestID)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := coordinationDecideBindRequest(t, s, dispatch.Request)
	fixture := requestDispatchFixture{s: s, worker: f.b}
	decision := coordinationDecideAssign(t, fixture, coordinator, "assign-successor-for-decision")
	taskWorkTestReceipt(t, s, coordinator.Member, refusal, domain.TurnStateNotStarted)
	return coordinationDecideWorker(t, fixture, dispatch.Request, decision, true), admission
}
