package store

import (
	"crypto/sha256"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func queueInstallQuoteFixture(t *testing.T, s *Store) {
	t.Helper()
	var installed bool
	if err := s.db.QueryRow(messageQuoteAvailable).Scan(&installed); err != nil {
		t.Fatal(err)
	}
	if !installed {
		coordinationDecideExec(t, s, messageQuoteSchema)
	}
	t.Logf("quote fixture: schema v%d, registered=%t", s.schemaVersion, installed)
}

func queueQuoteOriginal(t *testing.T, s *Store, scope MemberToolScope) domain.Message {
	t.Helper()
	m, err := s.CreateLocalUserMessage(ctx, MessageRequest{GroupID: scope.Claim.GroupID, TopicID: scope.TopicID,
		ActorID: "user", Content: "旧记录原文。\n\t统计说明。", RecipientIDs: []string{}}, nil)
	if err != nil || m.SourceKind != "local_user_chat" || m.SourceID == "" || len(m.Deliveries) != 0 {
		t.Fatalf("quoted original: %+v, %v", m, err)
	}
	return m
}

func queueQuotedReplySend(t *testing.T, s *Store, scope MemberToolScope, target domain.Message, recipients []string) domain.Message {
	t.Helper()
	m, err := s.SendMessageForTool(ctx, scope, MemberToolMessageInput{Content: ordinaryPublicReplyText,
		ReplyToMessageID: target.ID, RecipientIDs: recipients}, ordinaryPublicReplyKey)
	if err != nil || m.SourceKind != "member" || m.SourceID == "" || len(m.Deliveries) != len(recipients) || m.ReplyTo == nil ||
		m.ReplyTo.MessageID != target.ID || m.ReplyTo.GroupID != target.GroupID || m.ReplyTo.TopicID != target.TopicID ||
		m.ReplyTo.SenderID != target.SenderID || m.ReplyTo.SourceKind != target.SourceKind {
		t.Fatalf("ordinary quoted publication: %+v, %v", m, err)
	}
	return m
}

func TestQueueOrdinaryQuotedSendSuppressesDuplicateNativeReply(t *testing.T) {
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
			queueInstallQuoteFixture(t, s)
			scope := queueOrdinaryReplyScope(t, s, claim)
			original := queueQuoteOriginal(t, s, scope)
			published := queueQuotedReplySend(t, s, scope, original, tc.recipients)
			quotes := messageSequenceTableRows(t, s, "message_quotes")
			if len(quotes) != 1 {
				t.Fatalf("expected one original quote edge, got %d", len(quotes))
			}
			queueCandidate(t, s, claim, published.Content, provider.OutcomeSucceeded)
			before := queueTurn(t, s, claim)
			receipt := queueReceipt(provider.OutcomeSucceeded)
			settled, err := s.ApplyTurnReceipt(ctx, claim, receipt)
			if err != nil || settled.State != domain.TurnStateSucceeded || settled.AcceptedAt == nil ||
				!reflect.DeepEqual(settled.AcceptedAt, before.AcceptedAt) || settled.CandidateText != before.CandidateText ||
				settled.CandidateTerminal != before.CandidateTerminal {
				t.Fatalf("quoted settlement did not retain native evidence: %+v, %v", settled, err)
			}
			page, err := s.ListMessages(ctx, f.groupID, MessageListOptions{})
			if err != nil || len(page.Items) != 3 || settled.ReplyMessageID != "" {
				t.Fatalf("expected the input, original, and one (quoted) publication only: messages=%d quoted=%q native=%q, %v",
					len(page.Items), published.ID, settled.ReplyMessageID, err)
			}
			queueCounts(t, s, 1, 1, 0)
			queueDeliveryState(t, s, settled, domain.DeliveryStateAccepted)
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
			messageSequencePreserved(t, s, map[string][][]any{"message_quotes": quotes})
			r := MessageRequest{GroupID: f.groupID, TopicID: scope.TopicID, ActorID: scope.MemberID, Content: published.Content,
				IdempotencyKey: ordinaryPublicReplyKey, ThreadLinks: domain.ThreadLinks{ReplyToMessageID: original.ID}, RecipientIDs: tc.recipients}
			if got, found, err := s.LookupMessageReceipt(ctx, r); err != nil || !found || !reflect.DeepEqual(got, published) {
				t.Fatalf("quoted send receipt changed: %+v, %t, %v", got, found, err)
			}
		})
	}
}

func TestQueueOrdinaryQuotedSendLegacyAbsentTable(t *testing.T) {
	for _, name := range []string{"unquoted-receipt", "fingerprint-without-edge"} {
		t.Run(name, func(t *testing.T) {
			// Open the actual last pre-quote migration prefix; do not simulate absence by dropping a table.
			all := migrations
			migrations = all[:chatDiscussionWorkReturnVersion]
			t.Cleanup(func() { migrations = all })
			f, _, claim := queueClaimFixture(t)
			s := f.s
			var installed bool
			if err := s.db.QueryRow(messageQuoteAvailable).Scan(&installed); err != nil || installed {
				t.Fatalf("legacy v%d database should not have a quote table: %t, %v", s.schemaVersion, installed, err)
			}
			messageSequenceLedger(t, s, chatDiscussionWorkReturnVersion)
			scope := queueOrdinaryReplyScope(t, s, claim)
			published := queueOrdinaryReplySend(t, s, scope, nil)
			wantNative := name == "fingerprint-without-edge"
			if wantNative {
				original := queueQuoteOriginal(t, s, scope)
				queueRewriteQuotedFingerprint(t, s, published, original.ID)
			}
			queueCandidate(t, s, claim, published.Content, provider.OutcomeSucceeded)
			receipt := queueReceipt(provider.OutcomeSucceeded)
			settled, err := s.ApplyTurnReceipt(ctx, claim, receipt)
			if err != nil || settled.State != domain.TurnStateSucceeded || settled.ReplyMessageID == published.ID ||
				(settled.ReplyMessageID != "") != wantNative {
				t.Fatalf("wrong legacy settlement (want native=%t): %+v, %v", wantNative, settled, err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			migrations = all
			s = queueHandle(t, f.path, false)
			if replay, err := s.ApplyTurnReceipt(ctx, claim, receipt); err != nil || !reflect.DeepEqual(replay, settled) {
				t.Fatalf("lost historical v31 settlement at v%d: %+v, %v", s.schemaVersion, replay, err)
			}
			t.Logf("replayed receipt from v31 without a quote table on v%d (native reply=%t)", s.schemaVersion, wantNative)
		})
	}
}

func queueRewriteQuotedFingerprint(t *testing.T, s *Store, m domain.Message, quotedID string) {
	t.Helper()
	payload := (MessageRequest{GroupID: m.GroupID, TopicID: m.TopicID, Content: m.Content, ThreadLinks: domain.ThreadLinks{ReplyToMessageID: quotedID}}).payload()
	coordinationDecideExec(t, s, `DROP TRIGGER message_command_sources_update`)
	coordinationDecideExec(t, s, `UPDATE message_command_sources SET payload=? WHERE message_id=?`, payload, m.ID)
	coordinationDecideExec(t, s, `UPDATE mutation_idempotency SET payload=? WHERE record_id=?`, payload, m.ID)
}

func TestQueueOrdinaryQuotedSendKeepsOtherNativeReplies(t *testing.T) {
	for _, name := range []string{"different-text", "other-turn-same-key", "other-physical-topic", "recovered-claim",
		"explicit-recipient", "unexpected-delivery", "wrong-receipt", "non-blob-payload", "changed-quote-target",
		"other-quote-topic", "lost-original-source", "changed-excerpt"} {
		t.Run(name, func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			s := f.s
			queueInstallQuoteFixture(t, s)
			scope := queueOrdinaryReplyScope(t, s, claim)
			original := queueQuoteOriginal(t, s, scope)
			var recipients []string
			if name == "explicit-recipient" {
				recipients = []string{f.b}
			}
			published := queueQuotedReplySend(t, s, scope, original, recipients)
			text := published.Content
			switch name {
			case "different-text":
				text += "\n"
			case "other-turn-same-key", "other-physical-topic":
				if ended, err := s.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeSucceeded)); err != nil || ended.State != domain.TurnStateSucceeded || ended.ReplyMessageID != "" {
					t.Fatalf("settling the source Turn without a candidate: %+v, %v", ended, err)
				}
				topicID := scope.TopicID
				if name == "other-physical-topic" {
					topicID += "-other"
				}
				d := queueDelivery(t, s, f.groupID, f.a, topicID)
				next, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: d.ID, MaxActive: 1})
				if err != nil {
					t.Fatal(err)
				}
				claim = TurnClaim{GroupID: next.GroupID, TurnID: next.ID, Token: next.ClaimToken}
				scope = queueOrdinaryReplyScope(t, s, claim)
				if name == "other-turn-same-key" {
					if replay := queueQuotedReplySend(t, s, scope, original, nil); !reflect.DeepEqual(replay, published) {
						t.Fatalf("same key rewrote the original quote receipt: %+v", replay)
					}
				} else {
					// Even a counterfactual source linked to the new claim cannot cross physical topics.
					digest := sha256.Sum256([]byte(claim.Token))
					coordinationDecideExec(t, s, `DROP TRIGGER message_command_sources_update`)
					coordinationDecideExec(t, s, `UPDATE message_command_sources SET turn_id=?,claim_digest=? WHERE message_id=?`, claim.TurnID, digest[:], published.ID)
				}
			case "recovered-claim":
				if n, err := s.RecoverInFlight(ctx); err != nil || n != 1 {
					t.Fatalf("claim recovery: %d, %v", n, err)
				}
				if _, err := s.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeSucceeded)); !errors.Is(err, ErrStaleClaim) {
					t.Fatalf("previous claim was not fenced: %v", err)
				}
				claim.Token = queueTurn(t, s, claim).ClaimToken
			case "explicit-recipient":
				// Loss of its delivery cannot turn a directed fingerprint into a public one.
				coordinationDecideExec(t, s, `DELETE FROM deliveries WHERE id=?`, published.Deliveries[0].ID)
			case "unexpected-delivery":
				conv, err := s.GetOrCreateConversation(ctx, f.groupID, f.b, scope.TopicID)
				if err != nil {
					t.Fatal(err)
				}
				coordinationDecideExec(t, s, `INSERT INTO deliveries(id,message_id,conversation_id,recipient_id,group_id,topic_id,state,created_at)
				 SELECT 'unexpected-delivery',id,?,?,group_id,topic_id,'failed',created_at FROM messages WHERE id=?`, conv.ID, f.b, published.ID)
			case "wrong-receipt":
				coordinationDecideExec(t, s, `UPDATE mutation_idempotency SET record_id='not-this-message' WHERE record_id=?`, published.ID)
			case "non-blob-payload":
				coordinationDecideExec(t, s, `DROP TRIGGER message_command_sources_update`)
				coordinationDecideExec(t, s, `UPDATE message_command_sources SET payload=CAST(payload AS TEXT) WHERE message_id=?`, published.ID)
				coordinationDecideExec(t, s, `UPDATE mutation_idempotency SET payload=CAST(payload AS TEXT) WHERE record_id=?`, published.ID)
			case "changed-quote-target", "other-quote-topic":
				targetScope := scope
				if name == "other-quote-topic" {
					targetScope.TopicID += "-other"
				}
				other := queueQuoteOriginal(t, s, targetScope) // Identical body and source kind, different identity.
				coordinationDecideExec(t, s, `DROP TRIGGER message_quotes_no_update`)
				coordinationDecideExec(t, s, `UPDATE message_quotes SET quoted_message_id=?,quoted_topic_id=? WHERE message_id=?`, other.ID, other.TopicID, published.ID)
				if name == "other-quote-topic" {
					queueRewriteQuotedFingerprint(t, s, published, other.ID) // Isolate the edge's topic fence.
				}
			case "lost-original-source":
				coordinationDecideExec(t, s, `DROP TRIGGER message_command_sources_delete`)
				coordinationDecideExec(t, s, `DELETE FROM message_command_sources WHERE message_id=?`, original.ID)
			case "changed-excerpt":
				coordinationDecideExec(t, s, `DROP TRIGGER message_quotes_no_update`)
				coordinationDecideExec(t, s, `UPDATE message_quotes SET excerpt='not the original' WHERE message_id=?`, published.ID)
			}
			queueCandidate(t, s, claim, text, provider.OutcomeSucceeded)
			turn, err := s.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeSucceeded))
			if err != nil || turn.State != domain.TurnStateSucceeded || turn.ReplyMessageID == "" || turn.ReplyMessageID == published.ID {
				t.Fatalf("expected a distinct native reply: %+v, %v", turn, err)
			}
			reply, err := s.GetMessage(ctx, claim.GroupID, turn.ReplyMessageID)
			if err != nil || reply.SenderID != scope.MemberID || reply.TopicID != scope.TopicID || reply.Content != text ||
				reply.SourceID != "turn:"+turn.ID || reply.ReplyTo != nil || len(reply.Deliveries) != 0 {
				t.Fatalf("wrong native reply: %+v, %v", reply, err)
			}
		})
	}
}

func TestQueueOrdinaryQuotedSendKeepsTypedNativeReply(t *testing.T) {
	f, owner, child, _ := workReportOwner(t, 2)
	s := f.s
	queueInstallQuoteFixture(t, s)
	d, err := s.DelegateWork(ctx, owner.Member, domain.DelegateWorkInput{MemberID: child.ID, Question: "Check the first point."}, "quoted-child")
	if err != nil {
		t.Fatal(err)
	}
	worker := taskWorkTestClaim(t, s, domain.TaskWorkDelivery{GroupID: f.groupID, DeliveryID: d.DeliveryID})
	queueEvent(t, s, worker.Claim, provider.AcceptedEvent, "", "")
	published := queueQuotedReplySend(t, s, worker, queueQuoteOriginal(t, s, worker), nil)
	settleDiscussionNativeReply(t, s, worker.Claim, published, published.Content, provider.OutcomeSucceeded)
}

func TestQueueOrdinaryQuotedSendProbeUsesIndexedLookups(t *testing.T) {
	s, _ := openTestStore(t)
	queueInstallQuoteFixture(t, s)
	plan := resolutionPlan(t, s, ordinaryQuotedTurnReplyMatches, "turn", make([]byte, 32), "group", "member", "topic", ordinaryPublicReplyText, "create_message")
	for _, expected := range []string{
		"SEARCH s USING INDEX message_command_sources_turn (turn_id=?)",
		"SEARCH m USING INDEX sqlite_autoindex_messages_", "SEARCH q USING PRIMARY KEY (message_id=?)",
		"SEARCH e USING COVERING INDEX", "SEARCH d USING COVERING INDEX",
		"sqlite_autoindex_mutation_idempotency_", "(operation=? AND actor_id=? AND group_id=? AND idempotency_key=?)",
	} {
		if !strings.Contains(plan, expected) {
			t.Errorf("missing indexed lookup %q in plan: %s", expected, plan)
		}
	}
	if strings.Contains(plan, "SCAN ") {
		t.Fatalf("unrelated scan in quoted ordinary probe: %s", plan)
	}
}
