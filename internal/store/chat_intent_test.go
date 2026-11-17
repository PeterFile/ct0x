package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

type chatIntakeAPI interface {
	SetChatPolicy(context.Context, domain.ThreadRef, domain.ChatPolicyRequest, int64, string) (domain.ChatPolicy, error)
	GetChatPolicy(context.Context, domain.ThreadRef) (domain.ChatPolicy, error)
	CaptureChatCandidate(context.Context, MemberToolScope) (domain.ChatCandidate, error)
	ApplyChatAssessment(context.Context, MemberToolScope, string, domain.ChatAssessment, string, string) (domain.ChatAssessmentReceipt, error)
}

func chatIntake(t *testing.T, s *Store) chatIntakeAPI {
	t.Helper()
	api, ok := any(s).(chatIntakeAPI)
	if !ok {
		t.Fatal("Store lacks trusted chat intake and versioned policy")
	}
	return api
}

type chatFixture struct {
	messageFixture
	api       chatIntakeAPI
	scope     MemberToolScope
	candidate domain.ChatCandidate
	policy    domain.ChatPolicy
	input     domain.ChatAssessment
}

func chatSetup(t *testing.T, sender string, preparation bool) chatFixture {
	t.Helper()
	return chatSetupSource(t, sender, preparation, "就依照刚才商定的边界把那份摘要落实吧，交付三条可核对的结论。")
}

func chatSetupSource(t *testing.T, sender string, preparation bool, content string) chatFixture {
	t.Helper()
	f := setupMessages(t)
	api := chatIntake(t, f.s)
	thread := domain.ThreadRef{GroupID: f.groupID, TopicID: "chat-topic"}
	policyInput := domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 2, AllowPreparation: preparation}
	if preparation {
		policyInput.PreparationScopeKind = domain.ChatPreparationTopicSummary
	}
	policy, err := api.SetChatPolicy(ctx, thread, policyInput, 0, "policy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetCoordinator(ctx, f.groupID, f.a, 0, "coordinator"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.b}, 0, "dispatch-policy"); err != nil {
		t.Fatal(err)
	}
	if sender == "agent" {
		sender = f.b
	}
	var message domain.Message
	if sender == "user" {
		message = chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: thread.TopicID, ActorID: "user", Content: content, RecipientIDs: []string{f.a}}, []string{f.a})
	} else {
		invitation := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: thread.TopicID, ActorID: "user", Content: "请先提出摘要建议。", RecipientIDs: []string{f.b}}, []string{f.b})
		peer, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: invitation.Deliveries[0].ID, MaxActive: 1})
		if err != nil {
			t.Fatal(err)
		}
		peerScope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: peer.ID, Token: peer.ClaimToken})
		if err != nil {
			t.Fatal(err)
		}
		message, err = f.s.SendMessageForTool(ctx, peerScope, MemberToolMessageInput{Content: content, RecipientIDs: []string{f.a}}, "member-proposal")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.ApplyTurnReceipt(ctx, peerScope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
			t.Fatal(err)
		}
	}
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: message.Deliveries[0].ID, MaxActive: 1, ResourceKeys: []string{"chat-resource"}})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := api.CaptureChatCandidate(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	input := domain.ChatAssessment{Disposition: "request_work", Action: domain.ChatActionTextSubmission,
		Title: "讨论摘要", Description: "仅生成群聊文本", Acceptance: "包含三条可核对结论", Reason: "结合当前来源与前文明确约定"}
	return chatFixture{f, api, scope, candidate, policy, input}
}

func TestChatIntentExplicitAdmissionAtomicReplayAndReopen(t *testing.T) {
	f := chatSetup(t, "user", false)
	got, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "我会据此形成文本交付。", "chat-call")
	if err != nil {
		t.Fatal(err)
	}
	if got.TaskID == "" || got.MandateID == "" || got.DispatchRequestID == "" || got.NoticeMessageID == "" ||
		got.SourceSenderID != "user" || got.AssessedBy != f.a || got.TurnID != f.scope.Claim.TurnID ||
		got.Authority != "explicit_request" || got.PolicyRevision != 1 || got.CandidateGeneration != f.candidate.Generation ||
		got.AuthorizationMessageID != f.candidate.MessageID {
		t.Fatalf("incorrect trusted evidence: %+v", got)
	}
	task, err := f.s.GetTask(ctx, f.groupID, got.TaskID)
	if err != nil || task.Status != domain.TaskStatusOpen || task.ActiveAttemptID != "" || task.Acceptance != f.input.Acceptance {
		t.Fatalf("task=%+v err=%v", task, err)
	}
	dispatch, err := f.s.GetDispatchRequest(ctx, f.groupID, got.DispatchRequestID)
	if err != nil || dispatch.Request.TaskID != got.TaskID || dispatch.State.Status != domain.DispatchStatusPending {
		t.Fatalf("dispatch=%+v err=%v", dispatch, err)
	}
	notice, err := f.s.GetMessage(ctx, f.groupID, got.NoticeMessageID)
	if err != nil || notice.SenderID != f.a || notice.TopicID != f.scope.TopicID || len(notice.Deliveries) != 0 || !strings.Contains(notice.Content, got.TaskID) || !strings.Contains(notice.Content, got.DispatchRequestID) {
		t.Fatalf("notice=%+v err=%v", notice, err)
	}
	policy := f.policy.ChatPolicyRequest
	policy.Enabled = false
	if _, err := f.api.SetChatPolicy(ctx, f.candidate.ThreadRef, policy, 1, "disable"); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	for _, api := range []chatIntakeAPI{f.api, chatIntake(t, queueHandle(t, f.path, false))} {
		replayed, err := api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "我会据此形成文本交付。", "chat-call")
		if err != nil || !reflect.DeepEqual(replayed, got) {
			t.Fatalf("immutable replay=%+v err=%v", replayed, err)
		}
	}
	changed := f.input
	changed.Acceptance += "changed"
	if _, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, changed, "我会据此形成文本交付。", "chat-call"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed key payload: %v", err)
	}
	if _, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "我会据此形成文本交付。", "new-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("candidate duplicated: %v", err)
	}
	messageSequencePreserved(t, f.s, before)
	dispatchAssertFK(t, f.s)
}

func TestChatIntentQueuedRequestSurvivesEarlierDiscussionReply(t *testing.T) {
	for _, change := range []string{"none", "before-capture", "after-capture", "other-group", "other-topic"} {
		t.Run(change, func(t *testing.T) {
			f := chatSetupSource(t, "user", false, "先讨论摘要可能的结构，暂时不要形成任务。")
			queueEvent(t, f.s, f.scope.Claim, provider.AcceptedEvent, "", "")
			request, err := f.s.CreateLocalUserMessage(ctx, MessageRequest{GroupID: f.groupID, TopicID: f.scope.TopicID, ActorID: "user",
				Content: "现在按刚才的边界形成摘要，交付三条可核对的结论。", RecipientIDs: []string{f.a}}, []string{f.a})
			if err != nil {
				t.Fatal(err)
			}
			claimInput := ClaimTurnInput{GroupID: f.groupID, DeliveryID: request.Deliveries[0].ID, MaxActive: 1, ResourceKeys: []string{"chat-resource"}}
			if _, err := f.s.ClaimTurn(ctx, claimInput); !errors.Is(err, ErrTurnBusy) {
				t.Fatalf("queued request bypassed the earlier active turn: %v", err)
			}
			queued, err := f.s.GetMessage(ctx, f.groupID, request.ID)
			if err != nil || queued.Deliveries[0].State != domain.DeliveryStateQueued || queued.Deliveries[0].TurnID != "" {
				t.Fatalf("request did not remain queued: %+v err=%v", queued, err)
			}
			queueEvent(t, f.s, f.scope.Claim, provider.ResponseEvent, "讨论可以归纳为背景、结论和待核对项。", "")
			queueEvent(t, f.s, f.scope.Claim, provider.TerminalEvent, "", provider.OutcomeSucceeded)
			earlier, err := f.s.ApplyTurnReceipt(ctx, f.scope.Claim, queueReceipt(provider.OutcomeSucceeded))
			if err != nil || earlier.State != domain.TurnStateSucceeded || earlier.ReplyMessageID == "" {
				t.Fatalf("earlier discussion did not publish its reply: state=%s err=%v", earlier.State, err)
			}
			var sourceSequence, replySequence int64
			if err := f.s.db.QueryRow("SELECT sequence FROM message_log WHERE message_id=?", request.ID).Scan(&sourceSequence); err != nil {
				t.Fatal(err)
			}
			if err := f.s.db.QueryRow("SELECT sequence FROM message_log WHERE message_id=?", earlier.ReplyMessageID).Scan(&replySequence); err != nil || replySequence <= sourceSequence {
				t.Fatalf("reply did not commit after queued request: source=%d reply=%d err=%v", sourceSequence, replySequence, err)
			}
			newUser := func(groupID, topicID string) {
				t.Helper()
				if _, err := f.s.CreateLocalUserMessage(ctx, MessageRequest{GroupID: groupID, TopicID: topicID, ActorID: "user", Content: "先不要落实，约束变了。"}, nil); err != nil {
					t.Fatal(err)
				}
			}
			if change == "before-capture" {
				newUser(f.groupID, f.scope.TopicID)
			}
			turn, err := f.s.ClaimTurn(ctx, claimInput)
			if err != nil {
				t.Fatal(err)
			}
			scope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := f.api.CaptureChatCandidate(ctx, scope)
			if err != nil || candidate.MessageID != request.ID || candidate.SourceSenderID != "user" || candidate.Generation != sourceSequence {
				t.Fatalf("queued request lost its actual source: %+v err=%v", candidate, err)
			}
			history, err := f.s.ReadChannelForTool(ctx, scope, ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{f.candidate.MessageID, request.ID, earlier.ReplyMessageID} {
				if !slices.ContainsFunc(history.Items, func(m domain.Message) bool { return m.ID == id }) {
					t.Fatal("bound channel history omitted the request or earlier discussion")
				}
			}
			switch change {
			case "after-capture":
				newUser(f.groupID, f.scope.TopicID)
			case "other-group":
				newUser(f.otherID, f.scope.TopicID)
			case "other-topic":
				newUser(f.groupID, "other-topic")
			}
			t.Logf("source sequence=%d, earlier reply sequence=%d, candidate generation=%d, new user=%s", sourceSequence, replySequence, candidate.Generation, change)
			got, err := f.api.ApplyChatAssessment(ctx, scope, candidate.ID, f.input, "我会据此形成文本交付。", "queued-request")
			if change == "before-capture" || change == "after-capture" {
				chatAssertNotAdmitted(t, got, err, "stale_source")
				return
			}
			if err != nil {
				t.Fatalf("latest user request rejected after earlier agent reply: %v", err)
			}
			if got.TaskID == "" || got.DispatchRequestID == "" || got.MessageID != request.ID || got.AuthorizationMessageID != request.ID ||
				got.SourceSenderID != "user" || got.AssessedBy != f.a || got.TurnID != turn.ID || got.Authority != "explicit_request" ||
				got.CandidateGeneration != sourceSequence || got.PolicyRevision != f.policy.Revision {
				t.Fatalf("queued admission lost trusted evidence: %+v", got)
			}
			dispatchAssertFK(t, f.s)
		})
	}
}

func TestChatIntentMemberDiscussionPreservesUserAuthority(t *testing.T) {
	for _, sender := range []string{"user", "agent"} {
		t.Run(sender, func(t *testing.T) {
			f := chatSetup(t, sender, sender == "agent")
			if sender == "agent" {
				f.input.Disposition = "prepare_work"
				f.input.Title, f.input.Description, f.input.Acceptance = "", "", ""
			}
			public, err := f.s.SendMessageForTool(ctx, f.scope, MemberToolMessageInput{Content: "补充讨论，不代替用户更改要求。"}, "public-reply")
			if err != nil || public.SenderID != f.a || len(public.Deliveries) != 0 {
				t.Fatalf("bound public reply failed: %+v err=%v", public, err)
			}
			invitation, err := f.s.SendMessageForTool(ctx, f.scope, MemberToolMessageInput{Content: "请补充文本结构建议。", RecipientIDs: []string{f.b}}, "peer-invitation")
			if err != nil {
				t.Fatal(err)
			}
			peer, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: invitation.Deliveries[0].ID, MaxActive: 2})
			if err != nil {
				t.Fatal(err)
			}
			peerScope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: peer.ID, Token: peer.ClaimToken})
			if err != nil {
				t.Fatal(err)
			}
			discussion, err := f.s.SendMessageForTool(ctx, peerScope, MemberToolMessageInput{Content: "我的建议是将结论分成三点，这不是用户批准。"}, "peer-discussion")
			if err != nil || discussion.SenderID != f.b || len(discussion.Deliveries) != 0 {
				t.Fatalf("bound peer discussion failed: %+v err=%v", discussion, err)
			}
			captured, err := f.api.CaptureChatCandidate(ctx, f.scope)
			if err != nil || !reflect.DeepEqual(captured, f.candidate) {
				t.Fatalf("member discussion changed immutable candidate: %+v err=%v", captured, err)
			}
			got, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "仍按实际用户来源或书面预授权准备文本。", "after-discussion")
			if err != nil {
				t.Fatalf("member discussion incorrectly superseded user authority: %v", err)
			}
			wantAuthority, wantSource, wantAuthorization := "explicit_request", "user", f.candidate.MessageID
			if sender == "agent" {
				wantAuthority, wantSource, wantAuthorization = "written_preauthorization", f.b, f.policy.PreauthorizationMessageID
			}
			if got.TaskID == "" || got.SourceSenderID != wantSource || got.AssessedBy != f.a || got.Authority != wantAuthority ||
				got.AuthorizationMessageID != wantAuthorization || got.CandidateGeneration != f.candidate.Generation || got.PolicyRevision != f.policy.Revision {
				t.Fatalf("discussion replaced trusted authority: %+v", got)
			}
		})
	}
}

func TestChatIntentDeterministicAuthorityAndStaleFences(t *testing.T) {
	for _, scenario := range []string{"agent-consensus", "disabled", "policy-revision", "generation", "preparation-generation", "dispatch-policy", "coordinator-binding", "wrong-group", "wrong-topic", "wrong-member", "wrong-claim", "cancelled", "unknown", "file-action"} {
		t.Run(scenario, func(t *testing.T) {
			sender := "user"
			if scenario == "agent-consensus" || scenario == "preparation-generation" {
				sender = "agent"
			}
			f := chatSetup(t, sender, scenario == "preparation-generation")
			if scenario == "preparation-generation" {
				f.input.Disposition = "prepare_work"
				f.input.Title, f.input.Description, f.input.Acceptance = "", "", ""
			}
			want := ErrConflict
			switch scenario {
			case "disabled", "policy-revision":
				input := f.policy.ChatPolicyRequest
				input.Enabled = scenario != "disabled"
				if _, err := f.api.SetChatPolicy(ctx, f.candidate.ThreadRef, input, 1, "new-policy"); err != nil {
					t.Fatal(err)
				}
			case "generation", "preparation-generation":
				if _, err := f.s.CreateLocalUserMessage(ctx, MessageRequest{GroupID: f.groupID, TopicID: f.scope.TopicID, ActorID: "user", Content: "先不要落实，约束变了。"}, nil); err != nil {
					t.Fatal(err)
				}
			case "dispatch-policy":
				if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.b}, 1, "new-dispatch"); err != nil {
					t.Fatal(err)
				}
			case "coordinator-binding":
				if _, err := f.s.SetCoordinator(ctx, f.groupID, f.a, 1, "new-coordinator"); err != nil {
					t.Fatal(err)
				}
			case "wrong-group":
				f.scope.Claim.GroupID = f.otherID
				want = ErrMemberToolDenied
			case "wrong-topic":
				f.scope.TopicID += "wrong"
				want = ErrMemberToolDenied
			case "wrong-member":
				f.scope.MemberID = f.b
				want = ErrMemberToolDenied
			case "wrong-claim":
				f.scope.Claim.Token += "wrong"
				want = ErrMemberToolDenied
			case "cancelled":
				if _, err := f.s.RequestTurnCancel(ctx, f.groupID, f.scope.Claim.TurnID); err != nil {
					t.Fatal(err)
				}
				want = ErrMemberToolDenied
			case "unknown":
				if _, err := f.s.db.Exec("UPDATE turns SET state='unknown' WHERE id=?", f.scope.Claim.TurnID); err != nil {
					t.Fatal(err)
				}
				want = ErrMemberToolDenied
			case "file-action":
				f.input.Action = "write_files"
				want = ErrInvalid
			}
			before := messageSequenceSnapshot(t, f.s)
			got, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "评估回复", "assessment")
			if want != ErrMemberToolDenied {
				codes := map[string]string{"agent-consensus": "user_request_required", "disabled": "policy_disabled", "policy-revision": "stale_policy", "generation": "stale_source", "preparation-generation": "stale_source", "dispatch-policy": "stale_dispatch_configuration", "coordinator-binding": "stale_dispatch_configuration", "file-action": "unsupported_action"}
				chatAssertNotAdmitted(t, got, err, codes[scenario])
				after := messageSequenceSnapshot(t, f.s)
				for _, table := range []string{"tasks", "mandates", "routing_decisions", "coordination_dispatch_requests", "coordination_triggers", "resource_claims"} {
					if !reflect.DeepEqual(before[table], after[table]) {
						t.Fatalf("non-admission changed %s", table)
					}
				}
				return
			}
			if !errors.Is(err, want) || !reflect.ValueOf(got).IsZero() {
				t.Fatalf("gate got=%+v err=%v want=%v", got, err, want)
			}
			messageSequencePreserved(t, f.s, before)
		})
	}
}

func TestChatIntentPreparationRequiresEnabledWrittenUserPolicy(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		f := chatSetup(t, "agent", enabled)
		f.input.Disposition = "prepare_work"
		f.input.Title, f.input.Description, f.input.Acceptance = "", "", ""
		got, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "准备文本草稿", "preparation")
		if !enabled {
			chatAssertNotAdmitted(t, got, err, "preparation_not_authorized")
		} else if err != nil || got.Authority != "written_preauthorization" || got.SourceSenderID != f.b || got.AssessedBy != f.a || got.AuthorizationMessageID != f.policy.PreauthorizationMessageID {
			t.Fatalf("preauthorized work=%+v err=%v", got, err)
		}
	}
}

func TestChatIntentDiscussionNeverAdmitsAndDispatchFailureRollsBack(t *testing.T) {
	for _, disposition := range []string{"discuss", "clarify", "request_work"} {
		f := chatSetup(t, "user", false)
		f.input.Disposition = disposition
		if disposition == "clarify" {
			f.input.Question = "需要哪三条结论？"
		}
		if disposition != "request_work" {
			f.input.Action, f.input.Title, f.input.Description, f.input.Acceptance = "", "", "", ""
		}
		if disposition == "request_work" {
			if _, err := f.s.db.Exec(`CREATE TRIGGER chat_dispatch_failure BEFORE INSERT ON coordination_dispatch_state BEGIN SELECT RAISE(ABORT, 'synthetic failure'); END`); err != nil {
				t.Fatal(err)
			}
		}
		before := messageSequenceSnapshot(t, f.s)
		got, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "普通讨论回复", "discussion")
		if disposition == "request_work" {
			if err == nil || got.TaskID != "" {
				t.Fatalf("failed dispatch admitted work=%+v err=%v", got, err)
			}
			messageSequencePreserved(t, f.s, before)
		} else if err != nil || got.TaskID != "" || got.MandateID != "" || got.DispatchRequestID != "" || got.NoticeMessageID == "" {
			t.Fatalf("discussion admitted work=%+v err=%v", got, err)
		}
	}
}

func TestChatIntentPolicyEvidenceBudgetAndImmutability(t *testing.T) {
	f := chatSetup(t, "user", true)
	originalPolicyInput := f.policy.ChatPolicyRequest
	originalPolicyInput.PreauthorizationMessageID = ""
	if _, err := f.api.SetChatPolicy(ctx, f.candidate.ThreadRef, originalPolicyInput, 0, "policy"); err != nil {
		t.Fatalf("same policy key replay failed: %v", err)
	}
	for _, source := range []struct{ groupID, topicID, actor string }{{f.groupID, f.scope.TopicID, f.b}, {f.otherID, f.scope.TopicID, "user"}, {f.groupID, "other-topic", "user"}} {
		m, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: source.groupID, TopicID: source.topicID, ActorID: source.actor, Content: "不可信或范围不同的书面授权"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		input := f.policy.ChatPolicyRequest
		input.PreauthorizationMessageID = m.ID
		if _, err := f.api.SetChatPolicy(ctx, f.candidate.ThreadRef, input, 1, "bad-written-source"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("wrong written policy source accepted: %v", err)
		}
	}
	// Capture a fresh actual message after the deliberately unrelated sources.
	for i := 0; i < 3; i++ {
		if _, err := f.s.ApplyTurnReceipt(ctx, f.scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
			t.Fatal(err)
		}
		message, err := f.s.CreateLocalUserMessage(ctx, MessageRequest{GroupID: f.groupID, TopicID: f.scope.TopicID, ActorID: "user", Content: "请形成另一份文本摘要", RecipientIDs: []string{f.a}}, []string{f.a})
		if err != nil {
			t.Fatal(err)
		}
		turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: message.Deliveries[0].ID, MaxActive: 1})
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
		got, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "文本回复", "call-"+string(rune('a'+i)))
		if i < 2 && (err != nil || got.TaskID == "") {
			t.Fatalf("authorized budget item %d: %+v %v", i, got, err)
		}
		if i == 2 {
			chatAssertNotAdmitted(t, got, err, "budget_exhausted")
		}
	}
	for table, assignment := range map[string]string{"chat_policy_revisions": "enabled=enabled", "chat_candidates": "id=id", "chat_assessments": "id=id"} {
		for _, query := range []string{"UPDATE " + table + " SET " + assignment, "DELETE FROM " + table, "INSERT OR REPLACE INTO " + table + " SELECT * FROM " + table + " LIMIT 1"} {
			if _, err := f.s.db.Exec(query); err == nil {
				t.Fatalf("immutable chat evidence changed: %s", query)
			}
		}
	}
}

func TestChatIntentConcurrentSameKeyReplay(t *testing.T) {
	f := chatSetup(t, "user", false)
	type outcome struct {
		receipt domain.ChatAssessmentReceipt
		err     error
	}
	results := make(chan outcome, 2)
	for _, api := range []chatIntakeAPI{f.api, chatIntake(t, queueHandle(t, f.path, false))} {
		go func(api chatIntakeAPI) {
			receipt, err := api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "同一请求的文本回复", "concurrent-key")
			results <- outcome{receipt, err}
		}(api)
	}
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || !reflect.DeepEqual(first.receipt, second.receipt) || first.receipt.TaskID == "" {
		t.Fatalf("same-key concurrent intake diverged: %+v %+v", first, second)
	}
	for _, table := range []string{"tasks", "mandates", "routing_decisions", "coordination_dispatch_requests", "chat_assessments"} {
		var count int
		if err := f.s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("concurrent duplicate %s: %d %v", table, count, err)
		}
	}
}

func TestChatIntentV16MigrationAtomicAndRetainsUnknownClaims(t *testing.T) {
	original := migrations
	defer func() { migrations = original }()
	migrations = original[:15]
	f, turn, claim := queueClaimFixture(t)
	if _, err := f.s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	migrations = slices.Clone(original)
	migrations[15] += "\nINSERT INTO synthetic_missing_chat_migration VALUES(1);"
	if err := migrate(f.s.db); err == nil {
		t.Fatal("failed v16 migration committed")
	}
	messageSequenceLedger(t, f.s, 15)
	messageSequencePreserved(t, f.s, before)
	var leaked int
	if err := f.s.db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name LIKE 'chat_%'").Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("partial migration leaked %d objects: %v", leaked, err)
	}
	migrations = original
	if err := migrate(f.s.db); err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, f.s, len(original))
	messageSequencePreserved(t, f.s, before)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, f.path, false)
	got, err := reopened.GetTurn(ctx, turn.GroupID, turn.ID)
	if err != nil || got.State != domain.TurnStateUnknown {
		t.Fatalf("migration erased native uncertainty: %+v %v", got, err)
	}
	var held int
	if err := reopened.db.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", turn.ID).Scan(&held); err != nil || held == 0 {
		t.Fatalf("migration released unknown claims: %d %v", held, err)
	}
	dispatchAssertFK(t, reopened)
}

func TestChatIntentSchemaPreservesVersionedEvidence(t *testing.T) {
	s, _ := openTestStore(t)
	for _, table := range []string{"chat_policy_revisions", "chat_policies", "chat_candidates", "chat_assessments", "chat_submission_notices"} {
		var exists bool
		if err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name=?)", table).Scan(&exists); err != nil || !exists {
			t.Fatalf("missing durable chat intent evidence %s: exists=%t err=%v", table, exists, err)
		}
	}
}
