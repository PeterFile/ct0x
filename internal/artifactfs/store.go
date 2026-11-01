//go:build unix

// Package artifactfs stores immutable, SHA256-addressed copies on Unix filesystems.
// Platform callers authorize access by supplying trusted os.Root handles, not paths.
// Roots do not prevent mounts, hardlinks, or device access and are not OS isolation.
package artifactfs

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

var (
	ErrPath       = errors.New("成果源路径无效")
	ErrHash       = errors.New("成果哈希无效")
	ErrTooLarge   = errors.New("成果超过大小上限")
	ErrNotRegular = errors.New("成果源不是普通文件")
	ErrCorrupt    = errors.New("成果内容已损坏")
	ErrClosed     = errors.New("成果文件库已关闭")
)

// Blob describes bytes only; it carries no business identity or metadata version.
type Blob struct {
	Hash string
	Size int64
}

// Store supports concurrent operations. Its zero value is not usable.
type Store struct {
	mu       sync.RWMutex
	root     *os.Root
	dir      *os.File
	maxBytes int64
	closed   bool
}

// New creates/opens a private blobs directory (0700) under a platform-owned root.
// It syncs the parent directory to persist the blobs entry before returning success.
// The caller must exclude untrusted writers and keep this root outside source grants.
// Root handles passed to New and Publish remain caller-owned. Store owns new handles.
// maxBytes is inclusive, in [0, MaxInt64); reads also enforce this configured limit.
// Reopen with the same limit to avoid treating older, larger blobs as corrupt.
func New(privateRoot *os.Root, maxBytes int64) (*Store, error) {
	if privateRoot == nil || maxBytes < 0 || maxBytes == 1<<63-1 {
		return nil, errors.New("成果文件库配置无效：需要私有根目录和有效大小上限")
	}
	if err := privateRoot.MkdirAll("blobs", 0700); err != nil {
		return nil, fmt.Errorf("创建成果目录失败: %w", err)
	}
	parent, err := privateRoot.Open(".")
	if err != nil {
		return nil, fmt.Errorf("打开成果父目录失败: %w", err)
	}
	if err := errors.Join(parent.Sync(), parent.Close()); err != nil {
		return nil, fmt.Errorf("同步或关闭成果父目录失败: %w", err)
	}
	root, err := privateRoot.OpenRoot("blobs")
	if err != nil {
		return nil, fmt.Errorf("打开成果目录失败: %w", err)
	}
	dir, err := root.Open(".")
	if err != nil {
		root.Close()
		return nil, fmt.Errorf("打开成果目录句柄失败: %w", err)
	}
	if err := dir.Chmod(0700); err != nil {
		dir.Close()
		root.Close()
		return nil, fmt.Errorf("设置成果目录权限失败: %w", err)
	}
	return &Store{root: root, dir: dir, maxBytes: maxBytes}, nil
}

// Publish copies at most maxBytes+1 bytes once, hashing the stream into private staging.
// Paths must be relative without any '..' component. Contained relative symlinks
// are allowed. The source root must stay open until Publish returns. Concurrent
// source writes may produce a mixed stream, not a point-in-time source snapshot.
// Published files are 0400. No source hardlink or overwrite rename is used.
// File data and directory changes are synced, but a crash can leave staging files;
// an error after linking may leave a valid blob. Retrying is safe, not a transaction.
func (s *Store) Publish(source *os.Root, name string) (blob Blob, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return Blob{}, ErrClosed
	}
	if source == nil || !filepath.IsLocal(name) {
		return Blob{}, ErrPath
	}
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == ".." {
			return Blob{}, ErrPath
		}
	}
	info, err := source.Stat(name)
	if err != nil {
		return Blob{}, fmt.Errorf("检查成果源文件失败: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Blob{}, ErrNotRegular
	}
	// Nonblocking open also prevents a raced-in FIFO from blocking before fstat.
	in, err := source.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Blob{}, fmt.Errorf("打开成果源文件失败: %w", err)
	}
	defer in.Close()
	info, err = in.Stat()
	if err != nil {
		return Blob{}, fmt.Errorf("检查成果源文件句柄失败: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Blob{}, ErrNotRegular
	}
	stage := ".tmp-" + rand.Text()
	out, err := s.root.OpenFile(stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return Blob{}, fmt.Errorf("创建成果临时文件失败: %w", err)
	}
	defer func() {
		if out != nil {
			err = errors.Join(err, out.Close())
		}
		err = errors.Join(err, s.root.Remove(stage), s.dir.Sync())
		if err != nil {
			blob = Blob{}
			err = fmt.Errorf("发布成果失败: %w", err)
		}
	}()
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(out, hash), io.LimitReader(in, s.maxBytes+1))
	if err != nil {
		return Blob{}, err
	}
	if size > s.maxBytes {
		return Blob{}, ErrTooLarge
	}
	if err = out.Chmod(0400); err != nil {
		return Blob{}, err
	}
	if err = out.Sync(); err != nil {
		return Blob{}, err
	}
	err = out.Close()
	out = nil
	if err != nil {
		return Blob{}, err
	}
	digest := fmt.Sprintf("%x", hash.Sum(nil))
	if err = s.root.Link(stage, digest); errors.Is(err, os.ErrExist) {
		var existing *os.File
		existing, err = s.openVerified(digest)
		if err == nil {
			err = existing.Close()
		}
	}
	if err != nil {
		return Blob{}, err
	}
	return Blob{Hash: digest, Size: size}, nil
}

// Open accepts only canonical lowercase SHA256 hex. It verifies the complete blob
// before returning a read-only handle positioned at zero. Corruption (including
// non-regular digest entries or bytes exceeding maxBytes) matches ErrCorrupt;
// missing entries match os.ErrNotExist. Private-root mutation after verification
// is outside the trust contract. The caller must close every successful reader.
func (s *Store) Open(hash string) (io.ReadCloser, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	if len(hash) != sha256.Size*2 {
		return nil, ErrHash
	}
	for _, c := range hash {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, ErrHash
		}
	}
	file, err := s.openVerified(hash)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func (s *Store) openVerified(hash string) (*os.File, error) {
	info, err := s.root.Lstat(hash)
	if err != nil {
		return nil, fmt.Errorf("读取成果失败: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, ErrCorrupt
	}
	file, err := s.root.OpenFile(hash, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("打开成果失败: %w", err)
	}
	info, statErr := file.Stat()
	if statErr == nil && info.Mode().IsRegular() {
		digest := sha256.New()
		var size int64
		size, err = io.Copy(digest, io.LimitReader(file, s.maxBytes+1))
		if err == nil && size <= s.maxBytes && fmt.Sprintf("%x", digest.Sum(nil)) == hash {
			if _, err = file.Seek(0, io.SeekStart); err == nil {
				return file, nil
			}
		}
	}
	return nil, errors.Join(ErrCorrupt, err, statErr, file.Close())
}

// Close waits for in-flight operations and is idempotent. Already opened readers
// remain usable; it neither closes caller-owned source roots nor removes blobs.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := errors.Join(s.dir.Close(), s.root.Close()); err != nil {
		return fmt.Errorf("关闭成果文件库失败: %w", err)
	}
	return nil
}
