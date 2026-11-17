package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
)

func chatCandidateMetadataSource(t *testing.T, version int) (messageFixture, domain.Message) {
	t.Helper()
	original := migrations
	defer func() { migrations = original }()
	migrations = original[:version]
	f := setupMessages(t)
	message := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: "unconfigured-workspace-topic", ActorID: "user", Content: "请先讨论已有授权目录的工作边界。", RecipientIDs: []string{f.a, f.b}}, []string{f.a, f.b})
	return f, message
}

func chatCandidateMetadataScope(t *testing.T, f messageFixture, message domain.Message, memberID string) MemberToolScope {
	t.Helper()
	for _, delivery := range message.Deliveries {
		if delivery.RecipientID != memberID {
			continue
		}
		turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: delivery.ID, MaxActive: 2})
		if err != nil {
			t.Fatal(err)
		}
		scope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
		if err != nil {
			t.Fatal(err)
		}
		return scope
	}
	t.Fatal("missing member delivery")
	return MemberToolScope{}
}

func chatCandidateMetadataRead(t *testing.T, s *Store, scope MemberToolScope, id string) (domain.ChatCandidate, error) {
	t.Helper()
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	return chatCandidateInTx(ctx, tx, scope, id)
}

func TestChatCandidateWorkspaceMetadataUnconfiguredSnapshot(t *testing.T) {
	for _, version := range []int{19, 20} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			f, message := chatCandidateMetadataSource(t, version)
			scope := chatCandidateMetadataScope(t, f, message, f.a)
			thread := domain.ThreadRef{GroupID: f.groupID, TopicID: message.TopicID}
			policy, err := f.s.GetChatPolicy(ctx, thread)
			if err != nil {
				t.Fatal(err)
			}
			want := domain.ChatPolicy{ThreadRef: thread, WorkspaceChangesSupported: version >= 20}
			if !reflect.DeepEqual(policy, want) {
				t.Fatalf("unexpected unconfigured policy: %+v", policy)
			}
			candidate, err := f.s.CaptureChatCandidate(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(candidate.Policy, want) {
				t.Fatalf("candidate metadata disagrees with unconfigured policy: candidate=%+v policy=%+v", candidate.Policy, policy)
			}
			if candidate.MessageID != message.ID || candidate.SourceKind != "local_user_chat" || candidate.SourceID == "" || candidate.SourceSenderID != "user" {
				t.Fatalf("capture changed trusted source: %+v", candidate)
			}

			configured, err := f.s.SetChatPolicy(ctx, thread, domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 1, AllowWorkspaceChanges: version >= 20}, 0, "later-policy")
			if err != nil || configured.Revision != 1 || !configured.Enabled || configured.AllowWorkspaceChanges != (version >= 20) {
				t.Fatalf("later policy setup: %+v %v", configured, err)
			}
			freshScope, err := f.s.BindMemberTool(ctx, scope.Claim)
			if err != nil {
				t.Fatal(err)
			}
			read, err := chatCandidateMetadataRead(t, f.s, freshScope, candidate.ID)
			if err != nil || !reflect.DeepEqual(read, candidate) {
				t.Fatalf("candidate read reinterpreted captured rev0: %+v %v", read, err)
			}
			recaptured, err := f.s.CaptureChatCandidate(ctx, freshScope)
			if err != nil || !reflect.DeepEqual(recaptured, candidate) {
				t.Fatalf("rebinding acquired later policy/source: %+v %v", recaptured, err)
			}
			var stillUnconfigured bool
			if err := f.s.db.QueryRow("SELECT policy_revision IS NULL FROM chat_candidates WHERE id=?", candidate.ID).Scan(&stillUnconfigured); err != nil || !stillUnconfigured {
				t.Fatalf("capture snapshot was rewritten: %t %v", stillUnconfigured, err)
			}
		})
	}
}

func TestChatCandidateWorkspaceMetadataSameSourceBoundScopes(t *testing.T) {
	f, message := chatCandidateMetadataSource(t, 20)
	firstScope := chatCandidateMetadataScope(t, f, message, f.a)
	secondScope := chatCandidateMetadataScope(t, f, message, f.b)
	first, err := f.s.CaptureChatCandidate(ctx, firstScope)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.s.CaptureChatCandidate(ctx, secondScope)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Policy.WorkspaceChangesSupported || !second.Policy.WorkspaceChangesSupported || first.Policy.AllowWorkspaceChanges || second.Policy.AllowWorkspaceChanges || first.Policy.Revision != 0 || second.Policy.Revision != 0 {
		t.Fatalf("support metadata missing or turned into permission: first=%+v second=%+v", first.Policy, second.Policy)
	}
	if first.ID == second.ID || first.MemberID != f.a || second.MemberID != f.b || first.TurnID != firstScope.Claim.TurnID || second.TurnID != secondScope.Claim.TurnID || first.MessageID != message.ID || second.MessageID != message.ID || first.SourceID == "" || first.SourceID != second.SourceID || first.SourceKind != "local_user_chat" || second.SourceKind != "local_user_chat" {
		t.Fatalf("same source fabricated execution/source identity: first=%+v second=%+v", first, second)
	}
	if _, err := chatCandidateMetadataRead(t, f.s, secondScope, first.ID); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("new scope read another execution's candidate: %v", err)
	}
}

func TestChatCandidateWorkspaceMetadataReadErrorPropagates(t *testing.T) {
	f, message := chatCandidateMetadataSource(t, 20)
	scope := chatCandidateMetadataScope(t, f, message, f.a)
	if _, err := f.s.CaptureChatCandidate(ctx, scope); err != nil {
		t.Fatal(err)
	}
	coordinationDecideExec(t, f.s, "ALTER TABLE schema_migrations RENAME TO unavailable_metadata_ledger")
	candidate, err := f.s.CaptureChatCandidate(ctx, scope)
	if err == nil || errors.Is(err, ErrMemberToolDenied) || !reflect.ValueOf(candidate).IsZero() {
		t.Fatalf("actual metadata SQL error became a candidate or authorization denial: %+v %v", candidate, err)
	}
}
