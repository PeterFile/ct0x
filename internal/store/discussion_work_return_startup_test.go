package store

import (
	"crypto/rand"
	"testing"

	"agentworkspace/internal/domain"
)

func TestVerifyDiscussionWorkReturnStartupRejectsStaleClaim(t *testing.T) {
	ctx := t.Context()
	f := setupMessages(t)
	const topic, key = "bound-topic", "return-cwd"
	d := queueDelivery(t, f.s, f.groupID, f.a, topic)
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: d.ID, ResourceKeys: []string{key}, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	m, err := f.s.GetMember(ctx, f.groupID, f.a)
	if err != nil {
		t.Fatal(err)
	}
	route := domain.ChatDiscussionWorkReturnReceipt{ThreadRef: domain.ThreadRef{GroupID: f.groupID, TopicID: topic},
		DeliveryID: d.ID, MessageID: d.MessageID, ConversationID: d.ConversationID, RecipientID: d.RecipientID}
	claim := TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	verify := func(c TurnClaim) error {
		return f.s.VerifyDiscussionWorkReturnStartup(ctx, c, route, m.ProviderID, m.Model, key)
	}
	if err := verify(claim); err != nil {
		t.Fatalf("current claim denied: %v", err)
	}
	// Isolate the stale token: the Turn is still starting and still owns the exact key.
	current := claim
	current.Token = rand.Text()
	coordinationDecideExec(t, f.s, `UPDATE turns SET claim_token=? WHERE id=? AND claim_token=?`, current.Token, claim.TurnID, claim.Token)
	requireMemberToolDenied(t, verify(claim))
	if err := verify(current); err != nil {
		t.Fatalf("replacement claim denied: %v", err)
	}
}
