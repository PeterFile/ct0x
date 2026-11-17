//go:build unix

package platform

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func workspacePhysicalGrant(t *testing.T, root string) store.WorkspaceGrantReceipt {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := scheduler.WorkspaceResourceKeys([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	return store.WorkspaceGrantReceipt{ID: "grant", GroupID: "group", MemberID: "worker", Root: root, ResolvedRoot: resolved,
		ResourceKey: keys[0], NodeID: "local", Scope: "read_write", AuthorizedBy: "user"}
}

func TestWorkspaceTaskPhysicalGrantIsNotCWDOrAuthority(t *testing.T) {
	root := t.TempDir()
	grant := workspacePhysicalGrant(t, root)
	resolved, info, err := memberWorkspacePhysicalGrant(grant, []string{t.TempDir()})
	if err != nil || resolved != grant.ResolvedRoot || info == nil {
		t.Fatalf("current physical root rejected: %v", err)
	}
	for _, change := range []func(*store.WorkspaceGrantReceipt){
		func(g *store.WorkspaceGrantReceipt) { g.Scope = "read_only" },
		func(g *store.WorkspaceGrantReceipt) { g.NodeID = "remote" },
		func(g *store.WorkspaceGrantReceipt) { g.AuthorizedBy = "worker" },
		func(g *store.WorkspaceGrantReceipt) { g.ResourceKey = "workspace:old-unknown" },
		func(g *store.WorkspaceGrantReceipt) { g.Root = t.TempDir() },
		func(g *store.WorkspaceGrantReceipt) { g.ResolvedRoot = t.TempDir() },
	} {
		invalid := grant
		change(&invalid)
		if _, _, err := memberWorkspacePhysicalGrant(invalid, nil); !errors.Is(err, store.ErrMemberToolDenied) {
			t.Fatalf("untrusted/stale identity accepted: %v", err)
		}
	}
}

func TestWorkspaceTaskPhysicalGrantRejectsPrivateDataAndRootOverlap(t *testing.T) {
	private := t.TempDir()
	child := filepath.Join(private, "data")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{private, private}, {private, child}, {child, private}} {
		grant := workspacePhysicalGrant(t, pair[0])
		if _, _, err := memberWorkspacePhysicalGrant(grant, []string{pair[1]}); !errors.Is(err, store.ErrMemberToolDenied) {
			t.Fatalf("private root overlap accepted: %v", err)
		}
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(private, alias); err != nil {
		t.Fatal(err)
	}
	if _, _, err := memberWorkspacePhysicalGrant(workspacePhysicalGrant(t, alias), []string{private}); !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("symlink alias bypassed private boundary")
	}
}

func TestWorkspaceTaskPhysicalGrantRejectsRetargetAndReplacedResource(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	grant := workspacePhysicalGrant(t, alias)
	if _, _, err := memberWorkspacePhysicalGrant(grant, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, alias); err != nil {
		t.Fatal(err)
	}
	if _, _, err := memberWorkspacePhysicalGrant(grant, nil); !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("retargeted grant kept its old resource authority")
	}
	grant = workspacePhysicalGrant(t, root)
	if err := os.Rename(root, root+"-retained"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(root + "-retained") })
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := memberWorkspacePhysicalGrant(grant, nil); !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("replacement root acquired old unknown resource identity")
	}
}
