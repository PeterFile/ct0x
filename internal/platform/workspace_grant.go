package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

// WorkspaceGrantInput is explicit local-user authorization, separate from the
// member's workspace preference. Each member has one immutable directory grant;
// a read_write scope alone does not enable file tools or authorize a task.
type WorkspaceGrantInput struct {
	Root  string `json:"root"`
	Scope string `json:"scope"`
}

// AuthorizeWorkspaceGrant records authority and selects its directory atomically.
// It does not claim a delivery, execute a model, add tools or modify external files.
// This trusted local-user entry must never be registered as a member tool.
func (p *Preparer) AuthorizeWorkspaceGrant(ctx context.Context, groupID, memberID string, input WorkspaceGrantInput, key string) (store.WorkspaceGrantReceipt, error) {
	return p.store.AuthorizeWorkspaceGrant(ctx, store.AuthorizeWorkspaceGrantInput{
		GroupID: groupID, MemberID: memberID, Root: input.Root, Scope: input.Scope, IdempotencyKey: key,
	}, func(existing []store.WorkspaceGrantReceipt) (store.WorkspaceGrantDirectory, error) {
		keys, err := p.validateDurableWorkspaceRoots(existing, input.Root)
		if err != nil {
			return store.WorkspaceGrantDirectory{}, store.ErrInvalid
		}
		key := keys[len(keys)-1]
		if index, exists := p.grants[preparationRecipient{groupID, memberID}]; exists && p.rootKeys[index] != key {
			return store.WorkspaceGrantDirectory{}, store.ErrConflict
		}
		resolved, err := filepath.EvalSymlinks(input.Root)
		if err != nil {
			return store.WorkspaceGrantDirectory{}, store.ErrInvalid
		}
		resolvedKeys, err := scheduler.WorkspaceResourceKeys([]string{resolved})
		if err != nil || resolvedKeys[0] != key {
			return store.WorkspaceGrantDirectory{}, store.ErrInvalid
		}
		return store.WorkspaceGrantDirectory{ResolvedRoot: resolved, ResourceKey: key}, nil
	})
}

func (p *Preparer) GetWorkspaceGrant(ctx context.Context, groupID, memberID string) (store.WorkspaceGrantReceipt, error) {
	return p.store.GetWorkspaceGrant(ctx, groupID, memberID)
}

// Validate the complete physical root set, including unused recipients and
// immutable legacy grants. Aliases may share keys, but never a private root.
func (p *Preparer) validateDurableWorkspaceRoots(grants []store.WorkspaceGrantReceipt, candidate string) ([]string, error) {
	if err := p.revalidateWorkspaces(); err != nil {
		return nil, err
	}
	roots := slices.Clone(p.roots)
	for _, grant := range grants {
		if (grant.Scope != "read_only" && grant.Scope != "read_write") || grant.NodeID != "local" || grant.AuthorizedBy != "user" ||
			!filepath.IsAbs(grant.Root) || !filepath.IsAbs(grant.ResolvedRoot) || !strings.HasPrefix(grant.ResourceKey, "workspace:") {
			return nil, errPrepareWorkspace
		}
		roots = append(roots, grant.Root)
	}
	if candidate != "" {
		roots = append(roots, candidate)
	}
	keys, err := scheduler.WorkspaceResourceKeys(roots)
	if err != nil {
		return nil, p.staleDurableWorkspaceRootsError(grants)
	}
	if !slices.Equal(keys[:len(p.roots)], p.rootKeys) {
		return nil, errPrepareWorkspace
	}
	for i, grant := range grants {
		if keys[len(p.roots)+i] != grant.ResourceKey {
			return nil, p.staleDurableWorkspaceRootsError(grants)
		}
	}
	for _, key := range keys[len(p.roots):] {
		if key == p.rootKeys[0] {
			return nil, errPrepareWorkspace
		}
	}
	if p.privateDataRoot != "" {
		protected := append([]string{p.privateDataRoot}, roots[len(p.roots):]...)
		protectedKeys, err := scheduler.WorkspaceResourceKeys(protected)
		if err != nil || protectedKeys[0] != p.privateDataKey {
			return nil, errPrepareWorkspace
		}
		for _, key := range protectedKeys[1:] {
			if key == p.privateDataKey {
				return nil, errPrepareWorkspace
			}
		}
	}
	return keys, nil
}

// A stale receipt must not hide a conflict between still-current roots and
// trusted configuration. Audit the strictly smaller current subset on collection
// errors and identity mismatches before classifying staleness.
func (p *Preparer) staleDurableWorkspaceRootsError(grants []store.WorkspaceGrantReceipt) error {
	currentGrants := make([]store.WorkspaceGrantReceipt, 0, len(grants))
	for _, grant := range grants {
		current, err := scheduler.WorkspaceResourceKeys([]string{grant.Root})
		if err == nil && current[0] == grant.ResourceKey {
			currentGrants = append(currentGrants, grant)
		}
	}
	if len(currentGrants) < len(grants) {
		if _, err := p.validateDurableWorkspaceRoots(currentGrants, ""); err != nil {
			return err
		}
		return scheduler.ErrNotReady
	}
	return errPrepareWorkspace
}

// A stale claimed directory may have moved inside a private scope. Historical
// path geometry cannot prove separation from that retained physical identity.
// Unclaimed stale roots still require conservative, disjoint path boundaries.
func (p *Preparer) validateStalePrivateWorkspaceRoots(ctx context.Context, grants []store.WorkspaceGrantReceipt) error {
	roots := slices.Clone(p.roots)
	for _, grant := range grants {
		current, err := scheduler.WorkspaceResourceKeys([]string{grant.Root})
		if err != nil || current[0] != grant.ResourceKey {
			claimed, err := p.store.HasResourceClaim(ctx, grant.ResourceKey)
			if err != nil {
				return preparationReadError(ctx, err)
			}
			if claimed {
				return scheduler.ErrNotReady
			}
		}
		for _, path := range []string{grant.Root, grant.ResolvedRoot} {
			boundary, err := existingWorkspaceBoundary(path)
			if err != nil {
				return err
			}
			roots = append(roots, boundary)
		}
	}
	keys, err := scheduler.WorkspaceResourceKeys(roots)
	if err != nil {
		return scheduler.ErrNotReady
	}
	if !slices.Equal(keys[:len(p.roots)], p.rootKeys) {
		return errPrepareWorkspace
	}
	for _, key := range keys[len(p.roots):] {
		if key == p.rootKeys[0] {
			return scheduler.ErrNotReady
		}
	}
	if p.privateDataRoot != "" {
		protected := append([]string{p.privateDataRoot}, roots[len(p.roots):]...)
		protectedKeys, err := scheduler.WorkspaceResourceKeys(protected)
		if err != nil {
			return scheduler.ErrNotReady
		}
		if protectedKeys[0] != p.privateDataKey {
			return errPrepareWorkspace
		}
		for _, key := range protectedKeys[1:] {
			if key == p.privateDataKey {
				return scheduler.ErrNotReady
			}
		}
	}
	// Also fence an old durable parent renamed around the configured private
	// root: its retained identity remains authoritative even at a new path.
	privatePath, err := filepath.EvalSymlinks(p.privateRoot)
	if err != nil {
		return errPrepareWorkspace
	}
	for path := privatePath; ; path = filepath.Dir(path) {
		keys, err := scheduler.WorkspaceResourceKeys([]string{path})
		if err != nil {
			return errPrepareWorkspace
		}
		for _, grant := range grants {
			if keys[0] == grant.ResourceKey {
				return scheduler.ErrNotReady
			}
		}
		if filepath.Dir(path) == path {
			return nil
		}
	}
}

func existingWorkspaceBoundary(path string) (string, error) {
	for {
		if _, err := os.Lstat(path); err == nil {
			return path, nil // Do not climb past a dangling or retargeted symlink.
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", scheduler.ErrNotReady
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", scheduler.ErrNotReady
		}
		path = parent
	}
}

func durableWorkspaceKey(grants []store.WorkspaceGrantReceipt, groupID, memberID string) (string, error) {
	for _, grant := range grants {
		if grant.GroupID == groupID && grant.MemberID == memberID {
			return grant.ResourceKey, nil
		}
	}
	return "", scheduler.ErrNotReady
}
