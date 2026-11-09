package platform

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentworkspace/internal/provider"
	"agentworkspace/internal/skills"
	"agentworkspace/internal/store"
)

// The delegate performs real Catalog I/O, then gates its return. This proves
// admitted callback drain, not interruption of a blocked kernel syscall.
type memberSkillGatedSource struct {
	delegate    memberSkillSource
	operation   string
	entered     chan struct{}
	release     chan struct{}
	once        sync.Once
	selectCalls atomic.Int32
	listCalls   atomic.Int32
}

func (g *memberSkillGatedSource) pause(operation string) {
	if g.operation == operation {
		g.once.Do(func() {
			close(g.entered)
			<-g.release
		})
	}
}

func (g *memberSkillGatedSource) List() (skills.Listing, error) {
	g.listCalls.Add(1)
	listing, err := g.delegate.List()
	g.pause("list")
	return listing, err
}

func (g *memberSkillGatedSource) Select(id skills.Identity) (*skills.Snapshot, error) {
	g.selectCalls.Add(1)
	snapshot, err := g.delegate.Select(id)
	g.pause("select")
	return snapshot, err
}

func (g *memberSkillGatedSource) Close() error { return g.delegate.Close() }

func memberSkillGate(t *testing.T, f *memberSkillFixture, operation string) (*memberSkillGatedSource, func()) {
	t.Helper()
	// Install the private real delegate before Issue snapshots its wrapper.
	f.tools.Revoke(f.token)
	gate := &memberSkillGatedSource{delegate: f.catalog.source, operation: operation, entered: make(chan struct{}), release: make(chan struct{})}
	f.catalog.source = gate
	f.token = memberSkillIssue(t, f.tools, f.claim, f.catalog, "search_skills", "load_skill")
	var once sync.Once
	release := func() { once.Do(func() { close(gate.release) }) }
	t.Cleanup(release)
	return gate, release
}

func memberSkillWait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for controlled callback boundary")
	}
}

func TestMemberSkillGlobalDrainAndBorrowedOwner(t *testing.T) {
	for _, operation := range []string{"list", "select"} {
		for _, control := range []string{"revoke", "revoke-turn", "other-issue", "other-revoke", "other-revoke-turn"} {
			t.Run(operation+"/"+control, func(t *testing.T) {
				f := setupMemberSkills(t, "demo")
				id := f.search(t, MemberSkillSearchInput{}).Items[0].Identity
				otherClaim := memberCapabilityClaim(t, f.localMessageFixture, f.otherID)
				otherToken := ""
				if control != "other-issue" {
					otherToken = memberSkillIssue(t, f.tools, otherClaim, f.catalog, "load_skill")
				}
				gate, release := memberSkillGate(t, &f, operation)
				state := f.state(t)
				used := make(chan error, 1)
				go func() {
					if operation == "list" {
						_, err := f.tools.SearchSkills(context.Background(), f.token, MemberSkillSearchInput{})
						used <- err
					} else {
						_, err := f.tools.LoadSkill(context.Background(), f.token, MemberSkillLoadInput{Identity: &id})
						used <- err
					}
				}()
				memberSkillWait(t, gate.entered)
				started, controlled := make(chan struct{}), make(chan struct{})
				controlResult := make(chan error, 1)
				go func() {
					close(started)
					var err error
					switch control {
					case "revoke":
						f.tools.Revoke(f.token)
					case "revoke-turn":
						f.tools.RevokeTurn(f.claim.TurnID)
					case "other-issue":
						_, err = f.tools.Issue(context.Background(), otherClaim, MemberToolGrants{Operations: []string{"list_members"}})
					case "other-revoke":
						f.tools.Revoke(otherToken)
					case "other-revoke-turn":
						f.tools.RevokeTurn(otherClaim.TurnID)
					}
					controlResult <- err
					close(controlled)
				}()
				<-started
				select {
				case <-controlled:
					t.Error("global registry writer escaped an admitted I/O callback")
				case <-time.After(25 * time.Millisecond):
				}
				release()
				if err := <-used; err != nil {
					t.Fatalf("Revoke alone must allow an admitted authorized read to finish: %v", err)
				}
				memberSkillWait(t, controlled)
				if err := <-controlResult; err != nil {
					t.Fatal(err)
				}
				if control == "revoke" || control == "revoke-turn" {
					if state.snapshots != nil || state.captured != 0 {
						t.Fatal("drained registration retained snapshot references or byte count")
					}
					got, err := f.tools.LoadSkill(context.Background(), f.token, MemberSkillLoadInput{Identity: &id})
					memberSkillZeroError(t, got, err, store.ErrMemberToolDenied)
				}
				listing, err := f.catalog.source.List()
				if err != nil || len(listing.Entries) != 1 {
					t.Fatal("registry closed a borrowed catalog owner")
				}
			})
		}
	}
}

func TestMemberSkillFinalFenceAfterRealIO(t *testing.T) {
	for _, operation := range []string{"list", "select"} {
		for _, change := range []string{"cancel", "terminal", "old-claim", "context"} {
			t.Run(operation+"/"+change, func(t *testing.T) {
				f := setupMemberSkills(t, "demo")
				id := f.search(t, MemberSkillSearchInput{}).Items[0].Identity
				gate, release := memberSkillGate(t, &f, operation)
				state := f.state(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				type outcome struct {
					result any
					err    error
				}
				used := make(chan outcome, 1)
				go func() {
					if operation == "list" {
						got, err := f.tools.SearchSkills(ctx, f.token, MemberSkillSearchInput{})
						used <- outcome{got, err}
					} else {
						got, err := f.tools.LoadSkill(ctx, f.token, MemberSkillLoadInput{Identity: &id})
						used <- outcome{got, err}
					}
				}()
				memberSkillWait(t, gate.entered)
				var err error
				want := store.ErrMemberToolDenied
				switch change {
				case "cancel":
					_, err = f.db.RequestTurnCancel(context.Background(), f.claim.GroupID, f.claim.TurnID)
				case "terminal":
					err = f.db.RecordTurnEvent(context.Background(), f.claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "native"})
					if err == nil {
						err = f.db.RecordTurnEvent(context.Background(), f.claim, provider.Event{Kind: provider.TerminalEvent, NativeSessionID: "native", Terminal: provider.OutcomeSucceeded})
					}
				case "old-claim":
					_, err = f.raw.Exec("UPDATE turns SET claim_token=? WHERE id=?", "replacement-claim", f.claim.TurnID)
				case "context":
					cancel()
					want = context.Canceled
				}
				if err != nil {
					t.Fatal(err)
				}
				release()
				result := <-used
				memberSkillZeroError(t, result.result, result.err, want)
				if len(state.snapshots) != 0 || state.captured != 0 {
					t.Fatal("failed final fence admitted candidate snapshot")
				}
			})
		}
	}
}

func TestMemberSkillConcurrentAdmissionAndCanceledMutexWaiter(t *testing.T) {
	f := setupMemberSkills(t, "demo")
	id := f.search(t, MemberSkillSearchInput{}).Items[0].Identity
	gate, release := memberSkillGate(t, &f, "select")
	type outcome struct {
		result MemberSkillLoadResult
		err    error
	}
	results := make(chan outcome, 9)
	go func() {
		result, err := f.tools.LoadSkill(context.Background(), f.token, MemberSkillLoadInput{Identity: &id})
		results <- outcome{result, err}
	}()
	memberSkillWait(t, gate.entered)
	ctx, cancel := context.WithCancel(context.Background())
	waiting, canceled := make(chan struct{}), make(chan outcome, 1)
	go func() {
		close(waiting)
		result, err := f.tools.LoadSkill(ctx, f.token, MemberSkillLoadInput{Identity: &id})
		canceled <- outcome{result, err}
	}()
	<-waiting
	select {
	case <-canceled:
		t.Fatal("same-run load did not wait for the admitted selection")
	case <-time.After(25 * time.Millisecond):
	}
	cancel()
	for range 8 {
		go func() {
			result, err := f.tools.LoadSkill(context.Background(), f.token, MemberSkillLoadInput{Identity: &id})
			results <- outcome{result, err}
		}()
	}
	release()
	cancelResult := <-canceled
	memberSkillZeroError(t, cancelResult.result, cancelResult.err, context.Canceled)
	handle := ""
	for range 9 {
		got := <-results
		if got.err != nil || got.result.Handle == "" || handle != "" && handle != got.result.Handle {
			t.Fatalf("concurrent same-identity selection did not share one frozen handle: %v", got.err)
		}
		handle = got.result.Handle
	}
	state := f.state(t)
	if gate.selectCalls.Load() != 1 || len(state.snapshots) != 1 || state.captured <= 0 {
		t.Fatalf("run admitted duplicate candidates: calls=%d, count=%d", gate.selectCalls.Load(), len(state.snapshots))
	}
}

func TestMemberSkillReissueDestroysHandlesAndCursorState(t *testing.T) {
	for _, byTurn := range []bool{false, true} {
		t.Run(map[bool]string{false: "token", true: "turn"}[byTurn], func(t *testing.T) {
			f := setupMemberSkills(t, "alpha", "beta")
			page := f.search(t, MemberSkillSearchInput{Limit: 1})
			loaded := f.load(t, MemberSkillLoadInput{Identity: &page.Items[0].Identity})
			oldState := f.state(t)
			if byTurn {
				f.tools.RevokeTurn(f.claim.TurnID)
			} else {
				f.tools.Revoke(f.token)
			}
			if oldState.snapshots != nil || oldState.captured != 0 {
				t.Fatal("revocation did not clear referenced run state")
			}
			f.token = memberSkillIssue(t, f.tools, f.claim, f.catalog, "search_skills", "load_skill")
			if f.state(t) == oldState {
				t.Fatal("new issuance inherited old state pointer")
			}
			got, err := f.tools.LoadSkill(context.Background(), f.token, MemberSkillLoadInput{Handle: loaded.Handle})
			memberSkillZeroError(t, got, err, skills.ErrInvalid)
			search, err := f.tools.SearchSkills(context.Background(), f.token, MemberSkillSearchInput{Before: page.NextBefore})
			memberSkillZeroError(t, search, err, skills.ErrInvalid)
			if fresh := f.load(t, MemberSkillLoadInput{Identity: &page.Items[0].Identity}); fresh.Handle == loaded.Handle {
				t.Fatal("new issuance reused a prior handle")
			}
		})
	}
}

func TestMemberSkillPreflightRejectsStaleClaimWithoutIO(t *testing.T) {
	f := setupMemberSkills(t, "demo")
	id := f.search(t, MemberSkillSearchInput{}).Items[0].Identity
	gate, _ := memberSkillGate(t, &f, "none")
	for _, bad := range []store.TurnClaim{
		{GroupID: f.otherID, TurnID: f.claim.TurnID, Token: f.claim.Token},
		{GroupID: f.claim.GroupID, TurnID: f.claim.TurnID, Token: "old-claim"},
	} {
		_, err := NewMemberTools(f.db).Issue(context.Background(), bad, MemberToolGrants{Operations: []string{"load_skill"}, Skills: f.catalog})
		requireCapabilityDenied(t, err)
	}
	if _, err := f.db.RequestTurnCancel(context.Background(), f.claim.GroupID, f.claim.TurnID); err != nil {
		t.Fatal(err)
	}
	got, err := f.tools.LoadSkill(context.Background(), f.token, MemberSkillLoadInput{Identity: &id})
	memberSkillZeroError(t, got, err, store.ErrMemberToolDenied)
	page, err := f.tools.SearchSkills(context.Background(), f.token, MemberSkillSearchInput{})
	memberSkillZeroError(t, page, err, store.ErrMemberToolDenied)
	if gate.listCalls.Load() != 0 || gate.selectCalls.Load() != 0 {
		t.Fatal("invalid durable scope reached filesystem delegate")
	}
}

func memberSkillChangeScope(t *testing.T, f memberSkillFixture, change string, cancel context.CancelFunc) {
	t.Helper()
	var err error
	switch change {
	case "active":
	case "cancel":
		_, err = f.db.RequestTurnCancel(context.Background(), f.claim.GroupID, f.claim.TurnID)
	case "terminal":
		err = f.db.RecordTurnEvent(context.Background(), f.claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "native"})
		if err == nil {
			err = f.db.RecordTurnEvent(context.Background(), f.claim, provider.Event{Kind: provider.TerminalEvent, NativeSessionID: "native", Terminal: provider.OutcomeSucceeded})
		}
	case "old-claim":
		_, err = f.raw.Exec("UPDATE turns SET claim_token=? WHERE id=?", "replacement-claim", f.claim.TurnID)
	case "context":
		cancel()
	default:
		t.Fatalf("unknown scope transition: %s", change)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestMemberSkillDataDependentFailureFinalFence(t *testing.T) {
	for _, operation := range []string{"select-success", "list-success", "select-json", "list-json", "select-changed", "select-invalid", "list-limit", "captured-limit"} {
		for _, change := range []string{"active", "cancel", "terminal", "old-claim", "context"} {
			t.Run(operation+"/"+change, func(t *testing.T) {
				f := setupMemberSkills(t, "demo")
				id := f.search(t, MemberSkillSearchInput{}).Items[0].Identity
				input := MemberSkillLoadInput{Identity: &id}
				var dataErr error
				var residentID MemberSkillIdentity
				switch operation {
				case "select-json":
					memberSkillPut(t, f.root, "demo/escaped.txt", strings.Repeat("<", 16384))
					input.Resource, input.MaxBytes = "escaped.txt", 16384
					dataErr = skills.ErrLimit
				case "list-json":
					for n := range 8 {
						name := fmt.Sprintf("large-%d", n)
						memberSkillPut(t, f.root, name+"/SKILL.md", "---\nname: "+name+"\ndescription: fixture\nlicense: '"+strings.Repeat("<", 2000)+"'\n---\nbody")
					}
					dataErr = skills.ErrLimit
				case "select-changed":
					memberSkillPut(t, f.root, "demo/SKILL.md", memberSkillRaw("demo", "changed after listing"))
					dataErr = skills.ErrChanged
				case "select-invalid":
					if err := os.Remove(f.root + "/demo/SKILL.md"); err != nil {
						t.Fatal(err)
					}
					dataErr = skills.ErrInvalid
				case "list-limit":
					memberSkillPut(t, f.root, "demo/SKILL.md", memberSkillRaw("demo", strings.Repeat("x", 2<<20)))
					dataErr = skills.ErrLimit
				case "captured-limit":
					// Fill the held budget with actual captured files, not counters.
					raw := memberSkillRaw("resident", "body")
					memberSkillPut(t, f.root, "resident/SKILL.md", raw)
					remaining := (32 << 20) - len(raw)
					for n := 0; remaining > 0; n++ {
						size := min(remaining, 2<<20)
						memberSkillPut(t, f.root, fmt.Sprintf("resident/part-%d", n), strings.Repeat("x", size))
						remaining -= size
					}
					for _, item := range f.search(t, MemberSkillSearchInput{}).Items {
						if item.Name == "resident" {
							residentID = item.Identity
						}
					}
					dataErr = skills.ErrLimit
				}
				gate, release := memberSkillGate(t, &f, "none")
				if operation == "captured-limit" {
					f.load(t, MemberSkillLoadInput{Identity: &residentID})
					if f.state(t).captured != 32<<20 {
						t.Fatal("fixture did not capture the actual 32 MiB budget")
					}
				}
				isList := strings.HasPrefix(operation, "list-")
				gate.operation = "select"
				if isList {
					gate.operation = "list"
				}
				state := f.state(t)
				count, captured := len(state.snapshots), state.captured
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				type outcome struct {
					result any
					err    error
				}
				done := make(chan outcome, 1)
				go func() {
					if isList {
						result, err := f.tools.SearchSkills(ctx, f.token, MemberSkillSearchInput{})
						done <- outcome{result, err}
					} else {
						result, err := f.tools.LoadSkill(ctx, f.token, input)
						done <- outcome{result, err}
					}
				}()
				memberSkillWait(t, gate.entered)
				memberSkillChangeScope(t, f, change, cancel)
				release()
				got := <-done
				want := dataErr
				if change == "context" {
					want = context.Canceled
				} else if change != "active" {
					want = store.ErrMemberToolDenied
				}
				if want == nil {
					if got.err != nil || reflect.ValueOf(got.result).IsZero() {
						t.Fatalf("active successful read was not preserved: %+v, %v", got.result, got.err)
					}
					if !isList {
						count++
						captured += int64(len(memberSkillRaw("demo", "PRIVATE-BODY demo")))
					}
				} else {
					memberSkillZeroError(t, got.result, got.err, want)
				}
				if len(state.snapshots) != count || state.captured != captured {
					t.Fatalf("failure admitted or displaced snapshots: count=%d bytes=%d", len(state.snapshots), state.captured)
				}
			})
		}
	}
}

func TestMemberSkillCachedResourceFailureFinalFence(t *testing.T) {
	for _, change := range []string{"active", "cancel", "terminal", "old-claim", "context"} {
		t.Run(change, func(t *testing.T) {
			f := setupMemberSkills(t, "demo")
			id := f.search(t, MemberSkillSearchInput{}).Items[0].Identity
			gate, release := memberSkillGate(t, &f, "none")
			loaded := f.load(t, MemberSkillLoadInput{Identity: &id})
			got, err := f.tools.LoadSkill(context.Background(), f.token, MemberSkillLoadInput{Handle: loaded.Handle, Resource: "missing"})
			memberSkillZeroError(t, got, err, skills.ErrInvalid)
			state := f.state(t)
			captured := state.captured
			gate.operation = "cached"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type outcome struct {
				data []byte
				err  error
			}
			done := make(chan outcome, 1)
			go func() {
				var data []byte
				err := f.tools.withMemberSkills(ctx, f.token, "load_skill", func(_ store.MemberToolScope, _ memberSkillSource, state *memberSkillState) error {
					// Snapshot has no injectable reader: gate the shared callback
					// fence, then invoke the real immutable cached resource API.
					gate.pause("cached")
					var err error
					data, err = state.snapshots[loaded.Handle].ReadResource("missing")
					if err != nil {
						return memberSkillError(err)
					}
					return nil
				})
				done <- outcome{data, err}
			}()
			memberSkillWait(t, gate.entered)
			memberSkillChangeScope(t, f, change, cancel)
			release()
			result := <-done
			want := skills.ErrInvalid
			if change == "context" {
				want = context.Canceled
			} else if change != "active" {
				want = store.ErrMemberToolDenied
			}
			memberSkillZeroError(t, result.data, result.err, want)
			if len(state.snapshots) != 1 || state.captured != captured || gate.selectCalls.Load() != 1 {
				t.Fatal("failed cached read changed admission or selected the source again")
			}
		})
	}
}
