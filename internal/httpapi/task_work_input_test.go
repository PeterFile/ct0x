package httpapi

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestTaskWorkInputsHTTPScopedHistory(t *testing.T) {
	f := chatHTTPSetup(t)
	ctx := context.Background()
	engine, local := coordination.New(f.db), platform.NewLocalUser(f.db)
	claim := func(deliveryID string) store.MemberToolScope {
		t.Helper()
		turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.groupID, DeliveryID: deliveryID, MaxActive: 4})
		readinessHTTPOK(t, err)
		scope, err := f.db.BindMemberTool(ctx, store.TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
		readinessHTTPOK(t, err)
		return scope
	}
	settle := func(claim store.TurnClaim) {
		t.Helper()
		_, err := f.db.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted})
		readinessHTTPOK(t, err)
	}
	intake, err := f.db.BindMemberTool(ctx, f.claim)
	readinessHTTPOK(t, err)
	candidate, err := engine.CaptureChatCandidate(ctx, intake)
	readinessHTTPOK(t, err)
	admission, err := engine.AssessChatIntent(ctx, intake, candidate.ID, domain.ChatAssessment{
		Disposition: "request_work", Action: domain.ChatActionTextSubmission, Title: "群内摘要", Acceptance: "列出三条结论", Reason: "用户已确认交付",
	}, "记录摘要任务。", "admit-for-input")
	if err != nil || admission.Outcome != "admitted" {
		t.Fatalf("admission: %+v %v", admission, err)
	}
	settle(intake.Claim)
	dispatch, err := f.db.MaterializeDispatchRequest(ctx, admission.DispatchRequestID, 1, time.Now())
	readinessHTTPOK(t, err)
	queued, err := f.db.ResolveCoordinationDelivery(ctx, f.groupID, dispatch.Delivery.DeliveryID)
	readinessHTTPOK(t, err)
	co := store.CoordinationToolScope{Member: claim(dispatch.Delivery.DeliveryID), Provenance: queued.Provenance}
	decision, err := f.db.DecideDispatch(ctx, co, store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.peer.ID, Reason: "文本执行"}, "assign-for-input")
	readinessHTTPOK(t, err)
	assigned, err := f.db.MaterializeAssignment(ctx, decision.Assignment.ID, 1, time.Now())
	readinessHTTPOK(t, err)
	settle(co.Member.Claim)
	queued, err = f.db.ResolveCoordinationDelivery(ctx, f.groupID, assigned.Delivery.DeliveryID)
	readinessHTTPOK(t, err)
	worker := claim(assigned.Delivery.DeliveryID)
	_, err = f.db.BindAssignmentAttempt(ctx, store.CoordinationToolScope{Member: worker, Provenance: queued.Provenance})
	readinessHTTPOK(t, err)
	var sourceIDs []string
	for i, quote := range []string{"限定一百字", "同时写入项目目录"} {
		message, err := local.CreateMessage(ctx, f.groupID, platform.MessageInput{TopicID: intake.TopicID, Content: quote, RecipientIDs: []string{f.caller.ID}}, []string{"input-1", "input-2"}[i])
		readinessHTTPOK(t, err)
		scope := claim(message.Deliveries[0].ID)
		candidate, err := engine.CaptureChatCandidate(ctx, scope)
		readinessHTTPOK(t, err)
		r, err := engine.AssessChatIntentDormant(ctx, scope, candidate.ID, domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork,
			Reason: "原摘要附加要求", Followup: &domain.ChatFollowupAssessment{Constraint: quote}}, "记录原要求。", "input-"+message.ID)
		if err != nil || r.TaskID != admission.TaskID || r.WorkInputID == "" {
			t.Fatalf("continued input: %+v %v", r, err)
		}
		sourceIDs = append(sourceIDs, message.ID)
		settle(scope.Claim)
	}
	inbox, err := engine.ListWorkInputsForMember(ctx, worker)
	if err != nil || len(inbox) != 2 {
		t.Fatalf("worker inbox: %+v %v", inbox, err)
	}
	_, err = engine.AcknowledgeWorkInputWithKey(ctx, worker, inbox[0].ID, "confirm-first")
	readinessHTTPOK(t, err)

	handler := mustServer(t, DefaultAddress, TaskLifecycleRoutes(local)...).Handler
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "http://"+DefaultAddress+path, nil))
		return w
	}
	base := "/api/v1/groups/" + f.groupID + "/tasks/"
	url := base + admission.TaskID + "/work-inputs"
	first := decoded[listResult[domain.TaskWorkInput]](t, get(url+"?limit=1"), 200)
	if len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatalf("missing history page: %+v", first)
	}
	latest := first.Items[0]
	if latest.ReplyMessageID != sourceIDs[1] || latest.BeforeConstraint != "限定一百字" || latest.AfterConstraint != "同时写入项目目录" || latest.Consumption != nil || latest.Return != nil {
		t.Fatalf("latest input lost before/after evidence: %+v", latest)
	}
	last := decoded[listResult[domain.TaskWorkInput]](t, get(url+"?before="+first.NextCursor), 200)
	if len(last.Items) != 1 || last.NextCursor != "" || last.Items[0].ReplyMessageID != sourceIDs[0] || last.Items[0].Consumption == nil || last.Items[0].Consumption.TurnID != worker.Claim.TurnID {
		t.Fatalf("older input lost source or explicit confirmation: %+v", last)
	}
	taskLifecycleError(t, get(base+"other/work-inputs?before="+first.NextCursor), 400, "invalid_input")
	taskLifecycleError(t, get("/api/v1/groups/other/tasks/"+admission.TaskID+"/work-inputs"), 404, "not_found")

	impactURL := base + admission.TaskID + "/impacts"
	var impactInstalled bool
	if err := f.raw.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='task_constraint_impacts')`).Scan(&impactInstalled); err != nil {
		t.Fatal(err)
	}
	impacts := decoded[listResult[domain.TaskImpact]](t, get(impactURL), 200)
	if !impactInstalled {
		if len(impacts.Items) != 0 || impacts.NextCursor != "" {
			t.Fatalf("uninstalled impact schema must return an empty page: %+v", impacts)
		}
		returned, err := engine.HandBackWorkInputWithKey(ctx, worker, inbox[1].ID, "该额外要求要单独授权。", "scoped-return")
		readinessHTTPOK(t, err)
		replayed, err := engine.HandBackWorkInputWithKey(ctx, worker, inbox[1].ID, "该额外要求要单独授权。", "scoped-return")
		readinessHTTPOK(t, err)
		if replayed.Return == nil || *replayed.Return != *returned.Return {
			t.Fatalf("return replay changed the original question: %+v", replayed)
		}
		again := decoded[listResult[domain.TaskImpact]](t, get(impactURL), 200)
		if len(again.Items) != 0 || again.NextCursor != "" {
			t.Fatalf("uninstalled impact schema must keep empty page after return: %+v", again)
		}
		taskLifecycleError(t, get("/api/v1/groups/other/tasks/"+admission.TaskID+"/impacts"), 404, "not_found")
		return
	}
	if len(impacts.Items) != 1 || impacts.Items[0].Kind != domain.TaskImpactConstraintConfirmed ||
		impacts.Items[0].Source.InputID != inbox[0].ID || impacts.Items[0].Source.MessageID != inbox[0].ReplyMessageID ||
		impacts.Items[0].Source.SourceSequence != inbox[0].SourceSequence || impacts.Items[0].Source.SourceSequence <= 0 ||
		impacts.Items[0].Acknowledgement == nil || impacts.Items[0].Delivery != nil || impacts.Items[0].Decision != nil {
		t.Fatalf("incorrect impact (pending input must not appear, ACK is not delivery): %+v", impacts)
	}
	returned, err := engine.HandBackWorkInputWithKey(ctx, worker, inbox[1].ID, "该额外要求要单独授权。", "scoped-return")
	readinessHTTPOK(t, err)
	replayed, err := engine.HandBackWorkInputWithKey(ctx, worker, inbox[1].ID, "该额外要求要单独授权。", "scoped-return")
	readinessHTTPOK(t, err)
	if replayed.Return == nil || *replayed.Return != *returned.Return {
		t.Fatalf("return replay changed the original question: %+v", replayed)
	}
	firstImpact := decoded[listResult[domain.TaskImpact]](t, get(impactURL+"?limit=1"), 200)
	if len(firstImpact.Items) != 1 || firstImpact.NextCursor == "" {
		t.Fatalf("impact page: %+v", firstImpact)
	}
	handoff := firstImpact.Items[0]
	if handoff.Kind != domain.TaskImpactWorkInputReturned || handoff.Acknowledgement != nil || handoff.Plan != nil || handoff.Decision == nil ||
		handoff.Decision.Question != returned.Return.Question || handoff.Decision.Question == "" || !handoff.Decision.ScopeCurrent ||
		handoff.Decision.AttemptID != returned.Return.AttemptID || handoff.Decision.TurnID != worker.Claim.TurnID ||
		handoff.Source.MessageID != returned.ReplyMessageID || handoff.Source.SourceSequence != returned.SourceSequence ||
		handoff.Source.SourceID != returned.SourceID || handoff.Source.TriggerID != returned.TriggerID ||
		handoff.Source.AssignmentID != returned.AssignmentID || handoff.Source.MandateID != returned.MandateID ||
		handoff.Source.AuthorityGeneration != 1 || handoff.TaskRevision != returned.TaskRevision {
		t.Fatalf("handoff lost the concrete question or original authorized scope: %+v", handoff)
	}
	lastImpact := decoded[listResult[domain.TaskImpact]](t, get(impactURL+"?before="+firstImpact.NextCursor), 200)
	if len(lastImpact.Items) != 1 || lastImpact.NextCursor != "" || lastImpact.Items[0].ID != impacts.Items[0].ID {
		t.Fatalf("impact cursor failed to preserve exact confirmation: %+v", lastImpact)
	}
	taskLifecycleError(t, get(base+"other/impacts?before="+firstImpact.NextCursor), 400, "invalid_input")
	taskLifecycleError(t, get("/api/v1/groups/other/tasks/"+admission.TaskID+"/impacts"), 404, "not_found")
}
