package store

import (
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
)

func TestChatRepairV22RegistrationAndV21ReviewReplay(t *testing.T) {
	original := migrations
	t.Cleanup(func() { migrations = original })
	migrations = original[:21]
	f, worker, submission := chatReviewSubmitted(t)
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
	var version int
	if err := reopened.db.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version); err != nil || version != 22 {
		t.Fatalf("real v21 -> v22 registration missing: version=%d, %v", version, err)
	}
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
