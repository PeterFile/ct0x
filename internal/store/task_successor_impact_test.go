package store

import (
	"errors"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestTaskSuccessorConstraintImpactsFollowExactAcceptedArtifact(t *testing.T) {
	withTaskSuccessorSchema(t)
	f := chatSetup(t, "user", false)
	s := f.s
	thread := f.candidate.ThreadRef
	taskWorkTestReceipt(t, s, f.scope, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	dependentWorker := member(t, s, f.groupID)
	if _, err := s.SetDispatchPolicy(ctx, f.groupID, []string{f.b, dependentWorker.ID}, 1, "successor-dispatch-policy"); err != nil {
		t.Fatal(err)
	}

	old := createTestTask(t, s, f.groupID)
	oldSource := chatLocalMessage(t, s, MessageRequest{GroupID: f.groupID, TopicID: thread.TopicID, ActorID: "user", Content: "授权原交付。", RecipientIDs: []string{}}, nil)
	oldWork := requestWorkLink(domain.RequestWorkRequest{TopicID: thread.TopicID, MessageID: oldSource.ID, Reason: "交付旧格式成果"}, old)
	if _, err := s.RequestWork(ctx, f.groupID, oldWork, "original-work"); err != nil {
		t.Fatal(err)
	}
	publish := artifactPublishInput(f.groupID)
	publish.SourceTaskID = old.ID
	v1 := mustPublishArtifact(t, s, publish)
	oldSubmission := artifactDependencySubmit(t, s, old, v1)
	oldReview, err := s.ReviewTask(ctx, impactReviewInput(oldSubmission))
	if err != nil || oldReview.Outcome != domain.ReviewOutcomeAccepted {
		t.Fatalf("accept original result: %+v %v", oldReview, err)
	}
	accepted, err := s.GetTask(ctx, f.groupID, old.ID)
	if err != nil || accepted.Status != domain.TaskStatusAccepted || accepted.CurrentSubmissionID != oldSubmission.ID {
		t.Fatalf("original task was not accepted: %+v %v", accepted, err)
	}
	pinInput := artifactDependencyCreateInput(oldSubmission)
	downstream, err := s.CreateTask(ctx, pinInput)
	if err != nil {
		t.Fatal(err)
	}
	dependent := impactPinnedOwner(t, f, downstream, dependentWorker.ID, "successor-dependent-topic")

	other, otherVersion, otherSubmission := successorIndependentVersion(t, s, f.groupID, v1)
	otherPin := artifactDependencyCreateInput(otherSubmission)
	independent, err := s.CreateTask(ctx, otherPin)
	if err != nil {
		t.Fatal(err)
	}
	independentAttempt, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(independent))
	if err != nil {
		t.Fatal(err)
	}
	source := chatLocalMessage(t, s, MessageRequest{GroupID: f.groupID, TopicID: thread.TopicID, ActorID: "user", Content: "授权在原交付基础上另开文本续办。", RecipientIDs: []string{}}, nil)
	request := domain.RequestWorkRequest{
		TopicID: thread.TopicID, MessageID: source.ID, Mode: domain.RoutingDecisionCreateNew, Reason: "明确授权后继工作",
		NewTask:     &domain.NewTaskRequest{Title: "续办旧格式兼容", Acceptance: "交付兼容性结论"},
		Predecessor: &domain.TaskSuccessorInput{TaskID: old.ID, ExpectedRevision: old.Revision, SubmissionID: oldSubmission.ID, ReviewID: oldReview.ID},
	}
	admission, err := s.RequestWork(ctx, f.groupID, request, "successor-work")
	if err != nil || admission.Task.ID == "" || admission.Task.ID == old.ID || admission.DispatchRequestID == "" || admission.Predecessor == nil || *admission.Predecessor != *request.Predecessor {
		t.Fatalf("new successor admission: %+v %v", admission, err)
	}
	dispatch, err := s.GetDispatchRequest(ctx, f.groupID, admission.DispatchRequestID)
	if err != nil || dispatch.Request.TaskID != admission.Task.ID || dispatch.Request.MandateID != admission.Mandate.ID {
		t.Fatalf("successor dispatch provenance: %+v %v", dispatch, err)
	}
	coordinator := coordinationDecideBindRequest(t, s, dispatch.Request)
	decision, err := s.DecideDispatch(ctx, coordinator, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.b, Reason: "执行明确授权的续办"}, "assign-successor")
	if err != nil || decision.Assignment == nil {
		t.Fatalf("assign successor: %+v %v", decision, err)
	}
	taskWorkTestReceipt(t, s, coordinator.Member, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	owner := coordinationDecideWorker(t, requestDispatchFixture{s: s, worker: f.b}, dispatch.Request, decision, true)
	queueEvent(t, s, owner.Member.Claim, provider.AcceptedEvent, "", "")
	s.EnableTaskWorkExecution()

	const constraint = "保持旧格式兼容"
	input, candidate := impactInput(t, f, constraint, constraint)
	if input.TaskID != admission.Task.ID || input.WorkDeliveryID == "" || len(impacts(t, s, f.groupID, downstream.ID)) != 0 {
		t.Fatalf("new input did not remain pending on its successor: %+v", input)
	}
	updated, err := s.AcknowledgeWorkInputWithKey(ctx, owner.Member, input.WorkInputID, "ack-successor-input")
	if err != nil || updated.Consumption == nil || updated.Consumption.TurnID != owner.Member.Claim.TurnID || updated.Consumption.AttemptID != owner.AttemptID {
		t.Fatalf("successor owner ACK: %+v %v", updated, err)
	}
	root := impacts(t, s, f.groupID, admission.Task.ID)
	confirmed := requireImpact(t, root, domain.TaskImpactConstraintConfirmed, "")
	if confirmed.Plan == nil || confirmed.Plan.AfterConstraint != constraint || confirmed.Plan.AfterConstraintRevision != 1 ||
		confirmed.Source.MessageID != candidate.MessageID || confirmed.Acknowledgement == nil || *confirmed.Acknowledgement != *updated.Consumption {
		t.Fatalf("incorrect confirmed constraint: %+v", confirmed)
	}
	stale := requireImpact(t, root, domain.TaskImpactStaleSubmission, oldSubmission.ID)
	if stale.Submission == nil || stale.Submission.TaskID != old.ID || stale.Submission.TaskRevision != old.Revision {
		t.Fatalf("wrong predecessor submission: %+v", stale)
	}
	dependency := impacts(t, s, f.groupID, downstream.ID)
	pin := requireImpact(t, dependency, domain.TaskImpactStalePin, old.ID)
	if pin.Pin == nil || pin.Pin.SubmissionID != oldSubmission.ID || pin.Pin.ReviewID != oldReview.ID || pin.Pin.ArtifactID != v1.ArtifactID || pin.Pin.VersionID != v1.ID {
		t.Fatalf("wrong downstream artifact pin: %+v", pin)
	}
	cancel := requireImpact(t, dependency, domain.TaskImpactCancelRequested, dependent.Member.Claim.TurnID)
	if cancel.Cancellation == nil || cancel.Cancellation.State != domain.TurnStateCancelRequested {
		t.Fatalf("dependent cancellation not requested: %+v", cancel)
	}
	if held, err := s.HasResourceClaim(ctx, downstream.ID); err != nil || !held {
		t.Fatalf("dependent claim released before a terminal receipt: %v %v", held, err)
	}
	unknown := queueReceipt(provider.OutcomeUnknown)
	unknown.DirectChildReaped = false
	taskWorkTestReceipt(t, s, dependent.Member, unknown, domain.TurnStateUnknown)
	if held, err := s.HasResourceClaim(ctx, downstream.ID); err != nil || !held {
		t.Fatalf("unknown dependent receipt released its claim: %v %v", held, err)
	}
	taskWorkTestReceipt(t, s, dependent.Member, queueReceipt(provider.OutcomeAborted), domain.TurnStateAborted)
	if held, err := s.HasResourceClaim(ctx, downstream.ID); err != nil || held {
		t.Fatalf("dependent claim retained after terminal receipt: %v %v", held, err)
	}
	if got, err := s.GetTask(ctx, f.groupID, old.ID); err != nil || !reflect.DeepEqual(got, accepted) {
		t.Fatalf("accepted task changed: %+v %v", got, err)
	}
	if got, err := s.GetTaskSubmission(ctx, f.groupID, old.ID, oldSubmission.ID); err != nil || !reflect.DeepEqual(got, oldSubmission) {
		t.Fatalf("original submission changed: %+v %v", got, err)
	}
	if got, err := s.GetTaskReview(ctx, f.groupID, old.ID, oldReview.ID); err != nil || !reflect.DeepEqual(got, oldReview) {
		t.Fatalf("original review changed: %+v %v", got, err)
	}
	if got, err := s.GetArtifactVersion(ctx, f.groupID, v1.ArtifactID, v1.ID); err != nil || got != v1 {
		t.Fatalf("original artifact version changed: %+v %v", got, err)
	}
	if pins, err := s.ListTaskArtifactDependencies(ctx, f.groupID, downstream.ID, ListOptions{}); err != nil || !reflect.DeepEqual(pins.Items, pinInput.ArtifactDependencies) {
		t.Fatalf("original dependency pin changed: %+v %v", pins, err)
	}
	if _, err := s.CreateTask(ctx, pinInput); !errors.Is(err, ErrConflict) {
		t.Fatalf("new work could pin the stale submission: %v", err)
	}
	if _, err := s.CreateTask(ctx, otherPin); err != nil {
		t.Fatalf("unrelated pin to a different version of the same artifact was blocked: %v", err)
	}
	otherResult := taskSubmissionInput(independentAttempt)
	otherResult.ArtifactVersions = []domain.ArtifactVersionRef{{ArtifactID: otherVersion.ArtifactID, VersionID: otherVersion.ID}}
	finished, err := s.SubmitTask(ctx, otherResult)
	if err != nil {
		t.Fatalf("independent work could not submit its result: %v", err)
	}
	if _, err := s.ReviewTask(ctx, impactReviewInput(finished)); err != nil || len(impacts(t, s, f.groupID, other.ID)) != 0 || len(impacts(t, s, f.groupID, independent.ID)) != 0 {
		t.Fatalf("independent work did not finish untouched: %v", err)
	}
	taskWorkTestReceipt(t, s, owner.Member, queueReceipt(provider.OutcomeSucceeded), domain.TurnStateSucceeded)
	dispatchAssertFK(t, s)
}

func successorIndependentVersion(t *testing.T, s *Store, groupID string, v1 domain.ArtifactVersion) (domain.Task, domain.ArtifactVersion, domain.Submission) {
	t.Helper()
	other := createTestTask(t, s, groupID)
	publish := artifactPublishInput(groupID)
	publish.SourceTaskID = other.ID
	publish.ArtifactID = v1.ArtifactID
	publish.ExpectedVersionID = v1.ID
	publish.Name = ""
	v2 := mustPublishArtifact(t, s, publish)
	submission := artifactDependencySubmit(t, s, other, v2)
	if _, err := s.ReviewTask(ctx, impactReviewInput(submission)); err != nil {
		t.Fatal(err)
	}
	return other, v2, submission
}
