package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestChatRepairPermanentlyStaleReviewCanStillBeDiscussed(t *testing.T) {
	f, submission, review, delivery := chatRepairQueuedReview(t, true)
	if _, err := f.s.db.Exec("UPDATE tasks SET revision=revision+1 WHERE id=?", submission.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CheckChatRepairIntakeReady(ctx, f.groupID, delivery.ID); err != nil {
		t.Fatalf("permanently stale Review was held as transient execution readiness: %v", err)
	}
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: delivery.ID, MaxActive: 2})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := f.s.CaptureChatCandidate(ctx, scope)
	if err != nil || candidate.RepairSource == nil || candidate.RepairSource.ParentReviewID != review.ID {
		t.Fatalf("stale Review lost official context before semantic discussion: %+v, %v", candidate, err)
	}
	got, err := f.s.ApplyChatAssessment(ctx, scope, candidate.ID, domain.ChatAssessment{Disposition: "discuss", Reason: "原版本已被后续状态取代，仅解释历史拒收。"}, "当前以新的正式记录为准，本轮不创建工作。", "stale-discussion")
	if err != nil || got.Outcome != "discuss" || got.TaskID != "" {
		t.Fatalf("stale context forced new work: %+v, %v", got, err)
	}
}

type chatRepairReadinessAPI interface {
	CheckChatRepairIntakeReady(context.Context, string, string) error
}

func TestChatRepairReviewWaitsUnclaimedUntilParentTerminal(t *testing.T) {
	f, worker, submission := chatReviewSubmitted(t)
	chatRepairInstall(t, f.s)
	api, ok := any(f.s).(chatRepairReadinessAPI)
	if !ok {
		t.Fatal("Store lacks preclaim repair intake readiness")
	}
	var coordinator TurnClaim
	coordinator.GroupID = f.groupID
	if err := f.s.db.QueryRow("SELECT id,claim_token FROM turns WHERE group_id=? AND member_id=? AND state IN ('starting','running')", f.groupID, f.a).Scan(&coordinator.TurnID, &coordinator.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, coordinator, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	input := taskReviewInput(submission)
	input.Outcome, input.Reason = domain.ReviewOutcomeRejected, "不等原轮最终文本，按已约定的依据拒收；请在原范围补齐。"
	review, err := f.s.ReviewTask(ctx, input)
	if err != nil {
		t.Fatalf("pending native outcome prohibited valid user rejection: %v", err)
	}
	notice := chatReviewNotice(t, f.s, review)
	if len(notice.Deliveries) != 1 {
		t.Fatalf("Review did not preserve the original queued evaluation: %+v", notice)
	}
	delivery := notice.Deliveries[0]
	before := messageSequenceSnapshot(t, f.s)
	for i := 0; i < 2; i++ {
		if err := api.CheckChatRepairIntakeReady(ctx, f.groupID, delivery.ID); !errors.Is(err, ErrChatRepairPending) {
			t.Fatalf("pending parent was permanently consumed or treated ready: %v", err)
		}
	}
	messageSequencePreserved(t, f.s, before)
	current, err := f.s.GetMessage(ctx, f.groupID, notice.ID)
	if err != nil || !reflect.DeepEqual(current, notice) || current.Deliveries[0].TurnID != "" {
		t.Fatalf("readiness claimed/replaced the Review delivery: %+v, %v", current, err)
	}
	var claims int
	if err := f.s.db.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", worker.Member.Claim.TurnID).Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("waiting Review released original worker claim: %d, %v", claims, err)
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, worker.Member.Claim, queueReceipt(provider.OutcomeSucceeded)); err != nil {
		t.Fatal(err)
	}
	if err := api.CheckChatRepairIntakeReady(ctx, f.groupID, delivery.ID); err != nil {
		t.Fatalf("actual parent terminal did not unblock same delivery: %v", err)
	}
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: delivery.ID, MaxActive: 1})
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
	repairInput := chatRepairInput(t, f.input, review.ID, "review_request")
	receipt, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, repairInput, "原执行已到终态，按原请求继续。", "same-review-after-terminal")
	if err != nil || receipt.Outcome != "admitted" || receipt.RepairLink == nil || receipt.RepairLink.ParentReviewID != review.ID || receipt.MessageID != notice.ID {
		t.Fatalf("same Review required a second user request: %+v, %v", receipt, err)
	}
	if got, err := f.s.GetTaskReview(ctx, f.groupID, submission.TaskID, review.ID); err != nil || !reflect.DeepEqual(got, review) {
		t.Fatalf("native terminal changed actual Review: %+v, %v", got, err)
	}
}
