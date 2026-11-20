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
	"agentworkspace/internal/domain"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestPiFreshTaskWorkFlagRequiresSeparateExplicitProfile(t *testing.T) {
	ordinary := memberToolsEntry(t)
	entry := filepath.Join(filepath.Dir(ordinary), "fresh-task-work.ts")
	const flag = "--pi-fresh-task-work-extension"
	const proposal = "--enable-pi-task-decision-proposals"
	dir := t.TempDir()
	base := []string{"--data-dir", dir, "--enable-execution", "--enable-member-tools", "--pi-member-tools-extension", ordinary, "--enable-coordination-roles"}
	for _, args := range [][]string{
		{flag, entry},
		append(slices.Clone(base), proposal),
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
	if launch, err := cfg.piFreshTaskWorkLaunch(db, engine, providers); err != nil || launch == nil || cfg.enablePiTaskDecisionProposals {
		t.Fatalf("exact fresh pair plus ordinary entry rejected or not separate: %v", err)
	}
	on, err := parseConfig(append(slices.Clone(base), flag, entry, proposal), io.Discard)
	if err != nil || !on.enablePiTaskDecisionProposals {
		t.Fatalf("explicit proposal opt-in was not accepted: %v", err)
	}
	if launch, err := on.piFreshTaskWorkLaunch(db, engine, providers); err != nil || launch == nil || !launch.SupportsTaskDecision(providers[0]) {
		t.Fatalf("explicit exact proposal profile rejected: %v", err)
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
	if !strings.Contains(help.String(), flag) || !strings.Contains(help.String(), proposal) {
		t.Fatal("fresh or proposal option is missing from help")
	}
}

// #200 (04cdc956) used the same parser and differed only in these descriptions.
// Reconstruct the released entry in a temp dir without a second bridge pack.
func pi200MemberToolsEntry(t *testing.T, current string) string {
	t.Helper()
	data, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	const start = `  ], { description: "仅受信chat_intake/v1`
	const end = "    ...chatAssessmentFields,"
	prefix, rest, found := strings.Cut(string(data), start)
	_, suffix, ended := strings.Cut(rest, end)
	if !found || !ended {
		t.Fatal("cannot reconstruct the #200 intake descriptions")
	}
	legacy := prefix + start + "解释绑定来源；Generic不能评估授权。讨论/否定/引用选discuss，范围不清选clarify。prepare_work仅限受支持的topic_summary。\" +\n" +
		"    (continueWork ? `continue_work仅用于当前本机用户的明确续办请求；续办理由不得为空白，最多 ${workInputBytes.reason} UTF-8 字节。` : \"\") });\n" +
		`  const assessment = Type.Object({
    disposition,
    action: Type.Optional(Type.Union([Type.Literal("text_submission"), Type.Literal("workspace_change"),
      ...(investigation ? [Type.Literal(INVESTIGATION_ASSESSMENT_ACTION)] : []),
    ], { description: investigation ? "仅为语义建议；只读调查使用 prepare_work 和 reason，目标和资料由服务器绑定。模型不能授予权限。" :
      "结合可信来源和上下文提出文本交付或目录文件修改；只是一项语义建议，当前 Policy、目录授权和 Task 范围由服务器核定，模型不能授予 IO 权限。" })),
` + end + suffix
	impl, err := os.ReadFile(filepath.Join(filepath.Dir(current), "bridge.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	entry := filepath.Join(root, "index.ts")
	if err := os.WriteFile(entry, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bridge.mjs"), impl, 0600); err != nil {
		t.Fatal(err)
	}
	if !provider.PiInvestigationBridgeSupported(entry) {
		t.Fatal("fixture does not match the released #200 pair")
	}
	return entry
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
	current := memberToolsEntry(t)
	old := pi200MemberToolsEntry(t, current)
	fresh := filepath.Join(filepath.Dir(current), "fresh-task-work.ts")
	for _, tc := range []struct {
		name, entry                    string
		fresh, investigation, rejected bool
	}{
		{name: "default", entry: current},
		{name: "opt-in", entry: old, fresh: true},
		{name: "investigation-200", entry: old, investigation: true},
		{name: "combined", entry: current, fresh: true, investigation: true},
		{name: "combined-200", entry: old, fresh: true, investigation: true, rejected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.investigation {
				ok(t, os.Chmod(dir, 0700))
			}
			db, err := store.Open(filepath.Join(dir, "workspace.db"))
			ok(t, err)
			defer db.Close()
			args := memberToolsArgs(t, dir, fixture.path)
			args[slices.Index(args, "--pi-member-tools-extension")+1] = tc.entry
			args = append(args, "--enable-coordination-roles", "--enable-native-session-resume", "--native-pi-thinking=off")
			if tc.fresh {
				args = append(args, "--pi-fresh-task-work-extension", fresh)
			}
			var workspace []platform.MemberWorkspaceOptions
			if tc.investigation {
				args = append(args, "--enable-investigation-execution")
				_, owner, err := artifactReadRoutes(dir, db)
				ok(t, err)
				defer owner.Close()
				opts, err := workspaceRuntimeOptions(dir, owner)
				ok(t, err)
				workspace = append(workspace, opts)
			}
			cfg, err := parseConfig(args, io.Discard)
			ok(t, err)
			if cfg.executionMaxActive != defaultExecutionMaxActive || cfg.enableInvestigationExecution != tc.investigation {
				t.Fatal("flags changed the default capacity or the investigation opt-in")
			}
			engine := coordination.New(db)
			run, runtime, prep, err := executionRunner(cfg, db, engine, catalog, "http://127.0.0.1:7310/api/v1/member-tools", io.Discard, workspace...)
			if runtime != nil {
				defer runtime.Close()
			}
			if tc.rejected {
				if err == nil || !strings.Contains(err.Error(), "新版") || !strings.Contains(err.Error(), "--pi-member-tools-extension") ||
					run != nil || runtime != nil || prep != nil {
					t.Fatalf("joint intake must reject the #200 pair and request the new pair: %v", err)
				}
				return
			}
			ok(t, err)
			if run == nil || runtime == nil || prep == nil {
				t.Fatal("command composition did not return an execution runner")
			}
			tools := runtime.Tools()
			if tools.FreshTaskWorkEnabled() != tc.fresh || tools.WorkInputsEnabled() {
				t.Fatal("command composition selected the wrong execution or legacy preview profile")
			}
			_, err = os.Stat(filepath.Join(dir, "workspaces", platform.InvestigationCWDName))
			if tc.investigation && err != nil || !tc.investigation && !os.IsNotExist(err) {
				t.Fatal("investigation private CWD did not follow its explicit opt-in")
			}
			local := platform.NewLocalUser(db)
			group, err := local.CreateGroup(ctx, platform.GroupInput{Name: "intake fixture"}, "")
			ok(t, err)
			lead, err := local.CreateMember(ctx, group.ID, platform.MemberInput{Name: "intake", Role: "text", ProviderID: providers[0].ID, Model: "fixture/model"}, "")
			ok(t, err)
			const topic = "intake"
			if tc.investigation {
				// Startup enables readiness after runner and route composition.
				engine.EnableInvestigationExecution()
				_, err := engine.SetChatPolicy(ctx, domain.ThreadRef{GroupID: group.ID, TopicID: topic}, domain.ChatPolicyRequest{
					Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 1,
					Investigation: &domain.ChatInvestigationPolicy{Objective: "核对原文", AllowTopicMessages: true},
				}, 0, "policy")
				ok(t, err)
			}
			msg, err := local.CreateMessage(ctx, group.ID, platform.MessageInput{TopicID: topic, Content: "请评估这条消息。", RecipientIDs: []string{lead.ID}}, "source")
			ok(t, err)
			delivery := msg.Deliveries[0]
			prepared, err := prep.Prepare(ctx, store.PendingDelivery{GroupID: group.ID, Delivery: delivery})
			ok(t, err)
			// Both investigation pairs offer the excerpt without fresh Task Work.
			prompt := prepared.Request.Prompt
			if strings.Contains(prompt, "continue_work") != tc.fresh ||
				strings.Contains(prompt, domain.ChatActionReadOnlyInvestigation) != tc.investigation ||
				!strings.Contains(prompt, "discussion_only_excerpt") || prepared.Request.Thinking != "" {
				t.Errorf("intake prompt or native session selection did not match the installed schema (thinking=%q)", prepared.Request.Thinking)
			}
			turn, err := db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: group.ID, DeliveryID: delivery.ID, MaxActive: 1, ResourceKeys: prepared.ResourceKeys})
			ok(t, err)
			token, err := tools.IssueChatIntake(ctx, store.TurnClaim{GroupID: group.ID, TurnID: turn.ID, Token: turn.ClaimToken})
			ok(t, err)
			defer tools.Revoke(token)
			profile, err := tools.TransportProfile(ctx, token)
			ok(t, err)
			if profile.ContinueWork != tc.fresh {
				t.Errorf("real command intake continuation enabled=%v; want %v", profile.ContinueWork, tc.fresh)
			}
			if !tc.fresh {
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
