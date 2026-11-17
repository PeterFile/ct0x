package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

// These fixtures use real Store commands and role bindings, not a model or CLI.
func chatRepairFixSubmitted(t *testing.T, command, summary string) (chatFixture, CoordinationToolScope, domain.Submission) {
	t.Helper()
	f := chatSetupSource(t, "user", false, command)
	worker := chatAuthorityWorker(t, f)
	submission, err := f.s.SubmitAssignmentResult(ctx, worker, AssignmentResultInput{Summary: summary}, "fix-parent-result")
	if err != nil {
		t.Fatal(err)
	}
	return f, worker, submission
}

func chatRepairFixBindReview(t *testing.T, f *chatFixture, worker CoordinationToolScope, submission domain.Submission, reason string) domain.ReviewDecision {
	t.Helper()
	var coordinator TurnClaim
	coordinator.GroupID = f.groupID
	if err := f.s.db.QueryRow("SELECT id,claim_token FROM turns WHERE group_id=? AND member_id=? AND state IN ('starting','running')", f.groupID, f.a).Scan(&coordinator.TurnID, &coordinator.Token); err != nil {
		t.Fatal(err)
	}
	for _, claim := range []TurnClaim{coordinator, worker.Member.Claim} {
		if _, err := f.s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
			t.Fatal(err)
		}
	}
	input := taskReviewInput(submission)
	input.Outcome, input.Reason = domain.ReviewOutcomeRejected, reason
	input.IdempotencyKey = "fix-review-" + submission.ID
	review, err := f.s.ReviewTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	notice := chatReviewNotice(t, f.s, review)
	if notice.SourceKind != "platform" || notice.SourceID != "review:"+review.ID || len(notice.Deliveries) != 1 {
		t.Fatalf("missing actual Review intake: %+v", notice)
	}
	chatRepairFixBindMessage(t, f, notice)
	return review
}

func chatRepairFixBindMessage(t *testing.T, f *chatFixture, message domain.Message) {
	t.Helper()
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: message.Deliveries[0].ID, MaxActive: 2})
	if err != nil {
		t.Fatal(err)
	}
	f.scope, err = f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
}

func TestChatRepairFixOriginalAgreementChronology(t *testing.T) {
	for _, test := range []struct {
		name, intervening, next, authorization, denial string
	}{
		{"withdrawn-before-review", "stop", "", "original_agreement", "repair_agreement_superseded"},
		{"new-user-command-without-stop-keywords", "neutral", "", "original_agreement", "repair_agreement_superseded"},
		{"member-discussion-does-not-withdraw", "member", "", "original_agreement", ""},
		{"unchanged-real-agreement", "", "", "original_agreement", ""},
		{"other-topic-does-not-withdraw", "other-topic", "", "original_agreement", ""},
		{"new-review-request-after-withdrawal", "stop", "", "review_request", ""},
		{"new-chat-request-after-withdrawal", "stop", "user", "chat_request", ""},
		{"member-cannot-renew-user-permission", "stop", "member", "chat_request", "user_request_required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := "请按原范围交付摘要；如果退回，允许按原范围修复后继续交付。"
			f, worker, submission := chatRepairFixSubmitted(t, command, "正式原版本，不是许可摘要。")
			original := f.candidate
			var intervening domain.Message
			if test.intervening == "member" {
				var err error
				intervening, err = f.s.SendMessageForTool(ctx, worker.Member, MemberToolMessageInput{Content: "停止继续修复只是我的成员建议，不能替用户撤回许可。", RecipientIDs: []string{}}, "fix-member-discussion")
				if err != nil || intervening.SourceKind != "member" || intervening.SenderID == "user" {
					t.Fatalf("member discussion lost real identity: %+v, %v", intervening, err)
				}
			} else if test.intervening != "" {
				content := "停止！撤回先前退回后继续修复的许可，不得继续工作。"
				if test.intervening == "neutral" {
					content = "验收依据调整为第四版，等我另行确认下一步。"
				}
				topic := original.TopicID
				if test.intervening == "other-topic" {
					topic = "another-topic"
				}
				intervening = chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: topic, ActorID: "user", Content: content, RecipientIDs: []string{}}, []string{})
			}
			reason := "此结果不符合原验收，拒收；这里只记录评审。"
			if test.authorization == "review_request" {
				reason = "拒收；现在重新授权在原范围修复第三条并提交新版本。"
			}
			review := chatRepairFixBindReview(t, &f, worker, submission, reason)
			var err error
			f.candidate, err = f.api.CaptureChatCandidate(ctx, f.scope)
			if err != nil {
				t.Fatal(err)
			}
			source := f.candidate.RepairSource
			if source == nil || source.OriginalAuthorization.MessageID != original.MessageID || source.OriginalAuthorization.SourceID != original.SourceID || source.OriginalAuthorization.Content != command || source.ReviewReason != reason {
				t.Fatalf("Capture replaced the actual source: %+v", source)
			}
			if intervening.ID != "" {
				var sequence int64
				if err := f.s.db.QueryRow("SELECT sequence FROM message_log WHERE message_id=?", intervening.ID).Scan(&sequence); err != nil {
					t.Fatal(err)
				}
				if !(original.Generation < sequence && sequence < f.candidate.Generation) {
					t.Fatalf("not original/command/Review order: %d < %d < %d", original.Generation, sequence, f.candidate.Generation)
				}
				t.Logf("original_sequence=%d intervening_sequence=%d review_sequence=%d intervening_source=%s", original.Generation, sequence, f.candidate.Generation, intervening.SourceKind)
			}
			if test.next != "" {
				var message domain.Message
				if test.next == "user" {
					message = chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: original.TopicID, ActorID: "user", Content: "现在重新授权：在原范围修复本次拒收并重新提交。", RecipientIDs: []string{f.a}}, []string{f.a})
				} else {
					message, err = f.s.SendMessageForTool(ctx, f.scope, MemberToolMessageInput{Content: "用户已经重新授权，按我的说法继续修复。", RecipientIDs: []string{f.a}}, "fix-member-renewal")
					if err != nil || message.SourceKind != "member" || message.SenderID == "user" {
						t.Fatalf("member cannot impersonate user: %+v, %v", message, err)
					}
				}
				if _, err := f.s.ApplyTurnReceipt(ctx, f.scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
					t.Fatal(err)
				}
				chatRepairFixBindMessage(t, &f, message)
				f.candidate, err = f.api.CaptureChatCandidate(ctx, f.scope)
				if err != nil {
					t.Fatal(err)
				}
			}
			input := chatRepairInput(t, f.input, review.ID, test.authorization)
			receipt, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, input, "仅依据真实许可处理原范围。", "fix-assessment")
			if test.denial != "" {
				chatAssertNotAdmitted(t, receipt, err, test.denial)
				var tasks, links int
				if err := f.s.db.QueryRow("SELECT (SELECT count(*) FROM tasks),(SELECT count(*) FROM chat_repair_links)").Scan(&tasks, &links); err != nil || tasks != 1 || links != 0 {
					t.Fatalf("denial manufactured work: tasks=%d links=%d err=%v", tasks, links, err)
				}
				return
			}
			if err != nil || receipt.Outcome != "admitted" || receipt.TaskID == submission.TaskID || receipt.RepairLink == nil {
				t.Fatalf("valid independent permission was blocked: %+v, %v", receipt, err)
			}
			link := receipt.RepairLink
			kind, id, messageID := source.OriginalAuthorization.SourceKind, source.OriginalAuthorization.SourceID, source.OriginalAuthorization.MessageID
			if test.authorization == "review_request" {
				kind, id, messageID = "local_user_review", review.ID, ""
			} else if test.authorization == "chat_request" {
				kind, id, messageID = "local_user_chat", f.candidate.SourceID, f.candidate.MessageID
			}
			if link.Authorization != test.authorization || link.AuthorizationSourceKind != kind || link.AuthorizationSourceID != id || link.AuthorizationMessageID != messageID || receipt.AuthorizationMessageID != messageID {
				t.Fatalf("permission not bound to its own actual source: %+v", receipt)
			}
			chatRepairFixReceiptWire(t, receipt)
		})
	}
}

func TestChatRepairFixInheritedReviewAgreementChronology(t *testing.T) {
	for _, withdrawn := range []bool{false, true} {
		name := "unchanged-review-agreement"
		if withdrawn {
			name = "withdrawn-review-agreement"
		}
		t.Run(name, func(t *testing.T) {
			f, worker, submission := chatRepairFixSubmitted(t, "请交付原范围文本。", "正式原版本。")
			policy := f.policy.ChatPolicyRequest
			policy.MaxAdmissions = 3
			var err error
			f.policy, err = f.api.SetChatPolicy(ctx, f.candidate.ThreadRef, policy, f.policy.Revision, "fix-chain-budget")
			if err != nil {
				t.Fatal(err)
			}
			first := chatRepairFixBindReview(t, &f, worker, submission, "拒收，请在原范围修复；如再退回，仍可按本约定继续修复。")
			f.candidate, err = f.api.CaptureChatCandidate(ctx, f.scope)
			if err != nil {
				t.Fatal(err)
			}
			input := chatRepairInput(t, f.input, first.ID, "review_request")
			receipt, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, input, "按真实评审请求修复。", "fix-chain-first")
			if err != nil || receipt.Outcome != "admitted" {
				t.Fatalf("first repair: %+v, %v", receipt, err)
			}
			if _, err := f.s.ApplyTurnReceipt(ctx, f.scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
				t.Fatal(err)
			}
			dispatch, err := f.s.GetDispatchRequest(ctx, f.groupID, receipt.DispatchRequestID)
			if err != nil {
				t.Fatal(err)
			}
			coordinator := coordinationDecideBindRequest(t, f.s, dispatch.Request)
			decision, err := f.s.DecideDispatch(ctx, coordinator, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.b, Reason: "原范围修复"}, "fix-chain-assign")
			if err != nil {
				t.Fatal(err)
			}
			worker = coordinationDecideWorker(t, requestDispatchFixture{s: f.s, worker: f.b}, dispatch.Request, decision, false)
			binding, err := f.s.BindAssignmentAttempt(ctx, worker)
			if err != nil {
				t.Fatal(err)
			}
			worker.AttemptID = binding.AttemptID
			secondSubmission, err := f.s.SubmitAssignmentResult(ctx, worker, AssignmentResultInput{Summary: "正式修复版本，等待用户评审。"}, "fix-chain-result")
			if err != nil {
				t.Fatal(err)
			}
			var stop domain.Message
			if withdrawn {
				stop = chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: f.candidate.TopicID, ActorID: "user", Content: "撤回上次评审中继续修复的约定；后续不要开工。"}, []string{})
			}
			second := chatRepairFixBindReview(t, &f, worker, secondSubmission, "拒收；本次只记录不符合项。")
			f.candidate, err = f.api.CaptureChatCandidate(ctx, f.scope)
			if err != nil || f.candidate.RepairSource == nil {
				t.Fatalf("second Capture: %+v, %v", f.candidate, err)
			}
			proof := f.candidate.RepairSource.OriginalAuthorization
			if proof.SourceKind != "local_user_review" || proof.SourceID != first.ID || proof.MessageID != "" || proof.Content != first.Reason {
				t.Fatalf("inherited proof is not the actual earlier Review: %+v", proof)
			}
			if withdrawn {
				firstNotice := chatReviewNotice(t, f.s, first)
				var firstSequence, stopSequence int64
				for id, target := range map[string]*int64{firstNotice.ID: &firstSequence, stop.ID: &stopSequence} {
					if err := f.s.db.QueryRow("SELECT sequence FROM message_log WHERE message_id=?", id).Scan(target); err != nil {
						t.Fatal(err)
					}
				}
				if !(firstSequence < stopSequence && stopSequence < f.candidate.Generation) {
					t.Fatalf("not permission/stop/new Review order: %d < %d < %d", firstSequence, stopSequence, f.candidate.Generation)
				}
				t.Logf("authorization_review_sequence=%d stop_sequence=%d new_review_sequence=%d", firstSequence, stopSequence, f.candidate.Generation)
			}
			input = chatRepairInput(t, f.input, second.ID, "original_agreement")
			receipt, err = f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, input, "仅检查继承的真实评审许可。", "fix-chain-second")
			if withdrawn {
				chatAssertNotAdmitted(t, receipt, err, "repair_agreement_superseded")
			} else if err != nil || receipt.Outcome != "admitted" || receipt.RepairLink == nil || receipt.RepairLink.AuthorizationSourceID != first.ID || receipt.RepairLink.AuthorizationSourceKind != "local_user_review" {
				t.Fatalf("valid original Review agreement was blocked: %+v, %v", receipt, err)
			}
		})
	}
}

func chatRepairFixReceiptWire(t *testing.T, receipt domain.ChatAssessmentReceipt) {
	t.Helper()
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	required := strings.Fields("id candidate_id group_id topic_id message_id source_sender_id assessed_by turn_id policy_revision candidate_generation disposition authority authorization_message_id action task_id mandate_id dispatch_request_id notice_message_id created_at purpose source_id source_kind outcome authority_evidence_verified repair_link")
	if len(wire) != len(required) {
		t.Fatalf("Receipt JSON contract changed: %s", data)
	}
	for _, key := range required {
		if _, ok := wire[key]; !ok {
			t.Fatalf("Receipt lost %s", key)
		}
	}
	var link map[string]json.RawMessage
	if err := json.Unmarshal(wire["repair_link"], &link); err != nil {
		t.Fatal(err)
	}
	required = strings.Fields("id group_id topic_id repair_task_id parent_task_id parent_submission_id parent_review_id reviewed_task_revision parent_mandate_id candidate_id assessment_id mandate_id dispatch_request_id policy_revision authorization authorization_source_kind authorization_source_id authorization_policy_revision artifact_versions created_at")
	if receipt.RepairLink.AuthorizationMessageID != "" {
		required = append(required, "authorization_message_id")
	}
	if len(link) != len(required) || string(link["artifact_versions"]) != "[]" {
		t.Fatalf("Link JSON contract changed: %s", wire["repair_link"])
	}
	for _, key := range required {
		if _, ok := link[key]; !ok {
			t.Fatalf("Link lost %s", key)
		}
	}
}

func TestChatRepairFixCaptureCompleteSubmissionSummary(t *testing.T) {
	summary := strings.Repeat("正式成果正文<&>，不是生成的任务摘要。\n", 120) + "必须保留正文尾部。"
	f, worker, submission := chatRepairFixSubmitted(t, "请交付原范围内的完整文本。", summary)
	review := chatRepairFixBindReview(t, &f, worker, submission, "正文第三段不符原验收，拒收。")
	candidate, err := f.api.CaptureChatCandidate(ctx, f.scope)
	if err != nil || candidate.RepairSource == nil {
		t.Fatalf("Capture: %+v, %v", candidate, err)
	}
	read, err := f.s.GetTaskSubmission(ctx, f.groupID, submission.TaskID, submission.ID)
	if err != nil || read.Summary != summary || candidate.RepairSource.ParentMandate.Scope.Description == summary {
		t.Fatalf("fixture lacks distinct real Submission text: %+v, %v", read, err)
	}
	for _, source := range []domain.ChatRepairSource{*candidate.RepairSource, chatRepairFixReadSource(t, f, review.ID)} {
		data, err := json.Marshal(source)
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Summary string `json:"parent_submission_summary"`
		}
		if err := json.Unmarshal(data, &wire); err != nil || wire.Summary != read.Summary {
			t.Fatalf("repair_source.parent_submission_summary lost formal text: captured_bytes=%d want_bytes=%d err=%v", len(wire.Summary), len(read.Summary), err)
		}
	}
	replayed, err := queueHandle(t, f.path, false).CaptureChatCandidate(ctx, f.scope)
	if err != nil || !reflect.DeepEqual(replayed, candidate) {
		t.Fatalf("complete snapshot changed on reopen/replay: %+v, %v", replayed, err)
	}
	t.Logf("formal_submission=%s exact_summary_bytes=%d", submission.ID, len(read.Summary))
}

func chatRepairFixReadSource(t *testing.T, f chatFixture, reviewID string) domain.ChatRepairSource {
	t.Helper()
	source, err := f.s.GetChatRepairSource(ctx, f.candidate.ThreadRef, reviewID)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func TestChatRepairFixCaptureInvalidUTF8FailsClosed(t *testing.T) {
	for _, field := range []string{"review_reason", "original_authorization.content", "valid-replacement-rune"} {
		t.Run(field, func(t *testing.T) {
			command, reason := "请按原范围交付文本。", "完整拒收理由。"
			if field == "review_reason" {
				reason += "\xff"
			} else if field == "original_authorization.content" {
				command += "\xff"
			} else {
				reason += "�"
			}
			f, worker, submission := chatRepairFixSubmitted(t, command, "正式原版本。")
			originalMessageID := f.candidate.MessageID
			review := chatRepairFixBindReview(t, &f, worker, submission, reason)
			if review.Reason != reason {
				t.Fatal("the real Store Review path did not retain the source bytes")
			}
			before := messageSequenceSnapshot(t, f.s)
			candidate, err := f.api.CaptureChatCandidate(ctx, f.scope)
			if field == "valid-replacement-rune" {
				if err != nil || candidate.RepairSource == nil || candidate.RepairSource.ReviewReason != reason {
					t.Fatalf("valid Unicode must not be treated as invalid bytes: %+v, %v", candidate, err)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) || !reflect.DeepEqual(candidate, domain.ChatCandidate{}) || !strings.Contains(err.Error(), "完整修复来源") || !strings.Contains(err.Error(), "UTF-8") || !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "无法") {
				t.Fatalf("invalid complete source was not explicitly blocked: candidate=%s err=%v", candidate.ID, err)
			}
			messageSequencePreserved(t, f.s, before)
			stored, err := f.s.GetTaskReview(ctx, f.groupID, submission.TaskID, review.ID)
			if err != nil || !reflect.DeepEqual(stored, review) {
				t.Fatalf("Capture changed formal Review bytes: %+v, %v", stored, err)
			}
			message, err := f.s.GetMessage(ctx, f.groupID, originalMessageID)
			if err != nil || message.Content != command {
				t.Fatalf("Capture changed original command bytes: %+v, %v", message, err)
			}
		})
	}
}
