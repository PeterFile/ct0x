package platform

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/scheduler"
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
	return nil
}

func (p *Preparer) workspace(member domain.Member, conversation domain.Conversation) (string, string, error) {
	if member.Workspace != "" {
		index, granted := p.grants[preparationRecipient{member.GroupID, member.ID}]
		if !granted || !filepath.IsAbs(member.Workspace) {
			return "", "", scheduler.ErrNotReady
		}
		resolved, err := filepath.EvalSymlinks(member.Workspace)
		if err != nil {
			return "", "", scheduler.ErrNotReady
		}
		keys, err := scheduler.WorkspaceResourceKeys([]string{resolved})
		if err != nil || keys[0] != p.rootKeys[index] {
			return "", "", scheduler.ErrNotReady
		}
		// Use the authorized root's key, not a key for an arbitrary selected child.
		return resolved, p.rootKeys[index], nil
	}
	// JSON makes the three opaque authoritative IDs unambiguous. Only the hash,
	// never any ID or model-selected path component, enters the private path.
	identity, _ := json.Marshal([]string{member.GroupID, member.ID, conversation.ID})
	path := filepath.Join(p.privateRoot, fmt.Sprintf("scope-%x", sha256.Sum256(identity)))
	if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
		return "", "", errPrepareWorkspace
	}
	if !privatePreparationDirectory(path) {
		return "", "", errPrepareWorkspace
	}
	keys, err := scheduler.WorkspaceResourceKeys([]string{path})
	if err != nil {
		return "", "", errPrepareWorkspace
	}
	return path, keys[0], nil
}
