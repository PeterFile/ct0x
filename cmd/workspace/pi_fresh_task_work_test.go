package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
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
	unsupported := slices.Clone(providers)
	unsupported[0].Version = "1.4.1"
	if launch, err := cfg.piFreshTaskWorkLaunch(db, engine, unsupported); err == nil || launch != nil {
		t.Fatal("unsupported Pi version returned the opt-in profile")
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

func TestPiFreshTaskWorkExecutionRunnerIntakeProfile(t *testing.T) {
	ctx := context.Background()
	ok := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	fixture := newExecutionFixture(t, "1.4.0")
	catalog := platform.DiscoverProviders(ctx, provider.Search{Explicit: []provider.Location{{Name: provider.Pi, Path: fixture.path}}}, provider.Runner{})
	providers, err := providerSnapshot(catalog)
	ok(t, err)
	if len(providers) != 1 || providers[0].Version != "1.4.0" {
		t.Fatal("missing synthetic Pi metadata")
	}
	for _, name := range []string{"default", "opt-in"} {
		t.Run(name, func(t *testing.T) {
			enabled := name == "opt-in"
			dir := t.TempDir()
			db, err := store.Open(filepath.Join(dir, "workspace.db"))
			ok(t, err)
			defer db.Close()
			args := append(memberToolsArgs(t, dir, fixture.path), "--enable-coordination-roles", "--enable-native-session-resume", "--native-pi-thinking=off")
			if enabled {
				args = append(args, "--pi-fresh-task-work-extension", filepath.Join(filepath.Dir(memberToolsEntry(t)), "fresh-task-work.ts"))
			}
			cfg, err := parseConfig(args, io.Discard)
			ok(t, err)
			run, runtime, prep, err := executionRunner(cfg, db, coordination.New(db), catalog, "http://127.0.0.1:7310/api/v1/member-tools", io.Discard)
			ok(t, err)
			defer runtime.Close()
			tools := runtime.Tools()
			if run == nil || tools.FreshTaskWorkEnabled() != enabled || tools.WorkInputsEnabled() {
				t.Fatal("command composition selected the wrong execution or legacy preview profile")
			}
			local := platform.NewLocalUser(db)
			group, err := local.CreateGroup(ctx, platform.GroupInput{Name: "intake fixture"}, "")
			ok(t, err)
			lead, err := local.CreateMember(ctx, group.ID, platform.MemberInput{Name: "intake", Role: "text", ProviderID: providers[0].ID, Model: "fixture/model"}, "")
			ok(t, err)
			msg, err := local.CreateMessage(ctx, group.ID, platform.MessageInput{Content: "请续办原任务。", RecipientIDs: []string{lead.ID}}, "source")
			ok(t, err)
			delivery := msg.Deliveries[0]
			prepared, err := prep.Prepare(ctx, store.PendingDelivery{GroupID: group.ID, Delivery: delivery})
			ok(t, err)
			if strings.Contains(prepared.Request.Prompt, "continue_work") != enabled || (prepared.Request.Thinking == "off") != !enabled {
				t.Errorf("intake prompt or native session selection did not match the installed schema (thinking=%q)", prepared.Request.Thinking)
			}
			turn, err := db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: group.ID, DeliveryID: delivery.ID, MaxActive: 1, ResourceKeys: prepared.ResourceKeys})
			ok(t, err)
			token, err := tools.IssueChatIntake(ctx, store.TurnClaim{GroupID: group.ID, TurnID: turn.ID, Token: turn.ClaimToken})
			ok(t, err)
			defer tools.Revoke(token)
			profile, err := tools.TransportProfile(ctx, token)
			ok(t, err)
			if profile.ContinueWork != enabled {
				t.Errorf("real command intake schema enabled=%v; want %v", profile.ContinueWork, enabled)
			}
			if !enabled {
				server, err := httpapi.NewServer(httpapi.DefaultAddress, httpapi.MemberToolRoutes(tools)...)
				ok(t, err)
				req := httptest.NewRequest(http.MethodPost, "http://"+httpapi.DefaultAddress+"/api/v1/member-tools/send_message",
					strings.NewReader(`{"content":"记录续办。","intent_assessment":{"disposition":"continue_work","reason":"续办原任务"}}`))
				req.RemoteAddr = "127.0.0.1:12345"
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Idempotency-Key", "continue")
				w := httptest.NewRecorder()
				server.Handler.ServeHTTP(w, req)
				if w.Code != http.StatusForbidden {
					t.Fatalf("default signed intake continuation returned %d, want 403", w.Code)
				}
			}
		})
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "calls")); !os.IsNotExist(err) {
		t.Fatal("provisioning started native execution")
	}
}
