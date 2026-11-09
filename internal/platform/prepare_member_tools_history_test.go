package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

var historyProfileNames = []string{"list_members", "read_channel", "send_message", "create_member", "search_history", "read_history"}

func TestPrepareMemberToolsHistoryProfileValidation(t *testing.T) {
	for _, profile := range []struct {
		name  string
		names []string
		valid bool
	}{
		{"legacy unordered four", []string{"create_member", "send_message", "read_channel", "list_members"}, true},
		{"unordered six", []string{"read_history", "create_member", "send_message", "search_history", "read_channel", "list_members"}, true},
		{"search only", []string{"search_history"}, false},
		{"history only", []string{"search_history", "read_history"}, false},
		{"four plus search", append(slices.Clone(historyProfileNames[:4]), "search_history"), false},
		{"four plus read", append(slices.Clone(historyProfileNames[:4]), "read_history"), false},
		{"missing core", []string{"list_members", "read_channel", "create_member", "search_history", "read_history"}, false},
		{"unknown six", append(slices.Clone(historyProfileNames[:5]), "create_task"), false},
		{"duplicate six", append(slices.Clone(historyProfileNames[:5]), "search_history"), false},
	} {
		t.Run(profile.name, func(t *testing.T) {
			f := newPrepareFixture(t)
			options := configurePrepareMemberTools(t, f)
			// Isolate Prepare's complete-profile gate from the runtime's name validation.
			f.opts.MemberTools.operations = slices.Clone(profile.names)
			p, err := NewPreparer(f.s, f.opts)
			if profile.valid {
				if err != nil || p == nil {
					t.Fatalf("complete trusted profile rejected: %v", err)
				}
			} else if p != nil || !errors.Is(err, errPrepareConfig) {
				t.Fatalf("partial or invalid profile accepted: %v", err)
			}
			memberToolRuntimeEmpty(t, options.ConfigRoot)
			if len(f.opts.MemberTools.Tools().capabilities) != 0 {
				t.Fatal("profile validation issued authority")
			}
			f.assertQueued(t)
		})
	}
}

func TestPrepareMemberToolsHistoryPostclaimProfileHandoff(t *testing.T) {
	f := newPrepareFixture(t)
	selectPrepareMemberToolsPi(f)
	options := configurePrepareMemberTools(t, f)
	options.Operations = []string{"read_history", "create_member", "search_history", "send_message", "read_channel", "list_members"}
	runtime, err := NewMemberToolRuntime(f.s, options)
	if err != nil {
		t.Fatalf("six-name runtime missing: %v", err)
	}
	f.opts.MemberTools = runtime
	selected, extension := f.opts.Providers[1], f.opts.PiMemberToolsExtension
	body := `#!/bin/sh
set -eu
printf 'invocation\n' >> "$0.calls"
printf '%s\n' "$@" > argv
printf '%s' "$AGENT_WORKSPACE_MEMBER_TOOL_CONFIG" > config-path
/bin/cat > stdin
while [ "$1" != "--session-id" ]; do shift; done
printf '{"type":"session","version":3,"id":"%s","cwd":"%s"}\n' "$2" "$PWD"
/bin/cat "$0.events"
`
	if err := os.WriteFile(selected.Path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(selected.Path+".events", prepareMemberToolsNativeEvents(t, f.member.Model, "history profile fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	p := f.preparer(t)
	// Neither caller-owned names nor options may affect the captured trusted profile.
	options.Operations[0] = "create_task"
	f.opts.MemberTools, f.opts.PiMemberToolsExtension = nil, "/untrusted.ts"
	ctx := context.Background()
	prepare := func() (provider.Executor, provider.ExecuteRequest, []string) {
		t.Helper()
		got, err := p.Prepare(ctx, f.pending)
		if err != nil {
			t.Fatal(err)
		}
		return got.Executor, got.Request, got.ResourceKeys
	}
	before, unclaimed, _ := prepare()
	ready, request, keys := prepare()
	permissions := strings.Join(decodePreparation(t, request.Prompt).Permissions, "\n")
	wantTools := "基础平台工具仅有 " + strings.Join(historyProfileNames, "、") + "；调用者、群组、会话和话题由服务端绑定，模型不能改选身份或授权。"
	if !strings.Contains(permissions, wantTools) || strings.Contains(request.Prompt, options.ConfigRoot) {
		t.Fatal("prompt did not advertise exactly the canonical six profile or leaked config root")
	}
	for _, term := range []string{"不授予 Coordinator", "Assignment", "Mandate", "任务提交/接纳", "评审权限"} {
		if !strings.Contains(permissions, term) {
			t.Errorf("history profile lost privilege boundary %q", term)
		}
	}
	if len(runtime.Tools().capabilities) != 0 {
		t.Fatal("Prepare issued authority before claim")
	}
	memberToolRuntimeEmpty(t, options.ConfigRoot)
	f.assertQueued(t)
	logs := t.TempDir()
	if err := os.Chmod(logs, 0700); err != nil {
		t.Fatal(err)
	}
	unclaimed.RunID, unclaimed.RuntimeDir = "unclaimed-history", logs
	denied := before.Execute(ctx, unclaimed, nil)
	if denied.Outcome != provider.OutcomeUnknown || denied.Started || len(runtime.Tools().capabilities) != 0 {
		t.Fatal("six profile bypassed postclaim gate")
	}
	memberToolRuntimeEmpty(t, options.ConfigRoot)
	if _, err := os.Stat(selected.Path + ".calls"); !os.IsNotExist(err) {
		t.Fatal("preclaim execution launched native CLI")
	}
	turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.pending.Delivery.ID, ResourceKeys: keys, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	request.RunID, request.RuntimeDir = turn.ID, logs
	var token, configPath string
	receipt := ready.Execute(ctx, request, func(event provider.Event) error {
		if token == "" {
			entries, err := os.ReadDir(options.ConfigRoot)
			if err != nil || len(entries) != 1 {
				t.Fatal("native execution has no unique config")
			}
			configPath = filepath.Join(options.ConfigRoot, entries[0].Name())
			endpoint, secret := memberToolRuntimeConfig(t, configPath)
			token = secret
			if endpoint != options.Endpoint || token == claim.Token {
				t.Fatal("config shape or capability binding changed")
			}
			for _, name := range historyProfileNames {
				if err := runtime.Tools().withAuthorized(ctx, token, name, func(scope store.MemberToolScope, grants MemberToolGrants) error {
					if scope.Claim != claim || scope.MemberID != f.member.ID || scope.ConversationID != f.pending.Delivery.ConversationID ||
						scope.TopicID != f.trigger.TopicID || grants.ProviderID != selected.ID || grants.Model != f.member.Model || len(grants.Operations) != 6 {
						return errors.New("history authority differs from claimed identity or trusted profile")
					}
					return nil
				}); err != nil {
					return err
				}
			}
		}
		return f.s.RecordTurnEvent(ctx, claim, event)
	})
	if receipt.Outcome != provider.OutcomeSucceeded || !receipt.OutputComplete || !receipt.DirectChildReaped || token == "" {
		t.Fatalf("claimed six profile failed: %+v", receipt)
	}
	memberToolRuntimeEmpty(t, options.ConfigRoot)
	if len(runtime.Tools().capabilities) != 0 {
		t.Fatal("six profile left authority registered")
	}
	for _, name := range historyProfileNames {
		err := runtime.Tools().withAuthorized(ctx, token, name, func(store.MemberToolScope, MemberToolGrants) error { return nil })
		if !errors.Is(err, store.ErrMemberToolDenied) {
			t.Errorf("grant %s survived revoke: %v", name, err)
		}
	}
	argv, err := os.ReadFile(filepath.Join(request.CWD, "argv"))
	want := []string{"--offline", "--mode", "json", "-p", "--no-extensions", "--no-mcp", "--no-builtin-tools", "--no-skills", "--no-prompt-templates", "--no-context-files", "--no-approve", "--session-dir", filepath.Join(logs, turn.ID), "--session-id", turn.ID,
		"--tools", strings.Join(historyProfileNames, ","), "--extension", extension, "--model", f.member.Model}
	if err != nil || !slices.Equal(strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n"), want) {
		t.Fatal("Prepare factory discarded its supplied names or changed canonical argv")
	}
	for _, path := range []string{filepath.Join(request.CWD, "argv"), filepath.Join(request.CWD, "stdin"), receipt.StdoutPath, receipt.StderrPath} {
		data, err := os.ReadFile(path)
		if err != nil || strings.Contains(string(data), token) || strings.Contains(string(data), configPath) {
			t.Fatal("private capability or config path escaped into argv/prompt/logs")
		}
	}
	turn, err = f.s.GetTurn(ctx, f.group.ID, turn.ID)
	if err != nil || turn.State != domain.TurnStateRunning || turn.ClaimToken != claim.Token || turn.ReplyMessageID != "" {
		t.Fatal("profile handoff settled Turn or changed claim without receipt application")
	}
}
