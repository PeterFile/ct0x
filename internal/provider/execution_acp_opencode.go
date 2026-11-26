package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"agentworkspace/internal/preflight"
)

// Audited opencode-darwin-x64-baseline@1.19.6 package/bin/opencode. The
// compiled snapshot binds big-pickle to the OpenCode inference API and the
// bundled @ai-sdk/openai-compatible loader. A declared version cannot attest
// that snapshot; other builds need their own source proof before admission.
const (
	acpOpenCodeImageSize   = 195440480
	acpOpenCodeImageSHA256 = "7bbfae1858ea68bd3a6b9331c5b601e22f1a5a69be6324e6845bae276fe0ad1f"
)

func acpOpenCodeExecutableSHA256(real string, pinned os.FileInfo) string {
	return acpOpenCodeExecutableSHA256Context(context.Background(), real, pinned)
}

func acpOpenCodeExecutableSHA256Context(ctx context.Context, real string, pinned os.FileInfo) string {
	if pinned == nil || !pinned.Mode().IsRegular() || pinned.Size() <= 0 || pinned.Size() > acpOpenCodeImageSize || !acpTrustedPathContext(ctx, real, false) {
		return ""
	}
	file, err := os.Open(real)
	if err != nil {
		return ""
	}
	opened, err := file.Stat()
	if err != nil || !acpStableInfo(pinned, opened) {
		_ = file.Close()
		return ""
	}
	hash := sha256.New()
	n, readErr := io.Copy(hash, io.LimitReader(preflight.Reader(ctx, file), pinned.Size()+1))
	if ctx.Err() != nil {
		_ = file.Close()
		return ""
	}
	finished, finishErr := file.Stat()
	closeErr := file.Close()
	after, statErr := os.Lstat(real)
	if ctx.Err() != nil || n != pinned.Size() || readErr != nil || finishErr != nil || closeErr != nil || statErr != nil || !acpStableInfo(pinned, finished) || !acpStableInfo(pinned, after) {
		return ""
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func acpOpenCodeModelsPath(cacheHome string) string {
	return filepath.Join(cacheHome, "opencode", "models.json")
}

// v1.19.6 uses disk (explicit OPENCODE_MODELS_PATH or the XDG cache) before
// its compiled snapshot even with fetch disabled. A private absent path
// selects the audited snapshot and skips the default cache removal branch.
func acpOpenCodePrivateCache(dir string) (string, func() bool) {
	if !acpTrustedPath(dir, true) {
		return "", nil
	}
	home := filepath.Join(dir, "opencode-cache")
	catalog := acpOpenCodeModelsPath(home)
	cache := filepath.Dir(catalog)
	if os.Mkdir(home, 0700) != nil || os.Mkdir(cache, 0700) != nil {
		return "", nil
	}
	homeInfo, err := os.Lstat(home)
	cacheInfo, cacheErr := os.Lstat(cache)
	if err != nil || cacheErr != nil {
		return "", nil
	}
	samePrivateDir := func(path string, before os.FileInfo) bool {
		info, err := os.Lstat(path)
		return err == nil && info.Mode() == os.ModeDir|0700 && os.SameFile(before, info)
	}
	verify := func() bool {
		// Native bootstrap creates cache/bin; only the catalog must stay absent.
		return acpTrustedPath(cache, true) && samePrivateDir(home, homeInfo) && samePrivateDir(cache, cacheInfo) && acpAbsent(catalog)
	}
	if !verify() {
		return "", nil
	}
	return home, verify
}

func acpOpenCodePreflight(cwd string, env []string) bool {
	return acpOpenCodePreflightContext(context.Background(), cwd, env)
}

func acpOpenCodePreflightContext(ctx context.Context, cwd string, env []string) bool {
	if !preflight.Check(ctx, "opencode_prepare_sources", func() bool {
		if !acpOpenCodeSources(ctx, env) || !acpTrustedPathContext(ctx, cwd, true) {
			return false
		}
		info, err := os.Lstat(cwd)
		if err != nil || info.Mode().Perm()&0077 != 0 || ctx.Err() != nil {
			return false
		}
		files, err := os.ReadDir(cwd)
		return err == nil && len(files) == 0
	}) {
		return false
	}
	return preflight.Check(ctx, "opencode_prepare_account_check", func() bool {
		_, ok := acpOpenCodeAccountSource(ctx, env)
		return ok
	})
}

// Authentication/config sources remain with the CLI. Metadata absence checks
// never read auth.json, account tokens, or native session records.
func acpOpenCodeSources(ctx context.Context, env []string) bool {
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "OPENCODE_") || key == "NODE_OPTIONS" || key == "BUN_OPTIONS" || key == "BUN_INSPECT" {
			return false
		}
	}
	home := acpEnv(env, "HOME")
	if !filepath.IsAbs(home) || !acpTrustedPathContext(ctx, home, true) || !acpAbsent(filepath.Join(home, ".opencode")) {
		return false
	}
	data := acpOpenCodeData(env)
	if data == "" || !acpTrustedParent(ctx, data) || !acpAbsent(filepath.Join(data, "auth.json")) || ctx.Err() != nil {
		return false
	}
	system := "/etc/opencode"
	if runtime.GOOS == "darwin" {
		system = "/Library/Application Support/opencode"
		account, err := user.Current()
		if err != nil || account.Username == "" {
			return false
		}
		for _, path := range []string{filepath.Join("/Library/Managed Preferences", account.Username, "ai.opencode.managed.plist"), "/Library/Managed Preferences/ai.opencode.managed.plist"} {
			if ctx.Err() != nil || !acpAbsent(path) {
				return false
			}
		}
	} else if runtime.GOOS != "linux" {
		return false
	}
	return ctx.Err() == nil && acpAbsent(system)
}

func acpReadProfileJSON(ctx context.Context, path string) ([]byte, map[string]json.RawMessage, error) {
	raw, _, stable := acpReadStableContext(ctx, path, acpFrameLimit)
	if !stable || !acpStrictJSON(raw) {
		return nil, nil, ErrAdapterConfiguration
	}
	fields, ok := acpObject(raw)
	if !ok {
		return nil, nil, ErrAdapterConfiguration
	}
	return raw, fields, nil
}

func acpOnlyPinnedPlugin(fields map[string]json.RawMessage) bool {
	for _, key := range []string{"devDependencies", "peerDependencies", "optionalDependencies"} {
		if raw := fields[key]; raw != nil {
			entries, ok := acpObject(raw)
			if !ok || len(entries) != 0 {
				return false
			}
		}
	}
	dependencies, ok := acpObject(fields["dependencies"])
	return ok && len(dependencies) == 1 && string(dependencies["@opencode-ai/plugin"]) == `"1.19.6"`
}

// Freeze a source-trusted materialized dependency tree, not just a boolean that
// node_modules exists. The caller supplies its trusted lock hash; constructor
// freezes the tree digest and each Execute rechecks it before and after Wait.
// This detects drift, not hostile concurrent writers/ABA or a forged provenance.
func acpOpenCodeDependencies(profile ACPOpenCodeProfile) ([32]byte, error) {
	return acpOpenCodeDependenciesContext(context.Background(), profile)
}

func acpOpenCodeDependenciesContext(ctx context.Context, profile ACPOpenCodeProfile) (digest [32]byte, err error) {
	// Preserve sanitized pin failures, but surface cancellation from any subcheck.
	defer func() {
		if canceled := ctx.Err(); canceled != nil {
			digest, err = [32]byte{}, canceled
		}
	}()
	var empty [32]byte
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	dir := filepath.Clean(profile.ConfigDir)
	if !filepath.IsAbs(dir) || filepath.Base(dir) != "opencode" || len(profile.LockSHA256) != 64 || strings.ContainsRune(dir, 0) {
		return empty, ErrAdapterConfiguration
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 || !acpTrustedPathContext(ctx, dir, true) {
		return empty, ErrAdapterConfiguration
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return empty, ErrAdapterConfiguration
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return empty, ErrAdapterConfiguration
	}
	hasIgnore := false
	var ignoreRaw []byte
	for _, entry := range entries {
		switch entry.Name() {
		case "package.json", "package-lock.json", "node_modules":
		case ".gitignore":
			raw, _, stable := acpReadStableContext(ctx, filepath.Join(dir, entry.Name()), 128)
			if !stable || string(raw) != "node_modules\npackage.json\npackage-lock.json\nbun.lock\n.gitignore" {
				return empty, ErrAdapterConfiguration
			}
			hasIgnore, ignoreRaw = true, raw
		default:
			return empty, ErrAdapterConfiguration
		}
	}
	if !hasIgnore {
		return empty, ErrAdapterConfiguration
	}
	packageRaw, manifest, err := acpReadProfileJSON(ctx, filepath.Join(dir, "package.json"))
	if err != nil || !acpOnlyPinnedPlugin(manifest) || len(manifest) != 2 || string(manifest["private"]) != "true" {
		return empty, ErrAdapterConfiguration
	}
	lockRaw, lock, err := acpReadProfileJSON(ctx, filepath.Join(dir, "package-lock.json"))
	lockHash := sha256.Sum256(lockRaw)
	if err != nil || hex.EncodeToString(lockHash[:]) != profile.LockSHA256 {
		return empty, ErrAdapterConfiguration
	}
	packages, ok := acpObject(lock["packages"])
	root, rootOK := acpObject(packages[""])
	plugin, pluginOK := acpObject(packages["node_modules/@opencode-ai/plugin"])
	if !ok || !rootOK || !pluginOK || !acpOnlyPinnedPlugin(root) || string(plugin["version"]) != `"1.19.6"` {
		return empty, ErrAdapterConfiguration
	}
	_, installed, err := acpReadProfileJSON(ctx, filepath.Join(dir, "node_modules", "@opencode-ai", "plugin", "package.json"))
	if err != nil || string(installed["version"]) != `"1.19.6"` {
		return empty, ErrAdapterConfiguration
	}
	modules := filepath.Join(dir, "node_modules")
	info, err = os.Lstat(modules)
	if err != nil || !info.IsDir() || ctx.Err() != nil {
		return empty, ErrAdapterConfiguration
	}
	realModules, err := filepath.EvalSymlinks(modules)
	if err != nil {
		return empty, ErrAdapterConfiguration
	}
	hash := sha256.New()
	_, _ = hash.Write(ignoreRaw)
	_, _ = hash.Write(packageRaw)
	_, _ = hash.Write(lockRaw)
	count, total := 0, int64(0)
	walk := func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if walkErr != nil || count > 10000 {
			return ErrAdapterConfiguration
		}
		rel, err := filepath.Rel(modules, path)
		if err != nil {
			return ErrAdapterConfiguration
		}
		info, err := entry.Info()
		if err != nil {
			return ErrAdapterConfiguration
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(hash, "%d:%s:%s:%d:", len(rel), rel, info.Mode(), info.Size())
		if entry.Type()&os.ModeSymlink != 0 {
			// npm bin links may point only to regular files inside this tree.
			if filepath.Dir(path) != filepath.Join(modules, ".bin") {
				return ErrAdapterConfiguration
			}
			target, err := filepath.EvalSymlinks(path)
			if err != nil || !strings.HasPrefix(target, realModules+string(filepath.Separator)) || ctx.Err() != nil {
				return ErrAdapterConfiguration
			}
			targetInfo, err := os.Stat(target)
			if err != nil || !targetInfo.Mode().IsRegular() || ctx.Err() != nil {
				return ErrAdapterConfiguration
			}
			link, err := os.Readlink(path)
			if err != nil {
				return ErrAdapterConfiguration
			}
			_, _ = hash.Write([]byte(link))
			return nil
		}
		owner, owned := info.Sys().(*syscall.Stat_t)
		if !owned || owner.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
			return ErrAdapterConfiguration
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() > 32<<20 {
			return ErrAdapterConfiguration
		}
		total += info.Size()
		if total > 128<<20 {
			return ErrAdapterConfiguration
		}
		file, err := os.Open(path)
		if err != nil {
			return ErrAdapterConfiguration
		}
		opened, statErr := file.Stat()
		n, readErr := io.Copy(hash, io.LimitReader(preflight.Reader(ctx, file), info.Size()+1))
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return err
		}
		finished, finishErr := file.Stat()
		closeErr := file.Close()
		after, afterErr := os.Lstat(path)
		if readErr != nil || closeErr != nil || statErr != nil || finishErr != nil || afterErr != nil || n != info.Size() || !acpStableInfo(info, opened) || !acpStableInfo(info, finished) || !acpStableInfo(info, after) {
			return ErrAdapterConfiguration
		}
		return nil
	}
	if err := preflight.Run(ctx, "opencode_dependency_walk", func() error {
		return filepath.WalkDir(modules, walk)
	}); err != nil {
		return empty, ErrAdapterConfiguration
	}
	copy(empty[:], hash.Sum(nil))
	return empty, nil
}
