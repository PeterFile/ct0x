package store

import (
	"errors"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
)

func TestChatInvestigationLateDiscussionStartFence(t *testing.T) {
	for _, tc := range []struct {
		name, lineage string
		limits        int
	}{
		{"direct", "direct", 1},
		{"legacy-relayed", "legacy", 1},
		{"structured", "structured", 1},
		{"ambiguous", "direct", 2},
		{"structured-with-newer-chat", "structured", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := chatSetup(t, "user", false)
			s := f.s
			policy := investigationPolicy(t, f, domain.ChatInvestigationPolicy{Objective: investigationObjective, AllowTopicMessages: true}, 1)
			s.EnableInvestigationExecution()
			discussionEnd(t, s, f.scope)
			const quote = "本次只讨论，不开始调查。"
			post := func(content string, targets ...string) domain.Message {
				t.Helper()
				return chatLocalMessage(t, s, MessageRequest{GroupID: f.groupID, TopicID: policy.TopicID,
					ActorID: "user", Content: content, RecipientIDs: targets}, targets)
			}
			recordLimit := func(message domain.Message, delivery domain.Delivery) {
				t.Helper()
				scope, id := discussionArrive(t, s, f.groupID, delivery.ID)
				input := domain.ChatAssessment{Disposition: "discuss", Reason: "先交换意见。", DiscussionOnlyExcerpt: new(quote)}
				r, err := s.ApplyChatAssessment(ctx, scope, id, input, "先讨论。", "limit-"+id)
				if err != nil || r.Outcome != "discuss" || !r.DiscussionOnly || r.DiscussionOnlyExcerpt != quote || r.TaskID != "" ||
					r.SourceKind != "local_user_chat" || r.SourceID != message.SourceID || r.MessageID != message.ID || r.TurnID != scope.Claim.TurnID {
					t.Fatalf("limit was not persisted for the exact local-user command: %+v, %v", r, err)
				}
				discussionEnd(t, s, scope)
			}

			targets := []string{f.a, f.b}
			if tc.limits == 2 {
				targets = append(targets, member(t, s, f.groupID).ID)
			}
			root := post("  "+quote+"\n", targets...)
			source := root
			if tc.lineage != "direct" {
				publisher, id := discussionArrive(t, s, f.groupID, root.Deliveries[0].ID)
				discussionAssess(t, s, publisher, id)
				peer := root.Deliveries[1].RecipientID
				if tc.lineage == "structured" {
					source = discussionApply(t, s, publisher, id, "invite", domain.ChatDiscussionAction{
						Kind: domain.ChatDiscussionInvite, RecipientID: peer, Content: "请提出意见。"}).Message
				} else {
					var err error
					source, err = s.SendMessageForChatIntake(ctx, publisher, id, MemberToolMessageInput{Content: "请提出意见。", RecipientIDs: []string{peer}}, "relay")
					if err != nil {
						t.Fatal(err)
					}
				}
				discussionEnd(t, s, publisher)
			}
			assessor, id := discussionArrive(t, s, f.groupID, source.Deliveries[0].ID)
			admission, err := s.ApplyChatAssessment(ctx, assessor, id, investigationProposal(), "建议调查。", "admit")
			if err != nil || admission.Outcome != "admitted" || admission.TaskID == "" || !admission.AuthorityEvidenceVerified ||
				admission.MessageID != source.ID || admission.SourceID != source.SourceID || admission.SourceKind != source.SourceKind ||
				admission.TurnID != assessor.Claim.TurnID {
				t.Fatalf("fixture did not admit the investigation from the exact source: %+v, %v", admission, err)
			}
			discussionEnd(t, s, assessor)
			taskID := admission.TaskID
			scope, err := s.GetTaskInvestigationScope(ctx, f.groupID, taskID)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.CheckTaskInvestigationReady(ctx, f.groupID, taskID); err != nil {
				t.Fatalf("not ready before the competing intake: %v", err)
			}

			var wantReady, wantRole error
			if tc.limits != 0 {
				for _, delivery := range root.Deliveries[1:] {
					recordLimit(root, delivery)
				}
				wantReady, wantRole = ErrTaskInvestigationNotReady, ErrMemberToolDenied
			} else {
				other := post(quote+"（另一条消息）", f.b)
				recordLimit(other, other.Deliveries[0])
				if err := s.CheckTaskInvestigationReady(ctx, f.groupID, taskID); err != nil {
					t.Errorf("another command's verified limit revoked this investigation: %v", err)
				}
				post("补充一条参考资料。")
			}
			if err := s.CheckTaskInvestigationReady(ctx, f.groupID, taskID); !errors.Is(err, wantReady) {
				t.Errorf("pre-claim readiness: got %v, want %v", err, wantReady)
			}
			dispatch, err := s.GetDispatchRequest(ctx, f.groupID, admission.DispatchRequestID)
			if err != nil {
				t.Fatal(err)
			}
			role := coordinationDecideBindRequest(t, s, dispatch.Request)
			before := coordinationDecideSnapshot(t, s)
			if state, err := s.ResolveInvestigationExecution(ctx, role.Member, role.Provenance); !errors.Is(err, wantRole) ||
				(wantRole == nil && (state.Provenance != role.Provenance || state.Blocked != CoordinationReady)) ||
				(wantRole != nil && state != (CoordinationRoleState{})) {
				t.Errorf("first claimed role: blocked=%q, err=%v; want err=%v", state.Blocked, err, wantRole)
			}
			if page, err := s.ReadInvestigationChannel(ctx, role, ListOptions{}); !errors.Is(err, wantRole) || (len(page.Items) == 0) != (wantRole != nil) {
				t.Errorf("claimed role read: %d messages, err=%v; want err=%v", len(page.Items), err, wantRole)
			}
			if wantRole != nil {
				input := CoordinatorDecisionInput{Kind: "decline", Reason: "暂不安排。"}
				if got, err := s.DecideDispatchWithCapabilities(ctx, role, input, "late-decision", nil); !errors.Is(err, wantRole) || got.Decision.ID != "" {
					t.Errorf("fresh typed decision: got decision=%t, err=%v; want err=%v", got.Decision.ID != "", err, wantRole)
				}
			}
			if saved, err := s.GetChatAdmission(ctx, f.groupID, taskID); err != nil || !reflect.DeepEqual(saved, admission) {
				t.Errorf("historical admission changed: %+v, %v", saved, err)
			}
			if saved, err := s.GetTaskInvestigationScope(ctx, f.groupID, taskID); err != nil || !reflect.DeepEqual(saved, scope) {
				t.Errorf("historical scope changed: %+v, %v", saved, err)
			}
			coordinationDecidePreserved(t, s, before)
		})
	}
}
