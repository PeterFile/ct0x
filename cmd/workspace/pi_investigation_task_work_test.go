package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// Compose only: no server, scheduler, real CLI or model. The only discovery
// command is the repository's synthetic executable with fixed probe output.
func TestPiFreshInvestigationWorkComposition(t *testing.T) {
	ctx := t.Context()
	fake := newExecutionFixture(t, "1.4.0")
	ordinary := memberToolsEntry(t)
	entry := filepath.Join(filepath.Dir(ordinary), "fresh-investigation-work.ts")
	textEntry := filepath.Join(filepath.Dir(ordinary), "fresh-task-work.ts")
	const opt = "--pi-fresh-investigation-work-extension"
	const ownerOpt = "--enable-investigation-owner-management"
	const a7Opt = "--enable-discussion-work-return"
	catalog := platform.DiscoverProviders(ctx, provider.Search{Explicit: []provider.Location{{Name: provider.Pi, Path: fake.path}}}, provider.Runner{})
	providers, err := providerSnapshot(catalog)
	if err != nil || len(providers) != 1 || providers[0].Version != "1.4.0" {
		t.Fatalf("missing synthetic Pi provider: %v", err)
	}
	for _, mode := range []string{"first-only", "typed-only", "typed-with-text", "managed-owners", "managed-with-text-and-resume", "discussion-return"} {
		t.Run(mode, func(t *testing.T) {
			wantTyped := mode != "first-only"
			wantOwners := mode == "managed-owners" || mode == "managed-with-text-and-resume"
			wantReturn := mode == "discussion-return"
			wantText := mode == "typed-with-text" || mode == "managed-with-text-and-resume" || wantReturn
			private := filepath.Join(t.TempDir(), "private")
			if err := os.Mkdir(private, 0700); err != nil {
				t.Fatal(err)
			}
			base := append(memberToolsArgs(t, private, fake.path), "--enable-coordination-roles")
			if _, err := parseConfig(append(slices.Clone(base), opt, entry), io.Discard); err == nil {
				t.Fatal("typed opt-in accepted without existing investigation startup fences")
			}
			base = append(base, "--enable-investigation-execution")
			for _, wrong := range [][]string{{opt, ordinary}, {opt, "relative.ts"}, {opt, entry, opt, entry}, {ownerOpt}, {a7Opt}} {
				if _, err := parseConfig(append(slices.Clone(base), wrong...), io.Discard); err == nil {
					t.Fatal("ambiguous typed extension option accepted")
				}
			}
			if wantTyped {
				base = append(base, opt, entry)
			}
			if wantText {
				base = append(base, "--pi-fresh-task-work-extension", textEntry)
			}
			if wantOwners {
				base = append(base, ownerOpt)
			}
			if wantReturn {
				base = append(base, a7Opt)
			}
			if mode == "managed-with-text-and-resume" || wantReturn {
				base = append(base, "--enable-native-session-resume", "--native-pi-thinking", "max")
			}
			cfg, err := parseConfig(base, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(filepath.Join(private, "workspace.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			engine := coordination.New(db)
			launch, err := cfg.piFreshInvestigationWorkLaunch(db, engine, providers)
			if err != nil || (launch != nil) != wantTyped {
				t.Fatalf("wrong startup descriptor: %v", err)
			}
			if launch != nil {
				if launch.EnableDiscussionWorkReturn != wantReturn {
					t.Fatal("A7 flag did not bind the typed launcher")
				}
				// This is the actual cmd adapter. An empty plan must be denied
				// inside B without calling the runtime's memory callback.
				called := false
				if err := launch.Commit(ctx, store.FreshTaskWorkLaunch{}, func() bool { called = true; return true }); !errors.Is(err, store.ErrInvalid) || called {
					t.Fatal("cmd did not require the exact B contract", err)
				}
				cfgOther := cfg
				cfgOther.piPath = filepath.Join(private, "not-the-selected-pi")
				if _, err := cfgOther.piFreshInvestigationWorkLaunch(db, engine, providers); err == nil {
					t.Fatal("an unselected provider qualified the typed configuration")
				}
			}
			_, owner, err := artifactReadRoutes(private, db)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			workspace, err := workspaceRuntimeOptions(private, owner)
			if err != nil {
				t.Fatal(err)
			}
			schedule, runtime, preparer, err := executionRunner(cfg, db, engine, catalog, "http://127.0.0.1:1/api/v1/member-tools", io.Discard, workspace)
			if err != nil || schedule == nil || runtime == nil || preparer == nil {
				t.Fatalf("no trusted composition: %v", err)
			}
			defer runtime.Close()
			tools := runtime.Tools()
			if tools.TaskWorkReportsEnabled() != wantTyped || tools.InvestigationOwnerManagementEnabled() != wantOwners ||
				tools.FreshTaskWorkEnabled() != wantText || tools.WorkInputsEnabled() {
				t.Fatal("default or explicit profiles changed their separate opt-ins")
			}
			routes := httpapi.MemberRoleToolRoutes(tools)
			for op, want := range map[string]bool{"report_work": wantTyped, "read_work_report": wantTyped, "read_work_inputs": wantTyped,
				"delegate_work": wantOwners, "handback_work": wantOwners || wantReturn, "hand_back_work": false} {
				found := slices.ContainsFunc(routes, func(r httpapi.Route) bool { return r.Pattern == "/api/v1/member-tools/"+op })
				if found != want {
					t.Fatalf("wrong route availability for %s", op)
				}
			}
		})
	}
	if _, err := os.Stat(filepath.Join(fake.root, "calls")); !os.IsNotExist(err) {
		t.Fatal("composing the command started native execution")
	}
}
