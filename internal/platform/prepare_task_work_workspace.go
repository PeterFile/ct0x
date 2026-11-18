package platform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/scheduler"
)

// A text followup has no filesystem lineage to the Assignment's original Turn.
// This pin is for the new delivery's dedicated CWD and its scheduler claim.
type taskWorkWorkspace struct {
	path, key    string
	parent, info os.FileInfo
}

func (w taskWorkWorkspace) sealName() string {
	// Binds a server-created directory to both its delivery name and its exact
	// resource. No capability, source text or other task data is stored here.
	return fmt.Sprintf(".task-work-%x", sha256.Sum256([]byte(filepath.Base(w.path)+"\x00"+w.key)))
}

func sameTaskWorkDirectory(path string, expected os.FileInfo) bool {
	info, err := os.Lstat(path)
	return err == nil && expected != nil && info.Mode() == os.ModeDir|0700 && os.SameFile(info, expected)
}

func (w taskWorkWorkspace) pathMatches() bool {
	return sameTaskWorkDirectory(filepath.Dir(w.path), w.parent) && sameTaskWorkDirectory(w.path, w.info)
}

func (w taskWorkWorkspace) valid(dir *os.Root) bool {
	info, err := dir.Stat(".")
	if err != nil || info.Mode() != os.ModeDir|0700 || !os.SameFile(info, w.info) {
		return false
	}
	f, err := dir.Open(".")
	if err != nil {
		return false
	}
	names, err := f.Readdirnames(2)
	closeErr := f.Close()
	if err != nil && err != io.EOF || closeErr != nil || len(names) != 1 || names[0] != w.sealName() {
		return false
	}
	seal, err := dir.Lstat(names[0])
	return err == nil && seal.Mode() == 0600 && seal.Size() == 0 && w.pathMatches()
}

// Only Prepare provisions this namespace. Inspect validates any existing CWD
// without creating one or predicting the inode of a future directory.
func (p *Preparer) freshTaskWorkWorkspace(ctx context.Context, work domain.TaskWorkDelivery, create bool) (taskWorkWorkspace, error) {
	var empty taskWorkWorkspace
	grants, err := p.store.WorkspaceGrantSnapshot(ctx)
	if err != nil {
		return empty, preparationReadError(ctx, err)
	}
	if len(grants) != 0 {
		if _, err := p.validateDurableWorkspaceRoots(grants, ""); err != nil {
			if !errors.Is(err, scheduler.ErrNotReady) {
				return empty, err
			}
			if err := p.validateStalePrivateWorkspaceRoots(ctx, grants); err != nil {
				return empty, err
			}
		}
	}

	base, err := os.OpenRoot(p.privateRoot)
	if err != nil {
		return empty, errPrepareWorkspace
	}
	defer base.Close()
	parent, err := base.Stat(".")
	if err != nil {
		return empty, errPrepareWorkspace
	}
	r := p.memberTools
	keys, err := scheduler.WorkspaceResourceKeys([]string{p.privateRoot, r.configRoot, r.roleFence.executionRoot})
	if err != nil || keys[0] != p.rootKeys[0] || keys[0] == keys[1] || keys[0] == keys[2] || !sameTaskWorkDirectory(p.privateRoot, parent) {
		return empty, errPrepareWorkspace
	}
	identity, _ := json.Marshal([]string{work.GroupID, work.DeliveryID})
	name := fmt.Sprintf("task-work-%x", sha256.Sum256(identity)) // Never an ordinary scope-* directory.
	if err := ctx.Err(); err != nil {
		return empty, preparationReadError(ctx, err)
	}
	created := false
	if create {
		// Only the exclusive creator may seal a directory. An unsealed (even
		// empty) or replaced entry is not adopted or repaired under this name.
		err := base.Mkdir(name, 0700)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return empty, errPrepareWorkspace
		}
		created = err == nil
	}
	info, err := base.Lstat(name)
	if !create && errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil || info.Mode() != os.ModeDir|0700 {
		return empty, errPrepareWorkspace
	}
	dir, err := base.OpenRoot(name)
	if err != nil {
		return empty, errPrepareWorkspace
	}
	defer dir.Close()
	w := taskWorkWorkspace{path: filepath.Join(p.privateRoot, name), parent: parent, info: info}
	keys, err = scheduler.WorkspaceResourceKeys([]string{w.path})
	opened, openErr := dir.Stat(".")
	if err != nil || openErr != nil || !os.SameFile(info, opened) || !w.pathMatches() {
		return empty, errPrepareWorkspace
	}
	w.key = keys[0]
	if created {
		seal, err := dir.OpenFile(w.sealName(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil || seal.Close() != nil {
			return empty, errPrepareWorkspace
		}
	}
	if !w.valid(dir) {
		return empty, errPrepareWorkspace
	}
	return w, nil
}
