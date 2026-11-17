//go:build unix

package workspacefs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"
)

const (
	MaxTextBytes = 64 * 1024
	MaxEntries   = 200
)

var (
	ErrPath           = errors.New("工作目录相对路径无效")
	ErrTooLarge       = errors.New("文本超过大小上限")
	ErrUTF8           = errors.New("文本不是有效的 UTF-8")
	ErrTooManyEntries = errors.New("目录项超过列表上限")
	ErrNotRegular     = errors.New("目标不是普通文件或是符号链接")
	ErrNotDirectory   = errors.New("路径祖先不是普通目录或是符号链接")
	ErrConflict       = errors.New("文件内容或物理身份已改变")
)

// Snapshot describes observed bytes and ordinary permission bits, not authority
// or a filesystem snapshot. SHA256 is canonical lowercase hex over Text's bytes.
type Snapshot struct {
	Text   string
	SHA256 string
	Mode   os.FileMode
}

// Entry describes one directory entry without following a final symlink.
type Entry struct {
	Name string
	Mode os.FileMode
	Size int64
}

func reservedPath(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if part == stagingDirName {
			return true
		}
	}
	return false
}

func validPath(name string, directory bool) bool {
	return fs.ValidPath(name) && !strings.ContainsAny(name, "\\\x00") && (directory || name != ".") && !reservedPath(name)
}

// openDir rejects symlinks in every path component, then pins the final directory.
// Ancestor stability remains a caller requirement; these checks are not a CAS.
func (r *Root) openDir(name string) (*os.Root, error) {
	info, err := r.root.Stat(".")
	if err != nil {
		return nil, err
	}
	if name != "." {
		prefix := ""
		for _, part := range strings.Split(name, "/") {
			prefix = path.Join(prefix, part)
			info, err = r.root.Lstat(prefix)
			if err != nil {
				return nil, err
			}
			if !info.IsDir() {
				return nil, ErrNotDirectory
			}
		}
	}
	dir, err := r.root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	held, err := dir.Stat(".")
	if err != nil || !os.SameFile(info, held) {
		return nil, errors.Join(ErrConflict, err, dir.Close())
	}
	return dir, nil
}

func (r *Root) verifyDir(name string, held *os.Root) error {
	if err := r.checkRoot(); err != nil {
		return err
	}
	current, err := r.openDir(name)
	if err != nil {
		return errors.Join(ErrConflict, err)
	}
	defer current.Close()
	a, errA := held.Stat(".")
	b, errB := current.Stat(".")
	if errA != nil || errB != nil || !os.SameFile(a, b) {
		return errors.Join(ErrConflict, errA, errB)
	}
	return nil
}

func sameFileState(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime().Equal(b.ModTime())
}

func textHash(text string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(text))) }

func readText(ctx context.Context, parent *os.Root, name string) (Snapshot, os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, nil, err
	}
	before, err := parent.Lstat(name)
	if err != nil {
		return Snapshot{}, nil, err
	}
	if !before.Mode().IsRegular() {
		return Snapshot{}, nil, ErrNotRegular
	}
	// Nonblocking open prevents a raced-in FIFO from hanging before fstat.
	file, err := parent.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Snapshot{}, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Snapshot{}, nil, err
	}
	if !info.Mode().IsRegular() {
		return Snapshot{}, nil, ErrNotRegular
	}
	if !sameFileState(before, info) {
		return Snapshot{}, nil, ErrConflict
	}
	if info.Size() > MaxTextBytes {
		return Snapshot{}, nil, ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxTextBytes+1))
	if err != nil {
		return Snapshot{}, nil, err
	}
	if len(data) > MaxTextBytes {
		return Snapshot{}, nil, ErrTooLarge
	}
	if !utf8.Valid(data) {
		return Snapshot{}, nil, ErrUTF8
	}
	after, statErr := file.Stat()
	named, nameErr := parent.Lstat(name)
	if statErr != nil || nameErr != nil {
		return Snapshot{}, nil, errors.Join(ErrConflict, statErr, nameErr)
	}
	if !sameFileState(info, after) || !sameFileState(info, named) || int64(len(data)) != info.Size() {
		return Snapshot{}, nil, ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, nil, err
	}
	text := string(data)
	return Snapshot{Text: text, SHA256: textHash(text), Mode: info.Mode().Perm()}, info, nil
}

// Read observes at most MaxTextBytes of valid UTF-8 in a regular file.
// Paths are canonical slash-separated relative paths with no symlink components.
func (r *Root) Read(ctx context.Context, name string) (snapshot Snapshot, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer func() {
		if err != nil {
			err = fmt.Errorf("读取工作目录文本失败: %w", err)
		}
	}()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if !validPath(name, false) {
		return Snapshot{}, ErrPath
	}
	if err := r.checkRoot(); err != nil {
		return Snapshot{}, err
	}
	dirName := path.Dir(name)
	parent, err := r.openDir(dirName)
	if err != nil {
		return Snapshot{}, err
	}
	defer parent.Close()
	observed, _, err := readText(ctx, parent, path.Base(name))
	if err != nil {
		return Snapshot{}, err
	}
	if err := r.verifyDir(dirName, parent); err != nil {
		return Snapshot{}, err
	}
	return observed, nil
}

// List returns at most MaxEntries sorted entries, or ErrTooManyEntries and no
// partial list. The reserved staging directory is hidden and does not count.
// It is nonrecursive, accepts ".", and is not a point-in-time view.
func (r *Root) List(ctx context.Context, name string) (entries []Entry, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer func() {
		if err != nil {
			err = fmt.Errorf("列出工作目录失败: %w", err)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validPath(name, true) {
		return nil, ErrPath
	}
	if err := r.checkRoot(); err != nil {
		return nil, err
	}
	dir, err := r.openDir(name)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	file, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	// At most one entry can have the reserved name; scanning remains bounded.
	items, err := file.ReadDir(MaxEntries + 2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	result := make([]Entry, 0, len(items))
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if item.Name() == stagingDirName {
			continue
		}
		if len(result) == MaxEntries {
			return nil, ErrTooManyEntries
		}
		if !validPath(item.Name(), false) {
			return nil, ErrPath
		}
		info, err := item.Info()
		if err != nil {
			return nil, err
		}
		result = append(result, Entry{Name: item.Name(), Mode: info.Mode(), Size: info.Size()})
	}
	if err := r.verifyDir(name, dir); err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}
