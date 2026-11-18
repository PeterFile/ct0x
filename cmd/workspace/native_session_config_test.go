package main

import (
	"bytes"
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

func TestNativeSessionConfigDefaultOffAndPrerequisites(t *testing.T) {
	defaults, err := parseConfig(nil, io.Discard)
	if err != nil || defaults.enableNativeSessionResume || defaults.nativePiThinking != "" {
		t.Fatalf("native session resume must default off: %v", err)
	}
	base := []string{"--enable-execution", "--enable-member-tools", "--pi-member-tools-extension", memberToolsEntry(t)}
	for _, flag := range []string{"--enable-native-session-resume", "--enable-native-session-resume=false"} {
		c, err := parseConfig(append(append([]string{}, base...), flag), io.Discard)
		if err != nil || c.enableNativeSessionResume != (flag == "--enable-native-session-resume") {
			t.Fatalf("explicit opt-in/out rejected: %v", err)
		}
	}
	for _, thinking := range []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"} {
		c, err := parseConfig(append(slices.Clone(base), "--enable-native-session-resume", "--native-pi-thinking="+thinking), io.Discard)
		if err != nil || c.nativePiThinking != thinking || c.roleThinking != "" {
			t.Fatalf("native Pi thinking %q was rejected or changed the role default: %v", thinking, err)
		}
	}
	for _, args := range [][]string{
		{"--enable-native-session-resume"},
		{"--enable-execution", "--enable-native-session-resume"},
		{"--enable-native-session-resume", "--enable-member-tools"},
		append(slices.Clone(base), "--native-pi-thinking=low"),
		append(slices.Clone(base), "--enable-native-session-resume", "--native-pi-thinking="),
		append(slices.Clone(base), "--enable-native-session-resume", "--native-pi-thinking=Max"),
		{"--enable-execution", "--enable-member-tools", "--enable-coordination-roles", "--enable-codex-app-server", "--enable-native-session-resume", "--native-pi-thinking=low"},
	} {
		dir := filepath.Join(t.TempDir(), "must-not-exist")
		err := runWithSearch(context.Background(), append([]string{"--data-dir", dir}, args...), io.Discard, provider.Search{})
		if err == nil || !chinese(err.Error()) {
			t.Fatal("invalid native flag combination was accepted")
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("invalid flags created runtime state")
		}
	}
	var help bytes.Buffer
	if err := runWithSearch(context.Background(), []string{"--help"}, &help, provider.Search{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--enable-native-session-resume", "--native-pi-thinking", "默认禁用", "不启用文件会话"} {
		if !strings.Contains(help.String(), want) {
			t.Fatalf("help omits %q", want)
		}
	}
}

func TestNativeSessionExecutionCompositionAndOldToken403(t *testing.T) {
	ctx := context.Background()
	fixture := newExecutionFixture(t, "1.4.0")
	catalog := platform.DiscoverProviders(ctx, provider.Search{Explicit: []provider.Location{{Name: provider.Pi, Path: fixture.path}}}, provider.Runner{})
	items, _, err := catalog.List(10, "")
	if err != nil || len(items) != 1 {
		t.Fatalf("fixture catalog: %v", err)
	}
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "fixture.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	args := append(memberToolsArgs(t, dir, fixture.path), "--enable-native-session-resume", "--native-pi-thinking=low")
	c, err := parseConfig(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_, runtime, preparer, err := executionRunner(c, db, coordination.New(db), catalog, "http://127.0.0.1:7310/api/v1/member-tools", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if caps := preparer.AdapterReports()[items[0].ID].Capabilities; caps == nil || caps.NativeResume != provider.SupportSupported || caps.Steer != provider.SupportUnsupported {
		t.Fatal("real command composition dropped the native-session opt-in")
	}
	local := platform.NewLocalUser(db)
	group, err := local.CreateGroup(ctx, platform.GroupInput{Name: "native token fixture"}, "")
	if err != nil {
		t.Fatal(err)
	}
	member, err := local.CreateMember(ctx, group.ID, platform.MemberInput{Name: "member", Role: "text", ProviderID: items[0].ID, Model: "openai-codex/gpt-6.1-sol"}, "")
	if err != nil {
		t.Fatal(err)
	}
	claimPrepared := func() (store.TurnClaim, provider.Executor, provider.ExecuteRequest) {
		message, err := local.CreateMessage(ctx, group.ID, platform.MessageInput{Content: "fresh claim", RecipientIDs: []string{member.ID}}, "")
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := preparer.Prepare(ctx, store.PendingDelivery{GroupID: group.ID, Delivery: message.Deliveries[0]})
		if err != nil || prepared.Request.Thinking != "low" || prepared.Request.Model != member.Model {
			t.Fatalf("real command wiring lost the exact native-only thinking or selected member model: %v", err)
		}
		turn, err := db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: group.ID, DeliveryID: message.Deliveries[0].ID, MaxActive: 1, ResourceKeys: prepared.ResourceKeys})
		if err != nil {
			t.Fatal(err)
		}
		request := prepared.Request
		request.RunID, request.RuntimeDir = turn.ID, filepath.Join(dir, "runtime")
		return store.TurnClaim{GroupID: group.ID, TurnID: turn.ID, Token: turn.ClaimToken}, prepared.Executor, request
	}
	issue := func() (store.TurnClaim, string) {
		claim, _, _ := claimPrepared()
		token, err := runtime.Tools().Issue(ctx, claim, platform.MemberToolGrants{Operations: []string{"list_members"}})
		if err != nil {
			t.Fatal(err)
		}
		return claim, token
	}
	firstClaim, oldToken := issue()
	runtime.Tools().Revoke(oldToken)
	if _, err := db.ApplyTurnReceipt(ctx, firstClaim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	currentClaim, currentToken := issue()
	defer runtime.Tools().Revoke(currentToken)
	server, err := httpapi.NewServer(httpapi.DefaultAddress, httpapi.MemberToolRoutes(runtime.Tools())...)
	if err != nil {
		t.Fatal(err)
	}
	for token, want := range map[string]int{oldToken: http.StatusForbidden, currentToken: http.StatusOK} {
		req := httptest.NewRequest(http.MethodPost, "http://"+httpapi.DefaultAddress+"/api/v1/member-tools/list_members", strings.NewReader(`{}`))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, req)
		if response.Code != want {
			t.Fatalf("real member route token status=%d want=%d", response.Code, want)
		}
	}
	if oldToken == currentToken || oldToken == firstClaim.Token {
		t.Fatal("new claim reused old authority")
	}
	runtime.Tools().Revoke(currentToken)
	if _, err := db.ApplyTurnReceipt(ctx, currentClaim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	// With roles off, replace the scheduler directory before the next preparation.
	// Its identity must have been fixed at runtime construction.
	root := filepath.Join(dir, "runtime")
	if err := os.Rename(root, root+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	_, executor, request := claimPrepared()
	got := executor.Execute(ctx, request, nil)
	if got.Started || got.Outcome != provider.OutcomeUnknown {
		t.Fatal("generic Pi start did not fence the replaced scheduler root")
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatal("invocation wrote to the replacement runtime root")
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "calls")); !os.IsNotExist(err) {
		t.Fatal("composition/auth test invoked fixture model execution")
	}
}
