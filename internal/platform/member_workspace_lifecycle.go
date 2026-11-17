//go:build unix

package platform

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
	"agentworkspace/internal/workspacefs"
)

// MemberWorkspaceOptions borrows the existing immutable blob owner. Trusted
// composition must close/drain the runtime before closing Blobs. ScratchRoot is
// an existing private 0700 staging directory, not a model-visible source grant.
// ProtectedRoots includes the complete data/config/execution/private root set.
type MemberWorkspaceOptions struct {
	Blobs          *artifactfs.Store `json:"-"`
	ScratchRoot    string            `json:"-"`
	ProtectedRoots []string          `json:"-"`
}

type memberWorkspaceBoundary struct {
	path, resolved string
	info           os.FileInfo
}

// Production registrations always use a pinned workspacefs.Root. Keep the
// private IO contract narrow; it never accepts a model-supplied root or opener.
type memberWorkspaceRoot interface {
	memberWorkspaceFileIO
	List(context.Context, string) ([]workspacefs.Entry, error)
	Close() error
}

type memberWorkspaceGrant struct {
	// Registry revocation exclusion precedes this per-registration IO lock.
	// Across registrations the durable physical resource claim is authoritative.
	mu        sync.Mutex
	role      store.CoordinationToolScope
	directory store.WorkspaceGrantReceipt
	root      memberWorkspaceRoot
	info      os.FileInfo
}

// This private bounded registry is separate from public MemberToolGrants/Issue.
// Its entries will be issued only from a live worker role + current v19 grant +
// immutable task scope, never from a directory option on the ordinary profile.
// tools.mu protects grants/closed and excludes revocation across complete IO.
type memberWorkspaceRegistry struct {
	tools      *MemberTools
	blobs      *artifactfs.Store
	scratch    *os.Root
	boundaries []memberWorkspaceBoundary
	grants     map[[sha256.Size]byte]*memberWorkspaceGrant
	closed     bool
}

func newMemberWorkspaceRegistry(tools *MemberTools, options MemberWorkspaceOptions) (*memberWorkspaceRegistry, error) {
	if tools == nil || tools.db == nil || options.Blobs == nil || !privatePreparationDirectory(options.ScratchRoot) {
		return nil, store.ErrInvalid
	}
	registry := &memberWorkspaceRegistry{tools: tools, blobs: options.Blobs, grants: make(map[[sha256.Size]byte]*memberWorkspaceGrant)}
	paths := append([]string{options.ScratchRoot}, options.ProtectedRoots...)
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return nil, store.ErrInvalid
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, store.ErrInvalid
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			return nil, store.ErrInvalid
		}
		registry.boundaries = append(registry.boundaries, memberWorkspaceBoundary{path: filepath.Clean(path), resolved: resolved, info: info})
	}
	scratch, err := os.OpenRoot(options.ScratchRoot)
	if err != nil {
		return nil, store.ErrInvalid
	}
	registry.scratch = scratch
	info, err := scratch.Stat(".")
	if err != nil || !os.SameFile(info, registry.boundaries[0].info) || !registry.rootsValid() {
		_ = scratch.Close()
		return nil, store.ErrInvalid
	}
	return registry, nil
}

// EnableWorkspace is trusted composition only. It borrows the immutable blob
// owner and must be closed/drained before that owner or the Store is closed.
func (r *MemberToolRuntime) EnableWorkspace(options MemberWorkspaceOptions) error {
	r.tools.mu.Lock()
	defer r.tools.mu.Unlock()
	if r.roleFence == nil || r.tools.workspace != nil || len(r.tools.capabilities) != 0 {
		return store.ErrInvalid
	}
	options.ProtectedRoots = append(append([]string{}, options.ProtectedRoots...), r.configRoot, r.roleFence.executionRoot)
	registry, err := newMemberWorkspaceRegistry(r.tools, options)
	if err != nil {
		return err
	}
	r.tools.workspace = registry
	return nil
}

func (r *MemberToolRuntime) workspaceConfigured() bool {
	r.tools.mu.RLock()
	defer r.tools.mu.RUnlock()
	return r.tools.workspace != nil && !r.tools.workspace.closed
}

// Close drains and revokes only owned file registrations, never the borrowed
// blob owner. Composition calls it after scheduler and HTTP handler drain.
func (r *MemberToolRuntime) Close() error {
	r.tools.mu.RLock()
	registry := r.tools.workspace
	r.tools.mu.RUnlock()
	if registry != nil {
		return registry.Close()
	}
	return nil
}

func workspaceDirectory(scope domain.TaskWorkspaceScope, grant domain.TaskWorkspaceGrant) store.WorkspaceGrantReceipt {
	return store.WorkspaceGrantReceipt{ID: grant.GrantID, GroupID: scope.GroupID, MemberID: grant.MemberID, Root: grant.Root, ResolvedRoot: grant.ResolvedRoot, ResourceKey: scope.ResourceKey, NodeID: scope.NodeID, Scope: "read_write", AuthorizedBy: "user"}
}

func (r *memberWorkspaceRegistry) protectedPaths() []string {
	paths := make([]string, 0, len(r.boundaries))
	for _, boundary := range r.boundaries {
		paths = append(paths, boundary.path)
	}
	return paths
}

// Store resolution has the first-SQL live role/Attempt fence and resource claim.
// This private registration is not an ordinary Issue option or an IO authority.
func (r *memberWorkspaceRegistry) openGrantLocked(ctx context.Context, role store.CoordinationToolScope) (*memberWorkspaceGrant, error) {
	scope, grant, err := r.tools.db.ResolveWorkspaceForRole(ctx, role)
	if err != nil {
		return nil, err
	}
	if !r.rootsValid() {
		return nil, store.ErrMemberToolDenied
	}
	directory := workspaceDirectory(scope, grant)
	path, info, err := memberWorkspacePhysicalGrant(directory, r.protectedPaths())
	if err != nil {
		return nil, err
	}
	root, err := workspacefs.Open(path, info)
	if err != nil {
		return nil, store.ErrMemberToolDenied
	}
	entry := &memberWorkspaceGrant{role: role, directory: directory, root: root, info: info}
	if err := r.validateCurrent(ctx, entry); err != nil {
		_ = root.Close()
		return nil, err
	}
	return entry, nil
}

func (r *memberWorkspaceRegistry) validateCurrent(ctx context.Context, entry *memberWorkspaceGrant) error {
	scope, grant, err := r.tools.db.ResolveWorkspaceForRole(ctx, entry.role)
	if err != nil {
		return err
	}
	if workspaceDirectory(scope, grant) != entry.directory || !r.rootsValid() {
		return store.ErrMemberToolDenied
	}
	_, info, err := memberWorkspacePhysicalGrant(entry.directory, r.protectedPaths())
	if err != nil || !os.SameFile(info, entry.info) {
		return store.ErrMemberToolDenied
	}
	return nil
}

// The caller owns tools.mu (or is constructing the not-yet-published registry).
// Identity pins detect replacement; trusted stable ancestors remain required.
func (r *memberWorkspaceRegistry) rootsValid() bool {
	if r.closed || !privatePreparationDirectory(r.boundaries[0].path) {
		return false
	}
	for _, boundary := range r.boundaries {
		resolved, err := filepath.EvalSymlinks(boundary.path)
		info, statErr := os.Stat(boundary.path)
		if err != nil || statErr != nil || resolved != boundary.resolved || !os.SameFile(boundary.info, info) {
			return false
		}
	}
	info, err := r.scratch.Stat(".")
	return err == nil && os.SameFile(r.boundaries[0].info, info)
}

func (r *memberWorkspaceRegistry) revokeLocked(key [sha256.Size]byte) error {
	grant := r.grants[key]
	delete(r.grants, key)
	delete(r.tools.capabilities, key)
	delete(r.tools.mcpRequests, key)
	if grant != nil && grant.root != nil {
		return grant.root.Close()
	}
	return nil
}

// Close drains all file callbacks before closing owned pins and scratch. It does
// not close the borrowed immutable blob store or another profile's registration.
func (r *memberWorkspaceRegistry) Close() error {
	r.tools.mu.Lock()
	defer r.tools.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	var err error
	for key := range r.grants {
		err = errors.Join(err, r.revokeLocked(key))
	}
	return errors.Join(err, r.scratch.Close())
}
