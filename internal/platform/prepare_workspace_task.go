//go:build unix

package platform

import (
	"os"
	"path/filepath"
	"strings"

	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

// memberWorkspacePhysicalGrant checks observations only, not role/task/claim
// authority. Callers must first obtain the current exact grant under Store's
// live worker fence, and must revalidate it after IO while excluding revocation.
// Never derive a grant from CWD, a model path, or a historical scope alone.
func memberWorkspacePhysicalGrant(grant store.WorkspaceGrantReceipt, protected []string) (string, os.FileInfo, error) {
	denied := store.ErrMemberToolDenied
	if grant.Scope != "read_write" || grant.NodeID != "local" || grant.AuthorizedBy != "user" ||
		!filepath.IsAbs(grant.Root) || !filepath.IsAbs(grant.ResolvedRoot) || !strings.HasPrefix(grant.ResourceKey, "workspace:") {
		return "", nil, denied
	}
	resolved, err := filepath.EvalSymlinks(grant.Root)
	if err != nil || resolved != grant.ResolvedRoot {
		return "", nil, denied
	}
	keys, err := scheduler.WorkspaceResourceKeys([]string{grant.Root, grant.ResolvedRoot})
	if err != nil || keys[0] != grant.ResourceKey || keys[1] != grant.ResourceKey {
		return "", nil, denied
	}
	// Protected roots may contain each other. Check each against the source,
	// rather than incorrectly requiring the private configuration to be disjoint.
	for _, root := range protected {
		keys, err := scheduler.WorkspaceResourceKeys([]string{root, resolved})
		if err != nil || keys[0] == keys[1] || keys[1] != grant.ResourceKey {
			return "", nil, denied
		}
	}
	info, err := os.Lstat(resolved)
	if err != nil || !info.IsDir() {
		return "", nil, denied
	}
	return resolved, info, nil
}
