package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestPiFreshTaskWorkFlagRequiresSeparateExplicitProfile(t *testing.T) {
	ordinary := memberToolsEntry(t)
	entry := filepath.Join(filepath.Dir(ordinary), "fresh-task-work.ts")
	const flag = "--pi-fresh-task-work-extension"
	dir := t.TempDir()
	base := []string{"--data-dir", dir, "--enable-execution", "--enable-member-tools", "--pi-member-tools-extension", ordinary, "--enable-coordination-roles"}
	for _, args := range [][]string{
		{flag, entry},
		append(slices.Clone(base[:len(base)-1]), flag, entry),
		append(slices.Clone(base), flag+"="),
		append(slices.Clone(base), flag, "fresh-task-work.ts"),
		append(slices.Clone(base), flag, ordinary),
		append(slices.Clone(base), flag, entry, flag, entry),
	} {
		if _, err := parseConfig(args, io.Discard); err == nil {
			t.Fatal("ambiguous or unbound opt-in accepted")
		}
	}
	off, err := parseConfig(base, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if launch, err := off.piFreshTaskWorkLaunch(nil, nil, nil); err != nil || launch != nil {
		t.Fatal("ordinary entry enabled the fresh profile")
	}
	cfg, err := parseConfig(append(slices.Clone(base), flag, entry), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "workspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	engine := coordination.New(db)
	pi := filepath.Join(dir, "metadata-only")
	if err := os.WriteFile(pi, nil, 0700); err != nil {
		t.Fatal(err)
	}
	providers := []provider.Provider{{ID: "fixture", Name: provider.Pi, Version: "1.4.0", Installation: provider.Installed, Path: pi}}
	if launch, err := cfg.piFreshTaskWorkLaunch(db, engine, providers); err != nil || launch == nil {
		t.Fatalf("exact fresh pair plus ordinary entry rejected: %v", err)
	}
	if launch, err := cfg.piFreshTaskWorkLaunch(db, nil, providers); err == nil || launch != nil {
		t.Fatal("startup without B Engine did not fail closed")
	}
	alias := filepath.Join(dir, "alias.ts")
	if err := os.Symlink(entry, alias); err != nil {
		t.Fatal(err)
	}
	cfg.piMemberToolsExtension = alias
	if launch, err := cfg.piFreshTaskWorkLaunch(db, engine, providers); err == nil || launch != nil {
		t.Fatal("same-file alias was treated as an independent ordinary extension")
	}
	var help strings.Builder
	_, _ = parseConfig([]string{"--help"}, &help)
	if !strings.Contains(help.String(), flag) {
		t.Fatal("fresh option is missing from help")
	}
}
