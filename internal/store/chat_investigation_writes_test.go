package store

import (
	"errors"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func investigationCoordinator(t *testing.T, f chatFixture) (domain.DispatchRequest, CoordinationToolScope) {
	t.Helper()
	admitted, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "建议调查。", "admit")
	if err != nil || admitted.Outcome != "admitted" || admitted.Action != domain.ChatActionReadOnlyInvestigation {
		t.Fatalf("investigation admission: %+v, %v", admitted, err)
	}
	investigationFinishTurn(t, f)
	dispatch, err := f.s.GetDispatchRequest(ctx, f.groupID, admitted.DispatchRequestID)
	if err != nil {
		t.Fatal(err)
	}
	return dispatch.Request, coordinationDecideBindRequest(t, f.s, dispatch.Request)
}

func investigationCapabilityFact(t *testing.T, f chatFixture, r domain.DispatchRequest) DispatchWorkerCapabilityFact {
	t.Helper()
	fact := dispatchCapabilityFact(t, requestDispatchFixture{s: f.s, groupID: f.groupID, worker: f.b})
	fact.RequiredProfile, fact.TaskID, fact.TaskRevision = DispatchInvestigationWorkerProfile, r.TaskID, r.TaskRevision
	return fact
}

func investigationClaimWorker(t *testing.T, f chatFixture) CoordinationToolScope {
	t.Helper()
	r, coordinator := investigationCoordinator(t, f)
	fact := investigationCapabilityFact(t, f, r)
	input := CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.b, Reason: "分配给受限调查成员"}
	decision, err := f.s.DecideDispatchWithCapabilities(ctx, coordinator, input, "assign", []DispatchWorkerCapabilityFact{fact})
	if err != nil || decision.Assignment == nil {
		t.Fatalf("investigation assignment: %+v, %v; cause=%v", decision, err, errors.Unwrap(err))
	}
	return coordinationDecideWorker(t, requestDispatchFixture{s: f.s, worker: f.b}, r, decision, false)
}

func investigationBindWorker(t *testing.T, f chatFixture) CoordinationToolScope {
	t.Helper()
	role := investigationClaimWorker(t, f)
	binding, err := f.s.BindAssignmentAttempt(ctx, role)
	if err != nil || binding.AttemptID == "" {
		t.Fatalf("investigation binding: %+v, %v; cause=%v", binding, err, errors.Unwrap(err))
	}
	role.AttemptID = binding.AttemptID
	return role
}

func TestChatInvestigationDecisionRequiresScopedWorker(t *testing.T) {
	f, policy := investigationUserCandidate(t)
	r, role := investigationCoordinator(t, f)
	fact := investigationCapabilityFact(t, f, r)
	input := CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.b, Reason: "分配给受限调查成员"}
	before := coordinationDecideSnapshot(t, f.s)
	for name, change := range map[string]func(*DispatchWorkerCapabilityFact){
		"generic-profile":   func(f *DispatchWorkerCapabilityFact) { f.RequiredProfile = "" },
		"workspace-profile": func(f *DispatchWorkerCapabilityFact) { f.RequiredProfile = DispatchWorkspaceWorkerProfile },
		"different-task":    func(f *DispatchWorkerCapabilityFact) { f.TaskID += "-other" },
		"stale-revision":    func(f *DispatchWorkerCapabilityFact) { f.TaskRevision++ },
		"different-model":   func(f *DispatchWorkerCapabilityFact) { f.Model += "-other" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := fact
			change(&bad)
			got, err := f.s.DecideDispatchWithCapabilities(ctx, role, input, "assign", []DispatchWorkerCapabilityFact{bad})
			requestDispatchZero(t, got, err, ErrConflict)
			coordinationDecidePreserved(t, f.s, before)
		})
	}
	got, err := f.s.DecideDispatch(ctx, role, input, "assign")
	requestDispatchZero(t, got, err, ErrConflict)
	coordinationDecidePreserved(t, f.s, before)
	original, err := f.s.DecideDispatchWithCapabilities(ctx, role, input, "assign", []DispatchWorkerCapabilityFact{fact})
	if err != nil || original.Assignment == nil || original.Assignment.WorkerMemberID != f.b {
		t.Fatalf("exact worker fact rejected: %+v, %v; cause=%v", original, err, errors.Unwrap(err))
	}
	investigationDisablePolicy(t, f.s, policy)
	before = coordinationDecideSnapshot(t, f.s)
	if replay, err := f.s.DecideDispatchWithCapabilities(ctx, role, input, "assign", nil); err != nil || !reflect.DeepEqual(replay, original) {
		t.Fatalf("revocation displaced exact decision replay: %+v, %v", replay, err)
	}
	coordinationDecidePreserved(t, f.s, before)
}

func TestChatInvestigationDecisionRechecksPolicy(t *testing.T) {
	f, policy := investigationUserCandidate(t)
	r, role := investigationCoordinator(t, f)
	facts := []DispatchWorkerCapabilityFact{investigationCapabilityFact(t, f, r)}
	if state, err := f.s.ResolveInvestigationExecution(ctx, role.Member, role.Provenance); err != nil || state.Blocked != CoordinationReady {
		t.Fatalf("ready coordinator preflight: %+v, %v; cause=%v", state, err, errors.Unwrap(err))
	}
	assign := CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.b, Reason: "开始调查"}
	decline := CoordinatorDecisionInput{Kind: "decline", Reason: "暂无受限执行成员"}
	coordinationDecideExec(t, f.s, `CREATE TRIGGER investigation_decision_drift AFTER INSERT ON coordination_decisions
	 BEGIN UPDATE chat_policies SET revision=revision-1
	 WHERE (group_id,topic_id)=(SELECT group_id,topic_id FROM coordination_dispatch_requests WHERE id=NEW.request_id); END`)
	before := coordinationDecideSnapshot(t, f.s)
	got, err := f.s.DecideDispatchWithCapabilities(ctx, role, assign, "assign", facts)
	requestDispatchZero(t, got, err, ErrMemberToolDenied)
	coordinationDecidePreserved(t, f.s, before)
	coordinationDecideExec(t, f.s, `DROP TRIGGER investigation_decision_drift`)
	investigationDisablePolicy(t, f.s, policy)
	before = coordinationDecideSnapshot(t, f.s)
	for _, input := range []CoordinatorDecisionInput{assign, decline} {
		got, err := f.s.DecideDispatchWithCapabilities(ctx, role, input, input.Kind, facts)
		requestDispatchZero(t, got, err, ErrMemberToolDenied)
		coordinationDecidePreserved(t, f.s, before)
	}
}

func TestChatInvestigationDeclineDoesNotRequireWorkerFact(t *testing.T) {
	f, policy := investigationUserCandidate(t)
	_, role := investigationCoordinator(t, f)
	input := CoordinatorDecisionInput{Kind: "decline", Reason: "暂无受限执行成员"}
	original, err := f.s.DecideDispatchWithCapabilities(ctx, role, input, "decline", nil)
	if err != nil || original.Decision.Kind != domain.CoordinatorDecisionKindDecline || original.Assignment != nil {
		t.Fatalf("current investigation decline: %+v, %v; cause=%v", original, err, errors.Unwrap(err))
	}
	investigationDisablePolicy(t, f.s, policy)
	before := coordinationDecideSnapshot(t, f.s)
	if replay, err := f.s.DecideDispatchWithCapabilities(ctx, role, input, "decline", nil); err != nil || !reflect.DeepEqual(replay, original) {
		t.Fatalf("revocation displaced exact decline replay: %+v, %v", replay, err)
	}
	coordinationDecidePreserved(t, f.s, before)
}

func TestChatInvestigationDeclineAfterSoleWorkerLeaves(t *testing.T) {
	f, _ := investigationUserCandidate(t)
	r, role := investigationCoordinator(t, f)
	facts := []DispatchWorkerCapabilityFact{investigationCapabilityFact(t, f, r)}
	if state, err := f.s.ResolveInvestigationExecution(ctx, role.Member, role.Provenance); err != nil || state.Blocked != CoordinationReady {
		t.Fatalf("ready coordinator preflight: %+v, %v; cause=%v", state, err, errors.Unwrap(err))
	}
	queueEvent(t, f.s, role.Member.Claim, provider.AcceptedEvent, "", "")
	coordinationDecideExec(t, f.s, "UPDATE members SET status='left' WHERE group_id=? AND id=?", f.groupID, f.b)
	if err := f.s.CheckTaskInvestigationReady(ctx, f.groupID, r.TaskID); !errors.Is(err, ErrTaskInvestigationNotReady) {
		t.Fatalf("investigation still ready without an eligible worker: %v", err)
	}
	before := coordinationDecideSnapshot(t, f.s)
	assign := CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.b, Reason: "开始调查"}
	got, err := f.s.DecideDispatchWithCapabilities(ctx, role, assign, "assign", facts)
	requestDispatchZero(t, got, err, ErrConflict)
	coordinationDecidePreserved(t, f.s, before)

	decline := CoordinatorDecisionInput{Kind: "decline", Reason: "唯一可分派成员已离开"}
	original, err := f.s.DecideDispatchWithCapabilities(ctx, role, decline, "decline", facts)
	if err != nil || original.Decision.Kind != domain.CoordinatorDecisionKindDecline || original.Assignment != nil ||
		original.Decision.CoordinatorTurnID != role.Member.Claim.TurnID || original.Decision.Reason != decline.Reason {
		t.Fatalf("live decline after worker left: %+v, %v; cause=%v", original, err, errors.Unwrap(err))
	}
	if saved, err := f.s.GetDispatchDecision(ctx, r.GroupID, r.ID); err != nil || !reflect.DeepEqual(saved, original) {
		t.Fatalf("persisted decline: %+v, %v", saved, err)
	}
	after := coordinationDecideSnapshot(t, f.s)
	if len(after["coordination_assignments"]) != 0 || len(after["coordination_assignment_dispatch"]) != 0 {
		t.Fatal("decline created an assignment or worker dispatch")
	}
	if replay, err := f.s.DecideDispatchWithCapabilities(ctx, role, decline, "decline", facts); err != nil || !reflect.DeepEqual(replay, original) {
		t.Fatalf("exact decline replay after worker left: %+v, %v", replay, err)
	}
	coordinationDecidePreserved(t, f.s, after)
}

func TestChatInvestigationBindingRechecksPolicy(t *testing.T) {
	f, policy := investigationUserCandidate(t)
	role := investigationClaimWorker(t, f)
	if state, err := f.s.ResolveInvestigationExecution(ctx, role.Member, role.Provenance); err != nil || state.Blocked != CoordinationReady || state.AttemptID != "" {
		t.Fatalf("ready unbound worker preflight: %+v, %v; cause=%v", state, err, errors.Unwrap(err))
	}
	if held, err := f.s.HasResourceClaim(ctx, "worker-resource"); !held || err != nil {
		t.Fatalf("fixture did not hold a resource: %v, %v", held, err)
	}
	investigationDisablePolicy(t, f.s, policy)
	before := coordinationDecideSnapshot(t, f.s)
	binding, err := f.s.BindAssignmentAttempt(ctx, role)
	requestDispatchZero(t, binding, err, ErrMemberToolDenied)
	// Covers the absence of new Attempts and the unchanged Turn, delivery and claims.
	coordinationDecidePreserved(t, f.s, before)
}

func TestChatInvestigationRejectionDoesNotEnqueueRepair(t *testing.T) {
	f, _, role := investigationUserWorker(t)
	sub, err := f.s.SubmitAssignmentResult(ctx, role, AssignmentResultInput{Summary: "调查结论。"}, "result")
	if err != nil {
		t.Fatal(err, errors.Unwrap(err))
	}
	input := taskReviewInput(sub)
	input.Outcome = domain.ReviewOutcomeRejected
	decision, err := f.s.ReviewTask(ctx, input)
	if err != nil || decision.Outcome != domain.ReviewOutcomeRejected || decision.ReviewedBy != "user" {
		t.Fatalf("formal investigation rejection: %+v, %v", decision, err)
	}
	notice := chatReviewNotice(t, f.s, decision)
	if notice.TopicID != role.Member.TopicID || len(notice.Deliveries) != 0 {
		t.Fatalf("rejection produced a wake instead of a public notice: %+v", notice)
	}
	var queued bool
	if err := f.s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM chat_repair_review_intakes WHERE review_id=?)`, decision.ID).Scan(&queued); err != nil || queued {
		t.Fatalf("investigation rejection entered automatic repair: %v, %v", queued, err)
	}
}
