package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

const investigationClue = "导出偶尔只有表头"
const investigationClueTermsJSON = `["导出","只有表头"]`

func clueInvestigationConfig(t *testing.T, terms string) domain.ChatInvestigationPolicy {
	t.Helper()
	var p domain.ChatInvestigationPolicy
	if err := json.Unmarshal([]byte(`{"objective":"`+investigationObjective+`","allow_topic_messages":true,"clue_terms":`+terms+`}`), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestChatInvestigationClueAdmissionAndReplay(t *testing.T) {
	f := chatSetup(t, "user", false)
	s := f.s
	policy := investigationPolicy(t, f, clueInvestigationConfig(t, investigationClueTermsJSON), 1)
	s.EnableInvestigationExecution()
	investigationFinishTurn(t, f)
	post := func(body string, recipients ...string) domain.Message {
		return chatLocalMessage(t, s, MessageRequest{GroupID: f.groupID, TopicID: policy.TopicID, ActorID: "user", Content: body, RecipientIDs: recipients}, recipients)
	}
	proposal := investigationProposal()
	proposal.Title, proposal.Description, proposal.Acceptance, proposal.Reason = investigationClue, investigationClue, investigationClue, investigationClue
	// Neither topic history nor any field in the model's write can supply a missing term.
	post(investigationClue)
	partial := post("导出偶尔为空", f.a)
	investigationBindCandidate(t, &f, partial.Deliveries[0])
	if f.candidate.InvestigationClueMatched {
		t.Fatal("missing term advertised a clue intake")
	}
	denied, err := s.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, proposal, investigationClue, "partial")
	chatAssertNotAdmitted(t, denied, err, "preparation_scope_mismatch")
	notice, err := s.GetMessage(ctx, f.groupID, denied.NoticeMessageID)
	if err != nil || !strings.Contains(notice.Content, partial.ID) || !strings.Contains(notice.Content, partial.SourceID) {
		t.Fatalf("denial should identify the actual unmatched source: %+v, %v", notice, err)
	}
	investigationFinishTurn(t, f)

	const exact = "  " + investigationClue + "。\n"
	source := post(exact, f.a, f.b)
	investigationBindCandidate(t, &f, source.Deliveries[0])
	if !f.candidate.InvestigationClueMatched {
		t.Fatal("the bound local-user clue was not available to trusted issuance")
	}
	first, err := s.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, proposal, "建议核对获准资料。", "clue")
	if err != nil || first.Outcome != "admitted" || first.TaskID == "" || first.MandateID == "" || first.DispatchRequestID == "" ||
		first.SourceKind != "local_user_chat" || first.SourceID != source.SourceID || first.MessageID != source.ID || first.PolicyRevision != policy.Revision ||
		first.CandidateGeneration != f.candidate.Generation || first.ThreadRef != policy.ThreadRef || first.Authority != "written_preauthorization" ||
		!first.AuthorityEvidenceVerified || first.AuthorizationMessageID == source.ID {
		t.Fatalf("wrong clue admission or authority: %+v, %v", first, err)
	}
	before := coordinationDecideSnapshot(t, s)
	if replay, err := s.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, proposal, "建议核对获准资料。", "clue"); err != nil || !reflect.DeepEqual(replay, first) {
		t.Fatalf("replay changed receipt: %+v, %v", replay, err)
	}
	if r, err := s.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, proposal, "另一条提议。", "clue"); !errors.Is(err, ErrConflict) || r.ID != "" {
		t.Fatalf("same key with changed payload did not conflict: %+v, %v", r, err)
	}
	coordinationDecidePreserved(t, s, before)
	investigationFinishTurn(t, f)
	investigationBindCandidate(t, &f, source.Deliveries[1])
	duplicate, err := s.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, proposal, "不能重复。", "second-intake")
	chatAssertNotAdmitted(t, duplicate, err, "source_already_routed")
	investigationFinishTurn(t, f)
	another := post(investigationClue, f.a)
	investigationBindCandidate(t, &f, another.Deliveries[0])
	full, err := s.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, proposal, "再次核对。", "over-budget")
	chatAssertNotAdmitted(t, full, err, "budget_exhausted")
	for _, table := range []string{"tasks", "mandates", "coordination_dispatch_requests"} {
		if rows := messageSequenceTableRows(t, s, table); len(rows) != 1 {
			t.Fatalf("%s: want exactly one row, got %d", table, len(rows))
		}
	}

	// Resolve exact text and authority from immutable IDs, including after a policy edit/reopen.
	investigationDisablePolicy(t, s, policy)
	history := queueHandle(t, f.path, false)
	if saved, err := history.GetChatAdmission(ctx, f.groupID, first.TaskID); err != nil || !reflect.DeepEqual(saved, first) {
		t.Fatalf("admission history changed: %+v, %v", saved, err)
	}
	if saved, err := history.GetMessage(ctx, f.groupID, first.MessageID); err != nil || saved.Content != exact || saved.SourceID != first.SourceID {
		t.Fatalf("history did not preserve the actual body: %+v, %v", saved, err)
	}
	if saved, err := history.GetMessage(ctx, f.groupID, first.AuthorizationMessageID); err != nil || saved.SourceKind != "local_user_policy" ||
		saved.SourceID != policy.ConfigurationSourceID || !strings.Contains(saved.Content, `"clue_terms":`+investigationClueTermsJSON) {
		t.Fatalf("written authorization changed: %+v, %v", saved, err)
	}
	if _, err := history.GetChatAdmission(ctx, f.otherID, first.TaskID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("admission leaked across groups: %v", err)
	}
}

func TestChatInvestigationClueRejectsAutomaticTextRouting(t *testing.T) {
	f := newAutoTextFixture(t)
	const topic = "investigation"
	input := automaticTextPolicy(2)
	clue := clueInvestigationConfig(t, investigationClueTermsJSON)
	input.Investigation = &clue
	f.policy(t, topic, 0, input)
	f.s.EnableInvestigationExecution()

	scope, id := f.claim(t, f.post(t, topic, automaticTextRequest, f.co.ID), f.co.ID)
	if r, err := f.assess(scope, id, automaticTextProposal, "admit-text"); err != nil || r.TaskID == "" {
		t.Fatalf("fixture did not establish automatic routing: %+v, %v", r, err)
	}
	discussionEnd(t, f.s, scope)

	before := automaticWorkSnapshot(t, f.s)
	scope, id = f.claim(t, f.post(t, topic, investigationClue, f.co.ID), f.co.ID)
	candidate, err := f.s.CaptureChatCandidate(ctx, scope)
	if err != nil || candidate.InvestigationClueMatched {
		t.Fatalf("generated routing must not advertise a clue: matched=%t, err=%v", candidate.InvestigationClueMatched, err)
	}
	r, err := f.assess(scope, id, investigationProposal(), "clue")
	assertAutomaticTextQuestion(t, f.s, r, err, "automatic_text_routing_scope")
	messageSequencePreserved(t, f.s, before)
}

func TestChatInvestigationClueDiscussionOnly(t *testing.T) {
	for _, mode := range []string{"direct", "member", "relayed", "structured", "alternate-action", "repair-proposal"} {
		t.Run(mode, func(t *testing.T) {
			f := chatSetup(t, "user", true) // Also offer topic_summary, to catch an alternate work proposal.
			s := f.s
			policy := investigationPolicy(t, f, clueInvestigationConfig(t, investigationClueTermsJSON), 1)
			s.EnableInvestigationExecution()
			investigationFinishTurn(t, f)
			const limit = "本次只讨论。"
			body := limit + investigationClue
			if mode == "member" {
				body = "有什么新发现？"
			}
			root := chatLocalMessage(t, s, MessageRequest{GroupID: f.groupID, TopicID: policy.TopicID,
				ActorID: "user", Content: body, RecipientIDs: []string{f.a}}, []string{f.a})
			scope, id := discussionArrive(t, s, f.groupID, root.Deliveries[0].ID)
			source, restricted := root, root
			// There is intentionally no discussion_only_excerpt from any assessor.
			if mode == "member" || mode == "relayed" || mode == "structured" {
				discussionAssess(t, s, scope, id)
				if mode == "structured" {
					source = discussionApply(t, s, scope, id, "invite", domain.ChatDiscussionAction{
						Kind: domain.ChatDiscussionInvite, RecipientID: f.b, Content: investigationClue}).Message
				} else {
					body := investigationClue
					if mode == "member" {
						body = limit + body
					}
					var err error
					source, err = s.SendMessageForChatIntake(ctx, scope, id, MemberToolMessageInput{Content: body, RecipientIDs: []string{f.b}}, "relay")
					if err != nil {
						t.Fatal(err)
					}
					if mode == "member" {
						restricted = source
					}
				}
				discussionEnd(t, s, scope)
				scope, id = discussionArrive(t, s, f.groupID, source.Deliveries[0].ID)
			}
			proposal := investigationProposal()
			switch mode {
			case "alternate-action":
				proposal = domain.ChatAssessment{Disposition: "prepare_work", Action: domain.ChatActionTextSubmission, Reason: "建议摘要"}
			case "repair-proposal":
				proposal = f.input
				proposal.Repair = &domain.ChatRepairAssessment{ReviewID: "unrelated-review", Authorization: domain.ChatRepairChatRequest}
			}
			if candidate, err := s.CaptureChatCandidate(ctx, scope); err != nil || candidate.ID != id || candidate.InvestigationClueMatched {
				t.Fatalf("a restricted source must stay in the ordinary intake: %v", err)
			}
			r, err := s.ApplyChatAssessment(ctx, scope, id, proposal, "建议开始工作。", "clue")
			chatAssertNotAdmitted(t, r, err, "discussion_only")
			notice, err := s.GetMessage(ctx, f.groupID, r.NoticeMessageID)
			if err != nil || r.MessageID != source.ID || r.SourceID != source.SourceID || r.AuthorityEvidenceVerified ||
				!strings.Contains(notice.Content, restricted.SourceID) || !strings.Contains(notice.Content, "只讨论") || len(notice.Deliveries) != 0 {
				t.Fatalf("expected a non-waking denial with verified limitation source: %+v %+v, %v", r, notice, err)
			}
		})
	}
}

func TestChatInvestigationClueAdmissionFences(t *testing.T) {
	for _, tc := range []struct{ name, reason string }{
		{"disabled", "policy_disabled"}, {"no-runtime", "investigation_runtime_unavailable"},
		{"wrong-topic", "investigation_not_authorized"}, {"stale-policy", "stale_policy"}, {"stale-source", "stale_source"},
		{"unproven-user", "untrusted_source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := chatSetup(t, "user", false)
			s := f.s
			policy := investigationPolicy(t, f, clueInvestigationConfig(t, investigationClueTermsJSON), 1)
			investigationFinishTurn(t, f)
			if tc.name != "no-runtime" {
				s.EnableInvestigationExecution()
			}
			if tc.name == "disabled" {
				investigationDisablePolicy(t, s, policy)
			}
			post := MessageRequest{GroupID: f.groupID, TopicID: policy.TopicID, ActorID: "user", Content: investigationClue, RecipientIDs: []string{f.a}}
			if tc.name == "wrong-topic" {
				post.TopicID += "-other"
				if _, err := s.SetChatPolicy(ctx, domain.ThreadRef{GroupID: post.GroupID, TopicID: post.TopicID}, f.policy.ChatPolicyRequest, 0, "other-topic"); err != nil {
					t.Fatal(err)
				}
			}
			var source domain.Message
			if tc.name == "unproven-user" {
				var err error
				source, err = s.CreateMessage(ctx, post, post.RecipientIDs)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				source = chatLocalMessage(t, s, post, post.RecipientIDs)
			}
			investigationBindCandidate(t, &f, source.Deliveries[0])
			if tc.name == "stale-source" {
				post.Content, post.RecipientIDs = "请先不要调查。", nil
				chatLocalMessage(t, s, post, nil)
			}
			if tc.name == "stale-policy" {
				updated := policy.ChatPolicyRequest
				p := clueInvestigationConfig(t, `["修正后的范围"]`)
				updated.Investigation = &p
				if _, err := s.SetChatPolicy(ctx, policy.ThreadRef, updated, policy.Revision, "new-terms"); err != nil {
					t.Fatal(err)
				}
			}
			candidate, err := s.CaptureChatCandidate(ctx, f.scope)
			if err != nil || candidate.ID != f.candidate.ID || candidate.InvestigationClueMatched {
				t.Fatalf("unavailable clue profile advertised after capture: %v", err)
			}
			r, err := s.ApplyChatAssessment(ctx, f.scope, candidate.ID, investigationProposal(), "建议调查。", "clue")
			chatAssertNotAdmitted(t, r, err, tc.reason)
		})
	}
}

func TestChatInvestigationClueStoredScopeUnicode(t *testing.T) {
	f := chatSetup(t, "user", false)
	s := f.s
	policy := investigationPolicy(t, f, clueInvestigationConfig(t, `["�"]`), 1)
	s.EnableInvestigationExecution()
	investigationFinishTurn(t, f)
	message := chatLocalMessage(t, s, MessageRequest{GroupID: f.groupID, TopicID: policy.TopicID,
		ActorID: "user", Content: "线索：�", RecipientIDs: []string{f.a}}, []string{f.a})
	investigationBindCandidate(t, &f, message.Deliveries[0])
	r, err := s.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, investigationProposal(), "建议调查。", "unicode-clue")
	if err != nil || r.Outcome != "admitted" || r.TaskID == "" {
		t.Fatalf("expected admission for an actual replacement character: %+v, %v", r, err)
	}
	if scope, err := s.GetTaskInvestigationScope(ctx, f.groupID, r.TaskID); err != nil || !reflect.DeepEqual(scope.ClueTerms, policy.Investigation.ClueTerms) {
		t.Fatalf("admitted scope did not retain the exact term: %+v, %v", scope, err)
	}

	// Simulate malformed stored evidence; normal writes cannot replace an immutable scope.
	coordinationDecideExec(t, s, `DROP TRIGGER chat_investigation_policy_update`)
	coordinationDecideExec(t, s, `UPDATE chat_investigation_policy_revisions SET scope_json=replace(scope_json,?,?)
	 WHERE (group_id,topic_id,revision)=(?,?,?)`, `["�"]`, `["\ud800"]`, policy.GroupID, policy.TopicID, policy.Revision)
	if got, err := s.GetChatPolicy(ctx, policy.ThreadRef); err == nil {
		t.Errorf("current policy read accepted malformed stored clue: %+v", got.Investigation)
	}
	if got, err := s.GetTaskInvestigationScope(ctx, f.groupID, r.TaskID); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("admitted scope read accepted or hid malformed stored clue: %+v, %v", got.ClueTerms, err)
	}
}

func TestChatInvestigationCluePolicyRejectsInvalidTerms(t *testing.T) {
	f := chatSetup(t, "user", false)
	for _, terms := range []string{`[]`, `[""]`, `["　"]`, `["导出","导出"]`, `["a\u0000b"]`, `["a\nb"]`, `["a\rb"]`,
		`["` + strings.Repeat("雪", 22) + `"]`, `["1","2","3","4","5","6","7","8","9"]`} {
		input := f.policy.ChatPolicyRequest
		p := clueInvestigationConfig(t, terms)
		input.Investigation = &p
		if r, err := f.s.SetChatPolicy(ctx, f.policy.ThreadRef, input, f.policy.Revision, "invalid-terms"); !errors.Is(err, ErrInvalid) || r.Revision != 0 {
			t.Errorf("invalid terms %s accepted: revision=%d, %v", terms, r.Revision, err)
		}
	}
}
