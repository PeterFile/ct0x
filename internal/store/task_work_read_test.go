package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestTaskWorkBoundReads(t *testing.T) {
	f := chatSetup(t, "user", false)
	f.s.EnableTaskWorkExecution()
	original := chatAuthorityWorker(t, f)
	p := original.Provenance
	coordinationDecideExec(t, f.s, `UPDATE tasks SET title='Display title',description='Display description',acceptance='Display criteria' WHERE id=?`, p.TaskID)

	const quote = "最多一百字"
	body := "请继续摘要，" + quote + "。\n" + strings.Repeat("字\"\n", 1200)
	assessor, candidate := newWorkInputCandidate(t, f, body)
	proposal := domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "补充原摘要", Followup: &domain.ChatFollowupAssessment{Constraint: quote}}
	r, err := f.s.ApplyChatAssessmentDormant(ctx, assessor, candidate.ID, proposal, "记录要求。", "bound-input")
	if err != nil || r.WorkInputID == "" {
		t.Fatalf("record input: %+v %v", r, err)
	}
	items, err := f.s.ListWorkInputsForMember(ctx, original.Member)
	if err != nil || len(items) != 1 || items[0].ID != r.WorkInputID {
		t.Fatalf("disclose exact handle: %+v %v", items, err)
	}
	inputID := items[0].ID
	if got, err := f.s.GetWorkInputForMember(ctx, original.Member, inputID); err != nil || !reflect.DeepEqual(got, items[0]) ||
		got.Content != body || got.ReplyMessageID != candidate.MessageID || got.AfterConstraint != quote || got.Consumption != nil || got.Return != nil {
		t.Fatalf("read original input and metadata: id=%q err=%v", got.ID, err)
	}
	mandate, err := f.s.ReadTaskWorkMandate(ctx, original.Member)
	if err != nil || mandate.ID != r.MandateID || mandate.TaskID != p.TaskID || mandate.TaskRevisionAtAdmission != p.TaskRevision ||
		mandate.GroupID != p.GroupID || mandate.TopicID != p.TopicID || mandate.Scope.Objective != f.input.Title ||
		mandate.Scope.Description != f.input.Description || mandate.Scope.Acceptance != f.input.Acceptance {
		t.Fatalf("immutable mandate: %+v %v", mandate, err)
	}
	ack, err := f.s.AcknowledgeWorkInput(ctx, original.Member, inputID)
	if err != nil || ack.Consumption == nil || ack.Consumption.AttemptID != original.AttemptID || ack.Consumption.TurnID != original.Member.Claim.TurnID {
		t.Fatalf("explicit acknowledgement: receipt=%+v err=%v", ack.Consumption, err)
	}

	denyBoth := func(label string, scope MemberToolScope) {
		t.Helper()
		if got, err := f.s.GetWorkInputForMember(ctx, scope, inputID); !errors.Is(err, ErrMemberToolDenied) || got != (domain.TaskWorkInput{}) {
			t.Fatalf("%s read input: id=%q err=%v", label, got.ID, err)
		}
		if got, err := f.s.ReadTaskWorkMandate(ctx, scope); !errors.Is(err, ErrMemberToolDenied) || !reflect.DeepEqual(got, domain.Mandate{}) {
			t.Fatalf("%s read mandate: id=%q err=%v", label, got.ID, err)
		}
	}
	foreign := original.Member
	foreign.Claim.GroupID = f.otherID
	denyBoth("foreign group", foreign)

	refusal := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}
	coordinator := taskWorkTestCoordinator(t, f.s, p.GroupID, p.AssignmentID)
	for _, prior := range []MemberToolScope{assessor, coordinator, original.Member} {
		taskWorkTestReceipt(t, f.s, prior, refusal, domain.TurnStateNotStarted)
	}
	denyBoth("settled claim", original.Member)
	ordinary, _ := newWorkInputCandidate(t, f, "普通对话。", p.MemberID)
	if ordinary.ConversationID != original.Member.ConversationID || ordinary.Claim.TurnID == original.Member.Claim.TurnID {
		t.Fatal("expected a different Turn of the same conversation")
	}
	denyBoth("ordinary Turn of the same member", ordinary)
	taskWorkTestReceipt(t, f.s, ordinary, refusal, domain.TurnStateNotStarted)

	assessor, candidate = newWorkInputCandidate(t, f, "仍按原要求完成摘要。")
	proposal.Followup = nil
	wake, err := f.s.ApplyChatAssessment(ctx, assessor, candidate.ID, proposal, "记录要求。", "bound-wake")
	if err != nil || wake.WorkDeliveryID == "" {
		t.Fatalf("record owner wake: %+v %v", wake, err)
	}
	d, err := f.s.ResolveTaskWorkDelivery(ctx, p.GroupID, wake.WorkDeliveryID)
	if err != nil || d.Blocked != "" {
		t.Fatalf("ready owner wake: %+v %v", d, err)
	}
	next := taskWorkTestClaim(t, f.s, d)
	denyBoth("unbound continuation", next)
	if _, err := f.s.BindTaskWorkContinuation(ctx, next); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.GetWorkInputForMember(ctx, next, inputID); err != nil || !reflect.DeepEqual(got, ack) {
		t.Fatalf("continuation lost prior same-Assignment acknowledgement: id=%q receipt=%+v err=%v", got.ID, got.Consumption, err)
	}
	if got, err := f.s.ReadTaskWorkMandate(ctx, next); err != nil || !reflect.DeepEqual(got, mandate) {
		t.Fatalf("continuation lost admission scope: %+v %v", got, err)
	}
	returned, err := f.s.HandBackWorkInput(ctx, next, wake.WorkInputID, "请确认补充要求")
	if err != nil || returned.Return == nil {
		t.Fatalf("record return: %+v %v", returned, err)
	}
	if got, err := f.s.GetWorkInputForMember(ctx, next, wake.WorkInputID); err != nil || !reflect.DeepEqual(got, returned) {
		t.Fatalf("read exact return: %+v %v", got, err)
	}
	taskWorkTestReceipt(t, f.s, next, refusal, domain.TurnStateNotStarted)

	requester, second := newWorkInputCandidate(t, f, "请列出下周会议议程。", f.a)
	f.input.Title, f.input.Description, f.input.Acceptance = "下周议程", "整理会议事项", "列出时间和事项"
	admitted, err := f.s.ApplyChatAssessment(ctx, requester, second.ID, f.input, "登记独立工作。", "bound-admit")
	if err != nil || admitted.Outcome != "admitted" {
		t.Fatalf("admit other Task: %+v %v", admitted, err)
	}
	taskWorkTestReceipt(t, f.s, requester, refusal, domain.TurnStateNotStarted)
	dispatch, err := f.s.GetDispatchRequest(ctx, p.GroupID, admitted.DispatchRequestID)
	if err != nil {
		t.Fatal(err)
	}
	otherCoordinator := coordinationDecideBindRequest(t, f.s, dispatch.Request)
	fixture := requestDispatchFixture{s: f.s, worker: p.MemberID}
	decision := coordinationDecideAssign(t, fixture, otherCoordinator, "bound-assign")
	other := coordinationDecideWorker(t, fixture, dispatch.Request, decision, true)
	if other.Member.MemberID != original.Member.MemberID || other.Provenance.AssignmentID == p.AssignmentID {
		t.Fatal("expected another Assignment of the same member")
	}
	if got, err := f.s.GetWorkInputForMember(ctx, other.Member, inputID); !errors.Is(err, ErrMemberToolDenied) || got != (domain.TaskWorkInput{}) {
		t.Fatalf("other Assignment read input: id=%q err=%v", got.ID, err)
	}
	if got, err := f.s.ReadTaskWorkMandate(ctx, other.Member); err != nil || got.ID == mandate.ID || got.TaskID != other.Provenance.TaskID || got.Scope.Objective != f.input.Title {
		t.Fatalf("other Assignment must read only its own mandate: %+v %v", got, err)
	}
}
