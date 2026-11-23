package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func TestChatDiscussionTransactionOutcome(t *testing.T) {
	for _, mode := range []string{"confirmed-rollback", "lost-rollback-receipt", "lost-commit-receipt"} {
		t.Run(mode, func(t *testing.T) {
			f := setupMessages(t)
			commit := mode == "lost-commit-receipt"
			topic := strings.Repeat("<", 4000) // The unwind budget fails after publication and key insertion.
			if commit {
				topic = "relay"
			}
			origin := discussionUser(t, f, topic, f.a)
			turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: origin.Deliveries[0].ID,
				MaxActive: 1, ResourceKeys: []string{"workspace-relay"}})
			if err != nil {
				t.Fatal(err)
			}
			scope, candidate := discussionCapture(t, f.s, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
			discussionAssess(t, f.s, scope, candidate)
			const key = "Relay/Exact:1"
			input := domain.ChatDiscussionAction{Kind: "invite", Content: "请核对结论。", RecipientID: f.b}
			before := messageSequenceSnapshot(t, f.s)
			ack := errors.New("PRIVATE lost rollback receipt")
			cause := ErrChatDiscussionContextTooLarge
			var gateway *Store
			var boundary *assignmentBoundaryDriver
			if commit {
				cause = ErrInvalid // Even this business-looking commit error must stay unknown.
				gateway, _ = coordinationDecideWrapped(t, f.path, "ack-error", cause)
			} else {
				if mode == "confirmed-rollback" {
					ack = nil
				}
				gateway, boundary = assignmentBoundaryStore(t, f.path, ack)
			}
			got, err := gateway.ApplyChatDiscussion(ctx, scope, candidate, input, key)
			if !reflect.DeepEqual(got, domain.ChatDiscussionReceipt{}) || !errors.Is(err, cause) {
				t.Fatalf("no success receipt expected; original cause must remain available: %+v, %v", got, err)
			}
			if boundary != nil && (len(boundary.events) != 1 || boundary.events[0].rollbackCalls != 1 || boundary.events[0].rollbackErr != nil) {
				t.Fatalf("expected a real SQLite rollback: %+v", boundary.events)
			}
			if commit || ack != nil {
				var evidence interface {
					CommitOutcomeUnknown() bool
					RollbackConfirmed() bool
				}
				if !errors.Is(err, ErrChatDiscussionMutationUnknown) || !errors.As(err, &evidence) ||
					evidence.CommitOutcomeUnknown() != commit || evidence.RollbackConfirmed() ||
					(ack != nil && !commit && !errors.Is(err, ack)) || strings.Contains(err.Error(), "PRIVATE") {
					t.Fatalf("uncertainty or its evidence was lost: %v", err)
				}
			} else if err != ErrChatDiscussionContextTooLarge {
				t.Fatalf("confirmed rollback changed the context-limit rejection: %v", err)
			}
			if !commit {
				messageSequencePreserved(t, f.s, before)
			} else {
				var id string
				if err := f.s.db.QueryRow(`SELECT record_id FROM mutation_idempotency WHERE operation='create_message' AND actor_id=? AND group_id=? AND idempotency_key=?`,
					f.a, f.groupID, key).Scan(&id); err != nil || id == "" {
					t.Fatalf("the receipt was lost, not the committed key: %v", err)
				}
				after := messageSequenceSnapshot(t, f.s)
				if replay := discussionApply(t, f.s, scope, candidate, key, input); replay.Message.ID != id || len(replay.Message.Deliveries) != 1 {
					t.Fatal("explicit reconciliation with the original request/key did not reach the original receipt")
				}
				input.Content += "已更改。"
				if _, err := f.s.ApplyChatDiscussion(ctx, scope, candidate, input, key); !errors.Is(err, ErrConflict) || errors.Is(err, ErrChatDiscussionMutationUnknown) {
					t.Fatalf("a changed request under the original key must be a definite conflict: %v", err)
				}
				messageSequencePreserved(t, f.s, after)
			}
			var owner string
			if err := f.s.db.QueryRow("SELECT turn_id FROM resource_claims WHERE resource_key='workspace-relay'").Scan(&owner); err != nil || owner != turn.ID {
				t.Fatalf("write outcome changed the live workspace claim: %v", err)
			}
		})
	}
}
