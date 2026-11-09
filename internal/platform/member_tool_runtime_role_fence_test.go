package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// Uncompiled test draft: the private fence API and readTurn seam are provisional.
// One core instance models a future runtime-owned fence; nothing here wires it
// into MemberToolRuntime, grants tools, calls an adapter, or tests Revoke/drain.
// The proposed core keeps at most M records/slots and a persistent GC cursor,
// with at most M GC reads per admission round and no Release, Reset, or TTL.
type memberToolRoleFenceFixture struct {
	localMessageFixture
	runtime   *MemberToolRuntime
	fence     *memberToolRoleFence
	root      string
	maxActive int
}

func setupMemberToolRoleFence(t *testing.T, maxActive int) memberToolRoleFenceFixture {
	t.Helper()
	local := setupLocalMessages(t)
	base := t.TempDir()
	configRoot, executionRoot := filepath.Join(base, "config"), filepath.Join(base, "runtime")
	for _, root := range []string{configRoot, executionRoot} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	runtime, err := NewMemberToolRuntime(local.db, MemberToolRuntimeOptions{
		ConfigRoot: configRoot, Endpoint: "http://127.0.0.1:7310/api/v1/member-tools",
		Operations: []string{"list_members"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Use the runtime's Store, the same M used by ClaimTurn below, and the
	// execution root, not the credential ConfigRoot. Composition is not tested.
	fence, err := newMemberToolRoleFence(runtime.Tools().db, maxActive, executionRoot)
	if err != nil || fence == nil {
		t.Fatalf("construct fence: %v", err)
	}
	return memberToolRoleFenceFixture{local, runtime, fence, executionRoot, maxActive}
}

func (f memberToolRoleFenceFixture) claim(t *testing.T, groupID string) store.TurnClaim {
	t.Helper()
	member := f.member(t, groupID)
	message, err := f.local.CreateMessage(context.Background(), groupID, MessageInput{
		Content: "input", TopicID: "topic", RecipientIDs: []string{member.ID},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	deliveryID := message.Deliveries[0].ID
	turn, err := f.db.ClaimTurn(context.Background(), store.ClaimTurnInput{
		GroupID: groupID, DeliveryID: deliveryID, MaxActive: f.maxActive,
		ResourceKeys: []string{"role-fence/" + deliveryID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
}

func (f memberToolRoleFenceFixture) reserve(t *testing.T, claim store.TurnClaim) *memberToolRoleFenceOwner {
	t.Helper()
	owner, err := f.fence.reserve(context.Background(), claim, f.root)
	if err != nil || owner == nil {
		t.Fatalf("reserve claimed turn: %v", err)
	}
	return owner
}

func (f memberToolRoleFenceFixture) deny(t *testing.T, ctx context.Context, claim store.TurnClaim, root string) {
	t.Helper()
	if owner, err := f.fence.reserve(ctx, claim, root); err == nil || owner != nil {
		t.Fatalf("denied admission returned an owner or no error: %v", err)
	}
}

func (f memberToolRoleFenceFixture) requireTurn(t *testing.T, claim store.TurnClaim, state domain.TurnState, resources int) domain.Turn {
	t.Helper()
	turn, err := f.db.GetTurn(context.Background(), claim.GroupID, claim.TurnID)
	if err != nil || turn.ID != claim.TurnID || turn.GroupID != claim.GroupID || turn.ClaimToken != claim.Token || turn.State != state {
		t.Fatalf("unexpected durable turn identity/state: state=%s, error=%v", turn.State, err)
	}
	var count int
	if err := f.raw.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", claim.TurnID).Scan(&count); err != nil || count != resources {
		t.Fatalf("resource claims=%d, want %d: %v", count, resources, err)
	}
	return turn
}

// The only proposed read seam is fence.readTurn, with Store.GetTurn's signature.
// The hook runs inside that read call, after the SAME real Store returned a row.
// It may delay or reject the read, but cannot manufacture or change a DB row.
// Install it before concurrent admissions; never replace it while calls run.
type memberToolRoleFenceReader struct {
	db    *store.Store
	after func(context.Context, domain.Turn) error
}

func (r memberToolRoleFenceReader) getTurn(ctx context.Context, groupID, turnID string) (domain.Turn, error) {
	turn, err := r.db.GetTurn(ctx, groupID, turnID)
	if err == nil && r.after != nil {
		err = r.after(ctx, turn)
	}
	return turn, err
}

func TestMemberToolRoleFenceConcurrentOwner(t *testing.T) {
	f := setupMemberToolRoleFence(t, 2) // Spare capacity must not mask a duplicate-Turn bug.
	claim := f.claim(t, f.groupID)
	before := f.requireTurn(t, claim, domain.TurnStateStarting, 1)
	guard, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const contenders = 8
	type reservation struct {
		owner *memberToolRoleFenceOwner
		err   error
	}
	start, results := make(chan struct{}), make(chan reservation, contenders)
	for i := 0; i < contenders; i++ {
		go func() {
			<-start
			owner, err := f.fence.reserve(guard, claim, f.root)
			results <- reservation{owner, err}
		}()
	}
	close(start)
	var winner *memberToolRoleFenceOwner
	wins := 0
	for i := 0; i < contenders; i++ {
		select {
		case result := <-results:
			if result.err == nil {
				if result.owner == nil {
					t.Fatal("successful reservation has no opaque owner")
				}
				winner, wins = result.owner, wins+1
			} else if result.owner != nil {
				t.Fatal("failed reservation exposed an owner")
			}
		case <-guard.Done():
			t.Fatal("reservation contenders did not finish")
		}
	}
	if wins != 1 {
		t.Fatalf("same Turn obtained %d owners, want one", wins)
	}
	invokeStart, invoked := make(chan struct{}), make(chan bool, contenders)
	for i := 0; i < contenders; i++ {
		go func() {
			<-invokeStart
			invoked <- winner.beginInvoke()
		}()
	}
	close(invokeStart)
	opportunities := 0
	for i := 0; i < contenders; i++ {
		select {
		case allowed := <-invoked:
			if allowed {
				opportunities++
			}
		case <-guard.Done():
			t.Fatal("owner invocation checks did not finish")
		}
	}
	if opportunities != 1 {
		t.Fatalf("owner granted %d invocation opportunities, want one", opportunities)
	}
	winner.markLocalDone()
	f.deny(t, guard, claim, f.root)
	after := f.requireTurn(t, claim, domain.TurnStateStarting, 1)
	if !reflect.DeepEqual(before, after) || len(f.runtime.Tools().capabilities) != 0 {
		t.Fatal("core changed the Store or granted model tools")
	}

	// A real starting DB row with a replacement token must not create another
	// opportunity. Recovery's separate unknown/token transition is tested below.
	replacement := "replacement-" + claim.Token
	result, err := f.raw.Exec("UPDATE turns SET claim_token=? WHERE group_id=? AND id=? AND claim_token=?", replacement, claim.GroupID, claim.TurnID, claim.Token)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("change fixture token: rows=%d, error=%v", n, err)
	}
	claim.Token = replacement
	f.requireTurn(t, claim, domain.TurnStateStarting, 1)
	f.deny(t, guard, claim, f.root)
	if winner.beginInvoke() {
		t.Fatal("a completed owner regained an invocation opportunity")
	}
}

func TestMemberToolRoleFenceCapacityCursorAndOwnerReuse(t *testing.T) {
	const capacity = 3
	f := setupMemberToolRoleFence(t, capacity)
	claims := make([]store.TurnClaim, 0, capacity)
	owners := make([]*memberToolRoleFenceOwner, 0, capacity)
	for i := 0; i < capacity; i++ {
		claim := f.claim(t, f.groupID)
		claims = append(claims, claim)
		owners = append(owners, f.reserve(t, claim))
	}
	var reads []string
	failRead := false
	readFailure := errors.New("injected Store read failure")
	reader := memberToolRoleFenceReader{db: f.db, after: func(ctx context.Context, turn domain.Turn) error {
		if _, bounded := ctx.Deadline(); !bounded {
			t.Error("fence Store read has no bounded context")
		}
		reads = append(reads, turn.ID)
		if failRead {
			return readFailure
		}
		return nil
	}}
	f.fence.readTurn = reader.getTurn
	if err := f.fence.checkCapacity(context.Background()); err == nil || len(reads) != 0 {
		t.Fatal("live owners were reclaimed or queried before localDone")
	}
	for _, owner := range owners {
		owner.markLocalDone()
	}
	if err := f.fence.checkCapacity(context.Background()); err == nil || len(reads) != capacity {
		t.Fatalf("full round read %d rows, want M=%d and no admission", len(reads), capacity)
	}
	seen := make(map[string]bool)
	for _, id := range reads {
		seen[id] = true
	}
	if len(seen) != capacity {
		t.Fatal("one full round did not visit each occupied slot once")
	}

	// An error ends the current round but must still advance its persistent
	// cursor. Four one-read failures expose all three slots, then wrap around.
	failRead = true
	var firstReads []string
	for i := 0; i < capacity+1; i++ {
		reads = nil
		if err := f.fence.checkCapacity(context.Background()); err == nil || len(reads) != 1 {
			t.Fatalf("read failure did not stop admission after one query: reads=%d", len(reads))
		}
		firstReads = append(firstReads, reads[0])
	}
	seen = make(map[string]bool)
	for _, id := range firstReads[:capacity] {
		seen[id] = true
	}
	if len(seen) != capacity || firstReads[capacity] != firstReads[0] {
		t.Fatal("GC cursor restarted, skipped a slot, or failed to wrap")
	}
	failRead, reads = false, nil
	if _, err := f.db.ApplyTurnReceipt(context.Background(), claims[0], provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
		t.Fatal(err)
	}
	f.requireTurn(t, claims[0], domain.TurnStateNotStarted, 0)
	if err := f.fence.checkCapacity(context.Background()); err != nil || len(reads) < 1 || len(reads) > capacity {
		t.Fatalf("terminal reclamation exceeded M or did not make room: reads=%d, error=%v", len(reads), err)
	}
	next := f.claim(t, f.groupID)
	nextOwner := f.reserve(t, next)
	if owners[0].beginInvoke() {
		t.Fatal("an unused but completed old owner could invoke after slot reuse")
	}
	owners[0].markLocalDone() // An old handle must not mark the replacement done.
	if _, err := f.db.ApplyTurnReceipt(context.Background(), next, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
		t.Fatal(err)
	}
	reads = nil
	if err := f.fence.checkCapacity(context.Background()); err == nil || len(reads) > capacity {
		t.Fatal("old owner affected the replacement, or GC exceeded M")
	}
	nextOwner.markLocalDone()
	if err := f.fence.checkCapacity(context.Background()); err != nil {
		t.Fatalf("replacement's own completion did not permit reclamation: %v", err)
	}
}

func TestMemberToolRoleFenceRequiresLocalDoneAndCommittedTerminal(t *testing.T) {
	for _, outcome := range []provider.Outcome{provider.OutcomeNotStarted, provider.OutcomeSucceeded, provider.OutcomeFailed, provider.OutcomeAborted, provider.OutcomeTruncated} {
		t.Run(string(outcome), func(t *testing.T) {
			f := setupMemberToolRoleFence(t, 1)
			claim := f.claim(t, f.groupID)
			owner := f.reserve(t, claim)
			receipt := provider.ExecutionReceipt{Outcome: outcome}
			if outcome == provider.OutcomeNotStarted {
				if _, err := f.db.RequestTurnCancel(context.Background(), claim.GroupID, claim.TurnID); err != nil {
					t.Fatal(err)
				}
				f.requireTurn(t, claim, domain.TurnStateCancelRequested, 1)
			} else {
				if !owner.beginInvoke() {
					t.Fatal("new owner has no invocation opportunity")
				}
				receipt = provider.ExecutionReceipt{Outcome: outcome, Terminal: outcome, Started: true, Accepted: true,
					NativeSessionID: "native", DirectChildReaped: true, OutputComplete: true}
			}
			// A local receipt value is not a committed Store terminal.
			if err := f.fence.checkCapacity(context.Background()); err == nil {
				t.Fatal("uncommitted receipt made capacity available")
			}
			if _, err := f.db.ApplyTurnReceipt(context.Background(), claim, receipt); err != nil {
				t.Fatal(err)
			}
			f.requireTurn(t, claim, domain.TurnState(outcome), 0)
			if err := f.fence.checkCapacity(context.Background()); err == nil {
				t.Fatal("committed terminal alone reclaimed an in-flight owner")
			}
			owner.markLocalDone()
			if err := f.fence.checkCapacity(context.Background()); err != nil {
				t.Fatalf("localDone AND committed terminal did not reclaim: %v", err)
			}
			if err := f.fence.checkCapacity(context.Background()); err != nil {
				t.Fatal("capacity probe reserved a slot")
			}
			f.deny(t, context.Background(), claim, f.root)
			next := f.claim(t, f.groupID)
			f.reserve(t, next)
			f.requireTurn(t, next, domain.TurnStateStarting, 1)
		})
	}

	t.Run("candidate-is-not-committed", func(t *testing.T) {
		f := setupMemberToolRoleFence(t, 1)
		claim := f.claim(t, f.groupID)
		owner := f.reserve(t, claim)
		if !owner.beginInvoke() {
			t.Fatal("new owner has no invocation opportunity")
		}
		for _, event := range []provider.Event{
			{Kind: provider.AcceptedEvent, NativeSessionID: "native"},
			{Kind: provider.ResponseEvent, NativeSessionID: "native", Text: "candidate output"},
			{Kind: provider.TerminalEvent, NativeSessionID: "native", Terminal: provider.OutcomeSucceeded},
		} {
			if err := f.db.RecordTurnEvent(context.Background(), claim, event); err != nil {
				t.Fatal(err)
			}
		}
		owner.markLocalDone()
		turn := f.requireTurn(t, claim, domain.TurnStateRunning, 1)
		if turn.CandidateTerminal != domain.TurnStateSucceeded || turn.CandidateText != "candidate output" {
			t.Fatal("fixture did not persist candidate evidence")
		}
		if err := f.fence.checkCapacity(context.Background()); err == nil {
			t.Fatal("candidate terminal plus localDone reclaimed a live Store turn")
		}
		receipt := provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded,
			Started: true, Accepted: true, NativeSessionID: "native", DirectChildReaped: true, OutputComplete: true}
		if _, err := f.db.ApplyTurnReceipt(context.Background(), claim, receipt); err != nil {
			t.Fatal(err)
		}
		f.requireTurn(t, claim, domain.TurnStateSucceeded, 0)
		if err := f.fence.checkCapacity(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.deny(t, context.Background(), claim, f.root)
		f.reserve(t, f.claim(t, f.groupID))
	})
}

func TestMemberToolRoleFenceCancelUnknownAndRecoveryRetainClaims(t *testing.T) {
	f := setupMemberToolRoleFence(t, 1)
	claim := f.claim(t, f.groupID)
	owner := f.reserve(t, claim)
	owner.markLocalDone()
	if owner.beginInvoke() {
		t.Fatal("localDone owner retained an unused invocation opportunity")
	}
	if _, err := f.db.RequestTurnCancel(context.Background(), claim.GroupID, claim.TurnID); err != nil {
		t.Fatal(err)
	}
	f.requireTurn(t, claim, domain.TurnStateCancelRequested, 1)
	if err := f.fence.checkCapacity(context.Background()); err == nil {
		t.Fatal("cancellation intent reclaimed a record")
	}
	if _, err := f.db.ApplyTurnReceipt(context.Background(), claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}); err != nil {
		t.Fatal(err)
	}
	f.requireTurn(t, claim, domain.TurnStateUnknown, 1)
	if err := f.fence.checkCapacity(context.Background()); err == nil {
		t.Fatal("unknown outcome reclaimed a record")
	}
	if _, err := f.db.ApplyTurnReceipt(context.Background(), claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
		t.Fatal(err)
	}
	f.requireTurn(t, claim, domain.TurnStateUnknown, 1)
	if count, err := f.db.RecoverInFlight(context.Background()); err != nil || count != 1 {
		t.Fatalf("recover real in-flight turn: count=%d, error=%v", count, err)
	}
	turn, err := f.db.GetTurn(context.Background(), claim.GroupID, claim.TurnID)
	if err != nil || turn.ClaimToken == "" || turn.ClaimToken == claim.Token {
		t.Fatalf("recovery did not replace the claim token: %v", err)
	}
	recovered := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	f.requireTurn(t, recovered, domain.TurnStateUnknown, 1)
	f.deny(t, context.Background(), claim, f.root)
	f.deny(t, context.Background(), recovered, f.root)
	if _, err := f.db.ApplyTurnReceipt(context.Background(), claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); !errors.Is(err, store.ErrStaleClaim) {
		t.Fatalf("old receipt was not fenced by recovery: %v", err)
	}
	if err := f.fence.checkCapacity(context.Background()); err == nil {
		t.Fatal("recovery token change cleared an occupied record")
	}
	f.requireTurn(t, recovered, domain.TurnStateUnknown, 1)
}

func TestMemberToolRoleFenceRootClaimAndReadFailures(t *testing.T) {
	t.Run("constructor-boundaries", func(t *testing.T) {
		f := setupMemberToolRoleFence(t, 1)
		permissive := filepath.Join(t.TempDir(), "permissive")
		if err := os.Mkdir(permissive, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(permissive, 0755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(f.root, link); err != nil {
			t.Fatal(err)
		}
		for _, invalid := range []struct {
			name string
			db   *store.Store
			m    int
			root string
		}{
			{"nil-store", nil, 1, f.root},
			{"zero-capacity", f.db, 0, f.root},
			{"negative-capacity", f.db, -1, f.root},
			{"relative-root", f.db, 1, "relative-runtime"},
			{"permissive-root", f.db, 1, permissive},
			{"symlink-root", f.db, 1, link},
		} {
			if fence, err := newMemberToolRoleFence(invalid.db, invalid.m, invalid.root); err == nil || fence != nil {
				t.Fatalf("accepted invalid constructor input %s", invalid.name)
			}
		}
	})

	t.Run("trusted-root-and-server-claim", func(t *testing.T) {
		f := setupMemberToolRoleFence(t, 1)
		claim := f.claim(t, f.groupID)
		before := f.requireTurn(t, claim, domain.TurnStateStarting, 1)
		otherRoot := filepath.Join(t.TempDir(), "runtime")
		if err := os.Mkdir(otherRoot, 0700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "alias")
		if err := os.Symlink(f.root, link); err != nil {
			t.Fatal(err)
		}
		for _, root := range []string{f.runtime.configRoot, otherRoot, link} {
			f.deny(t, context.Background(), claim, root)
		}
		for _, invalid := range []store.TurnClaim{
			{GroupID: f.otherID, TurnID: claim.TurnID, Token: claim.Token},
			{GroupID: claim.GroupID, TurnID: claim.TurnID, Token: "foreign-token"},
			{GroupID: claim.GroupID, TurnID: "missing-turn", Token: claim.Token},
		} {
			f.deny(t, context.Background(), invalid, f.root)
		}
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		f.deny(t, cancelled, claim, f.root)

		original := f.root + "-original"
		if err := os.Rename(f.root, original); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(f.root, 0700); err != nil {
			t.Fatal(err)
		}
		f.deny(t, context.Background(), claim, f.root)
		if err := os.Remove(f.root); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(original, f.root); err != nil {
			t.Fatal(err)
		}
		f.deny(t, context.Background(), claim, f.root)
		if err := os.Remove(f.root); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(original, f.root); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(f.root, 0755); err != nil {
			t.Fatal(err)
		}
		f.deny(t, context.Background(), claim, f.root)
		if err := os.Chmod(f.root, 0700); err != nil {
			t.Fatal(err)
		}
		f.reserve(t, claim) // Failed checks must not have installed a reservation.
		after := f.requireTurn(t, claim, domain.TurnStateStarting, 1)
		if !reflect.DeepEqual(before, after) || len(f.runtime.Tools().capabilities) != 0 {
			t.Fatal("root/claim checks changed the Store or issued tool authority")
		}
	})

	for _, uncertainty := range []string{"missing-row", "changed-token", "read-error"} {
		t.Run(uncertainty, func(t *testing.T) {
			f := setupMemberToolRoleFence(t, 1)
			claim := f.claim(t, f.groupID)
			f.reserve(t, claim).markLocalDone()
			if _, err := f.db.ApplyTurnReceipt(context.Background(), claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
				t.Fatal(err)
			}
			f.requireTurn(t, claim, domain.TurnStateNotStarted, 0)
			// These are ordinary real-Store fixtures, not role provenance fixtures.
			// SQL only removes/changes identity AFTER a genuine committed terminal;
			// it never manufactures a successful or terminal execution outcome.
			failRead := true
			switch uncertainty {
			case "missing-row":
				result, err := f.raw.Exec("DELETE FROM turns WHERE group_id=? AND id=? AND claim_token=?", claim.GroupID, claim.TurnID, claim.Token)
				if err != nil {
					t.Fatal(err)
				}
				if n, err := result.RowsAffected(); err != nil || n != 1 {
					t.Fatalf("remove fixture row: rows=%d, error=%v", n, err)
				}
				if _, err := f.db.GetTurn(context.Background(), claim.GroupID, claim.TurnID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("fixture row is not missing: %v", err)
				}
			case "changed-token":
				result, err := f.raw.Exec("UPDATE turns SET claim_token=? WHERE group_id=? AND id=? AND claim_token=?", "changed-"+claim.Token, claim.GroupID, claim.TurnID, claim.Token)
				if err != nil {
					t.Fatal(err)
				}
				if n, err := result.RowsAffected(); err != nil || n != 1 {
					t.Fatalf("change fixture identity: rows=%d, error=%v", n, err)
				}
				changed := claim
				changed.Token = "changed-" + claim.Token
				f.requireTurn(t, changed, domain.TurnStateNotStarted, 0)
			case "read-error":
				reader := memberToolRoleFenceReader{db: f.db, after: func(_ context.Context, turn domain.Turn) error {
					if failRead && turn.ID == claim.TurnID {
						return errors.New("injected Store read failure")
					}
					return nil
				}}
				f.fence.readTurn = reader.getTurn
			}
			for i := 0; i < 2; i++ {
				if err := f.fence.checkCapacity(context.Background()); err == nil {
					t.Fatal("uncertain terminal read discarded an occupied record")
				}
			}
			next := f.claim(t, f.groupID)
			f.deny(t, context.Background(), next, f.root)
			f.requireTurn(t, next, domain.TurnStateStarting, 1)
			if uncertainty == "read-error" {
				failRead = false
				if err := f.fence.checkCapacity(context.Background()); err != nil {
					t.Fatalf("healthy exact-identity terminal read did not recover: %v", err)
				}
				f.reserve(t, next)
			}
		})
	}
}

func TestMemberToolRoleFenceCancellationAndMarkDoneDuringRead(t *testing.T) {
	f := setupMemberToolRoleFence(t, 2)
	first, second := f.claim(t, f.groupID), f.claim(t, f.groupID)
	firstOwner, secondOwner := f.reserve(t, first), f.reserve(t, second)
	if _, err := f.db.ApplyTurnReceipt(context.Background(), first, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
		t.Fatal(err)
	}
	firstOwner.markLocalDone()
	next := f.claim(t, f.otherID) // Store capacity is available; the fence is still full.
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	readCtx, cancelRead := context.WithCancel(context.Background())
	waitCtx, cancelWait := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancelRead()
		cancelWait()
		releaseOnce.Do(func() { close(release) })
	})
	var firstReads atomic.Int32
	reader := memberToolRoleFenceReader{db: f.db, after: func(ctx context.Context, turn domain.Turn) error {
		if turn.ID != first.TurnID {
			return nil
		}
		firstReads.Add(1)
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}}
	f.fence.readTurn = reader.getTurn
	readResult := make(chan error, 1)
	go func() { readResult <- f.fence.checkCapacity(readCtx) }()
	guard, cancelGuard := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelGuard()
	select {
	case <-entered:
	case <-guard.Done():
		t.Fatal("admission did not enter the Store reader")
	}

	waitStarted, waitResult := make(chan struct{}), make(chan bool, 1)
	go func() {
		close(waitStarted)
		owner, err := f.fence.reserve(waitCtx, next, f.root)
		waitResult <- owner == nil && err != nil
	}()
	select {
	case <-waitStarted:
	case <-guard.Done():
		t.Fatal("second admission caller did not start")
	}
	cancelWait()
	select {
	case denied := <-waitResult:
		if !denied {
			t.Fatal("cancelled admission obtained an owner or no error")
		}
	case <-guard.Done():
		t.Fatal("cancelled admission waited behind the held read")
	}
	if firstReads.Load() != 1 {
		t.Fatal("another admission entered the held GC round")
	}

	marked := make(chan struct{})
	go func() {
		secondOwner.markLocalDone()
		close(marked)
	}()
	select {
	case <-marked:
	case <-guard.Done():
		t.Fatal("MarkDone waited behind the Store read")
	}
	cancelRead()
	select {
	case err := <-readResult:
		if err == nil {
			t.Fatal("cancelled terminal read admitted new work")
		}
	case <-guard.Done():
		t.Fatal("context-aware held read did not cancel")
	}
	f.requireTurn(t, second, domain.TurnStateStarting, 1)
	f.requireTurn(t, next, domain.TurnStateStarting, 1)
	releaseOnce.Do(func() { close(release) })
	if err := f.fence.checkCapacity(context.Background()); err != nil {
		t.Fatalf("admission gate was not usable after cancellation: %v", err)
	}
	if firstReads.Load() != 2 {
		t.Fatal("cancelled read evicted the terminal record instead of retaining it")
	}
	f.reserve(t, next)
	// This checks logical cancellation with a cooperative reader and query
	// sequencing, not interruption of arbitrary DB/OS I/O or a latency bound.
}
