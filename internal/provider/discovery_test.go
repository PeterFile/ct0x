package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func executable(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiscoveryPreservesPriorityAndDeduplicatesPhysicalFiles(t *testing.T) {
	root := t.TempDir()
	pi := executable(t, filepath.Join(root, "explicit", "pi"), "exit 99")
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(pi, alias); err != nil {
		t.Fatal(err)
	}
	pathDir, common := filepath.Join(root, "bin"), filepath.Join(root, "common")
	codex := executable(t, filepath.Join(pathDir, "codex"), "exit 99")
	other := executable(t, filepath.Join(common, "pi"), "exit 99")
	if err := os.Link(pi, filepath.Join(pathDir, "pi")); err != nil {
		t.Fatal(err)
	}
	search := Search{Explicit: []Location{{Pi, alias}, {Pi, pi}}, PathDirs: []string{pathDir}, CommonDirs: []string{common}}
	got := Discover(search)
	if len(got) != 3 {
		t.Fatalf("expected three physical installations, got %+v", got)
	}
	for i, path := range []string{alias, codex, other} {
		p := got[i]
		if p.Path != path || p.Installation != Installed || p.Account != AccountUnknown || p.Connectivity != Unchecked || p.Capabilities != (Capabilities{}) {
			t.Fatalf("unexpected independent states: %+v", p)
		}
		if len(p.Evidence) != 1 || p.Evidence[0].Type != DiscoveryProbe || p.Evidence[0].At.IsZero() {
			t.Fatalf("missing discovery evidence: %+v", p)
		}
	}
	if got[0].ID == got[2].ID || got[0].ID != Discover(Search{Explicit: []Location{{Pi, pi}}})[0].ID {
		t.Fatal("IDs must distinguish installations and remain stable across symlink aliases")
	}
	data, _ := json.Marshal(got[0])
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil || fields["account"] != "unknown" || fields["connectivity"] != "unchecked" || reflect.ValueOf(fields["capabilities"]).Kind() != reflect.Map {
		t.Fatalf("unexpected JSON contract: %s", data)
	}
}

func TestDiscoveryKeepsDistinctProvidersSharingMultiplexer(t *testing.T) {
	root := t.TempDir()
	mux := executable(t, filepath.Join(root, "mux"), "exit 99")
	alias := filepath.Join(root, "alias")
	for _, path := range []string{alias, filepath.Join(root, Pi), filepath.Join(root, Codex)} {
		if err := os.Symlink(mux, path); err != nil {
			t.Fatal(err)
		}
	}
	got := Discover(Search{Explicit: []Location{{Pi, alias}, {Pi, filepath.Join(root, Pi)}}, PathDirs: []string{root}})
	if len(got) != 2 || got[0].Name != Pi || got[1].Name != Codex || got[0].Path != alias || got[1].Path != filepath.Join(root, Codex) || got[0].ID == got[1].ID {
		t.Fatalf("provider identity must survive shared shims while same-provider aliases deduplicate: %+v", got)
	}
}

func TestDiscoveryIsBoundedAndDoesNotInferAccountFromConfiguration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("OPENAI_API_KEY", "fixture-not-an-account")
	t.Setenv("PATH", root)
	marker := filepath.Join(root, "executed")
	body := ": > '" + marker + "'"
	fixture := executable(t, filepath.Join(root, "pi"), body)
	executable(t, filepath.Join(root, "nested", "codex"), body)
	if err := os.WriteFile(filepath.Join(root, "codex"), []byte("not executable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".pi", "agent"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".pi", "agent", "auth.json"), []byte(`{"openai-codex":{"type":"oauth"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if result := (Runner{}).Run(context.Background(), fixture, nil, nil); result.Failure != "" {
		t.Fatalf("fixture control failed with restricted PATH: %s", result.Failure)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("fixture control did not create marker: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if got := Discover(Search{}); len(got) != 0 || got == nil {
		t.Fatalf("empty search must not scan environment or home: %+v", got)
	}
	got := Discover(Search{PathDirs: []string{"", root}, Explicit: []Location{{"other", filepath.Join(root, "pi")}, {Pi, root}}})
	if len(got) != 1 || got[0].Name != Pi || got[0].Account != AccountUnknown || got[0].Version != "" {
		t.Fatalf("unexpected discovery: %+v", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("discovery executed a binary")
	}
}
