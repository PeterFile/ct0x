package store

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestChatRepairV22RegistrationAndV21ReviewReplay(t *testing.T) {
	original := migrations
	t.Cleanup(func() { migrations = original })
	migrations = original[:21]
	f, worker, submission := chatReviewSubmittedV21(t)
	migrations = original
	input := taskReviewInput(submission)
	input.Outcome, input.Reason = domain.ReviewOutcomeRejected, "按原约定拒收；保留此历史命令。"
	review, err := f.s.ReviewTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	before := chatReviewNotice(t, f.s, review)
	if len(before.Deliveries) != 0 {
		t.Fatal("v21 fixture has future repair routing")
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	messageSequenceLedger(t, reopened, len(original))
	got, err := reopened.ReviewTask(ctx, input)
	if err != nil || !reflect.DeepEqual(got, review) {
		t.Fatalf("upgrade rewrote historical Review receipt: %+v, %v", got, err)
	}
	after := chatReviewNotice(t, reopened, review)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("migration/replay backfilled historical delivery or source: %+v", after)
	}
	var held, count int
	if err := reopened.db.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", worker.Member.Claim.TurnID).Scan(&held); err != nil || held != 1 {
		t.Fatalf("upgrade released original in-flight claim: %d, %v", held, err)
	}
	if err := reopened.db.QueryRow("SELECT count(*) FROM chat_repair_review_intakes").Scan(&count); err != nil || count != 0 {
		t.Fatalf("historical Review became new intent: %d, %v", count, err)
	}
	dispatchAssertFK(t, reopened)
}

func chatReviewSubmittedV21(t *testing.T) (chatFixture, CoordinationToolScope, domain.Submission) {
	t.Helper()
	f := chatSetup(t, "user", false)
	admitted, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "文本任务。", "admit")
	if err != nil {
		t.Fatal(err)
	}
	seedV21ChatIntakeReceipt(t, f.s, f.scope.Claim)
	dispatch, err := f.s.GetDispatchRequest(ctx, f.groupID, admitted.DispatchRequestID)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := coordinationDecideBindRequest(t, f.s, dispatch.Request)
	decision, err := f.s.DecideDispatch(ctx, coordinator, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.b, Reason: "文本成员"}, "assign")
	if err != nil || decision.Assignment == nil {
		t.Fatalf("v21 chat worker assignment: %+v, %v", decision, err)
	}
	worker := coordinationDecideWorker(t, requestDispatchFixture{s: f.s, worker: f.b}, dispatch.Request, decision, true)
	submission, err := f.s.SubmitAssignmentResult(ctx, worker, AssignmentResultInput{Summary: "可追溯的原版本文本成果。"}, "chat-review-result")
	if err != nil {
		t.Fatal(err)
	}
	return f, worker, submission
}

// Seed the canonical pre-V23 not-started receipt in the historical schema.
// Only the intake settles here; the worker's original claim survives Review.
func seedV21ChatIntakeReceipt(t *testing.T, s *Store, claim TurnClaim) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	turn, err := lockTurnClaim(ctx, tx, claim)
	if err != nil {
		t.Fatal(err)
	}
	receipt := normalizeTurnReceipt(turn, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1})
	if turn.State != domain.TurnStateStarting || receipt.Outcome != provider.OutcomeNotStarted {
		t.Fatalf("v21 intake must be unstarted: state=%s, outcome=%s", turn.State, receipt.Outcome)
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE turns SET state=?,reason=?,receipt_json=?,updated_at=?
		WHERE group_id=? AND id=? AND claim_token=?`, receipt.Outcome, receipt.Reason, string(data), time.Now().UTC().Format(timestampLayout),
		claim.GroupID, claim.TurnID, claim.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET state='failed',error=?
		WHERE group_id=? AND id=? AND turn_id=? AND state<>'accepted'`, receipt.Reason, claim.GroupID, turn.DeliveryID, turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM resource_claims WHERE turn_id=?", turn.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestChatRepairRevisionZeroCaptureKeepsSupportedMetadata(t *testing.T) {
	f := setupMessages(t)
	message := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: "unconfigured-repair-topic", ActorID: "user", Content: "先讨论验收边界。", RecipientIDs: []string{f.a}}, []string{f.a})
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: message.Deliveries[0].ID, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := f.s.CaptureChatCandidate(ctx, scope)
	if err != nil || candidate.Policy.Revision != 0 || candidate.Policy.Enabled || !candidate.Policy.WorkspaceChangesSupported || candidate.RepairSource != nil {
		t.Fatalf("repair capture lost frozen metadata fix or fabricated permission: %+v, %v", candidate, err)
	}
	if _, err := f.s.SetChatPolicy(ctx, candidate.ThreadRef, domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 2}, 0, "configure-later"); err != nil {
		t.Fatal(err)
	}
	recaptured, err := f.s.CaptureChatCandidate(ctx, scope)
	if err != nil || !reflect.DeepEqual(recaptured, candidate) {
		t.Fatalf("repair capture reloaded current Policy into rev0 snapshot: %+v, %v", recaptured, err)
	}
}
