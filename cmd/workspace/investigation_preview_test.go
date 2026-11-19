//go:build unix

package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// The startup test below additionally uses a synthetic loopback listener.
func TestInvestigationPreviewOfflineRunner(t *testing.T) {
	for _, mode := range []string{"default-off", "explicit-on", "unsupported-version"} {
		t.Run(mode, func(t *testing.T) {
			version := "1.4.0"
			if mode == "unsupported-version" {
				version = "1.4.1"
			}
			fake := newExecutionFixture(t, version)
			dir := filepath.Join(t.TempDir(), "private")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			args := []string{"--data-dir", dir, "--pi-path", fake.path, "--enable-execution", "--enable-member-tools",
				"--enable-coordination-roles", "--pi-member-tools-extension", memberToolsEntry(t)}
			if mode != "default-off" {
				args = append(args, "--enable-investigation-execution")
			}
			cfg, err := parseConfig(args, io.Discard)
			if err != nil {
				t.Fatalf("unexpected investigation option: %v", err)
			}
			if _, err := parseConfig(append(args, "--enable-codex-app-server", "--enable-investigation-execution"), io.Discard); err == nil {
				t.Fatal("investigation may not share the Codex AppServer startup option")
			}
			db, err := store.Open(filepath.Join(dir, "workspace.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			_, closer, err := artifactReadRoutes(dir, db)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = closer.Close() })
			workspace, err := workspaceRuntimeOptions(dir, closer)
			if err != nil {
				t.Fatal(err)
			}
			other := newExecutionFixture(t, "1.4.0")
			catalog := platform.DiscoverProviders(t.Context(), provider.Search{Explicit: []provider.Location{
				{Name: provider.Pi, Path: fake.path}, {Name: provider.Pi, Path: other.path},
			}}, provider.Runner{Timeout: 2 * time.Second})
			schedule, runtime, preparer, err := executionRunner(cfg, db, coordination.New(db), catalog,
				"http://127.0.0.1:1/api/v1/member-tools", io.Discard, workspace)
			if mode == "unsupported-version" {
				if err == nil || !strings.Contains(err.Error(), "Pi 1.4.0") || runtime != nil || schedule != nil || preparer != nil {
					t.Fatal("a supported alternative Pi bypassed the exact selection")
				}
				return
			}
			if err != nil || schedule == nil || runtime == nil || preparer == nil {
				t.Fatalf("runner did not compose: %v", err)
			}
			t.Cleanup(func() { _ = runtime.Close() })
			_, err = os.Lstat(filepath.Join(dir, "workspaces", "investigation-cwd"))
			if mode == "default-off" && !os.IsNotExist(err) || mode == "explicit-on" && err != nil || runtime.Tools().WorkInputsEnabled() {
				t.Fatal("runner activated an unrequested investigation or work-input profile")
			}
			if _, err := os.Stat(filepath.Join(fake.root, "calls")); !os.IsNotExist(err) {
				t.Fatal("composition ran a model")
			}
		})
	}
}

func TestInvestigationPreviewStartupOption(t *testing.T) {
	const option = "--enable-investigation-execution"
	extension := memberToolsEntry(t)
	base := []string{option, "--enable-execution", "--enable-member-tools", "--enable-coordination-roles", "--pi-member-tools-extension", extension}
	for _, rejected := range []struct {
		extra []string
		want  string
	}{
		{[]string{"--data-dir", t.TempDir()}, "--pi-path"},
		{[]string{"--pi-path", filepath.Join(t.TempDir(), "pi")}, "--data-dir"},
		{[]string{"--pi-path", filepath.Join(t.TempDir(), "pi"), "--data-dir", t.TempDir(), "--enable-codex-app-server"}, "不能与 Codex AppServer"},
	} {
		if _, err := parseConfig(append(base, rejected.extra...), io.Discard); err == nil || !strings.Contains(err.Error(), rejected.want) {
			t.Fatalf("missing investigation config fence %q: %v", rejected.want, err)
		}
	}
	fake := newExecutionFixture(t, "1.4.0")
	dir := filepath.Join(t.TempDir(), "private")
	args := []string{"--listen", "127.0.0.1:0", "--data-dir", dir, "--pi-path", fake.path,
		"--enable-execution", "--enable-member-tools", "--enable-coordination-roles", "--pi-member-tools-extension", extension}
	url, _ := startRunWithSearch(t, provider.Search{}, args...)
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/api/v1/member-tools/read_investigation_channel", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatal("default startup mounted the investigation route")
	}
	var group domain.Group
	coordinationCompositionHTTP(t, "POST", url+"/api/v1/groups", platform.GroupInput{Name: "startup schema gate"}, "create-group", http.StatusCreated, &group)
	var policy domain.ChatPolicy
	coordinationCompositionHTTP(t, "GET", url+"/api/v1/groups/"+group.ID+"/coordination/chat-policy?topic_id=investigation", nil, "", http.StatusOK, &policy)
	if !policy.InvestigationSupported || policy.InvestigationExecutionEnabled || policy.Enabled || policy.Investigation != nil {
		t.Fatal("the v27 schema, disabled runtime and local policy were conflated")
	}
}

func TestInvestigationPreviewStartupOptIn(t *testing.T) {
	fake := newExecutionFixture(t, "1.4.0")
	dir := filepath.Join(t.TempDir(), "private")
	url, _ := startRunWithSearch(t, provider.Search{}, "--listen", "127.0.0.1:0", "--data-dir", dir, "--pi-path", fake.path,
		"--enable-execution", "--enable-member-tools", "--enable-coordination-roles", "--pi-member-tools-extension", memberToolsEntry(t),
		"--enable-investigation-execution")
	var group domain.Group
	coordinationCompositionHTTP(t, "POST", url+"/api/v1/groups", platform.GroupInput{Name: "private preview"}, "create-group", http.StatusCreated, &group)
	var policy domain.ChatPolicy
	coordinationCompositionHTTP(t, "GET", url+"/api/v1/groups/"+group.ID+"/coordination/chat-policy?topic_id=investigation", nil, "", http.StatusOK, &policy)
	if !policy.InvestigationSupported || !policy.InvestigationExecutionEnabled || policy.Enabled || policy.Investigation != nil {
		t.Fatal("startup should enable execution without granting a topic policy")
	}
	for _, route := range []struct {
		path, body string
		status     int
	}{
		{"read_investigation_channel", `{}`, http.StatusForbidden},
		{"read_investigation_artifact", `{"artifact_id":"unissued","version_id":"unissued"}`, http.StatusForbidden},
		{"mcp", `{}`, http.StatusNotFound},
	} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/api/v1/member-tools/"+route.path, strings.NewReader(route.body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer not-issued")
		request.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 3 * time.Second}
		response, err := client.Do(request)
		client.CloseIdleConnections()
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != route.status {
			t.Fatalf("route %s: want status %d, got %d", route.path, route.status, response.StatusCode)
		}
	}
}
