package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestB3LocalUserTaskWorkReports(t *testing.T) {
	a := openTaskLifecycleAPI(t, "")
	g := newGroup(t, a, "reports")
	other := newGroup(t, a, "foreign")
	task, path := newTaskLifecycleTask(t, a, g.ID)
	path += "/work-reports"
	page := decoded[struct {
		Items []domain.TaskWorkReport `json:"items"`
		Next  string                  `json:"next_cursor"`
	}](t, a.request("GET", path+"?limit=1", ""), 200)
	if page.Items == nil || len(page.Items) != 0 || page.Next != "" {
		t.Fatalf("ordinary task has no reports: %+v", page)
	}
	taskLifecycleError(t, a.request("GET", "/api/v1/groups/"+other.ID+"/tasks/"+task.ID+"/work-reports", ""), 404, "not_found")
	taskLifecycleError(t, a.request("GET", path+"?member_id=unbound", ""), 400, "invalid_input")
	w := a.request("POST", path, `{}`)
	if w.Code != 405 || !strings.Contains(w.Header().Get("Allow"), "GET") {
		t.Fatalf("task detail exposed a mutation: %d %s", w.Code, w.Body)
	}
}

func TestB3WorkReportsEngineAndHTTP(t *testing.T) {
	f := chatHTTPSetup(t)
	ctx, db := context.Background(), f.db
	engine, local := coordination.New(db), platform.NewLocalUser(db)
	routes := append(TaskRoutes(local), TaskLifecycleRoutes(local)...)
	a := groupAPI{db: db, handler: mustServer(t, DefaultAddress, routes...).Handler}
	claim := func(deliveryID string, keys ...string) store.MemberToolScope {
		t.Helper()
		turn, err := db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.groupID, DeliveryID: deliveryID, MaxActive: 4, ResourceKeys: keys})
		readinessHTTPOK(t, err)
		scope, err := db.BindMemberTool(ctx, store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
		readinessHTTPOK(t, err)
		return scope
	}
	settle := func(c store.TurnClaim, receipt provider.ExecutionReceipt, want domain.TurnState) {
		t.Helper()
		if turn, err := db.ApplyTurnReceipt(ctx, c, receipt); err != nil || turn.State != want {
			t.Fatalf("settlement: %+v %v; want %s", turn, err, want)
		}
	}
	refusal := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}
	settle(f.claim, refusal, domain.TurnStateNotStarted)
	owner, err := db.CreateMember(ctx, store.CreateMemberInput{GroupID: f.groupID, Name: "调查负责人", Role: "research", ProviderID: "fixture", Model: "fixture-model", ActorID: "user"})
	readinessHTTPOK(t, err)
	_, err = db.SetDispatchPolicy(ctx, f.groupID, []string{owner.ID, f.peer.ID}, 1, "b3-eligible")
	readinessHTTPOK(t, err)
	thread := domain.ThreadRef{GroupID: f.groupID, TopicID: "trusted-topic"}
	_, err = engine.SetChatPolicy(ctx, thread, domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 1,
		Investigation: &domain.ChatInvestigationPolicy{Objective: "核对未回应的问题", AllowTopicMessages: true, MaxDelegations: 1}}, 1, "b3-policy")
	readinessHTTPOK(t, err)
	engine.EnableInvestigationExecution()
	engine.EnableTaskWorkExecution()
	msg, err := local.CreateMessage(ctx, f.groupID, platform.MessageInput{TopicID: thread.TopicID, Content: "请调查未回应的问题。", RecipientIDs: []string{f.caller.ID}}, "b3-source")
	readinessHTTPOK(t, err)
	intake := claim(msg.Deliveries[0].ID)
	candidate, err := engine.CaptureChatCandidate(ctx, intake)
	readinessHTTPOK(t, err)
	admission, err := engine.AssessChatIntent(ctx, intake, candidate.ID, domain.ChatAssessment{Disposition: "prepare_work", Action: domain.ChatActionReadOnlyInvestigation, Reason: "核对未回应的问题"}, "在已批准范围内调查。", "b3-admit")
	if err != nil || admission.Outcome != "admitted" || admission.DispatchRequestID == "" {
		t.Fatalf("investigation admission: %+v %v", admission, err)
	}
	settle(intake.Claim, refusal, domain.TurnStateNotStarted)
	dispatch, err := db.MaterializeDispatchRequest(ctx, admission.DispatchRequestID, 1, time.Now())
	readinessHTTPOK(t, err)
	q, err := db.ResolveCoordinationDelivery(ctx, f.groupID, dispatch.Delivery.DeliveryID)
	readinessHTTPOK(t, err)
	co := store.CoordinationToolScope{Member: claim(dispatch.Delivery.DeliveryID), Provenance: q.Provenance}
	assign, err := db.DecideDispatchWithCapabilities(ctx, co, store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: owner.ID, Reason: "由调查负责人接办"}, "b3-assign", []store.DispatchWorkerCapabilityFact{{
		GroupID: f.groupID, MemberID: owner.ID, ProviderID: owner.ProviderID, Model: owner.Model, RequiredProfile: store.DispatchInvestigationWorkerProfile,
		TaskID: admission.TaskID, TaskRevision: q.Provenance.TaskRevision,
	}})
	readinessHTTPOK(t, err)
	assigned, err := db.MaterializeAssignment(ctx, assign.Assignment.ID, 1, time.Now())
	readinessHTTPOK(t, err)
	settle(co.Member.Claim, refusal, domain.TurnStateNotStarted)
	q, err = db.ResolveCoordinationDelivery(ctx, f.groupID, assigned.Delivery.DeliveryID)
	readinessHTTPOK(t, err)
	original := store.CoordinationToolScope{Member: claim(assigned.Delivery.DeliveryID), Provenance: q.Provenance}
	binding, err := db.BindAssignmentAttempt(ctx, original)
	readinessHTTPOK(t, err)
	const nativeID = "http-report-fixture"
	accept := func(c store.TurnClaim) {
		t.Helper()
		readinessHTTPOK(t, db.RecordTurnEvent(ctx, c, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: nativeID}))
	}
	terminal := provider.ExecutionReceipt{Started: true, Accepted: true, Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded,
		NativeSessionID: nativeID, DirectChildReaped: true, OutputComplete: true}
	accept(original.Member.Claim)
	d, err := engine.DelegateWork(ctx, original.Member, domain.DelegateWorkInput{MemberID: f.peer.ID, Question: "核对第一条。"}, "b3-child")
	readinessHTTPOK(t, err)
	child := claim(d.DeliveryID)
	accept(child.Claim)
	base := "/api/v1/groups/" + f.groupID + "/tasks/" + admission.TaskID
	path := base + "/work-reports"
	progress, err := engine.ReportWork(ctx, child, domain.ReportWorkInput{Kind: domain.TaskWorkReportProgress, Summary: "已核对。"}, "progress")
	readinessHTTPOK(t, err)
	if page := decoded[listResult[domain.TaskWorkReport]](t, a.request("GET", path, ""), 200); len(page.Items) != 1 || page.Items[0] != progress || progress.MessageID != "" || progress.DeliveryID != "" {
		t.Fatalf("progress not visible without a wake: %+v", page)
	}
	questionInput := domain.ReportWorkInput{Kind: domain.TaskWorkReportQuestion, Summary: "是否补充第一条来源？"}
	question, err := engine.ReportWork(ctx, child, questionInput, "owner-question")
	readinessHTTPOK(t, err)
	if retry, err := engine.ReportWork(ctx, child, questionInput, "owner-question"); err != nil || retry != question {
		t.Fatalf("report retry created another delivery: %+v %v", retry, err)
	}
	if got, err := engine.ResolveTaskWorkDelivery(ctx, f.groupID, question.DeliveryID); err != nil || got.SourceID != question.ID || got.MemberID != owner.ID || got.Blocked != "prior_turn_unsettled" {
		t.Fatalf("directed question did not wait at the original owner: %+v %v", got, err)
	}
	settle(child.Claim, terminal, domain.TurnStateSucceeded)
	settle(original.Member.Claim, terminal, domain.TurnStateSucceeded)
	continuing := claim(question.DeliveryID)
	attempt, err := engine.BindTaskWorkContinuation(ctx, continuing)
	readinessHTTPOK(t, err)
	if attempt.ID == binding.AttemptID || attempt.TaskID != admission.TaskID || continuing.MemberID != owner.ID {
		t.Fatalf("not the original assignment's owner continuation: %+v", attempt)
	}
	if _, err := engine.ReportWork(ctx, continuing, questionInput, "impersonate-child"); !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatalf("owner report wake acquired the child's ReportWork authority: %v", err)
	}
	if got, err := engine.GetWorkReportForMember(ctx, continuing, question.ID); err != nil || got != question {
		t.Fatalf("owner was not shown the trusted question: %+v %v", got, err)
	}
	first := decoded[listResult[domain.TaskWorkReport]](t, a.request("GET", path+"?limit=1", ""), 200)
	if len(first.Items) != 1 || first.Items[0] != question || first.NextCursor == "" {
		t.Fatalf("report page: %+v", first)
	}
	second := decoded[listResult[domain.TaskWorkReport]](t, a.request("GET", path+"?before="+first.NextCursor, ""), 200)
	if len(second.Items) != 1 || second.Items[0] != progress || second.NextCursor != "" {
		t.Fatalf("progress page: %+v", second)
	}
	taskLifecycleError(t, a.request("GET", base+"/work-inputs?before="+first.NextCursor, ""), 400, "invalid_input")
	taskLifecycleState(t, &a, base, domain.TaskStatusOpen, attempt.TaskRevision, attempt.ID, "")
	result, err := engine.SubmitTaskWorkResult(ctx, continuing, store.AssignmentResultInput{Summary: "负责人复核后提交文本结论。"}, "b3-result")
	readinessHTTPOK(t, err)
	taskLifecycleState(t, &a, base, domain.TaskStatusSubmitted, result.TaskRevision, attempt.ID, result.ID)
	review := decoded[domain.ReviewDecision](t, a.request("POST", base+"/reviews", taskLifecycleReviewBody(result.TaskRevision, result.ID, domain.ReviewOutcomeAccepted), "b3-review"), 201)
	if review.SubmissionID != result.ID || review.ReviewedBy != "user" || review.Outcome != domain.ReviewOutcomeAccepted {
		t.Fatalf("review was not a separate local-user decision: %+v", review)
	}
	taskLifecycleState(t, &a, base, domain.TaskStatusAccepted, result.TaskRevision, "", result.ID)
}
