package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestChatAssessmentDiscussLegacyInputSuppressesDuplicateNativeReply(t *testing.T) {
	for _, target := range []struct {
		name    string
		version int
	}{
		{"current-schema", len(migrations)},
		{"pre-typed-delivery-schema", taskWorkInputVersion},
	} {
		t.Run(target.name, func(t *testing.T) {
			all := migrations
			defer func() { migrations = all }()
			migrations = all[:16] // Queue the input before v17 added command provenance.
			f := setupMessages(t)
			request := MessageRequest{GroupID: f.groupID, TopicID: "legacy-topic", ActorID: "user",
				Content: "Can we discuss the summary?", RecipientIDs: []string{f.a}, IdempotencyKey: "legacy-input"}
			origin, err := f.s.CreateMessage(ctx, request, request.RecipientIDs)
			if err != nil || len(origin.Deliveries) != 1 || origin.Deliveries[0].State != domain.DeliveryStateQueued || origin.Deliveries[0].TurnID != "" {
				t.Fatalf("expected a queued legacy message: %+v, %v", origin, err)
			}
			if err := f.s.Close(); err != nil {
				t.Fatal(err)
			}
			migrations = all[:target.version]
			s := queueHandle(t, f.path, false)
			messageSequenceLedger(t, s, target.version)

			replay := chatLocalMessage(t, s, request, request.RecipientIDs)
			if replay.ID != origin.ID || !reflect.DeepEqual(replay.Deliveries, origin.Deliveries) || replay.SourceKind != "unknown" || replay.SourceID != "" {
				t.Fatalf("replaying the legacy message changed its delivery or granted provenance: %+v", replay)
			}
			deliveryID := origin.Deliveries[0].ID
			if _, err := s.ResolveTaskWorkDelivery(ctx, f.groupID, deliveryID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("legacy delivery should not be typed work: %v", err)
			}
			scope, candidate := discussionArrive(t, s, f.groupID, deliveryID)
			claim := scope.Claim
			queueEvent(t, s, claim, provider.AcceptedEvent, "", "")

			const text = "Ready to discuss."
			r, err := s.ApplyChatAssessment(ctx, scope, candidate, domain.ChatAssessment{
				Disposition: "discuss", Reason: "The user asked to discuss the summary.",
			}, text, "legacy-discuss")
			if err != nil || r.Disposition != "discuss" || r.Outcome != "discuss" ||
				r.CandidateID != candidate || r.MessageID != origin.ID || r.TurnID != claim.TurnID ||
				r.Purpose != domain.ChatIntakePurpose || r.SourceKind != "unknown" || r.SourceID != "" || r.AuthorityEvidenceVerified {
				t.Fatalf("expected a discuss assessment of unknown-source intake: %+v, %v", r, err)
			}
			notice, err := s.GetMessage(ctx, f.groupID, r.NoticeMessageID)
			if err != nil || notice.Content != text || notice.SenderID != f.a || notice.TopicID != origin.TopicID ||
				notice.SourceID != "assessment:"+r.ID || len(notice.Deliveries) != 0 {
				t.Fatalf("expected a public assessment notice from the recipient: %+v, %v", notice, err)
			}

			queueEvent(t, s, claim, provider.ResponseEvent, text, "")
			queueEvent(t, s, claim, provider.TerminalEvent, "", provider.OutcomeSucceeded)
			settled, err := s.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeSucceeded))
			if err != nil || settled.State != domain.TurnStateSucceeded || settled.AcceptedAt == nil ||
				settled.CandidateText != text || settled.CandidateTerminal != domain.TurnStateSucceeded {
				t.Fatalf("expected a settled native success with the exact notice text: %+v, %v", settled, err)
			}
			var kind, sourceID string
			var hasCommandSource bool
			if err := s.db.QueryRow(`SELECT source_kind,source_id,EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=?)
			 FROM chat_intake_executions WHERE candidate_id=? AND turn_id=?`, origin.ID, candidate, claim.TurnID).
				Scan(&kind, &sourceID, &hasCommandSource); err != nil || kind != "unknown" || sourceID != "" || hasCommandSource {
				t.Fatalf("expected unupgraded intake: kind=%q source=%q command source exists=%t, %v", kind, sourceID, hasCommandSource, err)
			}
			if settled.ReplyMessageID != "" {
				t.Fatalf("duplicate native reply %q for legacy discuss notice %q", settled.ReplyMessageID, notice.ID)
			}
			if page, err := s.ListMessages(ctx, f.groupID, MessageListOptions{}); err != nil || len(page.Items) != 2 {
				t.Fatalf("expected only the legacy input and its assessment notice: %+v, %v", page, err)
			}
		})
	}
}

func TestChatAssessmentDiscussNoticeProbeUsesIndexedLookups(t *testing.T) {
	s, _ := openTestStore(t)
	plan := resolutionPlan(t, s, discussNoticeTurnReplyMatch, "delivery", "conversation", "turn", "group", "topic", "member", "Ready to discuss.")
	for _, want := range []struct{ index, key string }{
		{"sqlite_autoindex_chat_candidates_", "(turn_id=?)"},
		{"sqlite_autoindex_chat_assessments_", "(candidate_id=?)"},
		{"sqlite_autoindex_messages_", "(id=?"},
		{"sqlite_autoindex_deliveries_", "(message_id=?)"},
	} {
		found := false
		for line := range strings.SplitSeq(plan, "\n") {
			if strings.HasPrefix(line, "SEARCH ") && strings.Contains(line, want.index) && strings.Contains(line, want.key) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing indexed lookup on %s for %s in plan:\n%s", want.index, want.key, plan)
		}
	}
	if strings.Contains(strings.ReplaceAll(plan, "SCAN CONSTANT ROW", ""), "SCAN ") {
		t.Fatalf("table scan in discuss notice probe:\n%s", plan)
	}
}
