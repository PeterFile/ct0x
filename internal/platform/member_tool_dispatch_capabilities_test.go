package platform

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"agentworkspace/internal/store"
)

func TestMemberDispatchCapabilitiesEveryTokenIsGuarded(t *testing.T) {
	f := memberRoleCoordinatorFixture(t)
	ctx := context.Background()
	token, err := f.tools.issueCoordinationRole(ctx, f.coordinatorScope)
	if err != nil {
		t.Fatal(err)
	}
	input := store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker.ID, Reason: "能力未知不得自动分派"}
	before := memberRoleSnapshot(t, f.localMessageFixture)
	got, err := f.tools.DecideDispatch(ctx, token, input, "unknown")
	if !errors.Is(err, store.ErrConflict) || !reflect.ValueOf(got).IsZero() {
		t.Fatalf("token without server facts bypassed capability gate: %+v, %v", got, err)
	}
	memberRolePreserved(t, f.localMessageFixture, before)
	f.tools.Revoke(token)

	facts := memberRoleCapabilityFacts(t, f.tools, f.groupID)
	token, err = f.tools.issueCoordinationRoleWithCapabilities(ctx, f.coordinatorScope, facts)
	if err != nil {
		t.Fatal(err)
	}
	defer f.tools.Revoke(token)
	// The caller's slice must not be able to alter the registered snapshot.
	for i := range facts {
		facts[i] = store.DispatchWorkerCapabilityFact{}
	}
	original, err := f.tools.DecideDispatch(ctx, token, input, "known")
	if err != nil || original.Assignment == nil || original.Assignment.WorkerMemberID != f.worker.ID {
		t.Fatalf("frozen facts were not copied into the claimed token: %+v, %v", original, err)
	}
	if _, err := f.raw.Exec("UPDATE members SET provider_id='changed',model='changed' WHERE id=?", f.worker.ID); err != nil {
		t.Fatal(err)
	}
	before = memberRoleSnapshot(t, f.localMessageFixture)
	replay, err := f.tools.DecideDispatch(ctx, token, input, "known")
	if err != nil || !reflect.DeepEqual(replay, original) {
		t.Fatalf("capability invalidation hid original token receipt: %+v, %v", replay, err)
	}
	memberRolePreserved(t, f.localMessageFixture, before)
}
