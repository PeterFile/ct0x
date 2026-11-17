package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func authorizeWriteWorkspace(t *testing.T, p *Preparer, group, member, root string) store.WorkspaceGrantReceipt {
	t.Helper()
	grant, err := p.AuthorizeWorkspaceGrant(context.Background(), group, member, WorkspaceGrantInput{Root: root, Scope: "read_write"}, "write")
	if err != nil || grant.Scope != "read_write" || grant.NodeID != "local" || grant.AuthorizedBy != "user" {
		t.Fatalf("explicit local-user write grant failed: %+v, %v", grant, err)
	}
	return grant
}

// Preserve the review's cross-group counterexample as a positive D1 regression.
func TestWorkspaceWriteGrantDoesNotPoisonOtherGroups(t *testing.T) {
	ctx := context.Background()
	f := newPrepareFixture(t)
	selectPrepareMemberToolsPi(f)
	configurePrepareMemberTools(t, f)
	p := f.preparer(t)
	operations := slices.Clone(p.memberTools.operations)
	before, err := p.Prepare(ctx, f.pending)
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.s.CreateGroup(ctx, store.CreateGroupInput{Name: "write authority in another group"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := f.s.CreateMember(ctx, store.CreateMemberInput{GroupID: other.ID, Name: "ordinary member", Role: "reader", ProviderID: f.member.ProviderID, Model: f.member.Model})
	if err != nil {
		t.Fatal(err)
	}
	write := authorizeWriteWorkspace(t, p, other.ID, member.ID, t.TempDir())
	after, err := p.Prepare(ctx, f.pending)
	if err != nil || after.Request.CWD != before.Request.CWD || !slices.Equal(after.ResourceKeys, before.ResourceKeys) {
		t.Fatalf("unrelated write receipt poisoned existing private preparation: %+v, %v", after, err)
	}
	read := authorizeWorkspace(t, p, f.group.ID, f.member.ID, t.TempDir())
	prepared, err := p.Prepare(ctx, f.pending)
	if err != nil || prepared.Request.CWD != read.ResolvedRoot || !slices.Equal(prepared.ResourceKeys, []string{read.ResourceKey}) {
		t.Fatalf("write receipt poisoned another group's read-only authorization/preparation: %+v, %v", prepared, err)
	}
	message := f.message(t, other.ID, "", "user", "ordinary discussion, not IO authority", member.ID)
	prepared, err = p.Prepare(ctx, store.PendingDelivery{GroupID: other.ID, Delivery: message.Deliveries[0]})
	if err != nil || prepared.Request.CWD != write.ResolvedRoot || !slices.Equal(prepared.ResourceKeys, []string{write.ResourceKey}) {
		t.Fatalf("write receipt lost its physical directory identity: %+v, %v", prepared, err)
	}
	run, ok := prepared.Executor.(*memberToolRuntimeRun)
	if !ok || run.role != nil || run.intake || run.runtime != p.memberTools || !slices.Equal(run.runtime.operations, operations) || len(run.runtime.Tools().capabilities) != 0 {
		t.Fatal("directory scope promoted an ordinary member's tool profile or issued authority")
	}
	f.assertQueued(t)
}

func TestWorkspaceWriteGrantPrivateIsolation(t *testing.T) {
	ctx := context.Background()
	f := newPrepareFixture(t)
	f.opts.PrivateDataRoot = filepath.Dir(f.opts.PrivateWorkspaceRoot)
	p := f.preparer(t)
	alias := filepath.Join(t.TempDir(), "private-alias")
	if err := os.Symlink(f.opts.PrivateWorkspaceRoot, alias); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{f.opts.PrivateWorkspaceRoot, alias, f.opts.PrivateDataRoot} {
		grant, err := p.AuthorizeWorkspaceGrant(ctx, f.group.ID, f.member.ID, WorkspaceGrantInput{Root: root, Scope: "read_write"}, "unsafe")
		if !errors.Is(err, store.ErrInvalid) || grant.ID != "" {
			t.Fatalf("write scope bypassed private protection for %s: %+v, %v", root, grant, err)
		}
	}
	if _, err := p.GetWorkspaceGrant(ctx, f.group.ID, f.member.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unsafe write request left authority: %v", err)
	}
	// A once-safe write root retargeted to private storage must also stay fenced.
	root := filepath.Join(t.TempDir(), "external")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	authorizeWriteWorkspace(t, p, f.group.ID, f.member.ID, root)
	f.exec(t, "UPDATE members SET workspace='' WHERE id=?", f.member.ID)
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.opts.PrivateWorkspaceRoot, root); err != nil {
		t.Fatal(err)
	}
	if got, err := p.Prepare(ctx, f.pending); !errors.Is(err, scheduler.ErrNotReady) || got.Executor != nil || len(got.ResourceKeys) != 0 {
		t.Fatalf("retargeted write root bypassed private isolation or became a fatal scope error: %+v, %v", got, err)
	}
	f.assertQueued(t)
}

func TestWorkspaceWriteGrantUnknownAliasRetainsClaim(t *testing.T) {
	ctx := context.Background()
	f := newPrepareFixture(t)
	p := f.preparer(t)
	root := t.TempDir()
	grant := authorizeWriteWorkspace(t, p, f.group.ID, f.member.ID, root)
	first, err := p.Prepare(ctx, f.pending)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.pending.Delivery.ID, ResourceKeys: first.ResourceKeys, MaxActive: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RequestTurnCancel(ctx, f.group.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	unknown, err := f.s.ApplyTurnReceipt(ctx, store.TurnClaim{GroupID: f.group.ID, TurnID: turn.ID, Token: turn.ClaimToken}, provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeUnknown})
	if err != nil || unknown.State != domain.TurnStateUnknown || unknown.CancelRequestedAt == nil {
		t.Fatalf("unknown/cancel evidence changed: %+v, %v", unknown, err)
	}
	other, err := f.s.CreateGroup(ctx, store.CreateGroupInput{Name: "read-only alias"})
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	member, err := f.s.CreateMember(ctx, store.CreateMemberInput{GroupID: other.ID, Name: "reader", Role: "reader", ProviderID: f.member.ProviderID, Model: f.member.Model, Workspace: alias})
	if err != nil {
		t.Fatal(err)
	}
	message := f.message(t, other.ID, "", "user", "must retain the shared lock", member.ID)
	pending := store.PendingDelivery{GroupID: other.ID, Delivery: message.Deliveries[0]}
	if _, err := p.Prepare(ctx, pending); !errors.Is(err, scheduler.ErrNotReady) {
		t.Fatalf("foreign member borrowed write authority: %v", err)
	}
	read := authorizeWorkspace(t, p, other.ID, member.ID, alias)
	second, err := p.Prepare(ctx, pending)
	if err != nil || read.ResourceKey != grant.ResourceKey || !slices.Equal(second.ResourceKeys, first.ResourceKeys) {
		t.Fatalf("mixed-scope alias changed the lock namespace: %+v, %v", second, err)
	}
	if _, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: other.ID, DeliveryID: pending.Delivery.ID, ResourceKeys: second.ResourceKeys, MaxActive: 3}); !errors.Is(err, store.ErrTurnBusy) {
		t.Fatalf("read-only alias released a write root's unknown claim: %v", err)
	}
	if held, err := f.s.GetTurn(ctx, f.group.ID, turn.ID); err != nil || !reflect.DeepEqual(held, unknown) {
		t.Fatalf("directory authorization altered retained execution evidence: %+v, %v", held, err)
	}
}

func TestWorkspaceWriteGrantStaleUnknownClaimBlocksPrivate(t *testing.T) {
	ctx := context.Background()
	f := newPrepareFixture(t)
	p := f.preparer(t)
	private, err := p.Prepare(ctx, f.pending)
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.s.CreateGroup(ctx, store.CreateGroupInput{Name: "unknown external writer"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := f.s.CreateMember(ctx, store.CreateMemberInput{GroupID: other.ID, Name: "writer", Role: "ordinary", ProviderID: f.member.ProviderID, Model: f.member.Model})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "external")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	grant := authorizeWriteWorkspace(t, p, other.ID, member.ID, root)
	message := f.message(t, other.ID, "", "user", "retained unknown external execution", member.ID)
	pending := store.PendingDelivery{GroupID: other.ID, Delivery: message.Deliveries[0]}
	prepared, err := p.Prepare(ctx, pending)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: other.ID, DeliveryID: pending.Delivery.ID, ResourceKeys: prepared.ResourceKeys, MaxActive: 3})
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := f.s.ApplyTurnReceipt(ctx, store.TurnClaim{GroupID: other.ID, TurnID: turn.ID, Token: turn.ClaimToken}, provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeUnknown})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, filepath.Join(private.Request.CWD, "moved-write-root")); err != nil {
		t.Fatal(err)
	}
	got, err := p.Prepare(ctx, f.pending)
	if !errors.Is(err, scheduler.ErrNotReady) || got.Executor != nil || len(got.ResourceKeys) != 0 {
		t.Fatalf("stale claimed write root allowed private preparation: %+v, %v", got, err)
	}
	if _, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.pending.Delivery.ID, ResourceKeys: []string{grant.ResourceKey}, MaxActive: 3}); !errors.Is(err, store.ErrTurnBusy) {
		t.Fatalf("moving the write root released its unknown claim: %v", err)
	}
	if held, err := f.s.GetTurn(ctx, other.ID, turn.ID); err != nil || !reflect.DeepEqual(held, unknown) || held.State != domain.TurnStateUnknown {
		t.Fatalf("private fallback rewrote unknown evidence: %+v, %v", held, err)
	}
	if saved, err := p.GetWorkspaceGrant(ctx, other.ID, member.ID); err != nil || saved != grant {
		t.Fatalf("private fallback rewrote write authority: %+v, %v", saved, err)
	}
	stored, err := f.s.GetMessage(ctx, f.group.ID, f.trigger.ID)
	if err != nil || stored.Deliveries[0].State != domain.DeliveryStateQueued || stored.Deliveries[0].TurnID != "" {
		t.Fatalf("private delivery acquired admission: %+v, %v", stored, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.opts.PrivateWorkspaceRoot), "invoked")); !os.IsNotExist(err) {
		t.Fatalf("preparation launched a provider: %v", err)
	}
}
