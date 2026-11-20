package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

const (
	ordinaryPublicReplyText = "三条样本合计 18。"
	ordinaryPublicReplyKey  = "public-samples"
)

func queueOrdinaryReplyScope(t *testing.T, s *Store, claim TurnClaim) MemberToolScope {
	t.Helper()
	queueEvent(t, s, claim, provider.AcceptedEvent, "", "")
	scope, err := s.BindMemberTool(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func queueOrdinaryReplySend(t *testing.T, s *Store, scope MemberToolScope, recipients []string) domain.Message {
	t.Helper()
	published, err := s.SendMessageForTool(ctx, scope, MemberToolMessageInput{Content: ordinaryPublicReplyText, RecipientIDs: recipients}, ordinaryPublicReplyKey)
	if err != nil || published.SourceKind != "member" || published.SourceID == "" || len(published.Deliveries) != len(recipients) {
		t.Fatalf("ordinary publication: %+v, %v", published, err)
	}
	return published
}

func TestQueueOrdinaryPublicSendSuppressesDuplicateNativeReply(t *testing.T) {
	for _, tc := range []struct {
		name       string
		recipients []string
	}{
		{"omitted-recipients", nil},
		{"empty-recipients", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			s := f.s
			scope := queueOrdinaryReplyScope(t, s, claim)
			published := queueOrdinaryReplySend(t, s, scope, tc.recipients)
			queueCandidate(t, s, claim, ordinaryPublicReplyText, provider.OutcomeSucceeded)
			before := queueTurn(t, s, claim)
			receipt := queueReceipt(provider.OutcomeSucceeded)
			settled, err := s.ApplyTurnReceipt(ctx, claim, receipt)
			if err != nil || settled.State != domain.TurnStateSucceeded || !reflect.DeepEqual(settled.AcceptedAt, before.AcceptedAt) ||
				settled.CandidateText != before.CandidateText || settled.CandidateTerminal != before.CandidateTerminal {
				t.Fatalf("success did not retain native evidence: %+v, %v", settled, err)
			}
			if settled.ReplyMessageID != "" {
				t.Fatalf("duplicate native reply %q for ordinary publication %q", settled.ReplyMessageID, published.ID)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = queueHandle(t, f.path, false)
			if replay, err := s.ApplyTurnReceipt(ctx, claim, receipt); err != nil || !reflect.DeepEqual(replay, settled) {
				t.Fatalf("settlement replay after reopen: %+v, %v", replay, err)
			}
			receipt.Reason = settled.Reason
			if audit := queueAudit(t, s, claim, provider.OutcomeSucceeded); audit != receipt {
				t.Fatalf("stored execution receipt changed: %+v", audit)
			}
			queueCounts(t, s, 1, 1, 0)
			queueDeliveryState(t, s, settled, domain.DeliveryStateAccepted)
			request := MessageRequest{GroupID: f.groupID, TopicID: scope.TopicID, ActorID: scope.MemberID, Content: ordinaryPublicReplyText,
				RecipientIDs: tc.recipients, IdempotencyKey: ordinaryPublicReplyKey}
			if got, found, err := s.LookupMessageReceipt(ctx, request); err != nil || !found || !reflect.DeepEqual(got, published) {
				t.Fatalf("ordinary publication/receipt changed: %+v, %t, %v", got, found, err)
			}
			if page, err := s.ListMessages(ctx, f.groupID, MessageListOptions{}); err != nil || len(page.Items) != 2 {
				t.Fatalf("timeline should contain only the input and the explicit send: %+v, %v", page, err)
			}
		})
	}
}

func TestQueueOrdinaryPublicSendKeepsOtherNativeReplies(t *testing.T) {
	for _, name := range []string{"different-body", "other-turn-same-key", "recovered-claim", "legacy-no-source", "receipt-mismatch",
		"directed-fingerprint", "directed-send", "accepted-delivery", "canceled-delivery"} {
		t.Run(name, func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			s := f.s
			scope := queueOrdinaryReplyScope(t, s, claim)
			text := ordinaryPublicReplyText
			var published domain.Message
			switch name {
			case "legacy-no-source":
				legacy, err := s.CreateMessage(ctx, MessageRequest{GroupID: f.groupID, TopicID: scope.TopicID, ActorID: f.a,
					Content: text, IdempotencyKey: ordinaryPublicReplyKey}, nil)
				if err != nil {
					t.Fatal(err)
				}
				published, err = s.SendMessageForTool(ctx, scope, MemberToolMessageInput{Content: text}, ordinaryPublicReplyKey)
				if err != nil || legacy.SourceID != "" || !reflect.DeepEqual(published, legacy) {
					t.Fatalf("replaying legacy message added source proof: %+v, %v", published, err)
				}
			case "directed-send":
				published = queueOrdinaryReplySend(t, s, scope, []string{f.b})
			default:
				published = queueOrdinaryReplySend(t, s, scope, nil)
			}
			switch name {
			case "different-body":
				text += "\n"
			case "other-turn-same-key":
				if ended, err := s.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeSucceeded)); err != nil || ended.State != domain.TurnStateSucceeded || ended.ReplyMessageID != "" {
					t.Fatalf("settling the source Turn without a candidate: %+v, %v", ended, err)
				}
				d := queueDelivery(t, s, f.groupID, f.a, scope.TopicID)
				next, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: d.ID, ResourceKeys: []string{"shared"}, MaxActive: 1})
				if err != nil {
					t.Fatal(err)
				}
				claim = TurnClaim{GroupID: next.GroupID, TurnID: next.ID, Token: next.ClaimToken}
				nextScope := queueOrdinaryReplyScope(t, s, claim)
				if replay := queueOrdinaryReplySend(t, s, nextScope, nil); !reflect.DeepEqual(replay, published) {
					t.Fatalf("same key rewrote the original source: %+v", replay)
				}
			case "recovered-claim":
				if n, err := s.RecoverInFlight(ctx); err != nil || n != 1 {
					t.Fatalf("claim recovery: %d, %v", n, err)
				}
				if _, err := s.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeSucceeded)); !errors.Is(err, ErrStaleClaim) {
					t.Fatalf("previous claim was not fenced: %v", err)
				}
				claim.Token = queueTurn(t, s, claim).ClaimToken
			case "receipt-mismatch":
				coordinationDecideExec(t, s, `UPDATE mutation_idempotency SET record_id='not-this-message' WHERE record_id=?`, published.ID)
			case "directed-fingerprint":
				// No delivery row, but the saved request was not public-only.
				rewriteQueueReplyFingerprint(t, s, published, []string{f.b})
			case "accepted-delivery", "canceled-delivery":
				// Keep the public fingerprint to independently exercise the delivery veto.
				conv, err := s.GetOrCreateConversation(ctx, f.groupID, f.b, scope.TopicID)
				if err != nil {
					t.Fatal(err)
				}
				const id = "recipient-delivery"
				coordinationDecideExec(t, s, `INSERT INTO deliveries(id,message_id,conversation_id,recipient_id,group_id,topic_id,created_at)
				 SELECT ?,id,?,?,group_id,topic_id,created_at FROM messages WHERE id=?`, id, conv.ID, f.b, published.ID)
				target, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: id, MaxActive: 2})
				if err != nil {
					t.Fatal(err)
				}
				targetClaim := TurnClaim{GroupID: f.groupID, TurnID: target.ID, Token: target.ClaimToken}
				if name == "accepted-delivery" {
					queueEvent(t, s, targetClaim, provider.AcceptedEvent, "", "")
					queueDeliveryState(t, s, target, domain.DeliveryStateAccepted)
				} else {
					if _, err := s.RequestTurnCancel(ctx, f.groupID, target.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := s.ApplyTurnReceipt(ctx, targetClaim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
						t.Fatal(err)
					}
					queueDeliveryState(t, s, target, domain.DeliveryStateFailed)
				}
			}
			settleDiscussionNativeReply(t, s, claim, published, text, provider.OutcomeSucceeded)
		})
	}
	for _, outcome := range []provider.Outcome{provider.OutcomeFailed, provider.OutcomeAborted, provider.OutcomeTruncated} {
		t.Run(string(outcome), func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			published := queueOrdinaryReplySend(t, f.s, queueOrdinaryReplyScope(t, f.s, claim), nil)
			settleDiscussionNativeReply(t, f.s, claim, published, published.Content, outcome)
		})
	}
	t.Run("false-structured-resolve", func(t *testing.T) {
		f := setupMessages(t)
		s := f.s
		origin := discussionUser(t, f, "bound-topic", f.a)
		scope, candidate := discussionArrive(t, s, f.groupID, origin.Deliveries[0].ID)
		queueEvent(t, s, scope.Claim, provider.AcceptedEvent, "", "")
		discussionAssess(t, s, scope, candidate)
		resolve := discussionApply(t, s, scope, candidate, ordinaryPublicReplyKey,
			domain.ChatDiscussionAction{Kind: domain.ChatDiscussionResolve, Content: ordinaryPublicReplyText})
		// Invalidate structured proof, then make the publication look ordinary
		// except for its event row. The ordinary matcher must not rescue it.
		coordinationDecideExec(t, s, `DROP TRIGGER chat_intake_executions_update`)
		coordinationDecideExec(t, s, `UPDATE chat_intake_executions SET claim_digest=zeroblob(32) WHERE candidate_id=?`, candidate)
		rewriteQueueReplyFingerprint(t, s, resolve.Message, nil)
		settleDiscussionNativeReply(t, s, scope.Claim, resolve.Message, ordinaryPublicReplyText, provider.OutcomeSucceeded)
	})
}

func rewriteQueueReplyFingerprint(t *testing.T, s *Store, m domain.Message, recipients []string) {
	t.Helper()
	payload := (MessageRequest{GroupID: m.GroupID, TopicID: m.TopicID, Content: m.Content, RecipientIDs: recipients}).payload()
	coordinationDecideExec(t, s, `DROP TRIGGER message_command_sources_update`)
	coordinationDecideExec(t, s, `UPDATE message_command_sources SET payload=? WHERE message_id=?`, payload, m.ID)
	coordinationDecideExec(t, s, `UPDATE mutation_idempotency SET payload=? WHERE record_id=?`, payload, m.ID)
}

func TestQueueOrdinaryPublicSendSettlementRollsBack(t *testing.T) {
	f, _, claim := queueClaimFixture(t)
	s := f.s
	published := queueOrdinaryReplySend(t, s, queueOrdinaryReplyScope(t, s, claim), nil)
	queueCandidate(t, s, claim, ordinaryPublicReplyText, provider.OutcomeSucceeded)
	terminal := queueReceipt(provider.OutcomeSucceeded)
	incomplete := terminal
	incomplete.OutputComplete = false
	unknown, err := s.ApplyTurnReceipt(ctx, claim, incomplete)
	if err != nil || unknown.State != domain.TurnStateUnknown || unknown.ReplyMessageID != "" {
		t.Fatalf("ordinary send is not native settlement evidence: %+v, %v", unknown, err)
	}
	before := queueReceiptJSON(t, s, claim)
	coordinationDecideExec(t, s, `CREATE TRIGGER fail_ordinary_settlement BEFORE DELETE ON resource_claims
	 BEGIN SELECT RAISE(ABORT,'test settlement failure'); END`)
	if got, err := s.ApplyTurnReceipt(ctx, claim, terminal); err == nil || got.ID != "" {
		t.Fatalf("failed settlement returned success: %+v, %v", got, err)
	}
	if got := queueTurn(t, s, claim); !reflect.DeepEqual(got, unknown) || queueReceiptJSON(t, s, claim) != before {
		t.Fatalf("failed settlement changed the turn or receipt: %+v", got)
	}
	queueCounts(t, s, 1, 1, 1)
	if page, err := s.ListMessages(ctx, f.groupID, MessageListOptions{}); err != nil || len(page.Items) != 2 || !reflect.DeepEqual(page.Items[0], published) {
		t.Fatalf("failed settlement altered the existing publications: %+v, %v", page, err)
	}
	coordinationDecideExec(t, s, `DROP TRIGGER fail_ordinary_settlement`)
	if got, err := s.ApplyTurnReceipt(ctx, claim, terminal); err != nil || got.State != domain.TurnStateSucceeded || got.ReplyMessageID != "" {
		t.Fatalf("successful retry did not retain the explicit send only: %+v, %v", got, err)
	}
	queueCounts(t, s, 1, 1, 0)
}

func TestQueueOrdinaryPublicSendProbeUsesIndexedLookups(t *testing.T) {
	s, _ := openTestStore(t)
	plan := resolutionPlan(t, s, ordinaryTurnReplyMatch, "turn", make([]byte, 32), "group", "member", "topic", ordinaryPublicReplyText,
		[]byte{}, []byte{}, "create_message")
	for _, expected := range []string{
		"SEARCH s USING INDEX message_command_sources_turn (turn_id=?)",
		"SEARCH m USING INDEX sqlite_autoindex_messages_", "(id=?",
		"SEARCH e USING COVERING INDEX", "SEARCH d USING COVERING INDEX",
		"sqlite_autoindex_mutation_idempotency_", "(operation=? AND actor_id=? AND group_id=? AND idempotency_key=?)",
	} {
		if !strings.Contains(plan, expected) {
			t.Errorf("missing indexed lookup %q in plan: %s", expected, plan)
		}
	}
	if strings.Contains(strings.ReplaceAll(plan, "SCAN CONSTANT ROW", ""), "SCAN ") {
		t.Fatalf("unrelated scan in ordinary probe: %s", plan)
	}
}
