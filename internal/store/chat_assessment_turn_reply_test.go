package store

import (
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func publicDiscussNotice(t *testing.T, f messageFixture) (domain.Message, TurnClaim) {
	t.Helper()
	s := f.s
	origin := discussionUser(t, f, "", f.a)
	if len(origin.Deliveries) != 1 || origin.Deliveries[0].RecipientID != f.a {
		t.Fatalf("expected one delivery of the local-user message: %+v", origin)
	}
	scope, candidate := discussionArrive(t, s, f.groupID, origin.Deliveries[0].ID)
	queueEvent(t, s, scope.Claim, provider.AcceptedEvent, "", "")

	// Store entry point for send_message with an intent_assessment.
	const text = "可以开始讨论"
	r, err := s.ApplyChatAssessment(ctx, scope, candidate, domain.ChatAssessment{
		Disposition: "discuss", Reason: "The user asked for discussion.",
	}, text, "discuss")
	if err != nil || r.Disposition != "discuss" || r.Outcome != "discuss" || r.AssessedBy != f.a ||
		r.MessageID != origin.ID || r.CandidateID != candidate || r.TurnID != scope.Claim.TurnID ||
		r.Purpose != domain.ChatIntakePurpose || r.SourceKind != "local_user_chat" || r.SourceID == "" || r.SourceID != origin.SourceID {
		t.Fatalf("assessment did not bind to the delivered local-user source: %+v, %v", r, err)
	}
	notice, err := s.GetMessage(ctx, f.groupID, r.NoticeMessageID)
	if err != nil || notice.SenderID != f.a || notice.TopicID != origin.TopicID || notice.Content != text ||
		len(notice.Deliveries) != 0 || notice.SourceID != "assessment:"+r.ID {
		t.Fatalf("expected a public notice from the assessing member: %+v, %v", notice, err)
	}
	return notice, scope.Claim
}

func TestChatAssessmentDiscussSuppressesDuplicateNativeReply(t *testing.T) {
	f := setupMessages(t)
	s := f.s
	notice, claim := publicDiscussNotice(t, f)
	before, err := s.ListMessages(ctx, f.groupID, MessageListOptions{})
	if err != nil || len(before.Items) != 2 {
		t.Fatalf("expected only the input and the assessment notice before settlement: %+v, %v", before, err)
	}

	queueEvent(t, s, claim, provider.ResponseEvent, notice.Content, "")
	queueEvent(t, s, claim, provider.TerminalEvent, "", provider.OutcomeSucceeded)
	candidate := queueTurn(t, s, claim)
	receipt := queueReceipt(provider.OutcomeSucceeded)
	settled, err := s.ApplyTurnReceipt(ctx, claim, receipt)
	if err != nil || settled.State != domain.TurnStateSucceeded || settled.CandidateText != notice.Content ||
		settled.CandidateTerminal != domain.TurnStateSucceeded || settled.NativeSessionID != receipt.NativeSessionID ||
		settled.AcceptedAt == nil || !reflect.DeepEqual(settled.AcceptedAt, candidate.AcceptedAt) {
		t.Fatalf("settlement did not retain native success evidence: %+v, %v", settled, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = queueHandle(t, f.path, false)
	if replay, err := s.ApplyTurnReceipt(ctx, claim, receipt); err != nil || !reflect.DeepEqual(replay, settled) {
		t.Fatalf("native receipt replay after reopen: %+v, %v", replay, err)
	}
	receipt.Reason = settled.Reason
	if audit := queueAudit(t, s, claim, provider.OutcomeSucceeded); audit != receipt {
		t.Fatalf("stored execution receipt changed: %+v", audit)
	}
	queueDeliveryState(t, s, settled, domain.DeliveryStateAccepted)
	if settled.ReplyMessageID != "" {
		t.Fatalf("duplicate native reply %q for public discuss notice %q", settled.ReplyMessageID, notice.ID)
	}
	if page, err := s.ListMessages(ctx, f.groupID, MessageListOptions{}); err != nil || !reflect.DeepEqual(page, before) {
		t.Fatalf("expected the original input and notice unchanged, with no new publication: %+v, %v", page, err)
	}
}

func TestChatAssessmentDiscussKeepsOtherNativeReplies(t *testing.T) {
	for _, name := range []string{"different-text", "other-turn"} {
		t.Run(name, func(t *testing.T) {
			f := setupMessages(t)
			s := f.s
			notice, claim := publicDiscussNotice(t, f)
			text := notice.Content
			switch name {
			case "different-text":
				text += "\n" // The persisted text must match byte for byte.
			case "other-turn":
				if ended, err := s.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeSucceeded)); err != nil ||
					ended.State != domain.TurnStateSucceeded || ended.ReplyMessageID != "" {
					t.Fatalf("could not settle the assessment Turn without a native reply: %+v, %v", ended, err)
				}
				// Same member, topic, and final text, but the new Turn has its own distinct notice.
				next := discussionUser(t, f, notice.TopicID, f.a)
				scope, candidate := discussionArrive(t, s, f.groupID, next.Deliveries[0].ID)
				claim = scope.Claim
				queueEvent(t, s, claim, provider.AcceptedEvent, "", "")
				discussionAssess(t, s, scope, candidate)
			}
			settleDiscussionNativeReply(t, s, claim, notice, text, provider.OutcomeSucceeded)
		})
	}
}

func TestChatAssessmentClarifyKeepsIdenticalNativeReply(t *testing.T) {
	f := setupMessages(t)
	s := f.s
	origin := discussionUser(t, f, "", f.a)
	scope, candidate := discussionArrive(t, s, f.groupID, origin.Deliveries[0].ID)
	queueEvent(t, s, scope.Claim, provider.AcceptedEvent, "", "")
	const question = "Which part of the summary should we discuss?"
	r, err := s.ApplyChatAssessment(ctx, scope, candidate, domain.ChatAssessment{
		Disposition: "clarify", Reason: "The requested scope is unclear.", Question: question,
	}, question, "clarify")
	if err != nil || r.Outcome != "clarify" {
		t.Fatalf("expected a clarification assessment: %+v, %v", r, err)
	}
	notice, err := s.GetMessage(ctx, f.groupID, r.NoticeMessageID)
	if err != nil || notice.Content != question || len(notice.Deliveries) != 0 {
		t.Fatalf("expected a public clarification notice: %+v, %v", notice, err)
	}
	settleDiscussionNativeReply(t, s, scope.Claim, notice, question, provider.OutcomeSucceeded)
}
