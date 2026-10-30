// Package skills treats skill files as untrusted data, never executable grants.
// Only explicitly named roots are inspected. Linux and macOS are supported;
// other platforms fail closed. os.Root does not isolate mounts or hard links.
package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type Root struct{ ID, Path string }

// Limits bound each operation. MaxEntries is per directory; MaxSkills and
// MaxTotalBytes apply across all roots during listing, or to one snapshot.
// Listing charges bytes actually read, including invalid entries.
type Limits struct {
	MaxRoots, MaxEntries, MaxSkills, MaxDirs, MaxFiles int
	MaxHeaderBytes, MaxFileBytes, MaxTotalBytes        int64
}

func DefaultLimits() Limits {
	return Limits{16, 1024, 512, 128, 512, 32 << 10, 2 << 20, 32 << 20}
}

// Identity versions the entire SKILL.md, not just its header. Supporting files
// are versioned only on explicit selection. SourcePath is a logical provenance
// label under the configured root, not authorization or a live resolved path.
type Identity struct{ RootID, Name, SkillSHA256 string }

type Metadata struct {
	Name, Description, License, Compatibility string
	AllowedTools                              string // Experimental metadata; never permission grants.
	Extra                                     map[string]string
}

type Entry struct {
	ID         Identity
	SourcePath string
	Metadata   Metadata
}

type Problem struct {
	RootID, Name string
	Err          error
}

type Listing struct {
	Entries  []Entry
	Problems []Problem
}

var (
	ErrInvalid = errors.New("技能输入或格式无效")
	ErrLimit   = errors.New("技能读取超过限制")
)

type openedRoot struct {
	Root
	handle *os.Root
}

type Catalog struct {
	roots  []openedRoot
	limits Limits
}

// Open anchors roots for the catalogue lifetime, including across renames.
// Explicit root paths may themselves be symlinks; their opened targets are the
// authorized boundaries. Call Close when finished; no source writes occur.
func Open(roots []Root, limits Limits) (*Catalog, error) {
	if !supportedPlatform {
		return nil, invalid("当前平台不支持安全的目录句柄读取")
	}
	for _, n := range []int{limits.MaxRoots, limits.MaxEntries, limits.MaxSkills, limits.MaxDirs, limits.MaxFiles} {
		if n <= 0 || n > 1<<20 {
			return nil, invalid("目录或数量限制无效")
		}
	}
	if limits.MaxHeaderBytes <= 0 || limits.MaxHeaderBytes > limits.MaxFileBytes || limits.MaxFileBytes > 1<<40 || limits.MaxTotalBytes <= 0 || limits.MaxTotalBytes > 1<<40 {
		return nil, invalid("字节限制无效")
	}
	if len(roots) > limits.MaxRoots {
		return nil, ErrLimit
	}
	c := &Catalog{limits: limits}
	seen := make(map[string]bool)
	for _, spec := range roots {
		if !sizedText(spec.ID, 128) || seen[spec.ID] || strings.TrimSpace(spec.Path) == "" {
			_ = c.Close()
			return nil, invalid("根名称为空、重复或路径无效")
		}
		seen[spec.ID] = true
		absolute, err := filepath.Abs(spec.Path)
		if err != nil {
			_ = c.Close()
			return nil, invalid("根路径无效")
		}
		// A final dot forces directory resolution before opening, even for FIFOs.
		handle, err := os.OpenRoot(absolute + string(os.PathSeparator) + ".")
		if err != nil {
			_ = c.Close()
			return nil, invalid("无法打开技能根目录")
		}
		spec.Path = absolute
		c.roots = append(c.roots, openedRoot{Root: spec, handle: handle})
	}
	slices.SortFunc(c.roots, func(a, b openedRoot) int { return strings.Compare(a.ID, b.ID) })
	return c, nil
}

func (c *Catalog) Close() error {
	var err error
	for _, root := range c.roots {
		err = errors.Join(err, root.handle.Close())
	}
	return err
}

// List considers only immediate directories/symlinks, ordered by root ID/name.
// It streams whole SKILL.md files for hashes, with bounded header scratch space,
// but never caches or discloses bodies. Invalid entries produce Problems; any
// exhausted limit fails the entire listing rather than returning partial success.
func (c *Catalog) List() (Listing, error) {
	out := Listing{Entries: []Entry{}, Problems: []Problem{}}
	remaining, count := c.limits.MaxTotalBytes, 0
	for _, root := range c.roots {
		entries, err := readDir(root.handle, ".", c.limits.MaxEntries)
		if err != nil {
			return Listing{}, err
		}
		for _, entry := range entries {
			if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
				continue
			}
			count++
			if count > c.limits.MaxSkills {
				return Listing{}, ErrLimit
			}
			m, hash, n, err := c.inspect(root, entry.Name(), remaining)
			remaining -= n
			if errors.Is(err, ErrLimit) {
				return Listing{}, err
			}
			if err != nil {
				out.Problems = append(out.Problems, Problem{RootID: root.ID, Name: entry.Name(), Err: err})
				continue
			}
			out.Entries = append(out.Entries, Entry{ID: Identity{root.ID, entry.Name(), hash},
				SourcePath: filepath.Join(root.Path, entry.Name(), "SKILL.md"), Metadata: m})
		}
	}
	return out, nil
}

func (c *Catalog) inspect(root openedRoot, name string, remaining int64) (Metadata, string, int64, error) {
	skill, err := root.handle.OpenRoot(name + "/.")
	if err != nil {
		return Metadata{}, "", 0, invalid("技能目录越界或无法打开")
	}
	defer skill.Close()
	max := min(c.limits.MaxFileBytes, remaining)
	f, err := openRegular(skill, "SKILL.md", max)
	if err != nil {
		return Metadata{}, "", 0, err
	}
	defer f.Close()
	limited := &io.LimitedReader{R: f, N: max + 1}
	hash := sha256.New()
	stream := io.TeeReader(limited, hash)
	prefix, err := io.ReadAll(io.LimitReader(stream, c.limits.MaxHeaderBytes+1))
	var header []byte
	if err == nil {
		header, _, err = splitHeader(prefix, c.limits.MaxHeaderBytes)
	}
	if err == nil {
		_, err = io.Copy(io.Discard, stream)
	}
	n := max + 1 - limited.N
	if n > max {
		err = ErrLimit
	}
	if err != nil {
		return Metadata{}, "", n, fmt.Errorf("读取技能失败：%w", err)
	}
	m, err := parseHeader(header, name)
	return m, hex.EncodeToString(hash.Sum(nil)), n, err
}

func openRegular(root *os.Root, name string, max int64) (*os.File, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|readFlags, 0)
	if err != nil {
		return nil, invalid("文件越界、链接循环或无法打开")
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, invalid("只允许普通文件")
	}
	if info.Size() > max {
		_ = f.Close()
		return nil, ErrLimit
	}
	return f, nil
}

func readDir(root *os.Root, name string, max int) ([]os.DirEntry, error) {
	f, err := root.OpenFile(name+"/.", os.O_RDONLY|readFlags, 0)
	if err != nil {
		return nil, invalid("目录越界或无法打开")
	}
	defer f.Close()
	entries, err := f.ReadDir(max + 1)
	if err != nil && err != io.EOF {
		return nil, invalid("无法读取目录")
	}
	if len(entries) > max {
		return nil, ErrLimit
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, nil
}
