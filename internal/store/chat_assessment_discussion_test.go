package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

const discussionOnlyQuote = "本次只讨论"

func TestChatDiscussionOnlySharedRoot(t *testing.T) {
	f := chatSetup(t, "user", true)
	discussionEnd(t, f.s, f.scope)
	root := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: f.policy.TopicID,
		ActorID: "user", Content: discussionOnlyQuote + "，先讨论摘要框架。"}, []string{f.a, f.b})
	if len(root.Deliveries) != 2 {
		t.Fatalf("expected two deliveries for a single root: %+v", root)
	}
	a, ca := discussionArrive(t, f.s, f.groupID, root.Deliveries[0].ID)
	b, cb := discussionArrive(t, f.s, f.groupID, root.Deliveries[1].ID)
	limit := domain.ChatAssessment{Disposition: "discuss", Reason: "讨论框架。", DiscussionOnlyExcerpt: new(discussionOnlyQuote)}
	original, err := f.s.ApplyChatAssessment(ctx, a, ca, limit, "先讨论。", "restrict-root")
	if err != nil || !original.DiscussionOnly || original.DiscussionOnlyExcerpt != discussionOnlyQuote || original.SourceID != root.SourceID {
		t.Fatalf("root restriction was not verified: %+v, %v", original, err)
	}

	proposal := domain.ChatAssessment{Disposition: "prepare_work", Action: domain.ChatActionTextSubmission, Reason: "建议准备话题摘要。"}
	checkDenied := func(scope MemberToolScope, candidate string, deliveries int64) {
		t.Helper()
		denied, err := f.s.ApplyChatAssessment(ctx, scope, candidate, proposal, "准备摘要。", "prepare-"+candidate)
		chatAssertNotAdmitted(t, denied, err, "discussion_only")
		notice, err := f.s.GetMessage(ctx, f.groupID, denied.NoticeMessageID)
		if err != nil || len(notice.Deliveries) != 0 || !strings.Contains(notice.Content, root.SourceID) || !strings.Contains(notice.Content, discussionOnlyQuote) {
			t.Fatalf("denial did not cite the exact restriction without a wake: %+v, %v", notice, err)
		}
		counts := messageSequenceRows(t, f.s.db, `SELECT (SELECT count(*) FROM tasks), (SELECT count(*) FROM coordination_assignments),
		 (SELECT count(*) FROM coordination_dispatch_requests), (SELECT count(*) FROM deliveries)`)
		if !reflect.DeepEqual(counts, [][]any{{int64(0), int64(0), int64(0), deliveries}}) {
			t.Fatalf("denial created work or wakes (tasks, assignments, dispatches, deliveries): %v", counts)
		}
	}
	checkDenied(b, cb, 3)
	if replay, err := f.s.ApplyChatAssessment(ctx, a, ca, limit, "先讨论。", "restrict-root"); err != nil || !reflect.DeepEqual(replay, original) {
		t.Fatalf("original restriction did not replay: %+v, %v", replay, err)
	}

	// The other recipient may start a discussion, but cannot shed the limit.
	discussionEnd(t, f.s, a)
	invite := discussionApply(t, f.s, b, cb, "handoff", domain.ChatDiscussionAction{
		Kind: domain.ChatDiscussionInvite, RecipientID: a.MemberID, Content: "请补充摘要框架。"})
	discussionEnd(t, f.s, b)
	child, cc := discussionArrive(t, f.s, f.groupID, invite.Event.DeliveryID)
	checkDenied(child, cc, 4)

	f.scope = child
	chatNextCandidate(t, &f, true)
	admitted, err := f.s.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "按最新要求准备。", "new-user-work")
	if err != nil || admitted.Outcome != "admitted" || admitted.SourceID == root.SourceID || admitted.CandidateGeneration <= original.CandidateGeneration {
		t.Fatalf("separate newer user command was not independently admitted: %+v, %v", admitted, err)
	}
}

func TestChatDiscussionOnlyInvestigationHandoff(t *testing.T) {
	f := chatSetup(t, "user", false)
	policy := investigationPolicy(t, f, domain.ChatInvestigationPolicy{Objective: investigationObjective, AllowTopicMessages: true}, 1)
	f.s.EnableInvestigationExecution()
	discussionEnd(t, f.s, f.scope)

	newOrigin := func(content string) domain.Message {
		return chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: policy.TopicID,
			ActorID: "user", Content: content, RecipientIDs: []string{f.a}}, []string{f.a})
	}
	handoff := func(origin domain.Message, excerpt *string) (MemberToolScope, string, domain.Message) {
		t.Helper()
		a, ca := discussionArrive(t, f.s, f.groupID, origin.Deliveries[0].ID)
		assessment := domain.ChatAssessment{Disposition: "discuss", Reason: "讨论待核对问题。", DiscussionOnlyExcerpt: excerpt}
		r, err := f.s.ApplyChatAssessment(ctx, a, ca, assessment, "先列出待核对的问题。", "assess-"+ca)
		if err != nil || r.Outcome != "discuss" || r.MessageID != origin.ID || r.ThreadRef != policy.ThreadRef ||
			r.SourceKind != "local_user_chat" || r.SourceID != origin.SourceID || r.SourceSenderID != "user" ||
			r.AssessedBy != f.a || r.TurnID != a.Claim.TurnID || r.PolicyRevision != policy.Revision ||
			r.DiscussionOnly != (excerpt != nil) || (excerpt != nil && r.DiscussionOnlyExcerpt != *excerpt) ||
			r.Authority != "" || r.AuthorityEvidenceVerified || r.TaskID != "" {
			t.Fatalf("wrong discussion evidence: %+v, %v", r, err)
		}
		invite := discussionApply(t, f.s, a, ca, "invite-"+ca, domain.ChatDiscussionAction{
			Kind: domain.ChatDiscussionInvite, Content: "请补充待核对的问题。", RecipientID: f.b})
		b, cb := discussionArrive(t, f.s, f.groupID, invite.Event.DeliveryID)
		discussionCheckContext(t, f.s, b, cb, origin, f.a, f.b, invite.Message.ID, invite.Message.ID, invite.Message.ID, false, invite)
		discussionEnd(t, f.s, a)
		return b, cb, invite.Message
	}

	origin := newOrigin(discussionOnlyQuote + "，先不开展调查。有哪些问题待核对？")
	scope, id, invitation := handoff(origin, new(discussionOnlyQuote))
	proposal := investigationProposal()
	const content = "建议调查。"
	denied, err := f.s.ApplyChatAssessment(ctx, scope, id, proposal, content, "restricted")
	chatAssertNotAdmitted(t, denied, err, "discussion_only")
	if denied.ThreadRef != policy.ThreadRef || denied.PolicyRevision != policy.Revision || denied.SourceID != invitation.SourceID ||
		denied.SourceKind != "member" || denied.MessageID != invitation.ID || denied.AssessedBy != f.b || denied.AuthorityEvidenceVerified {
		t.Fatalf("denial changed the assessor or proposal source: %+v", denied)
	}
	notice, err := f.s.GetMessage(ctx, origin.GroupID, denied.NoticeMessageID)
	if err != nil || notice.TopicID != origin.TopicID || notice.SenderID != f.b || len(notice.Deliveries) != 0 ||
		!strings.Contains(notice.Content, origin.ID) || !strings.Contains(notice.Content, origin.SourceID) ||
		!strings.Contains(notice.Content, discussionOnlyQuote) || !strings.HasSuffix(notice.Content, denied.Question) {
		t.Fatalf("notice must cite the exact local-user restriction in the original topic: %+v, %v", notice, err)
	}

	// Mentioning the old wording is not a restriction on the new source.
	ordinary := newOrigin("上次说过『本次只讨论』。这次有哪些问题待核对？")
	replay, err := f.s.ApplyChatAssessment(ctx, scope, id, proposal, content, "restricted")
	if err != nil || !reflect.DeepEqual(replay, denied) {
		t.Fatalf("replay reevaluated an old source: %+v, %v", replay, err)
	}
	discussionEnd(t, f.s, scope)
	scope, id, invitation = handoff(ordinary, nil)
	admitted, err := f.s.ApplyChatAssessment(ctx, scope, id, proposal, content, "ordinary")
	if err != nil || admitted.Outcome != "admitted" || admitted.PolicyRevision != policy.Revision || admitted.TaskID == "" ||
		admitted.SourceID != invitation.SourceID || admitted.Authority != "written_preauthorization" || !admitted.AuthorityEvidenceVerified {
		t.Fatalf("ordinary discussion could not use the same policy and budget: %+v, %v", admitted, err)
	}
	if proved := readDiscussionWork(t, f.s, policy.ThreadRef, invitation.ID, id, scope.Claim.TurnID); proved == nil ||
		proved.TaskID != admitted.TaskID || proved.MandateID != admitted.MandateID || proved.DispatchRequestID != admitted.DispatchRequestID || proved.Assignment != nil {
		t.Fatalf("reader did not prove the admitted, scoped investigation: %+v", proved)
	}
}

func TestChatDiscussionOnlyBlocksTopicSummaryWithBothScopes(t *testing.T) {
	f := chatSetup(t, "user", true)
	f.s.EnableInvestigationExecution()
	policy := investigationPolicy(t, f, domain.ChatInvestigationPolicy{Objective: investigationObjective, AllowTopicMessages: true}, 1)
	if !policy.PreparationSupported || !policy.InvestigationExecutionEnabled || policy.Investigation == nil {
		t.Fatalf("fixture needs both enabled scopes: %+v", policy)
	}
	// Set the policy before capturing the root assessment for the handoff.
	discussionEnd(t, f.s, f.scope)
	origin := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: policy.TopicID, ActorID: "user",
		Content: discussionOnlyQuote + "，先讨论摘要框架。"}, []string{f.a})
	a, ca := discussionArrive(t, f.s, f.groupID, origin.Deliveries[0].ID)
	input := domain.ChatAssessment{Disposition: "discuss", Reason: "讨论框架。", DiscussionOnlyExcerpt: new(discussionOnlyQuote)}
	if r, err := f.s.ApplyChatAssessment(ctx, a, ca, input, "先讨论。", "assess"); err != nil || !r.DiscussionOnly {
		t.Fatalf("restriction was not recorded: %+v, %v", r, err)
	}
	invite := discussionApply(t, f.s, a, ca, "invite", domain.ChatDiscussionAction{
		Kind: domain.ChatDiscussionInvite, Content: "请补充摘要框架。", RecipientID: f.b})
	discussionEnd(t, f.s, a)
	b, cb := discussionArrive(t, f.s, f.groupID, invite.Event.DeliveryID)
	proposal := domain.ChatAssessment{Disposition: "prepare_work", Action: domain.ChatActionTextSubmission, Reason: "建议准备话题摘要。"}
	r, err := f.s.ApplyChatAssessment(ctx, b, cb, proposal, "准备摘要。", "summary")
	chatAssertNotAdmitted(t, r, err, "discussion_only")
	if proved := readDiscussionWork(t, f.s, policy.ThreadRef, invite.Message.ID, cb, b.Claim.TurnID); proved != nil {
		t.Fatalf("reader mistook a denial for work: %+v", proved)
	}
	notice, err := f.s.ReadHistory(ctx, f.groupID, &policy.TopicID, r.NoticeMessageID)
	if err != nil || len(notice.Deliveries) != 0 || !strings.Contains(notice.Content, discussionOnlyQuote) {
		t.Fatalf("missing public restriction notice: %+v, %v", notice, err)
	}

	// A separately delivered, explicit user command in the same topic neither
	// inherits the restriction nor spends budget on behalf of the invitation.
	f.scope = b
	chatNextCandidate(t, &f, true)
	got, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "按最新要求准备。", "new-user-work")
	if err != nil || got.Outcome != "admitted" || got.Authority != "explicit_request" || got.AuthorizationMessageID != f.candidate.MessageID {
		t.Fatalf("new user command was not independently admitted: %+v, %v", got, err)
	}
}

func TestChatDiscussionOnlyInvestigationSourceDistinctions(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "newer-user-before-handoff"
		if legacy {
			name = "legacy-member-without-lineage"
		}
		t.Run(name, func(t *testing.T) {
			f := chatSetup(t, "user", false)
			policy := investigationPolicy(t, f, domain.ChatInvestigationPolicy{Objective: investigationObjective, AllowTopicMessages: true}, 1)
			f.s.EnableInvestigationExecution()
			discussionEnd(t, f.s, f.scope)
			origin := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: policy.TopicID,
				ActorID: "user", Content: discussionOnlyQuote, RecipientIDs: []string{f.a}}, []string{f.a})
			a, ca := discussionArrive(t, f.s, f.groupID, origin.Deliveries[0].ID)
			assessment := domain.ChatAssessment{Disposition: "discuss", Reason: "讨论问题。", DiscussionOnlyExcerpt: new(discussionOnlyQuote)}
			if _, err := f.s.ApplyChatAssessment(ctx, a, ca, assessment, "先讨论。", "assess"); err != nil {
				t.Fatal(err)
			}
			chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: policy.TopicID, ActorID: "user",
				Content: "最新要求：现在可以按本话题策略准备只读调查。"}, nil)

			var deliveryID string
			if legacy {
				msg, err := f.s.SendMessageForChatIntake(ctx, a, ca, MemberToolMessageInput{Content: "请提出意见。", RecipientIDs: []string{f.b}}, "invite")
				if err != nil || len(msg.Deliveries) != 1 || msg.SourceKind != "member" || msg.SourceID == "" {
					t.Fatalf("legacy member command: %+v, %v", msg, err)
				}
				deliveryID = msg.Deliveries[0].ID
			} else {
				invite := discussionApply(t, f.s, a, ca, "invite", domain.ChatDiscussionAction{
					Kind: domain.ChatDiscussionInvite, Content: "请提出意见。", RecipientID: f.b})
				deliveryID = invite.Event.DeliveryID
			}
			b, cb := discussionArrive(t, f.s, f.groupID, deliveryID)
			got, err := f.s.ApplyChatAssessment(ctx, b, cb, investigationProposal(), "建议调查。", "investigation")
			if !legacy {
				chatAssertNotAdmitted(t, got, err, "stale_source")
			} else if err != nil || got.TaskID == "" || got.Authority != "written_preauthorization" || got.SourceKind != "member" {
				t.Fatalf("legacy proposal did not retain baseline policy-only admission: %+v, %v", got, err)
			}
		})
	}
}

func TestChatDiscussionOnlyRejectsUnprovenExcerpt(t *testing.T) {
	for _, failure := range []string{"empty", "different-quote", "wrong-disposition", "member", "sender-only", "stale-user"} {
		t.Run(failure, func(t *testing.T) {
			sender := "user"
			if failure == "member" {
				sender = "agent"
			}
			f := chatSetupSource(t, sender, false, discussionOnlyQuote)
			input := domain.ChatAssessment{Disposition: "discuss", Reason: "讨论。", DiscussionOnlyExcerpt: new(discussionOnlyQuote)}
			want := ErrInvalid
			switch failure {
			case "empty":
				input.DiscussionOnlyExcerpt = new("")
			case "different-quote":
				input.DiscussionOnlyExcerpt = new("本次只讨论。")
			case "wrong-disposition":
				input.Disposition, input.Question = "clarify", "请确认本次范围？"
			case "sender-only":
				discussionEnd(t, f.s, f.scope)
				legacy, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: f.groupID, TopicID: f.scope.TopicID,
					ActorID: "user", Content: discussionOnlyQuote}, []string{f.a})
				if err != nil {
					t.Fatal(err)
				}
				investigationBindCandidate(t, &f, legacy.Deliveries[0])
			case "stale-user":
				chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: f.scope.TopicID,
					ActorID: "user", Content: "改按我现在的要求。"}, nil)
				want = ErrConflict
			}
			before := messageSequenceSnapshot(t, f.s)
			if r, err := f.s.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, input, "评估。", "unproven"); !errors.Is(err, want) || r.ID != "" {
				t.Fatalf("unverified excerpt was accepted: %+v, %v (want %v)", r, err, want)
			}
			messageSequencePreserved(t, f.s, before)
		})
	}
}
