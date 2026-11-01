package skills

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

var ErrChanged = errors.New("技能正文版本已变更，请重新列出并选择")

type File struct {
	Path, SHA256 string
	Size         int64
}

// Snapshot owns immutable copies of the selected bytes, not live source handles.
// Capture is bounded but not an atomic filesystem transaction: concurrent edits
// can yield an observed mixture of resource versions. Version hashes exactly
// those captured names/bytes. No script is executed and no permission is granted.
type Snapshot struct {
	entry    Entry
	body     string
	version  string
	files    map[string]string
	manifest []File
}

// Select revalidates SKILL.md against its listed full-file hash. Supporting files
// are first captured here, so resource changes after List are reflected in the
// snapshot version. Relative file symlinks must stay inside this selected skill;
// directory symlinks are rejected. Limits include SKILL.md and the root directory.
func (c *Catalog) Select(id Identity) (*Snapshot, error) {
	hash, err := hex.DecodeString(id.SkillSHA256)
	if len(id.Name) > 64 || !skillName.MatchString(id.Name) || err != nil || len(hash) != sha256.Size || strings.ToLower(id.SkillSHA256) != id.SkillSHA256 {
		return nil, invalid("选择身份、名称或完整文件哈希无效")
	}
	var source *openedRoot
	for n := range c.roots {
		if c.roots[n].ID == id.RootID {
			source = &c.roots[n]
			break
		}
	}
	if source == nil {
		return nil, invalid("未授权的技能根名称")
	}
	root, err := source.handle.OpenRoot(id.Name + "/.")
	if err != nil {
		return nil, invalid("所选技能目录越界或已不可用")
	}
	defer root.Close()
	s := &Snapshot{files: make(map[string]string)}
	remaining := c.limits.MaxTotalBytes
	capture := func(name string) error {
		if !safeResourcePath(name) {
			return invalid("资源路径必须是安全的相对路径")
		}
		if len(s.files) >= c.limits.MaxFiles {
			return ErrLimit
		}
		data, err := copyFile(root, name, min(c.limits.MaxFileBytes, remaining))
		if err != nil {
			return err
		}
		s.files[name] = data
		remaining -= int64(len(data))
		return nil
	}
	if err := capture("SKILL.md"); err != nil {
		return nil, err
	}
	raw := s.files["SKILL.md"]
	if contentHash(raw) != id.SkillSHA256 {
		return nil, ErrChanged
	}
	header, offset, err := splitHeader([]byte(raw), c.limits.MaxHeaderBytes)
	if err != nil {
		return nil, err
	}
	metadata, err := parseHeader(header, id.Name)
	if err != nil {
		return nil, err
	}
	s.entry = Entry{ID: id, SourcePath: filepath.Join(source.Path, id.Name, "SKILL.md"), Metadata: metadata}
	s.body = raw[offset:]
	directories, count := []string{"."}, 1
	for len(directories) > 0 {
		dir := directories[len(directories)-1]
		directories = directories[:len(directories)-1]
		entries, err := readDir(root, dir, c.limits.MaxEntries)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := path.Join(dir, entry.Name())
			if name == "SKILL.md" {
				continue
			}
			info, err := root.Stat(name)
			if err != nil {
				return nil, invalid("资源越界、链接循环或无法读取")
			}
			if info.IsDir() {
				if entry.Type()&os.ModeSymlink != 0 {
					return nil, invalid("不允许目录符号链接")
				}
				count++
				if count > c.limits.MaxDirs {
					return nil, ErrLimit
				}
				directories = append(directories, name)
			} else if !info.Mode().IsRegular() {
				return nil, invalid("资源必须是普通文件")
			} else if err := capture(name); err != nil {
				return nil, err
			}
		}
	}
	s.seal()
	return s, nil
}

func copyFile(root *os.Root, name string, max int64) (string, error) {
	f, err := openRegular(root, name, max)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return "", invalid("无法复制资源内容")
	}
	if int64(len(data)) > max {
		return "", ErrLimit
	}
	return string(data), nil
}

func (s *Snapshot) seal() {
	names := make([]string, 0, len(s.files))
	for name := range s.files {
		names = append(names, name)
	}
	slices.Sort(names)
	hash := sha256.New()
	_, _ = io.WriteString(hash, "agentworkspace/skills/snapshot/v1\x00")
	var size [8]byte
	for _, name := range names {
		data := s.files[name]
		// Length framing makes names and arbitrary binary content unambiguous.
		for _, part := range []string{name, data} {
			binary.BigEndian.PutUint64(size[:], uint64(len(part)))
			_, _ = hash.Write(size[:])
			_, _ = io.WriteString(hash, part)
		}
		s.manifest = append(s.manifest, File{Path: name, SHA256: contentHash(data), Size: int64(len(data))})
	}
	s.version = hex.EncodeToString(hash.Sum(nil))
}

func contentHash(data string) string {
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])
}
func safeResourcePath(name string) bool {
	return name != "." && fs.ValidPath(name) && !strings.ContainsAny(name, "\\:\x00")
}

func (s *Snapshot) Entry() Entry {
	e := s.entry
	e.Metadata.Extra = maps.Clone(e.Metadata.Extra)
	return e
}
func (s *Snapshot) Body() string    { return s.body }
func (s *Snapshot) Version() string { return s.version }
func (s *Snapshot) Files() []File   { return append([]File{}, s.manifest...) }

// ReadResource uses slash-separated relative names, including SKILL.md. Returned
// bytes are defensive copies. It never reads the original filesystem.
func (s *Snapshot) ReadResource(name string) ([]byte, error) {
	if !safeResourcePath(name) {
		return nil, invalid("资源路径必须是安全的相对路径")
	}
	data, ok := s.files[name]
	if !ok {
		return nil, invalid("快照中不存在该资源")
	}
	return []byte(data), nil
}
