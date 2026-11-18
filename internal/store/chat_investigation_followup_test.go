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
		{"original-task-compact", domain.TurnStateUnknown, "请继续原任务并核对源材料。"},
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
			if err != nil || notice.TopicID != policy.TopicID || len(notice.Deliveries) != 0 || !strings.HasSuffix(notice.Content, denied.Question) {
				t.Fatalf("clarification was not a public notice in the original topic: %+v, %v", notice, err)
			}
			messageSequencePreserved(t, f.s, before)
			if replay, err := f.s.ApplyChatAssessment(ctx, scope, candidate, proposal, reply, "misassessed-followup"); err != nil || !reflect.DeepEqual(replay, denied) {
				t.Fatalf("denial did not replay: %+v, %v", replay, err)
			}
			discussionEnd(t, f.s, scope)

			// A title, a larger token, and different casing are not exact Task references.
			fresh := local("请另开只读调查，收集「" + f.input.Title + "」的资料。示例编号：" + taskID + "X、" + strings.ToLower(taskID))
			proposal.Reason = "请继续原任务：" + taskID // The assessor's text is not the user's command.
			investigationAssertFollowupControl(t, f, fresh, proposal, policy, "local_user_chat")
		})
	}

	t.Run("source-and-topic-controls", func(t *testing.T) {
		f, policy := investigationFollowupFixture(t, 3)
		prior, err := f.s.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "文本任务。", "prior-task")
		if err != nil || prior.TaskID == "" || prior.Outcome != "admitted" {
			t.Fatalf("prior work was not admitted: %+v, %v", prior, err)
		}
		investigationFinishTurn(t, f)
		invitation := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: policy.TopicID, ActorID: "user",
			Content: "请提出调查建议。"}, []string{f.b})
		peer, _ := discussionArrive(t, f.s, f.groupID, invitation.Deliveries[0].ID)
		proposal := investigationProposal()
		msg, err := f.s.SendMessageForTool(ctx, peer, MemberToolMessageInput{Content: "建议参照既有任务 " + prior.TaskID + " 展开调查。",
			RecipientIDs: []string{f.a}}, "member-investigation-proposal")
		if err != nil {
			t.Fatal(err)
		}
		investigationAssertFollowupControl(t, f, msg, proposal, policy, "member")
		quoted := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: policy.TopicID, ActorID: "user",
			Content: "“请继续原任务并核对源材料。”是旧消息。请另开只读调查，核对「" + f.input.Title + "」的来源。"}, []string{f.a})
		investigationAssertFollowupControl(t, f, quoted, proposal, policy, "local_user_chat")

		other := policy.ThreadRef
		other.TopicID += "-other"
		otherPolicy, err := f.s.SetChatPolicy(ctx, other, policy.ChatPolicyRequest, 0, "other-topic-investigation")
		if err != nil {
			t.Fatal(err)
		}
		foreignRef := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: other.TopicID, ActorID: "user",
			Content: "请继续原任务：" + prior.TaskID}, []string{f.a})
		investigationAssertFollowupControl(t, f, foreignRef, proposal, otherPolicy, "local_user_chat")
	})
}

func investigationFollowupFixture(t *testing.T, budget int) (chatFixture, domain.ChatPolicy) {
	t.Helper()
	f := chatSetup(t, "user", false)
	policy := investigationPolicy(t, f, domain.ChatInvestigationPolicy{Objective: investigationObjective, AllowTopicMessages: true}, budget)
	f.s.EnableInvestigationExecution()
	chatNextCandidate(t, &f, true)
	return f, policy
}

func investigationAssertFollowupControl(t *testing.T, f chatFixture, source domain.Message, proposal domain.ChatAssessment, policy domain.ChatPolicy, sourceKind string) {
	t.Helper()
	scope, candidate := discussionArrive(t, f.s, f.groupID, source.Deliveries[0].ID)
	r, err := f.s.ApplyChatAssessment(ctx, scope, candidate, proposal, "建议调查。", "independent-investigation-"+candidate)
	if err != nil || r.Outcome != "admitted" || r.TaskID == "" || r.MandateID == "" || r.DispatchRequestID == "" ||
		r.ThreadRef != policy.ThreadRef || r.PolicyRevision != policy.Revision || r.SourceKind != sourceKind || r.SourceID != source.SourceID ||
		r.Authority != "written_preauthorization" || !r.AuthorityEvidenceVerified {
		t.Fatalf("independent policy-only investigation was not admitted: %+v, %v", r, err)
	}
	t.Logf("control topic=%s source=%s outcome=%s task=%s", r.TopicID, r.SourceKind, r.Outcome, r.TaskID)
	discussionEnd(t, f.s, scope)
}
