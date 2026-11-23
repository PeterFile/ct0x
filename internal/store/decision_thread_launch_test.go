package store

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func threadDecisionLaunchForTest(t *testing.T, s *Store, wake ThreadDecisionWake, keys []string) ThreadDecisionLaunch {
	t.Helper()
	m, err := s.GetMember(ctx, wake.Receipt.GroupID, wake.AskerID)
	if err != nil {
		t.Fatal(err)
	}
	return ThreadDecisionLaunch{Wake: wake, ProviderID: m.ProviderID, Model: m.Model, Workspace: m.Workspace, ResourceKeys: keys}
}

func claimedThreadLaunchFixture(t *testing.T) (chatFixture, TurnClaim, ThreadDecisionLaunch) {
	t.Helper()
	f, q := threadDecisionFixture(t)
	a := saveThreadDecisionForTest(t, f.s, q)
	wake, err := f.s.ResolveThreadDecisionWake(ctx, q.GroupID, a.WakeDeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{"thread-text", "thread-directory"} // Unsorted; both exact claims are required.
	launch := threadDecisionLaunchForTest(t, f.s, wake, keys)
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: q.GroupID, DeliveryID: a.WakeDeliveryID,
		ThreadDecisionAnswerID: a.ID, ResourceKeys: keys, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	return f, TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}, launch
}

func TestThreadDecisionLaunchRejectsDriftBeforeAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, change string
		member       bool
	}{
		{"provider-changed", `UPDATE members SET provider_id='other' WHERE id=?`, true},
		{"provider-cleared", `UPDATE members SET provider_id='' WHERE id=?`, true},
		{"model-changed", `UPDATE members SET model='other' WHERE id=?`, true},
		{"model-cleared", `UPDATE members SET model='' WHERE id=?`, true},
		{"workspace-changed", `UPDATE members SET workspace='/other' WHERE id=?`, true},
		{"workspace-cleared", `UPDATE members SET workspace='' WHERE id=?`, true},
		{"resource-missing", `DELETE FROM resource_claims WHERE turn_id=? AND resource_key='thread-text'`, false},
		{"resource-changed", `UPDATE resource_claims SET resource_key='other' WHERE turn_id=? AND resource_key='thread-text'`, false},
		{"resource-extra", `INSERT INTO resource_claims(resource_key,turn_id) VALUES('extra',?)`, false},
		{"receipt", `UPDATE turns SET receipt_json='{}' WHERE id=?`, false},
		{"barrier", `INSERT INTO native_run_bindings(turn_id,group_id,member_id,conversation_id,purpose,binding_version)
			SELECT id,group_id,member_id,conversation_id,'unknown',0 FROM turns WHERE id=?`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, claim, launch := claimedThreadLaunchFixture(t)
			peer := queueHandle(t, f.path, true)
			id := claim.TurnID
			if tc.member {
				id = launch.Wake.AskerID
			}
			coordinationDecideExec(t, peer, tc.change, id) // A committed writer after the plan was frozen.
			before := messageSequenceSnapshot(t, peer)
			calls := 0
			if err := f.s.CommitThreadDecisionLaunch(ctx, claim, launch, func() bool { calls++; return true }); !errors.Is(err, ErrMemberToolDenied) || calls != 0 {
				t.Fatalf("stale startup facts reached owner: %v, calls=%d", err, calls)
			}
			messageSequencePreserved(t, peer, before)
		})
	}
}

func TestThreadDecisionLaunchRereadsBindingAfterClaimLock(t *testing.T) {
	f, claim, launch := claimedThreadLaunchFixture(t)
	coordinationDecideExec(t, f.s, `CREATE TRIGGER fixture_barrier_at_launch AFTER UPDATE OF claim_token ON turns
	 WHEN NEW.id='`+claim.TurnID+`' BEGIN
	 INSERT INTO native_run_bindings(turn_id,group_id,member_id,conversation_id,purpose,binding_version)
	 VALUES(NEW.id,NEW.group_id,NEW.member_id,NEW.conversation_id,'unknown',0); END`)
	before := messageSequenceSnapshot(t, f.s)
	if err := f.s.CommitThreadDecisionLaunch(ctx, claim, launch, func() bool {
		t.Fatal("a barrier inserted by the claim-lock statement reached the owner")
		return false
	}); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("post-lock barrier: %v", err)
	}
	messageSequencePreserved(t, f.s, before)
}

func TestThreadDecisionLaunchFencesWritersAndSpendsOriginalOwnerOnce(t *testing.T) {
	f, claim, launch := claimedThreadLaunchFixture(t)
	gate, peer := queueHandle(t, f.path, true), nativePeerWithoutWaiting(t, f.messageFixture)
	gate.EnableThreadDecisionExecution()
	var spent atomic.Bool
	calls := 0
	entered, release := make(chan struct{}), make(chan struct{})
	admit := func() bool {
		calls++
		if !spent.CompareAndSwap(false, true) {
			return false
		}
		// Test-only rendezvous; the callback itself performs no I/O.
		close(entered)
		<-release
		return true
	}
	for _, tc := range []struct {
		name string
		omit func(*ThreadDecisionLaunch)
		want error
	}{
		{"provider", func(l *ThreadDecisionLaunch) { l.ProviderID = "" }, ErrInvalid},
		{"model", func(l *ThreadDecisionLaunch) { l.Model = "" }, ErrInvalid},
		{"workspace", func(l *ThreadDecisionLaunch) { l.Workspace = "" }, ErrMemberToolDenied},
		{"resources", func(l *ThreadDecisionLaunch) { l.ResourceKeys = nil }, ErrInvalid},
	} {
		bad := launch
		tc.omit(&bad)
		if err := gate.CommitThreadDecisionLaunch(ctx, claim, bad, func() bool { calls++; return true }); !errors.Is(err, tc.want) || calls != 0 {
			t.Fatalf("omitted %s reached owner: %v, calls=%d", tc.name, err, calls)
		}
	}
	before := messageSequenceSnapshot(t, peer)
	done := make(chan error, 1)
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	go func() { done <- gate.CommitThreadDecisionLaunch(ctx, claim, launch, admit) }()
	select {
	case err := <-done:
		t.Fatalf("exact plan did not reach admission: %v", err)
	case <-entered:
		requireNativeWriterHeld(t, peer, claim.TurnID)
	}
	unblock()
	if err := <-done; err != nil || calls != 1 {
		t.Fatalf("exact launch did not commit with one callback: %v, calls=%d", err, calls)
	}
	if err := gate.CommitThreadDecisionLaunch(ctx, claim, launch, admit); !errors.Is(err, ErrMemberToolDenied) || calls != 2 {
		t.Fatalf("a spent owner authorized a second launch: %v, calls=%d", err, calls)
	}
	messageSequencePreserved(t, peer, before) // No execution evidence or additional claim.
}
