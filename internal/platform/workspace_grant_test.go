package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func authorizeWorkspace(t *testing.T, p *Preparer, group, member, root string) store.WorkspaceGrantReceipt {
	t.Helper()
	got, err := p.AuthorizeWorkspaceGrant(context.Background(), group, member, WorkspaceGrantInput{Root: root, Scope: "read_only"}, "authorize")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestWorkspaceGrantAuthorizationReopenPreparation(t *testing.T) {
	ctx := context.Background()
	f := newPrepareFixture(t)
	root := t.TempDir()
	marker := filepath.Join(root, "unchanged")
	if err := os.WriteFile(marker, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	f.exec(t, "UPDATE members SET workspace=? WHERE id=?", root, f.member.ID)
	p := f.preparer(t)
	if _, err := p.Prepare(ctx, f.pending); !errors.Is(err, scheduler.ErrNotReady) {
		t.Fatalf("preference granted access: %v", err)
	}
	first := authorizeWorkspace(t, p, f.group.ID, f.member.ID, root)
	keys, err := scheduler.WorkspaceResourceKeys([]string{root})
	if err != nil || first.ResourceKey != keys[0] || first.Root != root {
		t.Fatalf("did not persist physical directory authority: %+v, %v", first, err)
	}
	got, err := p.Prepare(ctx, f.pending)
	if err != nil || got.ResourceKeys[0] != first.ResourceKey || got.Request.CWD != first.ResolvedRoot {
		t.Fatalf("existing preparer did not observe explicit authorization: %+v, %v", got, err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.s, err = store.Open(filepath.Join(filepath.Dir(f.opts.PrivateWorkspaceRoot), "fixture.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.s.Close()
	p = f.preparer(t)
	got, err = p.Prepare(ctx, f.pending)
	if err != nil || got.ResourceKeys[0] != first.ResourceKey {
		t.Fatalf("reopen lost trusted authorization: %+v, %v", got, err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	f.exec(t, "UPDATE members SET workspace=? WHERE id=?", alias, f.member.ID)
	got, err = p.Prepare(ctx, f.pending)
	if err != nil || got.ResourceKeys[0] != first.ResourceKey {
		t.Fatalf("physical alias changed resource key: %+v, %v", got, err)
	}
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	for _, preference := range []string{child, t.TempDir(), "relative"} {
		f.exec(t, "UPDATE members SET workspace=? WHERE id=?", preference, f.member.ID)
		if _, err := p.Prepare(ctx, f.pending); !errors.Is(err, scheduler.ErrNotReady) {
			t.Fatalf("changed preference expanded authorization: %v", err)
		}
	}
	f.exec(t, "UPDATE members SET workspace=? WHERE id=?", root, f.member.ID)
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root + "-old")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	p = f.preparer(t)
	if got, err := p.Prepare(ctx, f.pending); !errors.Is(err, scheduler.ErrNotReady) || got.Executor != nil {
		t.Fatalf("replacement must remain queued without stopping scheduling: %+v, %v", got, err)
	}
	replay := authorizeWorkspace(t, p, f.group.ID, f.member.ID, root)
	if replay != first {
		t.Fatal("replay reauthorized a replacement directory")
	}
	data, err := os.ReadFile(filepath.Join(root+"-old", "unchanged"))
	if err != nil || string(data) != "original" {
		t.Fatal("preparation modified external files")
	}
	f.assertQueued(t)
}

func TestWorkspaceGrantRejectsUnsafeRootsAndForeignMembers(t *testing.T) {
	for _, kind := range []string{"missing", "relative", "file", "private", "private-child", "private-parent", "private-alias", "data-root", "data-child", "trusted-overlap", "durable-overlap", "scope", "missing-key", "foreign", "inactive"} {
		t.Run(kind, func(t *testing.T) {
			f := newPrepareFixture(t)
			f.opts.PrivateDataRoot = filepath.Dir(f.opts.PrivateWorkspaceRoot)
			root := t.TempDir()
			input := WorkspaceGrantInput{Root: root, Scope: "read_only"}
			key, group, member := "authorize", f.group.ID, f.member.ID
			want := store.ErrInvalid
			switch kind {
			case "missing":
				input.Root += "-missing"
			case "relative":
				input.Root = "relative"
			case "file":
				input.Root = filepath.Join(root, "file")
				if err := os.WriteFile(input.Root, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "private", "private-child", "private-parent", "private-alias":
				input.Root = f.opts.PrivateWorkspaceRoot
				if kind == "private-child" {
					input.Root = filepath.Join(input.Root, "child")
					if err := os.Mkdir(input.Root, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "private-parent" {
					input.Root = filepath.Dir(input.Root)
				}
				if kind == "private-alias" {
					input.Root = filepath.Join(root, "alias")
					if err := os.Symlink(f.opts.PrivateWorkspaceRoot, input.Root); err != nil {
						t.Fatal(err)
					}
				}
			case "data-root":
				input.Root = f.opts.PrivateDataRoot
			case "data-child":
				input.Root = filepath.Join(f.opts.PrivateDataRoot, "runtime")
				if err := os.Mkdir(input.Root, 0700); err != nil {
					t.Fatal(err)
				}
			case "trusted-overlap", "durable-overlap":
				input.Root = filepath.Join(root, "child")
				if err := os.Mkdir(input.Root, 0700); err != nil {
					t.Fatal(err)
				}
				if kind == "trusted-overlap" {
					f.opts.WorkspaceGrants = []WorkspaceGrant{{GroupID: group, MemberID: "unused", Root: root}}
				} else {
					key = "candidate"
					other, err := f.s.CreateMember(context.Background(), store.CreateMemberInput{GroupID: group, Name: "other", Role: "reader"})
					if err != nil {
						t.Fatal(err)
					}
					authorizeWorkspace(t, f.preparer(t), group, other.ID, root)
				}
			case "scope":
				input.Scope = "read_write"
			case "missing-key":
				key = ""
			case "foreign":
				other, err := f.s.CreateGroup(context.Background(), store.CreateGroupInput{Name: "foreign"})
				if err != nil {
					t.Fatal(err)
				}
				group = other.ID
				want = store.ErrNotFound
			case "inactive":
				f.exec(t, "UPDATE members SET status='left' WHERE id=?", member)
				want = store.ErrNotFound
			}
			p := f.preparer(t)
			if got, err := p.AuthorizeWorkspaceGrant(context.Background(), group, member, input, key); !errors.Is(err, want) || got.ID != "" {
				t.Fatalf("unsafe authorization accepted: %+v, %v", got, err)
			}
			if _, err := p.GetWorkspaceGrant(context.Background(), f.group.ID, f.member.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("failed authorization left authority: %v", err)
			}
			f.assertQueued(t)
		})
	}
}

func TestWorkspaceGrantConcurrentReplayAndOverlaps(t *testing.T) {
	for _, mode := range []string{"replay", "overlap"} {
		t.Run(mode, func(t *testing.T) {
			f := newPrepareFixture(t)
			other, err := f.s.CreateMember(context.Background(), store.CreateMemberInput{GroupID: f.group.ID, Name: "other", Role: "reader"})
			if err != nil {
				t.Fatal(err)
			}
			second, err := store.Open(filepath.Join(filepath.Dir(f.opts.PrivateWorkspaceRoot), "fixture.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			p2, err := NewPreparer(second, f.opts)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			child := filepath.Join(root, "child")
			if err := os.Mkdir(child, 0700); err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			type result struct {
				receipt store.WorkspaceGrantReceipt
				err     error
			}
			results := make(chan result, 2)
			var wg sync.WaitGroup
			for i, p := range []*Preparer{f.preparer(t), p2} {
				member, path, key := f.member.ID, root, "authorize"
				if mode == "overlap" && i == 1 {
					member, path, key = other.ID, child, "other"
				}
				wg.Go(func() {
					<-start
					receipt, err := p.AuthorizeWorkspaceGrant(context.Background(), f.group.ID, member, WorkspaceGrantInput{Root: path, Scope: "read_only"}, key)
					results <- result{receipt, err}
				})
			}
			close(start)
			wg.Wait()
			close(results)
			var first store.WorkspaceGrantReceipt
			success, rejected := 0, 0
			for got := range results {
				if got.err == nil {
					success++
					if first.ID != "" && first != got.receipt {
						t.Fatal("concurrent replay returned different receipts")
					}
					first = got.receipt
				} else if errors.Is(got.err, store.ErrInvalid) {
					rejected++
				} else {
					t.Fatal(got.err)
				}
			}
			if mode == "replay" && success != 2 || mode == "overlap" && (success != 1 || rejected != 1) {
				t.Fatalf("transaction did not serialize grants: success=%d rejected=%d", success, rejected)
			}
		})
	}
}

func TestWorkspaceGrantAliasesKeepUnknownAndCancelFences(t *testing.T) {
	ctx := context.Background()
	f := newPrepareFixture(t)
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	p := f.preparer(t)
	authorizeWorkspace(t, p, f.group.ID, f.member.ID, root)
	group, err := f.s.CreateGroup(ctx, store.CreateGroupInput{Name: "foreign"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := f.s.CreateMember(ctx, store.CreateMemberInput{GroupID: group.ID, Name: "other", Role: "reader", ProviderID: f.member.ProviderID, Model: f.member.Model, Workspace: alias})
	if err != nil {
		t.Fatal(err)
	}
	message := f.message(t, group.ID, "topic", "user", "queued", member.ID)
	pending := store.PendingDelivery{GroupID: group.ID, Delivery: message.Deliveries[0]}
	if _, err := p.Prepare(ctx, pending); !errors.Is(err, scheduler.ErrNotReady) {
		t.Fatalf("foreign recipient borrowed another grant: %v", err)
	}
	first, err := p.Prepare(ctx, f.pending)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.pending.Delivery.ID, ResourceKeys: first.ResourceKeys, MaxActive: 3})
	if err != nil {
		t.Fatal(err)
	}
	// Granting an alias while another run is in flight must not create a new lock namespace.
	authorizeWorkspace(t, p, group.ID, member.ID, alias)
	second, err := p.Prepare(ctx, pending)
	if err != nil || second.ResourceKeys[0] != first.ResourceKeys[0] {
		t.Fatalf("aliases did not share a lock: %+v, %v", second, err)
	}
	if _, err := f.s.RequestTurnCancel(ctx, f.group.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	claimOther := func(s *store.Store) {
		t.Helper()
		if _, err := s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: group.ID, DeliveryID: pending.Delivery.ID, ResourceKeys: second.ResourceKeys, MaxActive: 3}); !errors.Is(err, store.ErrTurnBusy) {
			t.Fatalf("cancel/unknown released a shared resource: %v", err)
		}
	}
	claimOther(f.s)
	unknown, err := f.s.ApplyTurnReceipt(ctx, store.TurnClaim{GroupID: f.group.ID, TurnID: turn.ID, Token: turn.ClaimToken}, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown})
	if err != nil || unknown.State != domain.TurnStateUnknown {
		t.Fatalf("unknown outcome changed: %+v, %v", unknown, err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(filepath.Join(filepath.Dir(f.opts.PrivateWorkspaceRoot), "fixture.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	claimOther(reopened)
}

func TestWorkspaceGrantMixedRootsRetainClaimsAcrossRestart(t *testing.T) {
	for _, outcome := range []string{"unknown", "cancelling"} {
		for _, target := range []string{"trusted-child", "private-child", "trusted-alias", "missing-private", "replaced-private", "renamed-private-child"} {
			t.Run(outcome+"/"+target, func(t *testing.T) {
				ctx := context.Background()
				f := newPrepareFixture(t)
				root := filepath.Join(t.TempDir(), "external")
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				child := filepath.Join(root, "child")
				if err := os.Mkdir(child, 0700); err != nil {
					t.Fatal(err)
				}
				p := f.preparer(t)
				receipt := authorizeWorkspace(t, p, f.group.ID, f.member.ID, root)
				first, err := p.Prepare(ctx, f.pending)
				if err != nil {
					t.Fatal(err)
				}
				turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.pending.Delivery.ID, ResourceKeys: first.ResourceKeys, MaxActive: 3})
				if err != nil {
					t.Fatal(err)
				}
				if outcome == "cancelling" {
					if _, err := f.s.RequestTurnCancel(ctx, f.group.ID, turn.ID); err != nil {
						t.Fatal(err)
					}
				} else if _, err := f.s.ApplyTurnReceipt(ctx, store.TurnClaim{GroupID: f.group.ID, TurnID: turn.ID, Token: turn.ClaimToken}, provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeUnknown}); err != nil {
					t.Fatal(err)
				}
				group, err := f.s.CreateGroup(ctx, store.CreateGroupInput{Name: "independent recipient"})
				if err != nil {
					t.Fatal(err)
				}
				workspace := child
				if target == "trusted-alias" {
					workspace = filepath.Join(t.TempDir(), "alias")
					if err := os.Symlink(root, workspace); err != nil {
						t.Fatal(err)
					}
				} else if target == "private-child" {
					workspace = ""
					f.opts.PrivateWorkspaceRoot = child // Legacy options intentionally omit PrivateDataRoot.
				} else if target == "missing-private" || target == "replaced-private" || target == "renamed-private-child" {
					workspace = ""
					moved := filepath.Join(t.TempDir(), "moved")
					if err := os.Rename(root, moved); err != nil {
						t.Fatal(err)
					}
					if target == "replaced-private" {
						if err := os.Mkdir(root, 0700); err != nil {
							t.Fatal(err)
						}
					} else if target == "renamed-private-child" {
						alias := filepath.Join(t.TempDir(), "ancestor-alias")
						if err := os.Symlink(filepath.Join(moved, "child"), alias); err != nil {
							t.Fatal(err)
						}
						f.opts.PrivateWorkspaceRoot = filepath.Join(alias, "private")
						if err := os.Mkdir(f.opts.PrivateWorkspaceRoot, 0700); err != nil {
							t.Fatal(err)
						}
					}
				}
				member, err := f.s.CreateMember(ctx, store.CreateMemberInput{GroupID: group.ID, Name: "other", Role: "reader", ProviderID: f.member.ProviderID, Model: f.member.Model, Workspace: workspace})
				if err != nil {
					t.Fatal(err)
				}
				message := f.message(t, group.ID, "", "user", "queued", member.ID)
				pending := store.PendingDelivery{GroupID: group.ID, Delivery: message.Deliveries[0]}
				if workspace != "" {
					f.opts.WorkspaceGrants = []WorkspaceGrant{{GroupID: group.ID, MemberID: member.ID, Root: workspace}}
				}
				path := filepath.Join(filepath.Dir(p.privateRoot), "fixture.sqlite")
				if err := f.s.Close(); err != nil {
					t.Fatal(err)
				}
				f.s, err = store.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				if n, err := f.s.RecoverInFlight(ctx); err != nil || n != 1 {
					t.Fatalf("recovery lost the retained execution: %d, %v", n, err)
				}
				p = f.preparer(t)
				if target == "missing-private" || target == "replaced-private" || target == "renamed-private-child" {
					waiting := f.message(t, f.group.ID, "next", "user", "stale external remains queued", f.member.ID)
					if got, err := p.Prepare(ctx, store.PendingDelivery{GroupID: f.group.ID, Delivery: waiting.Deliveries[0]}); !errors.Is(err, scheduler.ErrNotReady) || got.Executor != nil {
						t.Fatalf("stale claimed root did not remain not-ready: %+v, %v", got, err)
					}
				}
				second, err := p.Prepare(ctx, pending)
				if target == "missing-private" || target == "replaced-private" {
					if err != nil || second.Executor == nil || len(second.ResourceKeys) != 1 || second.ResourceKeys[0] == receipt.ResourceKey {
						t.Fatalf("retained external claim blocked a disjoint private scope: %+v, %v", second, err)
					}
				} else if target == "trusted-alias" {
					if err != nil || len(second.ResourceKeys) != 1 || second.ResourceKeys[0] != receipt.ResourceKey {
						t.Fatalf("mixed aliases changed the lock namespace: %+v, %v", second, err)
					}
				} else {
					want := errPrepareWorkspace
					if target == "renamed-private-child" {
						want = scheduler.ErrNotReady
					}
					if !errors.Is(err, want) || second.Executor != nil || len(second.ResourceKeys) != 0 {
						// Exercise the actual claim path on the vulnerable implementation.
						_, claimErr := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: group.ID, DeliveryID: pending.Delivery.ID, ResourceKeys: second.ResourceKeys, MaxActive: 3})
						t.Fatalf("overlapping mixed authority prepared after restart: prepare=%v claim=%v", err, claimErr)
					}
				}
				if _, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: group.ID, DeliveryID: pending.Delivery.ID, ResourceKeys: []string{receipt.ResourceKey}, MaxActive: 3}); !errors.Is(err, store.ErrTurnBusy) {
					t.Fatalf("retained resource claim was released: %v", err)
				}
				held, err := f.s.GetTurn(ctx, f.group.ID, turn.ID)
				if err != nil || held.State != domain.TurnStateUnknown || held.ClaimToken == turn.ClaimToken || (held.CancelRequestedAt != nil) != (outcome == "cancelling") {
					t.Fatalf("restart changed unknown/cancel evidence: %+v, %v", held, err)
				}
				stored, err := f.s.GetMessage(ctx, group.ID, message.ID)
				if err != nil || stored.Deliveries[0].State != domain.DeliveryStateQueued || stored.Deliveries[0].TurnID != "" {
					t.Fatalf("overlap acquired a second admission: %+v, %v", stored, err)
				}
				var turns, claims int
				if err := f.db.QueryRow("SELECT count(*) FROM turns").Scan(&turns); err != nil {
					t.Fatal(err)
				}
				if err := f.db.QueryRow("SELECT count(*) FROM resource_claims WHERE resource_key=? AND turn_id=?", receipt.ResourceKey, turn.ID).Scan(&claims); err != nil || turns != 1 || claims != 1 {
					t.Fatalf("overlap changed durable claims: turns=%d claims=%d err=%v", turns, claims, err)
				}
			})
		}
	}
}

func TestWorkspaceGrantStaleSnapshotStillFencesEveryExternalRoot(t *testing.T) {
	for _, mutation := range []string{"missing", "replaced", "private-alias", "dangling-alias", "missing-shared-boundary"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			f := newPrepareFixture(t)
			root := filepath.Join(t.TempDir(), "external")
			if mutation == "missing-shared-boundary" {
				root = filepath.Join(filepath.Dir(f.opts.PrivateWorkspaceRoot), "external")
			}
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			p := f.preparer(t)
			authorizeWorkspace(t, p, f.group.ID, f.member.ID, root)
			other, err := f.s.CreateMember(ctx, store.CreateMemberInput{GroupID: f.group.ID, Name: "healthy external", Role: "reader", ProviderID: f.member.ProviderID})
			if err != nil {
				t.Fatal(err)
			}
			healthy := t.TempDir()
			if _, err := p.AuthorizeWorkspaceGrant(ctx, f.group.ID, other.ID, WorkspaceGrantInput{Root: healthy, Scope: "read_only"}, "healthy"); err != nil {
				t.Fatal(err)
			}
			message := f.message(t, f.group.ID, "", "user", "still subject to complete validation", other.ID)
			pending := store.PendingDelivery{GroupID: f.group.ID, Delivery: message.Deliveries[0]}
			if err := os.Rename(root, root+"-old"); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "replaced":
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
			case "private-alias", "dangling-alias":
				target := f.opts.PrivateWorkspaceRoot
				if mutation == "dangling-alias" {
					target = filepath.Join(target, "missing")
				}
				if err := os.Symlink(target, root); err != nil {
					t.Fatal(err)
				}
			}
			// Both durable and legacy external preparation must inspect unused receipts.
			for _, trusted := range []bool{false, true} {
				if trusted {
					f.opts.WorkspaceGrants = []WorkspaceGrant{{GroupID: f.group.ID, MemberID: other.ID, Root: healthy}}
				}
				if got, err := f.preparer(t).Prepare(ctx, pending); !errors.Is(err, scheduler.ErrNotReady) || got.Executor != nil {
					t.Fatalf("unused stale receipt was ignored or fatal: trusted=%v prepared=%+v err=%v", trusted, got, err)
				}
			}
			private, err := f.s.CreateMember(ctx, store.CreateMemberInput{GroupID: f.group.ID, Name: "private", Role: "reader", ProviderID: f.member.ProviderID})
			if err != nil {
				t.Fatal(err)
			}
			message = f.message(t, f.group.ID, "", "user", "private isolation", private.ID)
			pending = store.PendingDelivery{GroupID: f.group.ID, Delivery: message.Deliveries[0]}
			got, err := f.preparer(t).Prepare(ctx, pending)
			if mutation == "private-alias" || mutation == "dangling-alias" || mutation == "missing-shared-boundary" {
				if !errors.Is(err, scheduler.ErrNotReady) || got.Executor != nil {
					t.Fatalf("unproven private isolation was admitted: %+v, %v", got, err)
				}
			} else if err != nil || got.Executor == nil || filepath.Dir(got.Request.CWD) != f.opts.PrivateWorkspaceRoot {
				t.Fatalf("disjoint private workspace could not progress: %+v, %v", got, err)
			}
			f.assertQueued(t)
		})
	}
}

func TestWorkspaceGrantStalenessDoesNotHideTrustedOrStoreFailures(t *testing.T) {
	for _, failure := range []string{"trusted-root", "snapshot-read"} {
		t.Run(failure, func(t *testing.T) {
			f := newPrepareFixture(t)
			root := t.TempDir()
			p := f.preparer(t)
			authorizeWorkspace(t, p, f.group.ID, f.member.ID, root)
			f.exec(t, "UPDATE members SET workspace='' WHERE id=?", f.member.ID)
			if err := os.Remove(root); err != nil {
				t.Fatal(err)
			}
			want := errPrepareRead
			if failure == "trusted-root" {
				want = errPrepareWorkspace
				if err := os.Chmod(f.opts.PrivateWorkspaceRoot, 0750); err != nil {
					t.Fatal(err)
				}
			} else {
				f.exec(t, "DROP TABLE workspace_grants")
			}
			if got, err := p.Prepare(context.Background(), f.pending); !errors.Is(err, want) || errors.Is(err, scheduler.ErrNotReady) || got.Executor != nil {
				t.Fatalf("real failure was masked as staleness: %+v, %v", got, err)
			}
		})
	}
}

func TestWorkspaceGrantRoleReadinessUsesSameAuthorization(t *testing.T) {
	f := newRolePrepareFixture(t)
	f.enableRuntime(t)
	root := t.TempDir()
	f.exec(t, "UPDATE members SET workspace=? WHERE id=?", root, f.member.ID)
	p := f.preparer(t)
	assertInspectBlockedMatchesPrepare(t, f, p, "workspace_not_ready")
	receipt := authorizeWorkspace(t, p, f.group.ID, f.member.ID, root)
	before := f.snapshot(t)
	inspection, err := p.InspectRoleReadiness(context.Background(), f.pending)
	if err != nil || inspection.Blocked != nil {
		t.Fatalf("readiness did not reuse durable grant: %+v, %v", inspection, err)
	}
	got, err := p.Prepare(context.Background(), f.pending)
	if err != nil || got.ResourceKeys[0] != receipt.ResourceKey || got.Request.CWD != receipt.ResolvedRoot {
		t.Fatalf("role Prepare disagrees with readiness: %+v, %v", got, err)
	}
	f.assertUnchanged(t, before, false)
}
