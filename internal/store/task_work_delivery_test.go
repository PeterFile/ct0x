package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func taskWorkTestCoordinator(t *testing.T, s *Store, group, assignment string) MemberToolScope {
	t.Helper()
	view, err := s.GetAssignment(ctx, group, assignment)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := s.GetTurn(ctx, group, view.Decision.CoordinatorTurnID)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := s.BindMemberTool(ctx, TurnClaim{GroupID: group, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func taskWorkTestClaim(t *testing.T, s *Store, d domain.TaskWorkDelivery, keys ...string) MemberToolScope {
	t.Helper()
	turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: d.GroupID, DeliveryID: d.DeliveryID, MaxActive: 8, ResourceKeys: keys})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := s.BindMemberTool(ctx, TurnClaim{GroupID: d.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func taskWorkTestReceipt(t *testing.T, s *Store, scope MemberToolScope, receipt provider.ExecutionReceipt, want domain.TurnState) {
	t.Helper()
	if turn, err := s.ApplyTurnReceipt(ctx, scope.Claim, receipt); err != nil || turn.State != want {
		t.Fatalf("receipt: state=%s, err=%v, want=%s", turn.State, err, want)
	}
}

func taskWorkTestPeek(t *testing.T, s *Store, want domain.TaskWorkDelivery, blocked string) {
	t.Helper()
	got, err := s.ResolveTaskWorkDelivery(ctx, want.GroupID, want.DeliveryID)
	if err != nil || got.Blocked != blocked {
		t.Fatalf("typed peek: %+v %v; want blocked=%q", got, err, blocked)
	}
	got.Blocked, want.Blocked = "", ""
	if got != want {
		t.Fatalf("typed peek changed identity: %+v != %+v", got, want)
	}
}

func taskWorkTestFollowup(t *testing.T, f chatFixture, text string) (domain.ChatAssessmentReceipt, domain.TaskWorkDelivery) {
	t.Helper()
	scope, candidate := newWorkInputCandidate(t, f, text)
	proposal := domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "原任务补充"}
	r, err := f.s.ApplyChatAssessment(ctx, scope, candidate.ID, proposal, "记录补充。", "continue")
	if err != nil || r.Outcome != "continued" || r.WorkDeliveryID == "" || r.WorkInputID == "" || r.DispatchRequestID != "" {
		t.Fatalf("no typed followup: %+v %v", r, err)
	}
	replay, err := f.s.ApplyChatAssessment(ctx, scope, candidate.ID, proposal, "记录补充。", "continue")
	if err != nil || !reflect.DeepEqual(replay, r) {
		t.Fatalf("assessment replay changed the wake: %+v %v", replay, err)
	}
	d, err := f.s.ResolveTaskWorkDelivery(ctx, f.groupID, r.WorkDeliveryID)
	if err != nil || d.SourceID != r.WorkInputID || d.SourceKind != domain.TaskWorkSourceFollowup || d.MessageID != r.WorkWakeMessageID {
		t.Fatalf("lost wake identity: %+v %v", d, err)
	}
	d.Blocked = ""
	taskWorkTestReceipt(t, f.s, scope, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	return r, d
}

func TestTaskWorkOwnerContinuation(t *testing.T) {
	f := chatSetup(t, "user", false)
	f.s.EnableTaskWorkExecution()
	old := chatAuthorityWorker(t, f)
	p := old.Provenance
	co := taskWorkTestCoordinator(t, f.s, p.GroupID, p.AssignmentID)
	r, d := taskWorkTestFollowup(t, f, "继续原任务，保留来源。")
	if d.TaskID != p.TaskID || d.AssignmentID != p.AssignmentID || d.TaskRevision != p.TaskRevision || d.TopicID != p.TopicID ||
		d.OwnerMemberID != p.MemberID || d.MemberID != p.MemberID || d.ConversationID != p.ConversationID || d.DeliveryID == p.DeliveryID {
		t.Fatalf("wake changed original ownership: %+v", d)
	}
	if got, err := f.s.ResolveTaskWorkDelivery(ctx, f.otherID, d.DeliveryID); err == nil || got != (domain.TaskWorkDelivery{}) {
		t.Fatalf("foreign resolve disclosed work: %+v %v", got, err)
	}
	if _, err := f.s.ResolveTaskWorkDelivery(ctx, p.GroupID, p.DeliveryID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy worker delivery classified as work: %v", err)
	}
	refusal := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}
	taskWorkTestReceipt(t, f.s, co, refusal, domain.TurnStateNotStarted)
	queueEvent(t, f.s, old.Member.Claim, provider.AcceptedEvent, "", "")
	taskWorkTestPeek(t, f.s, d, "prior_turn_unsettled")
	terminal := queueReceipt(provider.OutcomeSucceeded)
	taskWorkTestReceipt(t, f.s, old.Member, terminal, domain.TurnStateSucceeded)

	s := queueHandle(t, f.path, true)
	s.EnableTaskWorkExecution()
	var durable string
	if err := s.db.QueryRow(`SELECT receipt_json FROM turns WHERE id=?`, old.Member.Claim.TurnID).Scan(&durable); err != nil {
		t.Fatal(err)
	}
	coordinationDecideExec(t, s, `UPDATE turns SET receipt_json='{}' WHERE id=?`, old.Member.Claim.TurnID)
	taskWorkTestPeek(t, s, d, "prior_turn_unsettled")
	if turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: d.GroupID, DeliveryID: d.DeliveryID, MaxActive: 8}); !errors.Is(err, ErrTurnBusy) || turn.ID != "" {
		t.Fatalf("terminal-looking state without a receipt allowed claim: %+v %v", turn, err)
	}
	coordinationDecideExec(t, s, `UPDATE turns SET receipt_json=? WHERE id=?`, durable, old.Member.Claim.TurnID)
	taskWorkTestPeek(t, s, d, "")
	next := taskWorkTestClaim(t, s, d, "worker-resource")
	forged := next
	forged.Claim.Token = old.Member.Claim.Token
	if _, err := s.BindTaskWorkContinuation(ctx, forged); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("forged claim acquired an attempt: %v", err)
	}
	if _, err := s.CaptureChatCandidate(ctx, next); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("typed work fell through to chat intake: %v", err)
	}
	input := AssignmentResultInput{Summary: "三条可核对的结论"}
	if _, err := s.SubmitTaskWorkResult(ctx, next, input, "worker-submit"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("unbound work used original attempt: %v", err)
	}
	attempt, err := s.BindTaskWorkContinuation(ctx, next)
	if err != nil || attempt.ID == "" || attempt.ID == old.AttemptID || attempt.TurnID != next.Claim.TurnID || attempt.TaskID != p.TaskID || attempt.TaskRevision != p.TaskRevision {
		t.Fatalf("expected a new same-revision formal attempt: %+v %v", attempt, err)
	}
	if again, err := s.BindTaskWorkContinuation(ctx, next); err != nil || again != attempt {
		t.Fatalf("bind replay: %+v %v", again, err)
	}
	original, err := s.GetAssignment(ctx, p.GroupID, p.AssignmentID)
	if err != nil || original.Binding == nil || original.Binding.AttemptID != old.AttemptID || original.Task.ActiveAttemptID != attempt.ID {
		t.Fatalf("lost original binding or new CAS: %+v %v", original, err)
	}
	if _, err := s.AcknowledgeWorkInputWithKey(ctx, next, r.WorkInputID, "confirm"); err != nil {
		t.Fatal(err)
	}
	result, err := s.SubmitTaskWorkResult(ctx, next, input, "worker-submit")
	if err != nil || result.ID == "" || result.AttemptID != attempt.ID || result.SubmittedBy != next.MemberID {
		t.Fatalf("continuation did not formally submit: %+v %v", result, err)
	}
	var notice string
	if err := s.db.QueryRow(`SELECT message_id FROM chat_submission_notices WHERE submission_id=?`, result.ID).Scan(&notice); err != nil {
		t.Fatal(err)
	}
	visible, err := s.GetMessage(ctx, d.GroupID, notice)
	if err != nil || visible.TopicID != d.TopicID || visible.SourceKind != "platform" || len(visible.Deliveries) != 0 {
		t.Fatalf("formal submission did not publish its notice: %+v %v", visible, err)
	}
	before := messageSequenceSnapshot(t, s)
	changed := AssignmentResultInput{Summary: "同一个 key 不可改变交付"}
	if got, err := s.SubmitTaskWorkResult(ctx, next, changed, "worker-submit"); !errors.Is(err, ErrConflict) || got.ID != "" {
		t.Fatalf("changed submission replayed under same key: %+v %v", got, err)
	}
	if again, err := s.SubmitTaskWorkResult(ctx, next, input, "worker-submit"); err != nil || !reflect.DeepEqual(result, again) {
		t.Fatalf("same-key replay after task is submitted: %+v %v", again, err)
	}
	messageSequencePreserved(t, s, before)
	taskWorkTestReceipt(t, s, next, refusal, domain.TurnStateNotStarted)
	if _, err := s.SubmitTaskWorkResult(ctx, next, input, "worker-submit"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("settled claim could replay formal submit: %v", err)
	}
	for _, table := range []string{"task_work_deliveries", "task_work_continuations"} {
		for _, q := range []string{"UPDATE " + table + " SET delivery_id=delivery_id", "DELETE FROM " + table, "INSERT OR REPLACE INTO " + table + " SELECT * FROM " + table} {
			if _, err := s.db.Exec(q); err == nil {
				t.Fatalf("immutable work provenance changed: %s", q)
			}
		}
	}
	dispatchAssertFK(t, s)
}

func TestTaskWorkQueuedOriginal(t *testing.T) {
	f := chatSetup(t, "user", false)
	f.s.EnableTaskWorkExecution()
	admitted, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "文本任务。", "admit")
	if err != nil {
		t.Fatal(err)
	}
	taskWorkTestReceipt(t, f.s, f.scope, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	dispatch, err := f.s.GetDispatchRequest(ctx, f.groupID, admitted.DispatchRequestID)
	if err != nil {
		t.Fatal(err)
	}
	co := coordinationDecideBindRequest(t, f.s, dispatch.Request)
	assigned, err := f.s.DecideDispatch(ctx, co, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.b, Reason: "文本成员"}, "assign")
	if err != nil || assigned.Assignment == nil {
		t.Fatalf("no real assignment: %+v %v", assigned, err)
	}
	a := assigned.Assignment.ID
	original, err := f.s.MaterializeAssignment(ctx, a, 1, time.Now().UTC())
	if err != nil || original.Delivery == nil {
		t.Fatalf("no queued original: %+v %v", original, err)
	}
	_, d := taskWorkTestFollowup(t, f, "继续本话题摘要。")
	taskWorkTestReceipt(t, f.s, co.Member, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	taskWorkTestPeek(t, f.s, d, "original_attempt_unbound")
	if turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: d.GroupID, DeliveryID: d.DeliveryID, MaxActive: 8}); !errors.Is(err, ErrTurnBusy) || turn.ID != "" {
		t.Fatalf("claimed followup overtook queued original: %+v %v", turn, err)
	}
	view, err := f.s.GetAssignment(ctx, f.groupID, a)
	if err != nil || view.Binding != nil || view.Task.ActiveAttemptID != "" {
		t.Fatalf("denied start acquired ownership: %+v %v", view, err)
	}
}
