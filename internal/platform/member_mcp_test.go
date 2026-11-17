package platform

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"agentworkspace/internal/store"
)

func TestMemberMCPTransportOperationsCopyAndLiveClaim(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	claim := memberCapabilityClaim(t, f, f.groupID)
	tools := NewMemberTools(f.db)
	want := []string{"read_channel", "send_message"}
	token, err := tools.Issue(ctx, claim, MemberToolGrants{Operations: want})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		got, err := tools.TransportOperations(ctx, token)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("live immutable profile: %v, %v", got, err)
		}
		got[0] = "create_member"
	}
	for _, bad := range []string{claim.Token, "unknown-capability"} {
		got, err := tools.TransportOperations(ctx, bad)
		if got != nil {
			t.Fatal("invalid token disclosed operations")
		}
		requireCapabilityDenied(t, err)
	}
	if _, err := f.db.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID); err != nil {
		t.Fatal(err)
	}
	got, err := tools.TransportOperations(ctx, token)
	if got != nil {
		t.Fatal("expired claim disclosed operations")
	}
	requireCapabilityDenied(t, err)
}

func TestMemberMCPTransportRoleProfileAllowsHistoricalReplay(t *testing.T) {
	f := memberRoleCoordinatorFixture(t)
	ctx := context.Background()
	token := memberRoleIssue(t, f.tools, f.coordinatorScope)
	want := []string{"list_members", "read_channel", "search_history", "read_history", "decide_dispatch"}
	before := memberRoleSnapshot(t, f.localMessageFixture)
	got, err := f.tools.TransportOperations(ctx, token)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("coordinator profile: %v, %v", got, err)
	}
	memberRolePreserved(t, f.localMessageFixture, before)
	input := store.CoordinatorDecisionInput{Kind: "decline", Reason: "No eligible work"}
	first, err := f.tools.DecideDispatch(ctx, token, input, "historical")
	if err != nil {
		t.Fatal(err)
	}
	state, err := f.db.ResolveCoordinationExecution(ctx, f.coordinatorScope.Member, f.coordinatorScope.Provenance)
	if err != nil || state.Blocked != store.CoordinationAlreadyDecided {
		t.Fatalf("fixture must no longer permit a new start: %+v, %v", state, err)
	}
	got, err = f.tools.TransportOperations(ctx, token)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("discovery incorrectly used startup readiness: %v, %v", got, err)
	}
	replay, err := f.tools.DecideDispatch(ctx, token, input, "historical")
	if err != nil || replay.Decision.ID != first.Decision.ID {
		t.Fatalf("discovery displaced historical replay: %v", err)
	}
	if _, err := f.db.RequestTurnCancel(ctx, f.groupID, f.coordinatorScope.Member.Claim.TurnID); err != nil {
		t.Fatal(err)
	}
	got, err = f.tools.TransportOperations(ctx, token)
	if got != nil || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatalf("expired role claim remained discoverable: %v, %v", got, err)
	}
}

func TestMemberMCPTransportDiscoveryConcurrentRevoke(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	tools := NewMemberTools(f.db)
	token, err := tools.Issue(ctx, memberCapabilityClaim(t, f, f.groupID), MemberToolGrants{Operations: []string{"read_channel"}})
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			for range 4 {
				operations, err := tools.TransportOperations(ctx, token)
				if err == nil {
					if !slices.Equal(operations, []string{"read_channel"}) {
						t.Error("concurrent discovery changed the registry")
					}
					operations[0] = "send_message"
				} else if !errors.Is(err, store.ErrMemberToolDenied) {
					t.Error(err)
				}
			}
		})
	}
	tools.Revoke(token)
	workers.Wait()
	operations, err := tools.TransportOperations(ctx, token)
	if operations != nil {
		t.Fatal("revoked capability remained discoverable")
	}
	requireCapabilityDenied(t, err)
}
