package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/provider"
)

func TestProviderSearchUsesOnlySpecifiedDirectories(t *testing.T) {
	root := t.TempDir()
	dirs := []string{filepath.Join(root, "one"), filepath.Join(root, "two")}
	t.Setenv("PATH", strings.Join(dirs, string(os.PathListSeparator)))
	t.Setenv("HOME", root)
	search := defaultProviderSearch()
	if !reflect.DeepEqual(search.PathDirs, dirs) || len(search.Explicit) != 0 || len(search.CommonDirs) > 4 {
		t.Fatalf("unbounded or non-inherited discovery: %+v", search)
	}
	found := false
	for _, path := range search.CommonDirs {
		if path == filepath.Join(root, ".local", "bin") {
			found = true
		}
		if !filepath.IsAbs(path) || path == root {
			t.Fatalf("common paths must be explicit binary dirs: %s", path)
		}
	}
	if !found {
		t.Fatal("missing conventional local binary directory")
	}
}

func TestRunProviderCatalogUsesExplicitPathsAndStartupSnapshot(t *testing.T) {
	root := t.TempDir()
	calls := filepath.Join(root, "calls")
	for name, version := range map[string]string{provider.Pi: "1.4.0", provider.Codex: "codex-cli 9.9.9"} {
		body := "#!/bin/sh\nprintf 'call\\n' >> '" + calls + "'\ncase \"$*\" in\n--version) printf '%s' '" + version + "';;\n'auth check --provider openai-codex --json --no-refresh') printf '%s' '{\"status\":\"ready\",\"provider\":\"openai-codex\",\"authType\":\"oauth\"}';;\n*) exit 93;;\nesac\n"
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "pi-alias")
	if err := os.Symlink(filepath.Join(root, "pi"), alias); err != nil {
		t.Fatal(err)
	}
	url, _ := startRunWithSearch(t, provider.Search{PathDirs: []string{root}, CommonDirs: []string{root}}, "--listen", "127.0.0.1:0", "--data-dir", filepath.Join(root, "data"), "--pi-path", alias, "--codex-path", filepath.Join(root, "codex"))
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	get := func(path string) []byte {
		t.Helper()
		response, err := client.Get(url + path)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("catalog wiring: %d %v", response.StatusCode, err)
		}
		return body
	}
	initial := get("/api/v1/providers")
	var page struct {
		Items      []provider.Provider `json:"items"`
		NextCursor string              `json:"next_cursor"`
	}
	if err := json.Unmarshal(initial, &page); err != nil || len(page.Items) != 2 || page.NextCursor != "" {
		t.Fatalf("explicit CLI paths did not discover real fixtures: %v", err)
	}
	if page.Items[0].Name != provider.Codex || page.Items[0].Version != "9.9.9" || page.Items[0].Account != provider.AccountUnknown || page.Items[1].Name != provider.Pi || page.Items[1].Version != "1.4.0" || page.Items[1].Path != alias || page.Items[1].Account != provider.Authenticated {
		t.Fatal("provider metadata/independent states were not wired through the main process")
	}
	for range 3 {
		if string(get("/api/v1/providers")) != string(initial) {
			t.Fatal("main process recomputed its snapshot")
		}
	}
	if err := json.Unmarshal(get("/api/v1/providers?limit=1"), &page); err != nil || page.NextCursor == "" {
		t.Fatal("main process did not wire provider paging")
	}
	get("/api/v1/providers?limit=1&before=" + page.NextCursor)
	body, err := os.ReadFile(calls)
	if err != nil || strings.Count(string(body), "call\n") != 3 {
		t.Fatalf("polling or alias discovery triggered extra probes: %v", err)
	}
}

func TestUndiscoveredExplicitProviderDoesNotInventAnInstallation(t *testing.T) {
	root := t.TempDir()
	url, _ := startRun(t, "--listen", "127.0.0.1:0", "--data-dir", filepath.Join(root, "data"), "--pi-path", filepath.Join(root, "missing"))
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Get(url + "/api/v1/providers")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || string(body) != "{\"items\":[],\"next_cursor\":\"\"}\n" {
		t.Fatalf("discovery must remain the source of truth: %d %s %v", response.StatusCode, body, err)
	}
}
