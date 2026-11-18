package store

import (
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestChatInvestigationExplicitFollowupFence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state domain.TurnState
		body  string
	}{
		{"exact-task-id", domain.TurnStateCancelRequested, ""},
		{"original-task-cue", domain.TurnStateUnknown, "请继续原任务，只核对源材料。"},
		{"original-task-compact", domain.TurnStateUnknown, "请继续原任务并核对源材料"},
		{"original-task-compact-check", domain.TurnStateUnknown, "请继续原任务并检查源材料"},
		{"original-task-compact-format", domain.TurnStateUnknown, "请续办原任务并保持旧格式"},
		{"original-task-then", domain.TurnStateUnknown, "\t\u3000继续原任务然后\u00a0确认这句话是否准确。附件里的这句话只是旧消息示例。\n"},
		{"original-task-space", domain.TurnStateUnknown, "请继续原任务\u3000保持旧格式"},
		{"original-task-punctuation", domain.TurnStateUnknown, "\t请 续办原任务: 保持旧格式。\n"},
		{"original-task-end", domain.TurnStateUnknown, "\t续办原任务吧\u3000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, policy := investigationFollowupFixture(t, 2)
			worker := chatAuthorityWorker(t, f)
			claim := worker.Member.Claim
			queueEvent(t, f.s, claim, provider.AcceptedEvent, "", "")
			turn, err := f.s.RequestTurnCancel(ctx, f.groupID, claim.TurnID)
			if err == nil && tc.state == domain.TurnStateUnknown {
				turn, err = f.s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown})
			}
			if err != nil || turn.State != tc.state {
				t.Fatalf("original worker state = %s, %v; want %s", turn.State, err, tc.state)
			}
			if claims := messageSequenceTableRows(t, f.s, "resource_claims"); len(claims) != 2 {
				t.Fatalf("fixture did not retain the coordinator and original worker claims: %v", claims)
			}
			taskID := worker.Provenance.TaskID
			assessor := member(t, f.s, f.groupID).ID
			local := func(body string) domain.Message {
				t.Helper()
				return chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: policy.TopicID,
					ActorID: "user", Content: body, RecipientIDs: []string{assessor}}, []string{assessor})
			}
			body := tc.body
			if body == "" {
				body = "请接着处理任务`" + taskID + "`，只核对源材料。"
			}
			source := local(body)
			scope, candidate := discussionArrive(t, f.s, f.groupID, source.Deliveries[0].ID)
			before := messageSequenceSnapshot(t, f.s)
			for _, table := range []string{"messages", "message_log", "chat_assessments", "mutation_idempotency"} {
				delete(before, table) // A denial may append only its receipt and public notice.
			}
			proposal := investigationProposal()
			const reply = "建议调查。"
			denied, err := f.s.ApplyChatAssessment(ctx, scope, candidate, proposal, reply, "misassessed-followup")
			var used int
			if countErr := f.s.db.QueryRow(`SELECT count(*) FROM chat_assessments a JOIN chat_candidates c ON c.id=a.candidate_id
			 WHERE c.group_id=? AND c.topic_id=? AND c.policy_revision=? AND a.decision_id IS NOT NULL`, f.groupID, policy.TopicID, policy.Revision).Scan(&used); countErr != nil {
				t.Fatal(countErr)
			}
			t.Logf("original_task=%s state=%s outcome=%s reason=%s task=%q mandate=%q dispatch=%q notice=%s budget=%d/%d", taskID,
				turn.State, denied.Outcome, denied.ReasonCode, denied.TaskID, denied.MandateID, denied.DispatchRequestID, denied.NoticeMessageID, used, policy.MaxAdmissions)
			chatAssertNotAdmitted(t, denied, err, "investigation_followup_requires_clarification")
			if used != 1 || denied.ThreadRef != policy.ThreadRef || denied.PolicyRevision != policy.Revision || denied.MessageID != source.ID ||
				denied.SourceKind != "local_user_chat" || denied.SourceID != source.SourceID || denied.Authority != "" || denied.AuthorityEvidenceVerified {
				t.Fatalf("denial consumed budget or changed source/authority: %+v, used=%d", denied, used)
			}
			notice, err := f.s.GetMessage(ctx, f.groupID, denied.NoticeMessageID)
			if err != nil || notice.TopicID != policy.TopicID || len(notice.Deliveries) != 0 ||
				strings.Count(notice.Content, "？") != 1 || !strings.HasSuffix(notice.Content, denied.Question) {
				t.Fatalf("clarification was not a single public question in the original topic: %+v, %v", notice, err)
			}
			messageSequencePreserved(t, f.s, before)
			after := messageSequenceSnapshot(t, f.s)
			if replay, err := f.s.ApplyChatAssessment(ctx, scope, candidate, proposal, reply, "misassessed-followup"); err != nil || !reflect.DeepEqual(replay, denied) {
				t.Fatalf("denial did not replay: %+v, %v", replay, err)
			}
			messageSequencePreserved(t, f.s, after)
			discussionEnd(t, f.s, scope)

			// A title, a larger token, and different casing are not exact Task references.
			fresh := local("请另开只读调查，收集「" + f.input.Title + "」的资料。示例编号：" + taskID + "X、" + strings.ToLower(taskID))
			proposal.Reason = "请继续原任务：" + taskID // The assessor's text is not the user's command.
			investigationAssertFollowupControl(t, f.s, fresh, proposal, policy, "local_user_chat")
		})
	}

	t.Run("local-phrasing-controls", func(t *testing.T) {
		for _, tc := range []struct{ name, body string }{
			{"unquoted-meta", "请继续原任务这句话只是旧消息示例，并非本次指令；请按现有策略另开只读调查。"},
			{"coordinated-meta", "请继续原任务并核对源材料这句话只是旧消息示例，并非本次指令。请按现有策略另开只读调查。"},
			{"normalized-meta", "\t\u3000请继续原任务，\u3000这句话 只是示例；请按现有策略另开只读调查。"},
			{"connective-meta", "请继续原任务并非本次指令；请按现有策略另开只读调查。"},
			{"connective-negation", "请继续原任务并不是现在的安排；请按现有策略另开只读调查。"},
			{"bare-connective", "请继续原任务并且。请按现有策略另开只读调查。"},
			{"quoted", "“请继续原任务并核对源材料。”是旧消息。请按现有策略另开只读调查。"},
			{"negated", "请不要继续原任务；请按现有策略另开只读调查。"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f, policy, _ := investigationFollowupPriorTask(t)
				source := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: policy.TopicID, ActorID: "user",
					Content: tc.body}, []string{f.a})
				investigationAssertFollowupControl(t, f.s, source, investigationProposal(), policy, "local_user_chat")
			})
		}
	})

	t.Run("source-and-topic-controls", func(t *testing.T) {
		f, policy, taskID := investigationFollowupPriorTask(t)
		invitation := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: policy.TopicID, ActorID: "user",
			Content: "请提出调查建议。"}, []string{f.b})
		peer, _ := discussionArrive(t, f.s, f.groupID, invitation.Deliveries[0].ID)
		proposal := investigationProposal()
		msg, err := f.s.SendMessageForTool(ctx, peer, MemberToolMessageInput{Content: "建议参照既有任务 " + taskID + " 展开调查。",
			RecipientIDs: []string{f.a}}, "member-investigation-proposal")
		if err != nil {
			t.Fatal(err)
		}
		investigationAssertFollowupControl(t, f.s, msg, proposal, policy, "member")

		foreignRef := func(other domain.ThreadRef, assessor string) {
			t.Helper()
			otherPolicy, err := f.s.SetChatPolicy(ctx, other, policy.ChatPolicyRequest, 0, "other-thread-investigation")
			if err != nil {
				t.Fatal(err)
			}
			source := chatLocalMessage(t, f.s, MessageRequest{GroupID: other.GroupID, TopicID: other.TopicID, ActorID: "user",
				Content: "请继续原任务：" + taskID}, []string{assessor})
			investigationAssertFollowupControl(t, f.s, source, proposal, otherPolicy, "local_user_chat")
		}
		foreignRef(domain.ThreadRef{GroupID: f.groupID, TopicID: policy.TopicID + "-other"}, f.a)
		if _, err := f.s.SetCoordinator(ctx, f.otherID, f.foreign, 0, "coordinator"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.SetDispatchPolicy(ctx, f.otherID, []string{member(t, f.s, f.otherID).ID}, 0, "dispatch-policy"); err != nil {
			t.Fatal(err)
		}
		foreignRef(domain.ThreadRef{GroupID: f.otherID, TopicID: policy.TopicID}, f.foreign)
	})
}

func investigationFollowupPriorTask(t *testing.T) (chatFixture, domain.ChatPolicy, string) {
	t.Helper()
	f, policy := investigationFollowupFixture(t, 2)
	prior, err := f.s.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "文本任务。", "prior-task")
	if err != nil || prior.TaskID == "" || prior.Outcome != "admitted" {
		t.Fatalf("prior work was not admitted: %+v, %v", prior, err)
	}
	investigationFinishTurn(t, f)
	return f, policy, prior.TaskID
}

func investigationFollowupFixture(t *testing.T, budget int) (chatFixture, domain.ChatPolicy) {
	t.Helper()
	f := chatSetup(t, "user", false)
	policy := investigationPolicy(t, f, domain.ChatInvestigationPolicy{Objective: investigationObjective, AllowTopicMessages: true}, budget)
	f.s.EnableInvestigationExecution()
	chatNextCandidate(t, &f, true)
	return f, policy
}

func investigationAssertFollowupControl(t *testing.T, s *Store, source domain.Message, proposal domain.ChatAssessment, policy domain.ChatPolicy, sourceKind string) {
	t.Helper()
	scope, candidate := discussionArrive(t, s, policy.GroupID, source.Deliveries[0].ID)
	defer discussionEnd(t, s, scope)
	r, err := s.ApplyChatAssessment(ctx, scope, candidate, proposal, "建议调查。", "independent-investigation-"+candidate)
	if err != nil || r.Outcome != "admitted" || r.TaskID == "" || r.MandateID == "" || r.DispatchRequestID == "" ||
		r.ThreadRef != policy.ThreadRef || r.PolicyRevision != policy.Revision || r.SourceKind != sourceKind || r.SourceID != source.SourceID ||
		r.Authority != "written_preauthorization" || !r.AuthorityEvidenceVerified {
		t.Fatalf("independent policy-only investigation was not admitted: %+v, %v", r, err)
	}
	t.Logf("control group=%s topic=%s source=%s outcome=%s task=%s", r.GroupID, r.TopicID, r.SourceKind, r.Outcome, r.TaskID)
}
