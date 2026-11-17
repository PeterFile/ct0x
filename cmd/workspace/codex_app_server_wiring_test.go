package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestCodexAppServerCompositionFlags(t *testing.T) {
	base := []string{"--enable-execution", "--enable-member-tools", "--enable-coordination-roles", "--pi-member-tools-extension", memberToolsEntry(t)}
	implicit, err := parseConfig(base, io.Discard)
	if err != nil || implicit.enableCodexAppServer {
		t.Fatalf("native Codex entry is not default off: %v", err)
	}
	explicit, err := parseConfig(append(append([]string{}, base...), "--enable-codex-app-server=false"), io.Discard)
	if err != nil || !reflect.DeepEqual(implicit, explicit) {
		t.Fatal("explicit false changed legacy Pi configuration")
	}
	codexOnly := []string{"--enable-execution", "--enable-member-tools", "--enable-coordination-roles", "--enable-codex-app-server", "--role-thinking=max"}
	for _, args := range [][]string{codexOnly, append(append([]string{}, base...), "--enable-codex-app-server")} {
		config, err := parseConfig(args, io.Discard)
		if err != nil || !config.enableCodexAppServer || !config.enableExecution || !config.enableMemberTools || !config.enableCoordinationRoles {
			t.Fatalf("valid native Codex configuration rejected: %v", err)
		}
	}
	for _, args := range [][]string{
		{"--enable-codex-app-server"},
		{"--enable-execution", "--enable-codex-app-server"},
		{"--enable-execution", "--enable-member-tools", "--enable-codex-app-server"},
		{"--enable-member-tools", "--enable-coordination-roles", "--enable-codex-app-server"},
		{"--enable-execution", "--enable-member-tools", "--enable-coordination-roles", "--enable-codex-app-server=false"},
		append(append([]string{}, codexOnly...), "--pi-member-tools-extension="),
	} {
		if _, err := parseConfig(args, io.Discard); err == nil || !chinese(err.Error()) {
			t.Fatalf("inconsistent native Codex flags accepted: %v", err)
		}
	}
	var help bytes.Buffer
	if err := runWithSearch(context.Background(), []string{"--help"}, &help, provider.Search{}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"--enable-codex-app-server", "默认禁用", "受限只读原生文本", "apply_patch", "view_image", "不承诺文件读取隔离", "Codex 不传此参数", "不启用文件任务或 Artifact", "未知能力不能自动分派"} {
		if !strings.Contains(help.String(), text) {
			t.Fatalf("native Codex help omitted %q", text)
		}
	}
}

func TestCodexAppServerRunnerRejectsMissingPrerequisitesBeforeRoots(t *testing.T) {
	dir := t.TempDir()
	config := config{dataDir: dir, enableCodexAppServer: true}
	_, tools, preparer, err := executionRunner(config, nil, nil, nil, "http://127.0.0.1:1/api/v1/member-tools", io.Discard)
	if err == nil || tools != nil || preparer != nil {
		t.Fatal("native composition accepted missing execution/tools/roles")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid native configuration allocated runtime directories")
	}
}

func TestCodexAppServerRoutesAndCatalogOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "default-off", true: "explicit-on"}[enabled], func(t *testing.T) {
			root := t.TempDir()
			cli := filepath.Join(root, "codex-fixture")
			// Discovery may invoke only this inert version fixture, never a model
			// or original CLI/auth command. 0.166 account probing stays unknown.
			body := "#!/bin/sh\ncase \"$*\" in\n--version) printf 'codex-cli 0.166.0\\n';;\n*) exit 99;;\nesac\n"
			if err := os.WriteFile(cli, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			args := []string{"--listen", "127.0.0.1:0", "--data-dir", filepath.Join(root, "data"), "--codex-path", cli, "--enable-execution", "--enable-member-tools", "--enable-coordination-roles"}
			if enabled {
				args = append(args, "--enable-codex-app-server")
			} else {
				args = append(args, "--pi-member-tools-extension", memberToolsEntry(t))
			}
			endpoint, _ := startRunWithSearch(t, provider.Search{}, args...)
			client := &http.Client{Timeout: 3 * time.Second}
			defer client.CloseIdleConnections()
			request, err := http.NewRequest(http.MethodPost, endpoint+"/api/v1/member-tools/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			wantStatus := http.StatusNotFound
			if enabled {
				wantStatus = http.StatusForbidden
			}
			if response.StatusCode != wantStatus {
				t.Fatalf("MCP route did not follow explicit opt-in on the actual loopback port: %d", response.StatusCode)
			}
			response, err = client.Get(endpoint + "/api/v1/providers")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var page struct {
				Items []struct {
					provider.Provider
					Adapter *platform.ProviderAdapterReport `json:"adapter"`
				} `json:"items"`
			}
			if json.NewDecoder(response.Body).Decode(&page) != nil || response.StatusCode != 200 || len(page.Items) != 1 {
				t.Fatal("actual cmd catalog projection was not mounted")
			}
			report := page.Items[0].Adapter
			wantID := provider.CodexOneShotAdapterID
			if enabled {
				wantID = provider.CodexAppServerAdapterID
			}
			if report == nil || report.ID != wantID || !report.Enabled || report.Capabilities == nil || report.Capabilities.Supports(provider.CapabilityPlatformTools) != enabled || page.Items[0].Account != provider.AccountUnknown {
				t.Fatal("cmd provider route did not publish the actual adapter independent of account state")
			}
		})
	}
}

func TestCodexAppServerRunnerUsesOneStoreRuntime(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "workspace.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	config := config{dataDir: root, enableExecution: true, enableMemberTools: true, enableCoordinationRoles: true, enableCodexAppServer: true, memberToolsMaxMembers: 4, roleThinking: "max"}
	// Empty discovery has no subprocesses and does not fabricate provider support.
	catalog := platform.DiscoverProviders(context.Background(), provider.Search{}, provider.Runner{})
	schedule, runtime, preparer, err := executionRunner(config, db, coordination.New(db), catalog, "http://[::1]:12345/api/v1/member-tools", io.Discard)
	if err != nil || schedule == nil || runtime == nil || preparer == nil || len(preparer.AdapterReports()) != 0 {
		t.Fatalf("Codex-only composition failed or manufactured capabilities: %v", err)
	}
	ctx := context.Background()
	group, err := db.CreateGroup(ctx, store.CreateGroupInput{Name: "same store"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := db.CreateMember(ctx, store.CreateMemberInput{GroupID: group.ID, Name: "fixture", Role: "synthetic", ProviderID: "unverified", Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	message, err := db.CreateMessage(ctx, store.MessageRequest{GroupID: group.ID, ActorID: "user", Content: "synthetic claim"}, []string{member.ID})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: group.ID, DeliveryID: message.Deliveries[0].ID, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	claim := store.TurnClaim{GroupID: group.ID, TurnID: turn.ID, Token: turn.ClaimToken}
	// No native executor runs: this trusted fixture claim proves Store identity.
	defer db.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1})
	token, err := runtime.Tools().Issue(ctx, claim, platform.MemberToolGrants{Operations: []string{"list_members"}})
	if err != nil {
		t.Fatalf("runtime does not borrow the exact scheduler Store: %v", err)
	}
	defer runtime.Tools().Revoke(token)
	members, err := runtime.Tools().ListMembers(ctx, token, store.ListOptions{})
	if err != nil || len(members.Items) != 1 || members.Items[0].ID != member.ID {
		t.Fatalf("runtime tools did not use the shared claim and Store: %v", err)
	}
}
