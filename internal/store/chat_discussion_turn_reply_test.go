package store

import (
	"errors"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestChatDiscussionResolveSuppressesDuplicateNativeReply(t *testing.T) {
	f, origin, invite, peer := discussionSerialHandoff(t)
	s := f.s
	queueEvent(t, s, peer, provider.AcceptedEvent, "", "")
	b, cb := discussionCapture(t, s, peer)
	discussionAssess(t, s, b, cb)
	answer := discussionApply(t, s, b, cb, "reply", domain.ChatDiscussionAction{Kind: domain.ChatDiscussionReply, Content: "去年同期仅供对照。"})
	// An identical native final after a structured reply (not a resolve) still publishes.
	peerSettled := settleDiscussionNativeReply(t, s, peer, answer.Message, answer.Message.Content, provider.OutcomeSucceeded)

	turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: answer.Event.DeliveryID, MaxActive: 1, ResourceKeys: []string{"discussion-held"}})
	if err != nil {
		t.Fatal(err)
	}
	claim := TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	queueEvent(t, s, claim, provider.AcceptedEvent, "", "")
	a, ca := discussionCapture(t, s, claim)
	discussionAssess(t, s, a, ca)
	const text = "经复核，去年同期列仅供对照，不计入只汇总本季实际发生额的本季合计，以免跨期混加、混淆统计口径。"
	resolve := discussionApply(t, s, a, ca, "resolve", domain.ChatDiscussionAction{Kind: domain.ChatDiscussionResolve, Content: text})
	discussionCheckPublication(t, s, resolve, origin, "resolve", answer.Message.ID, f.a, "", invite.Message.ID, "", text, "", 0)
	cancelled, err := s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID)
	if err != nil || cancelled.State != domain.TurnStateCancelRequested || cancelled.CancelRequestedAt == nil {
		t.Fatalf("cancellation was not recorded: %+v, %v", cancelled, err)
	}
	queueCandidate(t, s, claim, text, provider.OutcomeSucceeded)
	queueCounts(t, s, 3, 3, 1)
	before := make(map[string][][]any)
	for _, table := range []string{"messages", "message_log", "deliveries", "chat_discussion_events", "message_command_sources"} {
		before[table] = messageSequenceTableRows(t, s, table)
	}

	terminal := queueReceipt(provider.OutcomeSucceeded)
	incomplete := terminal
	incomplete.OutputComplete = false
	unknown, err := s.ApplyTurnReceipt(ctx, claim, incomplete)
	if err != nil || unknown.State != domain.TurnStateUnknown || unknown.ReplyMessageID != "" {
		t.Fatalf("a resolve does not confirm native settlement: %+v, %v", unknown, err)
	}
	queueCounts(t, s, 3, 3, 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = queueHandle(t, f.path, false)
	if n, err := s.RecoverInFlight(ctx); err != nil || n != 1 {
		t.Fatalf("recovering exact resolver turn: %d, %v", n, err)
	}
	if _, err := s.ApplyTurnReceipt(ctx, claim, terminal); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("pre-recovery claim was not fenced: %v", err)
	}
	recovered := queueTurn(t, s, claim)
	claim.Token = recovered.ClaimToken
	unknownReceipt := queueReceiptJSON(t, s, claim)

	// Fail late, after settlement has already written the native receipt.
	coordinationDecideExec(t, s, `CREATE TRIGGER fail_resolve_settlement BEFORE DELETE ON resource_claims
	 BEGIN SELECT RAISE(ABORT,'test settlement failure'); END`)
	if got, err := s.ApplyTurnReceipt(ctx, claim, terminal); err == nil || got.ID != "" {
		t.Fatalf("failed settlement returned success: %+v, %v", got, err)
	}
	if got := queueTurn(t, s, claim); !reflect.DeepEqual(got, recovered) || queueReceiptJSON(t, s, claim) != unknownReceipt {
		t.Fatalf("failed settlement changed the turn or its receipt: %+v", got)
	}
	queueCounts(t, s, 3, 3, 1)
	messageSequencePreserved(t, s, before)
	coordinationDecideExec(t, s, "DROP TRIGGER fail_resolve_settlement")

	settled, err := s.ApplyTurnReceipt(ctx, claim, terminal)
	if err != nil || settled.State != domain.TurnStateSucceeded || settled.CandidateText != text || settled.CandidateTerminal != domain.TurnStateSucceeded ||
		!reflect.DeepEqual(settled.AcceptedAt, recovered.AcceptedAt) || !reflect.DeepEqual(settled.CancelRequestedAt, cancelled.CancelRequestedAt) {
		t.Fatalf("resolver did not retain terminal evidence: %+v, %v", settled, err)
	}
	if settled.ReplyMessageID != "" {
		t.Fatalf("duplicate native conclusion got a reply link %q (structured resolve %q)", settled.ReplyMessageID, resolve.Message.ID)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = queueHandle(t, f.path, false)
	if replay, err := s.ApplyTurnReceipt(ctx, claim, terminal); err != nil || !reflect.DeepEqual(replay, settled) {
		t.Fatalf("receipt replay after reopen: %+v, %v", replay, err)
	}
	if audit := queueAudit(t, s, claim, provider.OutcomeSucceeded); audit.Terminal != terminal.Terminal || !audit.Started || !audit.Accepted ||
		!audit.OutputComplete || !audit.DirectChildReaped || audit.NativeSessionID != terminal.NativeSessionID {
		t.Fatalf("native receipt was lost: %+v", audit)
	}
	queueCounts(t, s, 3, 3, 0)
	messageSequencePreserved(t, s, before)
	if got, err := s.GetMessage(ctx, f.groupID, resolve.Message.ID); err != nil || got.Content != text || got.SourceKind != "member" ||
		got.SourceID != resolve.Message.SourceID || len(got.Deliveries) != 0 || got.Discussion == nil || got.Discussion.Kind != domain.ChatDiscussionResolve {
		t.Fatalf("saved resolve or its projection changed: %+v, %v", got, err)
	}
	if got := queueTurn(t, s, peer); !reflect.DeepEqual(got, peerSettled) {
		t.Fatalf("settlement changed the earlier native reply link: %+v", got)
	}
}

func TestChatDiscussionResolveKeepsOtherNativeReplies(t *testing.T) {
	for _, name := range []string{"foreign-turn", "different-text", "ordinary-send"} {
		t.Run(name, func(t *testing.T) {
			f := setupMessages(t)
			s := f.s
			origin := discussionUser(t, f, "", f.a)
			a, ca := discussionArrive(t, s, f.groupID, origin.Deliveries[0].ID)
			discussionAssess(t, s, a, ca)
			const conclusion = "去年同期仅供对照。"
			text, outcome := conclusion, provider.OutcomeSucceeded
			var published domain.Message
			if name == "ordinary-send" {
				var err error
				published, err = s.SendMessageForChatIntake(ctx, a, ca, MemberToolMessageInput{Content: conclusion, RecipientIDs: []string{}}, "ordinary")
				if err != nil {
					t.Fatal(err)
				}
				outcome = provider.OutcomeFailed // A trusted failure can still publish final text.
			} else {
				published = discussionApply(t, s, a, ca, "resolve", domain.ChatDiscussionAction{Kind: domain.ChatDiscussionResolve, Content: conclusion}).Message
			}
			switch name {
			case "foreign-turn": // Same actor, group, topic and exact text, but a different input and Turn.
				if got, err := s.ApplyTurnReceipt(ctx, a.Claim, queueReceipt(outcome)); err != nil || got.State != domain.TurnStateSucceeded || got.ReplyMessageID != "" {
					t.Fatalf("could not settle the author of the resolve: %+v, %v", got, err)
				}
				next := discussionUser(t, f, origin.TopicID, f.a)
				a, ca = discussionArrive(t, s, f.groupID, next.Deliveries[0].ID)
				discussionAssess(t, s, a, ca)
			case "different-text":
				text += "\n" // No trimming or normalization of either persisted text.
			}
			settleDiscussionNativeReply(t, s, a.Claim, published, text, outcome)
		})
	}
}

func settleDiscussionNativeReply(t *testing.T, s *Store, claim TurnClaim, published domain.Message, text string, outcome provider.Outcome) domain.Turn {
	t.Helper()
	queueCandidate(t, s, claim, text, outcome)
	turn, err := s.ApplyTurnReceipt(ctx, claim, queueReceipt(outcome))
	if err != nil || turn.State != domain.TurnState(outcome) || turn.ReplyMessageID == "" || turn.ReplyMessageID == published.ID {
		t.Fatalf("distinct native reply did not publish: %+v, %v", turn, err)
	}
	got, err := s.GetMessage(ctx, claim.GroupID, turn.ReplyMessageID)
	if err != nil || got.SenderID != published.SenderID || got.TopicID != published.TopicID || got.Content != text || len(got.Deliveries) != 0 ||
		got.SourceKind != "member" || got.SourceID != "turn:"+turn.ID || got.Discussion != nil {
		t.Fatalf("wrong native reply or provenance: %+v, %v", got, err)
	}
	return turn
}
