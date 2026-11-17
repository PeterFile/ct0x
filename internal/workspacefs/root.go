//go:build unix

// Package workspacefs provides bounded UTF-8 directory IO under a trusted root.
// Root identities and snapshots are observations, not business authorization.
// See README.md for the stable-ancestor, external-writer and unknown-result contract.
package workspacefs

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

var (
	ErrRootChanged = errors.New("工作目录物理身份已改变或句柄无效")
	ErrClosed      = errors.New("工作目录句柄已关闭")
)

// Root owns an os.Root bound to a trusted path and physical directory identity.
// It is safe for concurrent use; its zero value is not usable. It is not a grant.
// Open must only be called by trusted server code, never with model-supplied roots.
// The mutex covers this instance only, not other handles or external processes.
type Root struct {
	mu       sync.Mutex
	path     string
	identity os.FileInfo
	root     *os.Root
	io       writeIO
	closed   bool
}

// Open verifies expected against both the current absolute path and a new handle.
// expected must be obtained by the trusted caller from the actual directory.
// The caller must keep the directory's ancestors trusted and stable, and reserve
// .workspacefs-staging below writable directories exclusively for this protocol.
// Open walks resolved root ancestors; public paths and listings also reject the
// same physical namespace entry under filesystem-equivalent names.
// Public operations cannot address or list that namespace. Nonprotocol writers
// must be excluded from it for the whole write, using OS-level exclusivity if
// needed; this package does not isolate arbitrary same-UID processes.
func Open(path string, expected os.FileInfo) (*Root, error) {
	if !filepath.IsAbs(path) || expected == nil || !expected.IsDir() {
		return nil, ErrRootChanged
	}
	path = filepath.Clean(path)
	if reservedPath(filepath.ToSlash(path)) {
		return nil, ErrPath
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || !os.SameFile(expected, info) {
		return nil, errors.Join(ErrRootChanged, err)
	}
	// Preserve trusted ancestor symlink compatibility, but inspect the resolved
	// ancestry so a symlink cannot conceal a physical namespace component.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, errors.Join(ErrRootChanged, err)
	}
	name, err := filepath.Rel("/", resolved)
	if err != nil {
		return nil, errors.Join(ErrRootChanged, err)
	}
	filesystem, err := os.OpenRoot("/")
	if err != nil {
		return nil, errors.Join(ErrRootChanged, err)
	}
	defer filesystem.Close()
	handle, err := openDirAt(filesystem, filepath.ToSlash(name))
	if err != nil {
		return nil, errors.Join(ErrRootChanged, err)
	}
	r := &Root{
		path: path, identity: expected, root: handle,
		io: writeIO{publish: publish, sync: (*os.File).Sync},
	}
	if err := r.checkRoot(); err != nil {
		return nil, errors.Join(err, handle.Close())
	}
	return r, nil
}

func (r *Root) checkRoot() error {
	if r.closed {
		return ErrClosed
	}
	current, err := os.Lstat(r.path)
	if err != nil || !current.IsDir() || !os.SameFile(r.identity, current) {
		return errors.Join(ErrRootChanged, err)
	}
	held, err := r.root.Stat(".")
	if err != nil || !held.IsDir() || !os.SameFile(r.identity, held) {
		return errors.Join(ErrRootChanged, err)
	}
	return nil
}

// Close waits for operations on this instance and is idempotent.
func (r *Root) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return r.root.Close()
}
