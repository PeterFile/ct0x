package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

// Keep the review's moved-inside-private counterexample, including a real claim
// attempt if preparation regresses. Cancellation and in-flight claims also fence.
func TestWorkspaceGrantMovedClaimInsidePrivateScope(t *testing.T) {
	for _, outcome := range []string{"unknown", "cancelling", "in-flight"} {
		t.Run(outcome, func(t *testing.T) {
			ctx := context.Background()
			f := newPrepareFixture(t)
			f.opts.PrivateDataRoot = filepath.Dir(f.opts.PrivateWorkspaceRoot)
			p := f.preparer(t)
			group, err := f.s.CreateGroup(ctx, store.CreateGroupInput{Name: "private recipient"})
			if err != nil {
				t.Fatal(err)
			}
			member, err := f.s.CreateMember(ctx, store.CreateMemberInput{GroupID: group.ID, Name: "private", Role: "reader", ProviderID: f.member.ProviderID, Model: f.member.Model})
			if err != nil {
				t.Fatal(err)
			}
			message := f.message(t, group.ID, "", "user", "private pending", member.ID)
			pending := store.PendingDelivery{GroupID: group.ID, Delivery: message.Deliveries[0]}
			private, err := p.Prepare(ctx, pending)
			if err != nil {
				t.Fatal(err)
			}
			external := filepath.Join(t.TempDir(), "external")
			if err := os.Mkdir(external, 0700); err != nil {
				t.Fatal(err)
			}
			grant := authorizeWorkspace(t, p, f.group.ID, f.member.ID, external)
			prepared, err := p.Prepare(ctx, f.pending)
			if err != nil {
				t.Fatal(err)
			}
			turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.pending.Delivery.ID, ResourceKeys: prepared.ResourceKeys, MaxActive: 3})
			if err != nil {
				t.Fatal(err)
			}
			if outcome == "cancelling" {
				if _, err := f.s.RequestTurnCancel(ctx, f.group.ID, turn.ID); err != nil {
					t.Fatal(err)
				}
			} else if outcome == "unknown" {
				if _, err := f.s.ApplyTurnReceipt(ctx, store.TurnClaim{GroupID: f.group.ID, TurnID: turn.ID, Token: turn.ClaimToken}, provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeUnknown}); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Rename(external, filepath.Join(private.Request.CWD, "moved-grant")); err != nil {
				t.Fatal(err)
			}
			for phase := range 2 {
				if phase != 0 {
					if err := f.s.Close(); err != nil {
						t.Fatal(err)
					}
					f.s, err = store.Open(filepath.Join(filepath.Dir(f.opts.PrivateWorkspaceRoot), "fixture.sqlite"))
					if err != nil {
						t.Fatal(err)
					}
					defer f.s.Close()
					if n, err := f.s.RecoverInFlight(ctx); err != nil || n != 1 {
						t.Fatalf("recover: %d, %v", n, err)
					}
					p = f.preparer(t)
				}
				got, err := p.Prepare(ctx, pending)
				if err == nil {
					admitted, claimErr := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: group.ID, DeliveryID: pending.Delivery.ID, ResourceKeys: got.ResourceKeys, MaxActive: 3})
					t.Fatalf("unsafe stale-root private preparation: phase=%d original_key=%s private_key=%v claim_error=%v admitted=%t", phase, grant.ResourceKey, got.ResourceKeys, claimErr, admitted.ID != "")
				}
				if !errors.Is(err, scheduler.ErrNotReady) || got.Executor != nil || len(got.ResourceKeys) != 0 {
					t.Fatalf("want empty not-ready preparation: %+v, %v", got, err)
				}
				if _, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: group.ID, DeliveryID: pending.Delivery.ID, ResourceKeys: []string{grant.ResourceKey}, MaxActive: 3}); !errors.Is(err, store.ErrTurnBusy) {
					t.Fatalf("old resource claim was released: %v", err)
				}
			}
			held, err := f.s.GetTurn(ctx, f.group.ID, turn.ID)
			if err != nil || held.State != domain.TurnStateUnknown || held.ClaimToken == turn.ClaimToken || (held.CancelRequestedAt != nil) != (outcome == "cancelling") {
				t.Fatalf("recovery changed retained execution evidence: %+v, %v", held, err)
			}
			saved, err := p.GetWorkspaceGrant(ctx, f.group.ID, f.member.ID)
			if err != nil || saved != grant {
				t.Fatalf("staleness rewrote authority: %+v, %v", saved, err)
			}
			stored, err := f.s.GetMessage(ctx, group.ID, message.ID)
			if err != nil || stored.Deliveries[0].State != domain.DeliveryStateQueued || stored.Deliveries[0].TurnID != "" {
				t.Fatalf("private delivery acquired admission: %+v, %v", stored, err)
			}
			var turns, claims int
			if err := f.db.QueryRow("SELECT count(*) FROM turns").Scan(&turns); err != nil {
				t.Fatal(err)
			}
			if err := f.db.QueryRow("SELECT count(*) FROM resource_claims WHERE resource_key=? AND turn_id=?", grant.ResourceKey, turn.ID).Scan(&claims); err != nil || turns != 1 || claims != 1 {
				t.Fatalf("changed retained claims: turns=%d claims=%d err=%v", turns, claims, err)
			}
		})
	}
}

func TestWorkspaceGrantStaleUnclaimedRootAllowsPrivateProgress(t *testing.T) {
	for _, evidence := range []string{"absent", "terminal-only"} {
		t.Run(evidence, func(t *testing.T) {
			ctx := context.Background()
			f := newPrepareFixture(t)
			p := f.preparer(t)
			root := filepath.Join(t.TempDir(), "stale")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			grant := authorizeWorkspace(t, p, f.group.ID, f.member.ID, root)
			if evidence == "terminal-only" {
				turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.pending.Delivery.ID, ResourceKeys: []string{grant.ResourceKey}, MaxActive: 3})
				if err != nil {
					t.Fatal(err)
				}
				settled, err := f.s.ApplyTurnReceipt(ctx, store.TurnClaim{GroupID: f.group.ID, TurnID: turn.ID, Token: turn.ClaimToken}, provider.ExecutionReceipt{
					Started: true, Accepted: true, NativeSessionID: "terminal-fixture", Outcome: provider.OutcomeSucceeded,
					Terminal: provider.OutcomeSucceeded, OutputComplete: true, DirectChildReaped: true,
				})
				if err != nil || settled.State != domain.TurnStateSucceeded {
					t.Fatalf("terminal evidence did not settle: %+v, %v", settled, err)
				}
			}
			// A different, still-valid durable root keeps an unknown claim. The
			// fallback must inspect the stale key, not block on any global claim.
			healthy, err := f.s.CreateMember(ctx, store.CreateMemberInput{GroupID: f.group.ID, Name: "healthy external", Role: "reader", ProviderID: f.member.ProviderID})
			if err != nil {
				t.Fatal(err)
			}
			healthyGrant, err := p.AuthorizeWorkspaceGrant(ctx, f.group.ID, healthy.ID, WorkspaceGrantInput{Root: t.TempDir(), Scope: "read_only"}, "healthy")
			if err != nil {
				t.Fatal(err)
			}
			healthyMessage := f.message(t, f.group.ID, "healthy", "user", "held external", healthy.ID)
			held, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: healthyMessage.Deliveries[0].ID, ResourceKeys: []string{healthyGrant.ResourceKey}, MaxActive: 3})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.ApplyTurnReceipt(ctx, store.TurnClaim{GroupID: f.group.ID, TurnID: held.ID, Token: held.ClaimToken}, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}); err != nil {
				t.Fatal(err)
			}
			private, err := f.s.CreateMember(ctx, store.CreateMemberInput{GroupID: f.group.ID, Name: "private", Role: "reader", ProviderID: f.member.ProviderID})
			if err != nil {
				t.Fatal(err)
			}
			message := f.message(t, f.group.ID, "", "user", "safe private progress", private.ID)
			pending := store.PendingDelivery{GroupID: f.group.ID, Delivery: message.Deliveries[0]}
			if err := os.Rename(root, root+"-old"); err != nil {
				t.Fatal(err)
			}
			if err := f.s.Close(); err != nil {
				t.Fatal(err)
			}
			f.s, err = store.Open(filepath.Join(filepath.Dir(f.opts.PrivateWorkspaceRoot), "fixture.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.s.Close()
			if n, err := f.s.RecoverInFlight(ctx); err != nil || n != 1 {
				t.Fatalf("recover retained healthy claim: %d, %v", n, err)
			}
			got, err := f.preparer(t).Prepare(ctx, pending)
			if err != nil || got.Executor == nil || len(got.ResourceKeys) != 1 || got.ResourceKeys[0] == grant.ResourceKey || got.ResourceKeys[0] == healthyGrant.ResourceKey {
				t.Fatalf("unclaimed stale authority blocked safe private progress: %+v, %v", got, err)
			}
			if _, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: pending.Delivery.ID, ResourceKeys: got.ResourceKeys, MaxActive: 3}); err != nil {
				t.Fatalf("safe private claim failed: %v", err)
			}
			var staleClaims, healthyClaims int
			if err := f.db.QueryRow("SELECT count(*) FROM resource_claims WHERE resource_key=?", grant.ResourceKey).Scan(&staleClaims); err != nil {
				t.Fatal(err)
			}
			if err := f.db.QueryRow("SELECT count(*) FROM resource_claims WHERE resource_key=? AND turn_id=?", healthyGrant.ResourceKey, held.ID).Scan(&healthyClaims); err != nil || staleClaims != 0 || healthyClaims != 1 {
				t.Fatalf("claim lookup changed evidence: stale=%d healthy=%d err=%v", staleClaims, healthyClaims, err)
			}
		})
	}
}

// Preserve Terra's exact error-precedence scenario with two independent receipts.
func TestWorkspaceGrantStaleReceiptMustNotMaskTrustedOverlap(t *testing.T) {
	ctx := context.Background()
	f := newPrepareFixture(t)
	p := f.preparer(t)
	parent := filepath.Join(t.TempDir(), "parent")
	child := filepath.Join(parent, "child")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	authorizeWorkspace(t, p, f.group.ID, f.member.ID, parent)
	stale := filepath.Join(t.TempDir(), "stale")
	if err := os.Mkdir(stale, 0700); err != nil {
		t.Fatal(err)
	}
	other, err := f.s.CreateMember(ctx, store.CreateMemberInput{GroupID: f.group.ID, Name: "other", Role: "reader", ProviderID: f.member.ProviderID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.AuthorizeWorkspaceGrant(ctx, f.group.ID, other.ID, WorkspaceGrantInput{Root: stale, Scope: "read_only"}, "second"); err != nil {
		t.Fatal(err)
	}
	configured, err := f.s.CreateMember(ctx, store.CreateMemberInput{GroupID: f.group.ID, Name: "configured", Role: "reader", ProviderID: f.member.ProviderID, Model: f.member.Model, Workspace: child})
	if err != nil {
		t.Fatal(err)
	}
	message := f.message(t, f.group.ID, "", "user", "pending", configured.ID)
	pending := store.PendingDelivery{GroupID: f.group.ID, Delivery: message.Deliveries[0]}
	f.opts.WorkspaceGrants = []WorkspaceGrant{{GroupID: f.group.ID, MemberID: configured.ID, Root: child}}
	p = f.preparer(t)
	if _, err := p.Prepare(ctx, pending); !errors.Is(err, errPrepareWorkspace) {
		t.Fatalf("expected trusted/durable overlap before staleness: %v", err)
	}
	if err := os.Rename(stale, stale+"-old"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Prepare(ctx, pending); !errors.Is(err, errPrepareWorkspace) {
		t.Fatalf("stale independent receipt hid unchanged trusted/durable configuration error: %v", err)
	}
}

// Review-only overlay: physical directory fixtures only; no store or DB access.
func TestDelta2ReplacedReceiptMustNotMaskPrivateRootConflict(t *testing.T) {
	for _, kind := range []string{"private-root-alias", "private-data-descendant"} {
		t.Run(kind, func(t *testing.T) {
			data := t.TempDir()
			private := filepath.Join(data, "private")
			if err := os.Mkdir(private, 0700); err != nil {
				t.Fatal(err)
			}
			valid := private
			opts := PrepareOptions{PrivateWorkspaceRoot: private}
			if kind == "private-data-descendant" {
				valid = filepath.Join(data, "runtime")
				if err := os.Mkdir(valid, 0700); err != nil {
					t.Fatal(err)
				}
				opts.PrivateDataRoot = data
			}
			other := filepath.Join(t.TempDir(), "external")
			if err := os.Mkdir(other, 0700); err != nil {
				t.Fatal(err)
			}
			key := func(path string) string {
				keys, err := scheduler.WorkspaceResourceKeys([]string{path})
				if err != nil {
					t.Fatal(err)
				}
				return keys[0]
			}
			grant := func(path string) store.WorkspaceGrantReceipt {
				return store.WorkspaceGrantReceipt{Root: path, ResolvedRoot: path, ResourceKey: key(path), Scope: "read_only", NodeID: "local", AuthorizedBy: "user"}
			}
			grants := []store.WorkspaceGrantReceipt{grant(valid), grant(other)}
			p, err := NewPreparer(&store.Store{}, opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.validateDurableWorkspaceRoots(grants, ""); !errors.Is(err, errPrepareWorkspace) {
				t.Fatalf("expected genuine private-root conflict before replacement: %v", err)
			}
			if err := os.Rename(other, other+"-old"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(other, 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := p.validateDurableWorkspaceRoots(grants, ""); !errors.Is(err, errPrepareWorkspace) {
				t.Fatalf("unrelated replacement hid unchanged private-root conflict: %v", err)
			}
		})
	}
}

func TestWorkspaceGrantStaleClaimReadFailureIsNotReadiness(t *testing.T) {
	ctx := context.Background()
	f := newPrepareFixture(t)
	p := f.preparer(t)
	root := t.TempDir()
	authorizeWorkspace(t, p, f.group.ID, f.member.ID, root)
	f.exec(t, "UPDATE members SET workspace='' WHERE id=?", f.member.ID)
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	f.exec(t, "DROP TABLE resource_claims")
	if got, err := p.Prepare(ctx, f.pending); !errors.Is(err, errPrepareRead) || errors.Is(err, scheduler.ErrNotReady) || got.Executor != nil || strings.Contains(err.Error(), "resource_claims") {
		t.Fatalf("claim read failure was hidden or leaked SQL: %+v, %v", got, err)
	}
}
