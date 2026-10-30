package scheduler

import (
	"errors"
	"os"
	"path/filepath"
)

var (
	ErrInvalidWorkspace     = errors.New("工作区根必须是已存在且不重叠的绝对目录")
	ErrWorkspaceUnsupported = errors.New("当前平台不支持工作区目录身份锁")
)

// WorkspaceResourceKeys validates the complete trusted authorized-root snapshot.
// Keys follow input order; directory aliases share a device/inode identity.
// Distinct overlapping roots are rejected. Roots must remain stable while claimed.
// This is local write coordination, not authorization or a filesystem sandbox.
func WorkspaceResourceKeys(roots []string) ([]string, error) {
	keys := make([]string, 0, len(roots))
	paths := make([]string, 0, len(roots))
	infos := make([]os.FileInfo, 0, len(roots))
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			return nil, ErrInvalidWorkspace
		}
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, ErrInvalidWorkspace
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			return nil, ErrInvalidWorkspace
		}
		key, err := workspaceIdentity(info)
		if err != nil {
			return nil, err
		}
		for i, previous := range infos {
			if os.SameFile(previous, info) {
				continue
			}
			contains, err := workspaceContains(previous, resolved)
			if err != nil || contains {
				return nil, ErrInvalidWorkspace
			}
			contains, err = workspaceContains(info, paths[i])
			if err != nil || contains {
				return nil, ErrInvalidWorkspace
			}
		}
		keys = append(keys, key)
		paths = append(paths, resolved)
		infos = append(infos, info)
	}
	return keys, nil
}

// Compare ancestor identities rather than path prefixes, including case aliases.
func workspaceContains(ancestor os.FileInfo, child string) (bool, error) {
	for {
		info, err := os.Stat(child)
		if err != nil {
			return false, err
		}
		if os.SameFile(ancestor, info) {
			return true, nil
		}
		parent := filepath.Dir(child)
		if parent == child {
			return false, nil
		}
		child = parent
	}
}
