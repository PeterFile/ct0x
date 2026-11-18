package platform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func privatePreparationDirectory(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Lstat(filepath.Clean(path))
	return err == nil && info.Mode() == os.ModeDir|0700
}

func (p *Preparer) configureWorkspaces(opts PrepareOptions) error {
	if !privatePreparationDirectory(opts.PrivateWorkspaceRoot) {
		return errPrepareWorkspace
	}
	p.privateRoot = filepath.Clean(opts.PrivateWorkspaceRoot)
	p.roots = []string{p.privateRoot}
	for _, grant := range opts.WorkspaceGrants {
		recipient := preparationRecipient{grant.GroupID, grant.MemberID}
		if _, duplicate := p.grants[recipient]; duplicate || strings.TrimSpace(grant.GroupID) == "" ||
			strings.TrimSpace(grant.MemberID) == "" || strings.TrimSpace(grant.Root) == "" {
			return errPrepareConfig
		}
		p.grants[recipient] = len(p.roots)
		p.roots = append(p.roots, grant.Root)
	}
	keys, err := scheduler.WorkspaceResourceKeys(p.roots)
	if err != nil {
		return errPrepareWorkspace
	}
	// Grant aliases may share each other's identity, but not the private root:
	// a private scope is locked separately, so granting its parent would bypass it.
	for _, key := range keys[1:] {
		if key == keys[0] {
			return errPrepareWorkspace
		}
	}
	p.rootKeys = keys
	if opts.PrivateDataRoot != "" {
		dataKeys, err := scheduler.WorkspaceResourceKeys([]string{opts.PrivateDataRoot})
		if err != nil {
			return errPrepareWorkspace
		}
		p.privateDataRoot, p.privateDataKey = opts.PrivateDataRoot, dataKeys[0]
	}
	return nil
}

func (p *Preparer) revalidateWorkspaces() error {
	if !privatePreparationDirectory(p.privateRoot) {
		return errPrepareWorkspace
	}
	keys, err := scheduler.WorkspaceResourceKeys(p.roots)
	if err != nil || !slices.Equal(keys, p.rootKeys) {
		return errPrepareWorkspace
	}
	if p.privateDataRoot != "" {
		keys, err := scheduler.WorkspaceResourceKeys([]string{p.privateDataRoot})
		if err != nil || keys[0] != p.privateDataKey {
			return errPrepareWorkspace
		}
	}
	return nil
}

// Only Prepare may create a private scope. Inspect validates existing targets;
// absence under a revalidated trusted root is not a future write/claim promise.
func (p *Preparer) resolvePreparationWorkspace(ctx context.Context, member domain.Member, conversation domain.Conversation, createPrivate bool) (string, string, error) {
	workspace, err := p.resolvePreparationWorkspaceSnapshot(ctx, member, conversation, createPrivate)
	return workspace.cwd, workspace.key, err
}

type preparationWorkspace struct {
	cwd, key string
	grant    store.WorkspaceGrantReceipt
}

func (p *Preparer) resolvePreparationWorkspaceSnapshot(ctx context.Context, member domain.Member, conversation domain.Conversation, createPrivate bool) (preparationWorkspace, error) {
	var empty preparationWorkspace
	grants, err := p.store.WorkspaceGrantSnapshot(ctx)
	if err != nil {
		return empty, preparationReadError(ctx, err)
	}
	if len(grants) != 0 {
		if _, err := p.validateDurableWorkspaceRoots(grants, ""); err != nil {
			if member.Workspace != "" || !errors.Is(err, scheduler.ErrNotReady) {
				return empty, err
			}
			if err := p.validateStalePrivateWorkspaceRoots(ctx, grants); err != nil {
				return empty, err
			}
		}
	}
	if member.Workspace != "" {
		if !filepath.IsAbs(member.Workspace) {
			return empty, scheduler.ErrNotReady
		}
		var key string
		var issued store.WorkspaceGrantReceipt
		if index, granted := p.grants[preparationRecipient{member.GroupID, member.ID}]; granted {
			key = p.rootKeys[index]
		} else {
			key, err = durableWorkspaceKey(grants, member.GroupID, member.ID)
			if err != nil {
				return empty, err
			}
			if p.enableNativeSessionResume {
				for _, grant := range grants {
					if grant.GroupID == member.GroupID && grant.MemberID == member.ID {
						issued = grant
						break
					}
				}
			}
		}
		resolved, err := filepath.EvalSymlinks(member.Workspace)
		if err != nil {
			return empty, scheduler.ErrNotReady
		}
		keys, err := scheduler.WorkspaceResourceKeys([]string{resolved})
		if err != nil || keys[0] != key {
			return empty, scheduler.ErrNotReady
		}
		// Preserve the selected grant, not an unrelated receipt for this member.
		return preparationWorkspace{cwd: resolved, key: key, grant: issued}, nil
	}
	path := p.privatePreparationPath(member, conversation)
	if createPrivate {
		if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
			return empty, errPrepareWorkspace
		}
		if !privatePreparationDirectory(path) {
			return empty, errPrepareWorkspace
		}
	} else {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return empty, nil
		}
		if err != nil {
			return empty, &preparationFailure{category: errPrepareWorkspace, cause: err}
		}
		if info.Mode() != os.ModeDir|0700 {
			return empty, errPrepareWorkspace
		}
	}
	keys, err := scheduler.WorkspaceResourceKeys([]string{path})
	if err != nil {
		return empty, errPrepareWorkspace
	}
	return preparationWorkspace{cwd: path, key: keys[0]}, nil
}

func (p *Preparer) privatePreparationPath(member domain.Member, conversation domain.Conversation) string {
	// JSON makes the three opaque authoritative IDs unambiguous. Only the hash,
	// never any ID or model-selected path component, enters the private path.
	identity, _ := json.Marshal([]string{member.GroupID, member.ID, conversation.ID})
	return filepath.Join(p.privateRoot, fmt.Sprintf("scope-%x", sha256.Sum256(identity)))
}
