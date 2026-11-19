package platform

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"agentworkspace/internal/store"
)

func TestMemberMCPRequestBindingCapacityAndRevocation(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	tools := NewMemberTools(f.db)
	claim := memberCapabilityClaim(t, f, f.groupID)
	grants := MemberToolGrants{Operations: []string{"read_channel"}}
	token, err := tools.Issue(ctx, claim, grants)
	if err != nil {
		t.Fatal(err)
	}
	id, original := sha256.Sum256([]byte("first")), sha256.Sum256([]byte("original request"))
	changed := sha256.Sum256([]byte("changed request"))
	for _, bad := range []string{"", "unknown-token", claim.Token} {
		requireCapabilityDenied(t, tools.BindMCPRequest(bad, id, original))
	}
	if tools.mcpRequests != nil {
		t.Fatal("unknown tokens allocated request maps")
	}
	restarted := NewMemberTools(f.db)
	requireCapabilityDenied(t, restarted.BindMCPRequest(token, id, original))
	if restarted.mcpRequests != nil {
		t.Fatal("a new registry inherited a previous process token")
	}
	if err := tools.BindMCPRequest(token, id, original); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < maxMemberMCPRequests; i++ {
		request := sha256.Sum256([]byte(fmt.Sprintf("request-%d", i)))
		if err := tools.BindMCPRequest(token, request, original); err != nil {
			t.Fatal(err)
		}
	}
	extra := sha256.Sum256([]byte("over capacity"))
	if err := tools.BindMCPRequest(token, extra, original); !errors.Is(err, ErrMemberMCPRequestCapacity) {
		t.Fatalf("new id was not rejected at capacity: %v", err)
	}
	if err := tools.BindMCPRequest(token, id, original); err != nil {
		t.Fatalf("capacity blocked an existing request replay: %v", err)
	}
	if err := tools.BindMCPRequest(token, id, changed); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("capacity evicted or replaced the original binding: %v", err)
	}
	key := sha256.Sum256([]byte(token))
	if len(tools.mcpRequests[key].ordinary) != maxMemberMCPRequests || tools.mcpRequests[key].ordinary[id] != original {
		t.Fatal("capacity handling modified live bindings")
	}
	tools.Revoke(token)
	if len(tools.mcpRequests) != 0 {
		t.Fatal("Revoke did not release the token's request bindings")
	}
	requireCapabilityDenied(t, tools.BindMCPRequest(token, id, original))
	replacement, err := tools.Issue(ctx, claim, grants)
	if err != nil || replacement == token {
		t.Fatalf("explicit revocation did not permit a distinct capability: %v", err)
	}
	if err := tools.BindMCPRequest(replacement, id, changed); err != nil {
		t.Fatalf("new token collided with the revoked token's id: %v", err)
	}
	tools.RevokeTurn(claim.TurnID)
	if len(tools.mcpRequests) != 0 {
		t.Fatal("RevokeTurn did not release its token's bindings")
	}
	requireCapabilityDenied(t, tools.BindMCPRequest(replacement, id, changed))
}
