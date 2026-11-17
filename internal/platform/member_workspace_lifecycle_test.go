//go:build unix

package platform

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/store"
	"agentworkspace/internal/workspacefs"
)

func TestMemberWorkspaceRegistryOwnsPinsButBorrowsBlobStore(t *testing.T) {
	f := newMemberWorkspaceIO(t)
	local := setupLocalMessages(t)
	tools := NewMemberTools(local.db)
	scratchPath := t.TempDir()
	if err := os.Chmod(scratchPath, 0700); err != nil {
		t.Fatal(err)
	}
	registry, err := newMemberWorkspaceRegistry(tools, MemberWorkspaceOptions{Blobs: f.blobs, ScratchRoot: scratchPath, ProtectedRoots: []string{scratchPath}})
	if err != nil || !registry.rootsValid() {
		t.Fatalf("trusted registry roots: %v", err)
	}
	// Seed only lifetime ownership, not an authorization fixture. No token is issued.
	key := sha256.Sum256([]byte("owned"))
	foreign := sha256.Sum256([]byte("ordinary"))
	registry.grants[key] = &memberWorkspaceGrant{root: f.root}
	tools.capabilities[key] = memberToolRegistration{}
	tools.capabilities[foreign] = memberToolRegistration{}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal("Close must be idempotent")
	}
	if _, err := f.root.Read(context.Background(), "file"); !errors.Is(err, workspacefs.ErrClosed) {
		t.Fatal("workspace pin outlived its owner")
	}
	if _, exists := tools.capabilities[foreign]; !exists {
		t.Fatal("file registry revoked another profile")
	}
	if _, exists := tools.capabilities[key]; exists || len(registry.grants) != 0 {
		t.Fatal("file registrations survived close")
	}
	if _, err := registry.scratch.Stat("."); !errors.Is(err, os.ErrClosed) {
		t.Fatal("private scratch descriptor leaked")
	}
	if _, err := f.blobs.Open("invalid"); errors.Is(err, artifactfs.ErrClosed) {
		t.Fatal("borrowed blob owner was closed")
	}
}

func TestMemberWorkspaceRegistryRootReplacementAndProtectedIdentityFailClosed(t *testing.T) {
	f := newMemberWorkspaceIO(t)
	local := setupLocalMessages(t)
	protected := t.TempDir()
	scratch := t.TempDir()
	if err := os.Chmod(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	registry, err := newMemberWorkspaceRegistry(NewMemberTools(local.db), MemberWorkspaceOptions{Blobs: f.blobs, ScratchRoot: scratch, ProtectedRoots: []string{protected}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { registry.Close() })
	if err := os.Rename(protected, protected+"-old"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(protected + "-old") })
	if err := os.Mkdir(protected, 0700); err != nil {
		t.Fatal(err)
	}
	if registry.rootsValid() {
		t.Fatal("replaced private boundary kept stale physical identity")
	}
}

func TestMemberWorkspaceRegistryRejectsUnsafeOwnerConfiguration(t *testing.T) {
	f := newMemberWorkspaceIO(t)
	local := setupLocalMessages(t)
	tools := NewMemberTools(local.db)
	for _, mode := range []os.FileMode{0755, 0770} {
		root := t.TempDir()
		if err := os.Chmod(root, mode); err != nil {
			t.Fatal(err)
		}
		if registry, err := newMemberWorkspaceRegistry(tools, MemberWorkspaceOptions{Blobs: f.blobs, ScratchRoot: root}); registry != nil || !errors.Is(err, store.ErrInvalid) {
			t.Fatal("nonprivate scratch owner accepted")
		}
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	for _, options := range []MemberWorkspaceOptions{{ScratchRoot: root}, {Blobs: f.blobs, ScratchRoot: alias},
		{Blobs: f.blobs, ScratchRoot: "relative"}, {Blobs: f.blobs, ScratchRoot: root, ProtectedRoots: []string{"relative"}}} {
		if registry, err := newMemberWorkspaceRegistry(tools, options); registry != nil || !errors.Is(err, store.ErrInvalid) {
			t.Fatal("unsafe owner configuration accepted")
		}
	}
}
