package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

// Final fixtures require actual contiguous v21/v22 migrations, never a
// hand-built authority schema or a synthetic migration registration.
func chatRepairInstall(t *testing.T, s *Store) {
	t.Helper()
	var version int
	if err := s.db.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version); err != nil || version != 22 {
		t.Fatalf("real v21 -> v22 schema required: version=%d, %v", version, err)
	}
}

func chatRepairInput(t *testing.T, base domain.ChatAssessment, reviewID, authorization string) domain.ChatAssessment {
	t.Helper()
	data, err := json.Marshal(map[string]any{"disposition": base.Disposition, "action": base.Action, "title": base.Title, "description": base.Description, "acceptance": base.Acceptance, "reason": base.Reason,
		"repair": map[string]any{"review_id": reviewID, "authorization": authorization}})
	if err != nil {
		t.Fatal(err)
	}
	var input domain.ChatAssessment
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	return input
}

func chatRepairQueuedReview(t *testing.T, unresolved bool, originalContent ...string) (chatFixture, domain.Submission, domain.ReviewDecision, domain.Delivery) {
	t.Helper()
	var f chatFixture
	var worker CoordinationToolScope
	var submission domain.Submission
	if len(originalContent) == 0 {
		f, worker, submission = chatReviewSubmitted(t)
	} else {
		f = chatSetupSource(t, "user", false, originalContent[0])
		worker = chatAuthorityWorker(t, f)
		var err error
		submission, err = f.s.SubmitAssignmentResult(ctx, worker, AssignmentResultInput{Summary: "原版本。"}, "original-result")
		if err != nil {
			t.Fatal(err)
		}
	}
	chatRepairInstall(t, f.s)
	var coordinator TurnClaim
	coordinator.GroupID = f.groupID
	if err := f.s.db.QueryRow("SELECT id,claim_token FROM turns WHERE group_id=? AND member_id=? AND state IN ('starting','running')", f.groupID, f.a).Scan(&coordinator.TurnID, &coordinator.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, coordinator, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	outcome := provider.OutcomeNotStarted
	if unresolved {
		outcome = provider.OutcomeUnknown
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, worker.Member.Claim, provider.ExecutionReceipt{Outcome: outcome, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	input := taskReviewInput(submission)
	input.Outcome, input.Reason = domain.ReviewOutcomeRejected, "第三条缺少原约定依据，请在相同范围补齐后提交新版本。"
	review, err := f.s.ReviewTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	notice := chatReviewNotice(t, f.s, review)
	if notice.SourceKind != "platform" || notice.SourceID != "review:"+review.ID || len(notice.Deliveries) != 1 || notice.Deliveries[0].RecipientID != f.a {
		t.Fatalf("actual rejected Review did not route exactly one existing intake evaluation: %+v", notice)
	}
	return f, submission, review, notice.Deliveries[0]
}

func chatRepairReviewCandidate(t *testing.T, unresolved bool, originalContent ...string) (chatFixture, domain.Submission, domain.ReviewDecision) {
	t.Helper()
	f, submission, review, delivery := chatRepairQueuedReview(t, unresolved, originalContent...)
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: delivery.ID, MaxActive: 2})
	if err != nil {
		t.Fatal(err)
	}
	f.scope, err = f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	f.candidate, err = f.api.CaptureChatCandidate(ctx, f.scope)
	if err != nil {
		t.Fatal(err)
	}
	return f, submission, review
}

func TestChatRepairActualReviewUsesExistingIntakeAndNewTask(t *testing.T) {
	f, submission, review := chatRepairReviewCandidate(t, false)
	data, err := json.Marshal(f.candidate)
	if err != nil {
		t.Fatal(err)
	}
	var candidate struct {
		RepairSource *domain.ChatRepairSource `json:"repair_source"`
	}
	if err := json.Unmarshal(data, &candidate); err != nil || candidate.RepairSource == nil || candidate.RepairSource.ParentReviewID != review.ID || candidate.RepairSource.ReviewReason != review.Reason {
		t.Fatalf("candidate lost formal Review evidence: %s, %v", data, err)
	}
	input := chatRepairInput(t, f.input, review.ID, "review_request")
	got, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, input, "按原约定处理此拒收。", "repair-call")
	if err != nil || got.Outcome != "admitted" || got.TaskID == "" || got.TaskID == submission.TaskID || got.Authority != "explicit_request" || got.AuthorizationMessageID != "" || got.SourceKind != "platform" {
		t.Fatalf("repair did not use a distinct authorized Task: %+v, %v", got, err)
	}
	parent, err := f.s.GetTask(ctx, f.groupID, submission.TaskID)
	if err != nil || parent.Status != domain.TaskStatusOpen || parent.Revision != submission.TaskRevision+1 || parent.ActiveAttemptID != "" {
		t.Fatalf("repair rewrote original Task: %+v, %v", parent, err)
	}
	newTask, err := f.s.GetTask(ctx, f.groupID, got.TaskID)
	if err != nil || newTask.Acceptance != candidate.RepairSource.ParentMandate.Scope.Acceptance || newTask.Description != candidate.RepairSource.ParentMandate.Scope.Description {
		t.Fatalf("repair widened original acceptance/constraints: %+v, %v", newTask, err)
	}
	before := messageSequenceSnapshot(t, f.s)
	for _, api := range []chatIntakeAPI{f.api, chatIntake(t, queueHandle(t, f.path, false))} {
		replay, err := api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, input, "按原约定处理此拒收。", "repair-call")
		if err != nil || !reflect.DeepEqual(replay, got) {
			t.Fatalf("repair receipt changed on replay/reopen: %+v, %v", replay, err)
		}
	}
	changed := chatRepairInput(t, f.input, review.ID, "original_agreement")
	if _, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, changed, "按原约定处理此拒收。", "repair-call"); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key changed authorization: %v", err)
	}
	messageSequencePreserved(t, f.s, before)
	var originals int
	if err := f.s.db.QueryRow("SELECT count(*) FROM coordination_assignments WHERE task_id=?", submission.TaskID).Scan(&originals); err != nil || originals != 1 {
		t.Fatalf("repair deleted old Assignment history: %d, %v", originals, err)
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, f.scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	dispatch, err := f.s.GetDispatchRequest(ctx, f.groupID, got.DispatchRequestID)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := coordinationDecideBindRequest(t, f.s, dispatch.Request)
	decision, err := f.s.DecideDispatch(ctx, coordinator, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.b, Reason: "原范围修复"}, "repair-assign")
	if err != nil {
		t.Fatal(err)
	}
	worker := coordinationDecideWorker(t, requestDispatchFixture{s: f.s, worker: f.b}, dispatch.Request, decision, false)
	binding, err := f.s.BindAssignmentAttempt(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	worker.AttemptID = binding.AttemptID
	fixed, err := f.s.SubmitAssignmentResult(ctx, worker, AssignmentResultInput{Summary: "按原验收补齐第三条，等待真实用户评审。"}, "repair-result")
	if err != nil || fixed.TaskID != got.TaskID || fixed.TaskID == parent.ID {
		t.Fatalf("repair did not execute through existing coordinator/worker submission: %+v, %v", fixed, err)
	}
	secondInput := taskReviewInput(fixed)
	secondInput.Outcome, secondInput.IdempotencyKey = domain.ReviewOutcomeRejected, "reject-repair-result"
	secondInput.Reason = "仍需补齐第三条的原约定依据。"
	secondReview, err := f.s.ReviewTask(ctx, secondInput)
	if err != nil {
		t.Fatal(err)
	}
	secondSource, err := chatRepairReader(t, f.s).GetChatRepairSource(ctx, f.candidate.ThreadRef, secondReview.ID)
	if err != nil || secondSource.ParentTaskID != got.TaskID || secondSource.OriginalAuthorization.SourceKind != "local_user_review" || secondSource.OriginalAuthorization.SourceID != review.ID || secondSource.OriginalAuthorization.Content != review.Reason {
		t.Fatalf("a later repair cycle lost the actual original user Review command: %+v, %v", secondSource, err)
	}
	dispatchAssertFK(t, f.s)
}

func TestChatRepairRejectedDoesNotForceAdmission(t *testing.T) {
	f, _, _ := chatRepairReviewCandidate(t, false)
	input := domain.ChatAssessment{Disposition: "clarify", Reason: "实际用户仅拒收，现有许可尚不明确。", Question: "是否只在原范围补齐第三条依据？"}
	got, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, input, input.Question, "review-question")
	if err != nil || got.Outcome != "clarify" || got.TaskID != "" || got.Question != input.Question {
		t.Fatalf("rejected Review forced work rather than one concrete question: %+v, %v", got, err)
	}
	var tasks int
	if err := f.s.db.QueryRow("SELECT count(*) FROM tasks").Scan(&tasks); err != nil || tasks != 1 {
		t.Fatalf("clarification reopened work: %d, %v", tasks, err)
	}
}

func TestChatRepairCannotBypassUnknownParent(t *testing.T) {
	f, _, review := chatRepairReviewCandidate(t, true)
	input := chatRepairInput(t, f.input, review.ID, "review_request")
	before := messageSequenceSnapshot(t, f.s)
	got, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, input, "先核对原执行状态。", "unknown-parent")
	if !errors.Is(err, ErrChatRepairPending) || !reflect.DeepEqual(got, domain.ChatAssessmentReceipt{}) {
		t.Fatalf("transient parent state consumed the unique Review assessment: %+v, %v", got, err)
	}
	messageSequencePreserved(t, f.s, before)
}
