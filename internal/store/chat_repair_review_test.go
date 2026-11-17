package store

import (
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
)

func TestChatRepairMissingRoutingPreservesReviewAndReplayNeverBackfills(t *testing.T) {
	f, _, submission := chatReviewSubmitted(t)
	chatRepairInstall(t, f.s)
	policy := f.policy.ChatPolicyRequest
	policy.Enabled = false
	if _, err := f.api.SetChatPolicy(ctx, f.candidate.ThreadRef, policy, 1, "disable-routing"); err != nil {
		t.Fatal(err)
	}
	input := taskReviewInput(submission)
	input.Outcome, input.Reason = domain.ReviewOutcomeRejected, "请按原范围补齐。"
	review, err := f.s.ReviewTask(ctx, input)
	if err != nil {
		t.Fatalf("unavailable repair routing rolled back actual user Review: %v", err)
	}
	notice := chatReviewNotice(t, f.s, review)
	if len(notice.Deliveries) != 0 || notice.SourceKind != "platform" {
		t.Fatalf("disabled policy still routed repair intake: %+v", notice)
	}
	policy.Enabled = true
	if _, err := f.api.SetChatPolicy(ctx, f.candidate.ThreadRef, policy, 2, "restore-routing"); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	got, err := f.s.ReviewTask(ctx, input)
	if err != nil || !reflect.DeepEqual(got, review) {
		t.Fatalf("old Review replay changed: %+v, %v", got, err)
	}
	messageSequencePreserved(t, f.s, before)
	var count int
	if err := f.s.db.QueryRow("SELECT count(*) FROM chat_repair_review_intakes").Scan(&count); err != nil || count != 0 {
		t.Fatalf("historical Review replay retroactively woke intake: %d, %v", count, err)
	}
}

func TestChatRepairReviewIntakeWriteFailureRollsBackWholeCommand(t *testing.T) {
	f, _, submission := chatReviewSubmitted(t)
	chatRepairInstall(t, f.s)
	if _, err := f.s.db.Exec(`CREATE TRIGGER fail_repair_intake BEFORE INSERT ON chat_repair_review_intakes
	 BEGIN SELECT RAISE(ABORT,'injected intake persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	input := taskReviewInput(submission)
	input.Outcome = domain.ReviewOutcomeRejected
	if _, err := f.s.ReviewTask(ctx, input); err == nil {
		t.Fatal("failed intake relationship left a partly committed Review/delivery")
	}
	messageSequencePreserved(t, f.s, before)
	if _, err := f.s.db.Exec("DROP TRIGGER fail_repair_intake"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ReviewTask(ctx, input); err != nil {
		t.Fatalf("rolled-back Review command burned its original key: %v", err)
	}
}
