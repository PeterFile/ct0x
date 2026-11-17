package store

import (
	"errors"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
)

func TestChatRepairOriginalAgreementUsesCompleteRealUserCommand(t *testing.T) {
	command := "请按刚才的边界交付三条摘要；如果我退回，请遵照这里的范围补齐依据后继续交付，无需另问一次。"
	f, _, review := chatRepairReviewCandidate(t, false, command)
	if f.candidate.RepairSource == nil || f.candidate.RepairSource.OriginalAuthorization.Content != command || f.candidate.RepairSource.OriginalAuthorization.SourceID == "" {
		t.Fatalf("original permission was replaced by generated task text: %+v", f.candidate)
	}
	input := chatRepairInput(t, f.input, review.ID, "original_agreement")
	receipt, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, input, "按原用户许可继续。", "original-agreement")
	if err != nil || receipt.Outcome != "admitted" || receipt.RepairLink == nil || receipt.RepairLink.AuthorizationSourceKind != "local_user_chat" || receipt.RepairLink.AuthorizationSourceID != f.candidate.RepairSource.OriginalAuthorization.SourceID {
		t.Fatalf("actual original agreement required duplicate authorization: %+v, %v", receipt, err)
	}
}

func TestChatRepairNextRealChatAndDuplicateReview(t *testing.T) {
	f, _, review := chatRepairReviewCandidate(t, false)
	chatNextCandidate(t, &f, true)
	input := chatRepairInput(t, f.input, review.ID, "chat_request")
	receipt, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, input, "按当前真实用户请求处理原拒收。", "chat-repair")
	if err != nil || receipt.Outcome != "admitted" || receipt.RepairLink == nil || receipt.RepairLink.AuthorizationSourceID != f.candidate.SourceID || receipt.AuthorizationMessageID != f.candidate.MessageID {
		t.Fatalf("actual next user chat did not bind the real Review: %+v, %v", receipt, err)
	}
	policy := f.policy.ChatPolicyRequest
	policy.MaxAdmissions = 4
	if _, err := f.api.SetChatPolicy(ctx, f.candidate.ThreadRef, policy, 1, "more-budget"); err != nil {
		t.Fatal(err)
	}
	chatNextCandidate(t, &f, true)
	got, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, input, "重复事件不能新开工。", "duplicate-review")
	chatAssertNotAdmitted(t, got, err, "repair_already_linked")
	var count int
	if err := f.s.db.QueryRow("SELECT count(*) FROM tasks").Scan(&count); err != nil || count != 2 {
		t.Fatalf("same Review produced another repair Task: %d, %v", count, err)
	}
}

func TestChatRepairReplayPrecedesNewEligibility(t *testing.T) {
	f, parent, review := chatRepairReviewCandidate(t, false)
	input := chatRepairInput(t, f.input, review.ID, "review_request")
	receipt, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, input, "原范围修复。", "stable-repair")
	if err != nil || receipt.Outcome != "admitted" {
		t.Fatalf("initial repair: %+v, %v", receipt, err)
	}
	policy := f.policy.ChatPolicyRequest
	policy.Enabled = false
	if _, err := f.api.SetChatPolicy(ctx, f.candidate.ThreadRef, policy, 1, "disable"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("UPDATE tasks SET revision=revision+1 WHERE id=?", parent.TaskID); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	replayed, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, input, "原范围修复。", "stable-repair")
	if err != nil || !reflect.DeepEqual(replayed, receipt) {
		t.Fatalf("new eligibility displaced saved original receipt: %+v, %v", replayed, err)
	}
	changed := input
	changed.Repair = &domain.ChatRepairAssessment{ReviewID: review.ID, Authorization: "original_agreement"}
	if _, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, changed, "原范围修复。", "stable-repair"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed payload bypassed original key conflict: %v", err)
	}
	messageSequencePreserved(t, f.s, before)
}

func TestChatRepairRequiresExactSemanticEnumAndBoundReview(t *testing.T) {
	f, _, review := chatRepairReviewCandidate(t, false)
	for _, authorization := range []string{"", "repair", "user", "review_request ", "agent_consensus"} {
		input := chatRepairInput(t, f.input, review.ID, authorization)
		if err := validateChatAssessment(input, "评估。"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsupported permission claim %q: %v", authorization, err)
		}
	}
	input := chatRepairInput(t, f.input, "other-review", "review_request")
	got, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, input, "不能选择另一来源。", "wrong-review")
	chatAssertNotAdmitted(t, got, err, "repair_source_mismatch")
}
