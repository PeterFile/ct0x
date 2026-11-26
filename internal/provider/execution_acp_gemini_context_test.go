package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func acpContextFixture(t *testing.T) (acpExecutor, ExecuteRequest, memberToolConfig, []string) {
	t.Helper()
	e, request, base := acpFixture(t, "gemini-prepared")
	home, workspace := t.TempDir(), t.TempDir()
	request.CWD = filepath.Join(workspace, "child")
	for _, dir := range []string{request.CWD, filepath.Join(workspace, ".git"), filepath.Join(home, ".gemini")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	files := []ACPGeminiContextFile{}
	for _, path := range []string{filepath.Join(home, ".gemini", "GEMINI.md"), filepath.Join(home, ".gemini", "AGENTS.md"), filepath.Join(workspace, "AGENTS.md")} {
		body := []byte("Preserved synthetic instruction. `@./inactive.md`\n")
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(body)
		files = append(files, ACPGeminiContextFile{Path: path, SHA256: hex.EncodeToString(hash[:])})
	}
	if err := os.WriteFile(filepath.Join(home, ".gemini", "settings.json"), []byte(`{"context":{"fileName":"AGENTS.md"},"security":{"auth":{"selectedType":"fixture"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Fixture-only credential sentinel: it must never be opened/copied by preflight.
	if err := os.WriteFile(filepath.Join(home, ".gemini", "oauth_creds.json"), []byte("fixture-auth-must-stay-in-original-home"), 0000); err != nil {
		t.Fatal(err)
	}
	e.config.GeminiContext = &ACPGeminiContextManifest{Version: "0.68.0", CWD: request.CWD, Files: files}
	env := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "AW_ACP_FIXTURE_MODE=gemini-prepared"}
	if _, ok := acpGeminiContextDigest(e.config, request.CWD, env); !ok {
		t.Fatal("valid complete synthetic closure rejected")
	}
	return e, request, base.member, env
}

func TestACPGeminiContextClosureAndIndependentSkillsReachChild(t *testing.T) {
	e, request, member, env := acpContextFixture(t)
	factory, err := NewACPExecutor(e.config, e.timeout)
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the original host manifest cannot change the executor's closure.
	e.config.GeminiContext.Files[0].SHA256 = strings.Repeat("0", 64)
	e = factory.(acpExecutor)
	dir := t.TempDir()
	launch, reason := e.prepareWithEnvironment(context.Background(), request, dir, member, env)
	if reason != "" {
		t.Fatal(reason)
	}
	r := e.execute(context.Background(), request, nil, launch)
	if r.Outcome != OutcomeSucceeded {
		t.Fatalf("preserved instructions blocked MCP-only interaction: %+v", r)
	}
	for _, entry := range mustACPReadDir(t, dir) {
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil || strings.Contains(string(raw), "fixture-auth-must-stay-in-original-home") {
			t.Fatal("original auth copied into private preparation")
		}
	}
	if acpEnv(launch.env, "HOME") != acpEnv(env, "HOME") {
		t.Fatal("credential HOME moved")
	}
}

func mustACPReadDir(t *testing.T, path string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestACPGeminiContextManifestRejectsUnprovenClosure(t *testing.T) {
	for _, change := range []string{"hash", "version", "cwd", "missing", "extra", "ambient", "default-name", "active-import", "invalid-utf8", "bounded", "symlink", "writable", "extra-root", "boundary", "admin", "unicode-name", "system-env"} {
		t.Run(change, func(t *testing.T) {
			e, request, _, env := acpContextFixture(t)
			m := e.config.GeminiContext
			path := m.Files[0].Path
			body := []byte("synthetic changed instruction\n")
			switch change {
			case "hash":
				m.Files[0].SHA256 = strings.Repeat("0", 64)
			case "version":
				m.Version = "0.69.0"
			case "cwd":
				m.CWD = filepath.Dir(request.CWD)
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "extra":
				m.Files = append(m.Files, ACPGeminiContextFile{Path: filepath.Join(t.TempDir(), "unknown.md"), SHA256: m.Files[0].SHA256})
			case "ambient":
				m.Files = m.Files[1:]
			case "default-name":
				if err := os.WriteFile(filepath.Join(request.CWD, "GEMINI.md"), body, 0600); err != nil {
					t.Fatal(err)
				}
			case "active-import", "invalid-utf8", "bounded":
				if change == "active-import" {
					body = []byte("Instruction\n@./unlisted-file.md\n")
				}
				if change == "invalid-utf8" {
					body = []byte{0xff}
				}
				if change == "bounded" {
					body = []byte(strings.Repeat("x", acpContextFileLimit+1))
				}
				if err := os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
				hash := sha256.Sum256(body)
				m.Files[0].SHA256 = hex.EncodeToString(hash[:])
			case "symlink":
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".original", path); err != nil {
					t.Fatal(err)
				}
			case "writable":
				if err := os.Chmod(path, 0660); err != nil {
					t.Fatal(err)
				}
			case "extra-root", "boundary", "admin":
				raw := `{"context":{"includeDirectories":["/untrusted"]}}`
				if change == "boundary" {
					raw = `{"context":{"memoryBoundaryMarkers":[]}}`
				}
				if change == "admin" {
					raw = `{"admin":{"skills":{"enabled":false}}}`
				}
				if err := os.WriteFile(filepath.Join(acpEnv(env, "HOME"), ".gemini", "settings.json"), []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
			case "unicode-name":
				global := filepath.Join(acpEnv(env, "HOME"), ".gemini")
				if err := os.WriteFile(filepath.Join(global, "settings.json"), []byte(`{"context":{"fileName":"\u0085AGENTS.md\u0085"}}`), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(global, "\u0085AGENTS.md\u0085"), []byte("unlisted instruction"), 0600); err != nil {
					t.Fatal(err)
				}
			case "system-env":
				env = append(env, "GEMINI_SYSTEM_MD=/untrusted")
			}
			if _, ok := acpGeminiContextDigest(e.config, request.CWD, env); ok {
				t.Fatal("unproven context/source admitted")
			}
		})
	}
}

func TestACPGeminiInstalledImportAndBacktickRules(t *testing.T) {
	for _, tc := range []struct {
		text   string
		active bool
	}{
		{"@./file", true}, {"\n@/absolute\n", true}, {" @letter/file", true}, {"\t@file", true},
		{"email@example.com", false}, {"(@./not-import)", false}, {"@_not-letter", false}, {"\v@file", false},
		{"`@./inactive`", false}, {"```\n@./inactive\n```", false}, {"``@./inactive``", false},
		{"`@./inactive` @./active", true}, {"`unterminated\n@./active", true},
		{"``broken\n@./active", true}, {"```broken\n@./active", true},
		{strings.Repeat("`", 10000) + "\n@./inactive\n" + strings.Repeat("`", 9999), false},
		{strings.Repeat("`", acpContextFileLimit) + "\n@./active", true},
	} {
		if got := acpGeminiActiveImport(tc.text); got != tc.active {
			t.Errorf("installed import classification: %q got=%v", tc.text, got)
		}
	}
}

func TestACPGeminiProjectMemoryMetadataAndLegacyMigration(t *testing.T) {
	for _, mode := range []string{"unregistered", "registered", "fallback", "legacy", "ownership", "ownership-nel", "registry", "unrelated"} {
		t.Run(mode, func(t *testing.T) {
			e, request, _, env := acpContextFixture(t)
			global := filepath.Join(acpEnv(env, "HOME"), ".gemini")
			if mode != "unregistered" {
				body := acpJSON(map[string]any{"projects": map[string]string{request.CWD: "fixture"}})
				if mode == "registry" {
					body = []byte(`{"projects":42}`)
				}
				if err := os.WriteFile(filepath.Join(global, "projects.json"), body, 0600); err != nil {
					t.Fatal(err)
				}
				for _, base := range []string{"tmp", "history"} {
					dir := filepath.Join(global, base, "fixture")
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
					owner := request.CWD
					if mode == "ownership" {
						owner = filepath.Dir(owner)
					}
					if mode == "ownership-nel" {
						owner = "\u0085" + owner + "\u0085"
					}
					if err := os.WriteFile(filepath.Join(dir, ".project_root"), []byte(owner), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			slug, name := "fixture", "MEMORY.md"
			if mode == "fallback" {
				name = "AGENTS.md"
			}
			if mode == "legacy" {
				hash := sha256.Sum256([]byte(request.CWD))
				slug = hex.EncodeToString(hash[:])
			}
			if mode == "unrelated" {
				slug = "other-project"
			}
			// A directory at the candidate filename proves this is an existence
			// guard: no memory body, native session, or credential is read.
			if mode != "ownership" && mode != "ownership-nel" && mode != "registry" {
				if err := os.MkdirAll(filepath.Join(global, "tmp", slug, "memory", name), 0700); err != nil {
					t.Fatal(err)
				}
			}
			_, ok := acpGeminiContextDigest(e.config, request.CWD, env)
			if ok != (mode == "unrelated") {
				t.Fatalf("project-memory guard mode=%s accepted=%v", mode, ok)
			}
		})
	}
}

func TestACPChildGeminiSourceDriftBeforeAndAfterPrompt(t *testing.T) {
	for _, when := range []string{"before", "after", "memory-after"} {
		t.Run(when, func(t *testing.T) {
			e, request, member, env := acpContextFixture(t)
			launch, reason := e.prepareWithEnvironment(context.Background(), request, t.TempDir(), member, env)
			if reason != "" {
				t.Fatal(reason)
			}
			change := func() {
				if when == "memory-after" {
					path := filepath.Join(acpEnv(env, "HOME"), ".gemini", "tmp", "new-slug", "memory", "MEMORY.md")
					if err := os.MkdirAll(path, 0700); err != nil {
						t.Error(err)
					}
				} else if err := os.WriteFile(e.config.GeminiContext.Files[0].Path, []byte("drift"), 0600); err != nil {
					t.Error(err)
				}
			}
			if when == "before" {
				change()
			}
			r := e.execute(context.Background(), request, func(event Event) error {
				if when != "before" && event.Kind == TerminalEvent {
					change()
				}
				return nil
			}, launch)
			if when == "before" {
				if r.Started || r.Outcome != OutcomeNotStarted {
					t.Fatalf("changed source launched: %+v", r)
				}
			} else if r.Outcome != OutcomeUnknown || r.OutputComplete || !r.DirectChildReaped {
				t.Fatalf("post-run drift settled: %+v", r)
			}
		})
	}
}
