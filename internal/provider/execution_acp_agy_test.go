package provider

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const acpAgyFixtureScript = `
import json, os, subprocess, sys

mode = os.environ.get("AW_AGY_FIXTURE_MODE", "normal")
gemini_dir = ""
args = sys.argv[1:]
for i, a in enumerate(args):
    if a == "--gemini-dir" and i + 1 < len(args):
        gemini_dir = args[i + 1]

if "--stdio" not in args or "--sandbox" not in args or "--disable-telemetry" not in args or "--include-default-customizations=false" not in args or not gemini_dir:
    sys.exit(90)

run_dir = os.path.dirname(gemini_dir)
with open(os.path.join(gemini_dir, "aw-app", "aw-cli", "settings.json")) as f:
    settings = json.load(f)
if settings.get("toolPermission") != "strict" or not settings.get("enableTerminalSandbox") or settings.get("allowNonWorkspaceAccess") is not False:
    sys.exit(91)
if "command(*)" not in (settings.get("permissions") or {}).get("deny", []):
    sys.exit(92)
with open(os.path.join(gemini_dir, "config", "hooks.json")) as f:
    hooks = json.load(f)
gate = hooks["aw-strict-tool-gate"]["PreToolUse"][0]["hooks"][0]["command"]

server_name = ""
for line in sys.stdin:
    msg = json.loads(line)
    mid = msg.get("id")
    method = msg.get("method", "")
    if method == "initialize":
        sys.stdout.write(json.dumps({
            "jsonrpc": "2.0",
            "id": mid,
            "result": {
                "protocolVersion": 1,
                "agentCapabilities": {"loadSession": True},
                "agentInfo": {"name": "Antigravity ACP Server", "version": "1.5.16"},
            },
        }) + "\n")
        sys.stdout.flush()
    elif method == "session/new":
        mcp_servers = (msg.get("params") or {}).get("mcpServers") or []
        server_name = mcp_servers[0]["name"]
        # Verify PreToolUse gate script allows only exact issued MCP tool and denies native tools.
        p_ok = subprocess.run([gate], input=json.dumps({"toolCall": {"name": f"mcp_{server_name}_read_channel"}}), text=True, capture_output=True)
        p_no = subprocess.run([gate], input=json.dumps({"toolCall": {"name": "run_command"}}), text=True, capture_output=True)
        if json.loads(p_ok.stdout).get("decision") != "allow" or json.loads(p_no.stdout).get("decision") != "deny":
            sys.exit(93)
        avail = [{"modelId": "gemini-4-neon-high"}, {"modelId": "gemini-3.8-flash-low"}]
        if mode == "missing_model":
            avail = [{"modelId": "gemini-4-neon-high"}]
        sys.stdout.write(json.dumps({
            "jsonrpc": "2.0",
            "id": mid,
            "result": {
                "sessionId": "agy-session-1",
                "models": {"currentModelId": "gemini-4-neon-high", "availableModels": avail},
                "modes": {"currentModeId": "default"},
                "configOptions": [
                    {"id": "model", "currentValue": "gemini-4-neon"},
                    {"id": "thought_level", "currentValue": "high"},
                    {"id": "mode", "currentValue": "default"},
                ],
            },
        }) + "\n")
        sys.stdout.flush()
    elif method == "session/set_model":
        if (msg.get("params") or {}).get("modelId") != "gemini-3.8-flash-low":
            sys.exit(94)
        sys.stdout.write(json.dumps({
            "jsonrpc": "2.0",
            "method": "session/update",
            "params": {
                "sessionId": "agy-session-1",
                "update": {
                    "sessionUpdate": "config_option_update",
                    "configOptions": [
                        {"id": "model", "currentValue": "gemini-3.8-flash"},
                        {"id": "thought_level", "currentValue": "low"},
                        {"id": "mode", "currentValue": "default"},
                    ],
                },
            },
        }) + "\n")
        sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": mid, "result": {}}) + "\n")
        sys.stdout.flush()
    elif method == "session/prompt":
        if mode.startswith("cancel_"):
            sid = "unrelated" if mode == "cancel_wrong_session" else "agy-session-1"
            for chunk in ["", "AGY-IN-PROGRESS", "SECOND-CHUNK"]:
                print(json.dumps({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": sid, "update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": chunk}}}}), flush=True)
            if mode == "cancel_unsettled":
                print(json.dumps({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": sid, "update": {"sessionUpdate": "tool_call", "toolCallId": "unsettled", "title": "pending tool", "status": "in_progress"}}}), flush=True)
            cancel = json.loads(sys.stdin.readline())
            assert cancel == {"jsonrpc": "2.0", "method": "session/cancel", "params": {"sessionId": "agy-session-1"}}
            if mode != "cancel_no_result":
                stop = "end_turn" if mode == "cancel_raced_completion" else "cancelled"
                print(json.dumps({"jsonrpc": "2.0", "id": mid, "result": {"stopReason": stop}}), flush=True)
            break
        tool_title = f"mcp_{server_name}_read_channel"
        if mode == "rogue_tool":
            tool_title = "run_command"
        # Real agy emits tool_call(in_progress) -> tool_call_update(pending) -> permission -> in_progress -> completed
        sys.stdout.write(json.dumps({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": "agy-session-1", "update": {"sessionUpdate": "tool_call", "toolCallId": "tc1", "title": tool_title, "status": "in_progress"}}}) + "\n")
        sys.stdout.write(json.dumps({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": "agy-session-1", "update": {"sessionUpdate": "tool_call_update", "toolCallId": "tc1", "status": "pending"}}}) + "\n")
        sys.stdout.write(json.dumps({
            "jsonrpc": "2.0",
            "id": "perm-1",
            "method": "session/request_permission",
            "params": {
                "sessionId": "agy-session-1",
                "toolCall": {"toolCallId": "tc1", "title": tool_title},
                "options": [
                    {"optionId": "allow1", "kind": "allow_once"},
                    {"optionId": "reject1", "kind": "reject_once"},
                ],
            },
        }) + "\n")
        sys.stdout.flush()
        resp = json.loads(sys.stdin.readline())
        outcome = (resp.get("result") or {}).get("outcome") or {}
        if mode == "rogue_tool":
            if outcome.get("optionId") != "reject1":
                sys.exit(95)
            sys.stdout.write(json.dumps({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": "agy-session-1", "update": {"sessionUpdate": "tool_call_update", "toolCallId": "tc1", "status": "failed"}}}) + "\n")
        else:
            if outcome.get("optionId") != "allow1":
                sys.exit(96)
            sys.stdout.write(json.dumps({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": "agy-session-1", "update": {"sessionUpdate": "tool_call_update", "toolCallId": "tc1", "status": "in_progress"}}}) + "\n")
            sys.stdout.write(json.dumps({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": "agy-session-1", "update": {"sessionUpdate": "tool_call_update", "toolCallId": "tc1", "status": "completed"}}}) + "\n")
            sys.stdout.write(json.dumps({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": "agy-session-1", "update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "AGY-FIXTURE-OK"}}}}) + "\n")
        if mode == "tamper_hook":
            with open(gate, "a") as gf:
                gf.write("# tampered\n")
        sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": mid, "result": {"stopReason": "end_turn"}}) + "\n")
        sys.stdout.flush()
        break
`

func newAgyFixtureSetup(t *testing.T, mode string) (Adapter, Provider, ExecuteRequest) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	runtimeDir := filepath.Join(root, "runtime")
	cwdDir := filepath.Join(root, "cwd")
	for _, d := range []string{binDir, runtimeDir, cwdDir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	agyPath := filepath.Join(binDir, "agy")
	if err := os.WriteFile(agyPath, []byte("#!"+python+"\n"+acpAgyFixtureScript), 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "member.json")
	if err := os.WriteFile(configPath, []byte(`{"endpoint":"http://127.0.0.1:12345/api/v1/member-tools","token":"`+acpFixtureToken+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AW_AGY_FIXTURE_MODE", mode)
	p := Provider{ID: "agy_1", Name: Agy, Path: agyPath, Version: "1.5.16", Installation: Installed}
	adapter := NewAgyACPMemberToolAdapter(configPath, []string{"read_channel"})
	req := ExecuteRequest{RunID: "run_" + mode, CWD: cwdDir, RuntimeDir: runtimeDir, Model: "gemini-3.8-flash-low", Prompt: "ping"}
	return adapter, p, req
}

func TestAgyACPAdapterCapabilitiesAndValidation(t *testing.T) {
	dir := t.TempDir()
	bin := executable(t, filepath.Join(dir, "agy"), "exit 0")
	cfg := filepath.Join(dir, "member.json")
	if err := os.WriteFile(cfg, []byte(`{"endpoint":"http://127.0.0.1:1/api/v1/member-tools","token":"abc"}`), 0600); err != nil {
		t.Fatal(err)
	}
	a := NewAgyACPMemberToolAdapter("", []string{"read_channel"})
	p := Provider{ID: "agy_1", Name: Agy, Path: bin, Version: "1.5.16", Installation: Installed}
	caps, err := a.Capabilities(p)
	if err != nil {
		t.Fatalf("unexpected capabilities error: %v", err)
	}
	if !caps.Supports(CapabilityPlatformTools) || !caps.Supports(CapabilityTerminalEvidence) ||
		caps.NativeResume != SupportUnsupported || caps.Steer != SupportUnsupported ||
		caps.Evidence.Protocol != ACPV1Protocol {
		t.Fatalf("unexpected agy capabilities: %+v", caps)
	}
	if _, err := a.NewExecutor(p, time.Second); err == nil {
		t.Fatal("blank configPath must be rejected by NewExecutor")
	}
	for _, bad := range []Provider{
		{ID: "agy_1", Name: Agy, Path: bin, Version: "1.4.0", Installation: Installed},
		{ID: "pi_1", Name: Pi, Path: bin, Version: "1.5.16", Installation: Installed},
		{ID: "agy_1", Name: Agy, Path: bin, Version: "1.5.16", Installation: ""},
	} {
		if _, err := NewAgyACPMemberToolAdapter(cfg, []string{"read_channel"}).Capabilities(bad); err == nil {
			t.Fatalf("invalid provider accepted: %+v", bad)
		}
	}
	if !acpValidModel("gemini-3.8-flash-low") || acpValidModel("-bad") || acpValidModel("bad model") || acpValidModel("") {
		t.Fatal("unexpected model validation behavior")
	}
	if name, ok := acpAgyToolName("aw12", "read_channel"); !ok || name != "mcp_aw12_read_channel" {
		t.Fatalf("unexpected tool name: %q %v", name, ok)
	}
}

func TestAgyACPProjectExperienceBindsIssuedTools(t *testing.T) {
	dir := t.TempDir()
	bin := executable(t, filepath.Join(dir, "agy"), "exit 0")
	names, err := PiProjectExperienceToolNames([]string{"search_history", "read_history", ReadArtifactTool})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "member.json")
	raw, err := json.Marshal(map[string]any{"endpoint": "http://127.0.0.1:12345/api/v1/member-tools",
		"token": acpFixtureToken, "project_experience_tools": names})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	p := Provider{ID: "agy_fixture", Name: Agy, Path: bin, Version: "1.5.16", Installation: Installed}
	if _, err := NewAgyACPMemberToolAdapter(path, names).NewExecutor(p, time.Second); err != nil {
		t.Fatalf("issued project experience profile did not construct: %v", err)
	}
	if _, err := NewAgyACPMemberToolAdapter(path, names[:len(names)-1]).NewExecutor(p, time.Second); err == nil {
		t.Fatal("constructor accepted names that differ from the issued projection")
	}
	if err := validateACPConfig(ACPConfig{VendorProfile: ACPGemini068, Version: "0.68.0",
		Model: "example-model", AgentPath: bin, MemberToolConfigPath: path, ToolNames: names}); err == nil {
		t.Fatal("Gemini inherited the AGY project experience profile")
	}
}

func TestAgyFreshTaskWorkRequiresExactBinding(t *testing.T) {
	dir := t.TempDir()
	bin := executable(t, filepath.Join(dir, "agy"), "exit 0")
	path := filepath.Join(dir, "member.json")
	const endpoint = "http://127.0.0.1:12345/api/v1/member-tools"
	raw, err := json.Marshal(struct {
		Endpoint string `json:"endpoint"`
		Token    string `json:"token"`
	}{endpoint, acpFixtureToken})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	selected := Provider{ID: "agy_fixture", Name: Agy, Path: bin, Version: "1.5.16", Installation: Installed}
	binding := FreshTaskWorkBinding{ConfigPath: path, ConfigSHA256: FreshTaskWorkConfigSHA256(endpoint, acpFixtureToken),
		RunID: "exact_task_turn", Operations: PiFreshTaskWorkToolNames()}
	executor, err := NewAgyFreshTaskWorkExecutor(t.Context(), selected, binding, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result := executor.Execute(t.Context(), ExecuteRequest{RunID: "another_task_turn"}, nil); result.Started || result.Outcome != OutcomeUnknown {
		t.Fatalf("another Turn escaped the launch binding: %+v", result)
	}
	binding.ConfigSHA256[0] ^= 1
	if _, err := NewAgyFreshTaskWorkExecutor(t.Context(), selected, binding, time.Second); err == nil {
		t.Fatal("AGY accepted a changed issued config digest")
	}
}

func TestACPAgyFourLayerContainmentAndLifecycle(t *testing.T) {
	adapter, p, req := newAgyFixtureSetup(t, "normal")
	reg, err := NewAdapterRegistry(AdapterRegistration{ID: AgyACPAdapterID, Protocol: ACPV1Protocol, Adapter: adapter})
	if err != nil {
		t.Fatal(err)
	}
	exec, err := reg.NewExecutor(AdapterSelection{ID: AgyACPAdapterID, Protocol: ACPV1Protocol}, p, 5*time.Second, CapabilityPlatformTools, CapabilityTerminalEvidence)
	if err != nil {
		t.Fatalf("NewExecutor failed: %v", err)
	}
	var events []Event
	r := exec.Execute(context.Background(), req, func(e Event) error { events = append(events, e); return nil })
	if r.Outcome != OutcomeSucceeded || !r.Started || !r.Accepted || !r.OutputComplete || !r.DirectChildReaped || r.Terminal != OutcomeSucceeded {
		t.Fatalf("expected succeeded AGY ACP lifecycle, got %+v", r)
	}
	if len(events) != 4 || events[2].Kind != ResponseEvent || events[2].Text != "AGY-FIXTURE-OK" {
		t.Fatalf("unexpected events: %+v", events)
	}
}

func TestACPAgyRejectsUnauthorizedToolMissingModelAndTamperedHook(t *testing.T) {
	for _, mode := range []string{"rogue_tool", "missing_model", "tamper_hook"} {
		t.Run(mode, func(t *testing.T) {
			adapter, p, req := newAgyFixtureSetup(t, mode)
			exec, err := adapter.NewExecutor(p, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			r := exec.Execute(context.Background(), req, nil)
			if r.Outcome != OutcomeUnknown {
				t.Fatalf("mode %s must remain unknown, got %+v (logs: %s)", mode, r, func() string {
					b, _ := os.ReadFile(r.StdoutPath)
					return strings.TrimSpace(string(b))
				}())
			}
		})
	}
}

func TestACPAgyAcceptsWhilePromptIsRunning(t *testing.T) {
	adapter, p, req := newAgyFixtureSetup(t, "cancel_terminal")
	exec, err := adapter.NewExecutor(p, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accepted := 0
	r := exec.Execute(ctx, req, func(e Event) error {
		if e.Kind == AcceptedEvent && e.NativeSessionID == "agy-session-1" {
			accepted++
			cancel()
		}
		return nil
	})
	if accepted != 1 || !r.Accepted || r.Terminal != OutcomeAborted || !r.DirectChildReaped {
		t.Fatalf("expected one live acceptance before fixture returns, got %d events and %+v", accepted, r)
	}
	if r.Outcome != OutcomeUnknown {
		t.Fatalf("ordinary context cancellation must remain unknown: %+v", r)
	}
}

func TestACPAgyDurableCancellationRequiresSettledPrompt(t *testing.T) {
	for _, tc := range []struct {
		mode           string
		outcome        Outcome
		acceptedEvents int
	}{
		{"cancel_terminal", OutcomeAborted, 1},
		{"cancel_no_result", OutcomeUnknown, 1},
		{"cancel_unsettled", OutcomeUnknown, 1},
		{"cancel_raced_completion", OutcomeUnknown, 1},
		{"cancel_wrong_session", OutcomeUnknown, 0},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			adapter, p, req := newAgyFixtureSetup(t, tc.mode)
			exec, err := adapter.NewExecutor(p, 20*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			accepted := 0
			r := exec.Execute(ctx, req, func(e Event) error {
				if e.Kind == AcceptedEvent {
					if e.NativeSessionID != "agy-session-1" {
						t.Errorf("wrong accepted session %q", e.NativeSessionID)
					}
					accepted++
					cancel(ErrTurnCancelRequested)
				}
				if e.Kind == ResponseEvent {
					t.Error("interrupted prompt published a response")
				}
				return nil
			})
			if r.Outcome != tc.outcome || !r.DirectChildReaped || accepted != tc.acceptedEvents {
				t.Fatalf("expected outcome %s and %d acceptance events, got %d events and %+v", tc.outcome, tc.acceptedEvents, accepted, r)
			}
			if tc.outcome == OutcomeAborted && (r.Terminal != OutcomeAborted || !r.Accepted || !r.OutputComplete || r.ExitCode != 0) {
				t.Fatalf("aborted receipt was incomplete: %+v", r)
			}
		})
	}
}
