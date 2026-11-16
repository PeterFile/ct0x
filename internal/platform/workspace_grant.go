package platform

import (
	"context"
	"errors"
	"path/filepath"
	"slices"

	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

// WorkspaceGrantInput is explicit local-user authorization, separate from the
// member's workspace preference. D1 supports one immutable read-only directory.
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
		if grant.Scope != "read_only" || grant.NodeID != "local" || grant.AuthorizedBy != "user" {
			return nil, errPrepareWorkspace
		}
		roots = append(roots, grant.Root)
	}
	if candidate != "" {
		roots = append(roots, candidate)
	}
	keys, err := scheduler.WorkspaceResourceKeys(roots)
	if err != nil || !slices.Equal(keys[:len(p.roots)], p.rootKeys) {
		return nil, errPrepareWorkspace
	}
	for i, grant := range grants {
		if keys[len(p.roots)+i] != grant.ResourceKey {
			return nil, errPrepareWorkspace
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

func (p *Preparer) durableWorkspaceKey(ctx context.Context, groupID, memberID string) (string, error) {
	grant, err := p.store.GetWorkspaceGrant(ctx, groupID, memberID)
	if errors.Is(err, store.ErrNotFound) {
		return "", scheduler.ErrNotReady
	}
	if err != nil {
		return "", preparationReadError(ctx, err)
	}
	grants, err := p.store.WorkspaceGrantSnapshot(ctx)
	if err != nil {
		return "", preparationReadError(ctx, err)
	}
	if _, err := p.validateDurableWorkspaceRoots(grants, ""); err != nil {
		return "", err
	}
	return grant.ResourceKey, nil
}
