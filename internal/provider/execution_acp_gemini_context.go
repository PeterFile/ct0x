package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"agentworkspace/internal/preflight"
)

const acpContextFileLimit = 256 << 10

// These checks assume trusted, stable ancestors, not hostile concurrent writers.
// Never follow an instruction/config/metadata symlink or accept another owner.
func acpTrustedPath(path string, directory bool) bool {
	return acpTrustedPathContext(context.Background(), path, directory)
}

func acpTrustedPathContext(ctx context.Context, path string, directory bool) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
		return false
	}
	for current := path; ; current = filepath.Dir(current) {
		if ctx.Err() != nil {
			return false
		}
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) && (current == path || stat.Uid != 0) {
			return false
		}
		if current == path {
			if directory != info.IsDir() || !directory && !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
				return false
			}
		} else {
			// External-volume roots can be owner-controlled and group-writable.
			// Trust that mount boundary, not arbitrary writable path components.
			mount := false
			if parent, err := os.Lstat(filepath.Dir(current)); err == nil {
				if parentStat, ok := parent.Sys().(*syscall.Stat_t); ok {
					mount = parentStat.Dev != stat.Dev && stat.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0002 == 0
				}
			}
			if !info.IsDir() || info.Mode().Perm()&0022 != 0 && !mount && !(stat.Uid == 0 && info.Mode()&os.ModeSticky != 0) {
				return false
			}
		}
		if current == filepath.Dir(current) {
			return ctx.Err() == nil
		}
	}
}

func acpStableInfo(a, b os.FileInfo) bool {
	if a == nil || b == nil || !os.SameFile(a, b) || a.Mode() != b.Mode() || a.Size() != b.Size() || !a.ModTime().Equal(b.ModTime()) {
		return false
	}
	left, leftOK := a.Sys().(*syscall.Stat_t)
	right, rightOK := b.Sys().(*syscall.Stat_t)
	return leftOK && rightOK && left.Uid == right.Uid && left.Gid == right.Gid
}

func acpReadStable(path string, limit int64) ([]byte, os.FileInfo, bool) {
	return acpReadStableContext(context.Background(), path, limit)
}

func acpReadStableContext(ctx context.Context, path string, limit int64) ([]byte, os.FileInfo, bool) {
	if !acpTrustedPathContext(ctx, path, false) {
		return nil, nil, false
	}
	before, err := os.Lstat(path)
	if err != nil || before.Size() < 0 || before.Size() > limit || ctx.Err() != nil {
		return nil, nil, false
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, false
	}
	opened, statErr := file.Stat()
	raw, readErr := io.ReadAll(io.LimitReader(preflight.Reader(ctx, file), limit+1))
	if ctx.Err() != nil {
		_ = file.Close()
		return nil, nil, false
	}
	finished, finishErr := file.Stat()
	closeErr := file.Close()
	after, afterErr := os.Lstat(path)
	ok := ctx.Err() == nil && statErr == nil && readErr == nil && finishErr == nil && closeErr == nil && afterErr == nil &&
		int64(len(raw)) == before.Size() && acpStableInfo(before, opened) && acpStableInfo(before, finished) && acpStableInfo(before, after)
	return raw, before, ok
}

// Exact installed 0.68 findImports + /(`+)([\s\S]*?)\1/g semantics.
// No imports are admitted in this first manifest profile, even a missing target.
func acpGeminiActiveImport(text string) bool {
	// Suffix maxima implement greedy opener backtracking without quadratic
	// allocation on a bounded file containing a very long backtick run.
	type ticks struct{ start, size, maximum int }
	var runs []ticks
	for i := 0; i < len(text); {
		if text[i] != '`' {
			i++
			continue
		}
		start := i
		for i < len(text) && text[i] == '`' {
			i++
		}
		runs = append(runs, ticks{start: start, size: i - start})
	}
	for i := len(runs) - 1; i >= 0; i-- {
		runs[i].maximum = runs[i].size
		if i+1 < len(runs) {
			runs[i].maximum = max(runs[i].maximum, runs[i+1].maximum)
		}
	}
	var code [][2]int
	cursor := 0
	for i := 0; i < len(text); {
		next := strings.IndexByte(text[i:], '`')
		if next < 0 {
			break
		}
		i += next
		for cursor < len(runs) && runs[cursor].start+runs[cursor].size <= i {
			cursor++
		}
		run, later := runs[cursor].start+runs[cursor].size-i, 0
		if cursor+1 < len(runs) {
			later = runs[cursor+1].maximum
		}
		width := max(run/2, min(run, later))
		if width == 0 {
			i++
			continue
		}
		end := i + 2*width + strings.Index(text[i+width:], strings.Repeat("`", width))
		code = append(code, [2]int{i, end})
		i = end
	}
	region := 0
	space := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
	for i := 0; i < len(text); {
		next := strings.IndexByte(text[i:], '@')
		if next < 0 {
			break
		}
		i += next
		if i > 0 && !space(text[i-1]) {
			i++
			continue
		}
		j := i + 1
		for j < len(text) && !space(text[j]) {
			j++
		}
		if j > i+1 {
			c := text[i+1]
			if c == '.' || c == '/' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
				for region < len(code) && code[region][1] <= i {
					region++
				}
				if region == len(code) || i < code[region][0] {
					return true
				}
			}
		}
		i = j + 1
	}
	return false
}

// ECMAScript trim differs from Go TrimSpace (notably NEL and BOM). The
// installed loader uses JS trim for filenames and slug ownership metadata.
func acpGeminiTrim(text string) string {
	return strings.Trim(text, "\t\v\f \n\r\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff")
}

func acpGeminiContextNames(settings map[string]json.RawMessage) ([]string, bool) {
	names := []string{"GEMINI.md"}
	if settings["context"] == nil {
		return names, true
	}
	context, ok := acpObject(settings["context"])
	if !ok {
		return nil, false
	}
	for key, raw := range context {
		switch key {
		case "fileName", "includeDirectoryTree", "fileFiltering", "discoveryMaxDirs", "importFormat":
		case "includeDirectories":
			var dirs []string
			if json.Unmarshal(raw, &dirs) != nil || len(dirs) != 0 {
				return nil, false
			}
		case "loadMemoryFromIncludeDirectories":
			if string(raw) != "false" {
				return nil, false
			}
		case "memoryBoundaryMarkers":
			var markers []string
			if json.Unmarshal(raw, &markers) != nil || !slices.Equal(markers, []string{".git"}) {
				return nil, false
			}
		default:
			return nil, false
		}
	}
	if raw := context["fileName"]; raw != nil {
		var extra []string
		var single string
		if json.Unmarshal(raw, &single) == nil {
			extra = []string{single}
		} else if json.Unmarshal(raw, &extra) != nil {
			return nil, false
		}
		if len(extra) > 8 {
			return nil, false
		}
		for _, name := range extra {
			name = acpGeminiTrim(name)
			if name == "" {
				continue
			}
			if len(name) > 128 || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00\r\n") || !utf8.ValidString(name) {
				return nil, false
			}
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names, true
}

func acpGeminiHome(env []string) string {
	if home := acpEnv(env, "GEMINI_CLI_HOME"); home != "" {
		return home
	}
	return acpEnv(env, "HOME")
}

// Reads only bounded noncredential registry/ownership metadata. A registered
// workspace guards its slug plus legacy migration hash; before registration,
// all possible tmp slugs must be memory-free since the CLI chooses the slug.
func acpGeminiMemoryAbsent(home, cwd string, names []string) bool {
	global := filepath.Join(home, ".gemini")
	registry := filepath.Join(global, "projects.json")
	slug := ""
	if !acpAbsent(registry) {
		raw, _, ok := acpReadStable(registry, acpFrameLimit)
		fields, object := acpObject(raw)
		var projects map[string]string
		if !ok || !acpStrictJSON(raw) || !object || len(fields) != 1 || json.Unmarshal(fields["projects"], &projects) != nil || projects == nil || len(projects) > 1024 {
			return false
		}
		for path, id := range projects {
			if !filepath.IsAbs(path) || filepath.Clean(path) != path || !acpGeminiSlug(id) {
				return false
			}
		}
		slug = projects[cwd]
	}
	hash := sha256.Sum256([]byte(cwd))
	targets := []string{hex.EncodeToString(hash[:])}
	if slug != "" {
		targets = append(targets, slug)
		for _, base := range []string{"tmp", "history"} {
			parent := filepath.Join(global, base)
			if !acpAbsent(parent) && !acpTrustedPath(parent, true) {
				return false
			}
			dir := filepath.Join(parent, slug)
			if !acpAbsent(dir) && !acpTrustedPath(dir, true) {
				return false
			}
			marker := filepath.Join(dir, ".project_root")
			if !acpAbsent(marker) {
				body, _, ok := acpReadStable(marker, 4096)
				if !ok || !utf8.Valid(body) || acpGeminiTrim(string(body)) != cwd {
					return false
				}
			}
		}
	} else {
		tmp := filepath.Join(global, "tmp")
		if !acpAbsent(tmp) {
			if !acpTrustedPath(tmp, true) {
				return false
			}
			entries, err := os.ReadDir(tmp)
			if err != nil || len(entries) > 1024 {
				return false
			}
			for _, entry := range entries {
				if !acpGeminiSlug(entry.Name()) || !acpTrustedPath(filepath.Join(tmp, entry.Name()), true) {
					return false
				}
				targets = append(targets, entry.Name())
			}
		}
	}
	for _, id := range targets {
		dir := filepath.Join(global, "tmp", id)
		if !acpAbsent(dir) && !acpTrustedPath(dir, true) {
			return false
		}
		memory := filepath.Join(dir, "memory")
		if !acpAbsent(memory) && !acpTrustedPath(memory, true) {
			return false
		}
		for _, name := range append([]string{"MEMORY.md"}, names...) {
			if !acpAbsent(filepath.Join(memory, name)) {
				return false
			}
		}
	}
	return true
}

func acpGeminiSlug(id string) bool {
	if id == "" || len(id) > 192 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return id != "-"
}

func acpGeminiContextDigest(c ACPConfig, cwd string, env []string) ([32]byte, bool) {
	var empty [32]byte
	if !acpGeminiPreflight(cwd, env) || !acpTrustedPath(cwd, true) {
		return empty, false
	}
	home := acpGeminiHome(env)
	settings := map[string]json.RawMessage{}
	hash := sha256.New()
	settingsPath := filepath.Join(home, ".gemini", "settings.json")
	if !acpAbsent(settingsPath) {
		raw, info, ok := acpReadStable(settingsPath, acpFrameLimit)
		if !ok {
			return empty, false
		}
		settings, ok = acpObject(raw)
		if !ok {
			return empty, false
		}
		_, _ = hash.Write(raw)
		stat := info.Sys().(*syscall.Stat_t)
		_, _ = fmt.Fprintf(hash, "%s:%v:%d:%v:%d:%d", settingsPath, info.ModTime(), info.Size(), info.Mode(), stat.Dev, stat.Ino)
	}
	names, ok := acpGeminiContextNames(settings)
	if !ok || !acpGeminiMemoryAbsent(home, cwd, names) {
		return empty, false
	}
	manifest := c.GeminiContext
	expected := map[string]string{}
	if manifest != nil {
		if manifest.Version != c.Version || manifest.Version != "0.68.0" || manifest.CWD != cwd || len(manifest.Files) > 64 {
			return empty, false
		}
		for _, file := range manifest.Files {
			decoded, err := hex.DecodeString(file.SHA256)
			if !filepath.IsAbs(file.Path) || filepath.Clean(file.Path) != file.Path || expected[file.Path] != "" || err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != file.SHA256 {
				return empty, false
			}
			expected[file.Path] = file.SHA256
		}
	}
	// Installed discovery stops at the first .git ancestor, or at CWD if none.
	ceiling := cwd
	for dir := cwd; ; dir = filepath.Dir(dir) {
		marker := filepath.Join(dir, ".git")
		if !acpAbsent(marker) {
			info, err := os.Lstat(marker)
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				return empty, false
			}
			ceiling = dir
			break
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	_, _ = fmt.Fprintf(hash, "%s:%s:%v", cwd, ceiling, names)
	candidates := []string{}
	for _, name := range names {
		candidates = append(candidates, filepath.Join(home, ".gemini", name))
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if dir == filepath.Join(home, ".gemini") {
			break
		}
		for _, name := range names {
			candidates = append(candidates, filepath.Join(dir, name))
		}
		if dir == ceiling || dir == filepath.Dir(dir) {
			break
		}
	}
	seen := map[string]bool{}
	total := 0
	for _, path := range candidates {
		if seen[path] {
			continue
		}
		seen[path] = true
		if acpAbsent(path) {
			continue
		}
		pinned, listed := expected[path]
		if !listed {
			return empty, false
		}
		body, info, ok := acpReadStable(path, acpContextFileLimit)
		total += len(body)
		digest := sha256.Sum256(body)
		if !ok || total > 1<<20 || !utf8.Valid(body) || strings.ContainsRune(string(body), 0) || hex.EncodeToString(digest[:]) != pinned || acpGeminiActiveImport(string(body)) {
			return empty, false
		}
		stat := info.Sys().(*syscall.Stat_t)
		_, _ = fmt.Fprintf(hash, "%s:%x:%v:%v:%d:%d", path, digest, info.Mode(), info.ModTime(), stat.Dev, stat.Ino)
		delete(expected, path)
	}
	if len(expected) != 0 {
		return empty, false
	}
	copy(empty[:], hash.Sum(nil))
	return empty, true
}
