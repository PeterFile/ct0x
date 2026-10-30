package platform

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/provider"
)

func catalogExecutable(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProviderSnapshotIsImmutableAfterStartupContextEnds(t *testing.T) {
	root := t.TempDir()
	calls := filepath.Join(root, "calls")
	path := catalogExecutable(t, filepath.Join(root, "pi"), "printf 'call\\n' >> '"+calls+"'\ncase \"$*\" in\n--version) printf 1.4.0;;\n'auth check --provider openai-codex --json --no-refresh') printf '%s' '{\"status\":\"ready\",\"provider\":\"openai-codex\",\"authType\":\"oauth\"}';;\n*) exit 93;;\nesac")
	ctx, cancel := context.WithCancel(context.Background())
	catalog := DiscoverProviders(ctx, provider.Search{Explicit: []provider.Location{{Name: provider.Pi, Path: path}}}, provider.Runner{Timeout: time.Second, OutputLimit: 4096})
	cancel()
	items, next, err := catalog.List(0, "")
	if err != nil || len(items) != 1 || next != "" || items[0].Account != provider.Authenticated || len(items[0].Evidence) != 3 {
		t.Fatalf("startup snapshot missing confirmed independent states: %+v %q %v", items, next, err)
	}
	items[0].Name, items[0].Evidence[0].Reason = "mutated", "mutated"
	items[0].Evidence = append(items[0].Evidence, provider.Evidence{Reason: "mutated"})
	again, _, err := catalog.List(1, "")
	if err != nil || again[0].Name != provider.Pi || len(again[0].Evidence) != 3 || again[0].Evidence[0].Reason == "mutated" {
		t.Fatal("a caller mutated the shared snapshot")
	}
	body, err := os.ReadFile(calls)
	if err != nil || strings.Count(string(body), "call\n") != 2 {
		t.Fatalf("snapshot reads invoked a subprocess: %v", err)
	}
	for _, limit := range []int{-1, 201} {
		if _, _, err := catalog.List(limit, ""); err == nil {
			t.Fatalf("invalid limit accepted: %d", limit)
		}
	}
}

func TestProviderSharedBudgetPreservesEveryDiscoveredCandidate(t *testing.T) {
	root := t.TempDir()
	slow := catalogExecutable(t, filepath.Join(root, "slow"), "exec /bin/sleep 5")
	marker := filepath.Join(root, "unexpected-probe")
	later := catalogExecutable(t, filepath.Join(root, "later"), ": > '"+marker+"'; printf 'codex-cli 0.162.1'")
	search := provider.Search{Explicit: []provider.Location{{Name: provider.Pi, Path: slow}, {Name: provider.Codex, Path: later}}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	catalog := DiscoverProviders(ctx, search, provider.Runner{Timeout: 2 * time.Second, OutputLimit: 64})
	if time.Since(start) >= 1500*time.Millisecond {
		t.Fatal("shared startup budget did not bound probing")
	}
	items, _, err := catalog.List(50, "")
	if err != nil || len(items) != 2 {
		t.Fatalf("budget expiry discarded discoveries: %+v %v", items, err)
	}
	for _, p := range items {
		if p.ID == "" || p.Path == "" || p.Version != "" || p.Installation != provider.Installed || p.Account != provider.AccountUnknown || p.Connectivity != provider.Unchecked || p.Capabilities != (provider.Capabilities{}) || len(p.Evidence) != 3 {
			t.Fatalf("timeout fabricated readiness or lost metadata: %+v", p)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("probe launched after the shared budget expired")
	}
}
