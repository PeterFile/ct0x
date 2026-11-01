package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/provider"
)

func memberToolsEntry(t *testing.T) string {
	t.Helper()
	entry, err := filepath.Abs("../../extensions/pi-bridge/index.ts")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{entry, filepath.Join(filepath.Dir(entry), "bridge.mjs")} {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("trusted bridge artifact is incomplete: %v", err)
		}
	}
	return entry
}

func memberToolsArgs(t *testing.T, dir, cli string) []string {
	t.Helper()
	return []string{"--listen", "127.0.0.1:0", "--data-dir", dir, "--pi-path", cli,
		"--enable-execution", "--enable-member-tools", "--pi-member-tools-extension", memberToolsEntry(t)}
}

func TestMemberToolsConfig(t *testing.T) {
	entry := memberToolsEntry(t)
	defaults, err := parseConfig(nil, io.Discard)
	if err != nil || defaults.enableMemberTools || defaults.piMemberToolsExtension != "" || defaults.memberToolsMaxMembers != 4 {
		t.Fatalf("member tools must default off with capacity four: %+v %v", defaults, err)
	}
	base := []string{"--enable-execution", "--enable-member-tools", "--pi-member-tools-extension", entry}
	for _, capacity := range []string{"1", "4", "200"} {
		got, err := parseConfig(append(append([]string{}, base...), "--member-tools-max-members", capacity), io.Discard)
		wantCapacity, _ := strconv.Atoi(capacity)
		if err != nil || !got.enableMemberTools || got.piMemberToolsExtension != entry || got.memberToolsMaxMembers != wantCapacity {
			t.Fatalf("valid capacity %s rejected: %+v %v", capacity, got, err)
		}
	}
	for _, tt := range []struct{ listen, want string }{
		{"127.0.0.1:0", "127.0.0.1:0"}, {"[::1]:0", "[::1]:0"}, {"[0:0:0:0:0:0:0:1]:1234", "[::1]:1234"},
	} {
		got, err := parseConfig(append(append([]string{}, base...), "--listen", tt.listen), io.Discard)
		if err != nil || got.listen != tt.want {
			t.Fatalf("canonical listener %s: %+v %v", tt.listen, got, err)
		}
	}
	if _, err := parseConfig([]string{"--listen", "127.0.0.2:0"}, io.Discard); err != nil {
		t.Fatalf("disabled listener compatibility changed: %v", err)
	}
	unreadable := filepath.Join(t.TempDir(), "unreadable.ts")
	if err := os.WriteFile(unreadable, nil, 0000); err != nil {
		t.Fatal(err)
	}
	invalid := [][]string{
		{"--enable-member-tools"},
		{"--enable-member-tools", "--pi-member-tools-extension", entry},
		{"--enable-execution", "--enable-member-tools"},
		{"--pi-member-tools-extension", entry},
		{"--member-tools-max-members", "4"},
		{"--enable-member-tools=false", "--member-tools-max-members", "1"},
	}
	for _, path := range []string{"", "relative/index.ts", filepath.Join(t.TempDir(), "missing.ts"), t.TempDir(), unreadable, entry + "\x00"} {
		invalid = append(invalid, []string{"--enable-execution", "--enable-member-tools", "--pi-member-tools-extension", path})
	}
	for _, capacity := range []string{"-1", "0", "201", "not-an-int"} {
		invalid = append(invalid, append(append([]string{}, base...), "--member-tools-max-members", capacity))
	}
	for _, listen := range []string{"127.0.0.2:0", "[::ffff:127.0.0.1]:0"} {
		invalid = append(invalid, append(append([]string{}, base...), "--listen", listen))
	}
	for _, args := range invalid {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "must-not-exist")
			err := runWithSearch(context.Background(), append([]string{"--data-dir", dir}, args...), io.Discard, provider.Search{})
			if err == nil || !chinese(err.Error()) {
				t.Fatalf("inconsistent member tool configuration accepted: %v", err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("invalid flags created runtime state: %v", err)
			}
		})
	}
	var help bytes.Buffer
	if err := runWithSearch(context.Background(), []string{"--help"}, &help, provider.Search{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--enable-member-tools", "--pi-member-tools-extension", "--member-tools-max-members", "默认 4", "1..200", "Pi", "Codex", "平台工具", "令牌"} {
		if !strings.Contains(help.String(), want) {
			t.Errorf("help omits %q", want)
		}
	}
}

func TestMemberToolsDisabledNoRootOrRoutes(t *testing.T) {
	web := t.TempDir()
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<!doctype html><title>fixture</title>"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"--enable-execution"}} {
		dir := t.TempDir()
		url, stop := startRun(t, append([]string{"--listen", "127.0.0.1:0", "--data-dir", dir, "--web-dir", web}, args...)...)
		client := &http.Client{Timeout: 3 * time.Second}
		for _, operation := range []string{"list_members", "read_channel", "send_message", "create_member"} {
			response, err := client.Post(url+"/api/v1/member-tools/"+operation, "application/json", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			// The existing SPA root has GET/HEAD only; unknown POST stays JSON 405.
			if response.StatusCode != http.StatusMethodNotAllowed || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") || !chinese(string(body)) {
				t.Fatalf("disabled private route leaked into SPA: %d %s", response.StatusCode, body)
			}
		}
		client.CloseIdleConnections()
		if err := stop(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(dir, "member-tools")); !os.IsNotExist(err) {
			t.Fatalf("disabled tools created a credential root: %v", err)
		}
	}
}

func TestMemberToolsStartupRejectsUnsafeRoot(t *testing.T) {
	f := newExecutionFixture(t, "1.4.0")
	for _, kind := range []string{"symlink", "file", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			root := filepath.Join(dir, "member-tools")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(t.TempDir(), root)
			case "file":
				err = os.WriteFile(root, []byte("unchanged"), 0600)
			case "permissions":
				err = os.Mkdir(root, 0750)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(root)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var output bytes.Buffer
			err = runWithSearch(ctx, memberToolsArgs(t, dir, f.path), &output, provider.Search{})
			if err == nil || errors.Is(err, context.DeadlineExceeded) || !chinese(err.Error()) || output.Len() != 0 {
				t.Fatalf("unsafe tool root reached startup: %v", err)
			}
			after, err := os.Lstat(root)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatal("startup changed an unsafe existing entry")
			}
			lock, err := lockDataDir(dir)
			if err != nil {
				t.Fatalf("startup failure retained ownership: %v", err)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
