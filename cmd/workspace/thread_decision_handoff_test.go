package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestThreadDecisionHandoffOptInAndOldStorage(t *testing.T) {
	const handoff = "--enable-thread-decision-handoff"
	if _, err := parseConfig([]string{handoff}, io.Discard); err == nil {
		t.Fatal("accepted a flag without the native role prerequisites")
	}
	root := t.TempDir()
	cli := filepath.Join(root, "codex-fixture")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\n[ \"$*\" = --version ] && printf 'codex-cli 0.166.0\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	args := []string{"--data-dir", root, "--enable-execution", "--enable-member-tools", "--enable-coordination-roles", "--enable-codex-app-server"}
	base, err := parseConfig(args, io.Discard)
	if err != nil || base.enableThreadDecisionHandoff {
		t.Fatalf("handoff not opt-in: %v", err)
	}
	enabled, err := parseConfig(append(args, handoff), io.Discard)
	if err != nil || !enabled.enableThreadDecisionHandoff {
		t.Fatalf("verified prerequisites did not select handoff: %v", err)
	}
	db, err := store.Open(filepath.Join(root, "public-v30.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	catalog := platform.DiscoverProviders(context.Background(), provider.Search{Explicit: []provider.Location{{Name: provider.Codex, Path: cli}}}, provider.Runner{})
	engine := coordination.New(db)
	const endpoint = "http://127.0.0.1:7310/api/v1/member-tools"
	if runner, runtime, preparer, err := executionRunner(enabled, db, engine, catalog, endpoint, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "待决请求、答复和话题答复") || runner != nil || runtime != nil || preparer != nil {
		t.Fatalf("missing F storage was not gated: %v", err)
	}
	if runner, runtime, preparer, err := executionRunner(base, db, engine, catalog, endpoint, io.Discard); err != nil || runner == nil || runtime == nil || preparer == nil {
		t.Fatalf("old database no longer composes with the flag off: %v", err)
	} else {
		t.Cleanup(func() { _ = runtime.Close() })
	}
}
