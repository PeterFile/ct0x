package provider

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestCodexAppServerPassiveRemoteControlStatus(t *testing.T) {
	// Root's installed-0.166 initialize/config preflight observed this method.
	// Its evidence omits params; these synthetic values follow the local schema.
	for _, mode := range []string{"remote-status", "remote-bad-status", "unknown-connection", "early-thread-status"} {
		t.Run(mode, func(t *testing.T) {
			e, request := appServerFixture(t, mode)
			var kinds []EventKind
			receipt := e.Execute(context.Background(), request, func(event Event) error { kinds = append(kinds, event.Kind); return nil })
			if mode == "remote-status" || mode == "early-thread-status" {
				if receipt.Outcome != OutcomeSucceeded || len(kinds) != 4 || kinds[0] != SessionEvent || kinds[1] != AcceptedEvent {
					t.Fatalf("passive connection status interrupted the real handshake or produced execution evidence: %+v events=%v", receipt, kinds)
				}
			} else if receipt.Outcome != OutcomeUnknown || receipt.Accepted || len(kinds) != 0 {
				t.Fatalf("unknown connection method/status was silently trusted: %+v events=%v", receipt, kinds)
			}
			data, _ := os.ReadFile(filepath.Join(request.CWD, "client.jsonl"))
			if strings.Contains(string(data), "remoteControl/") {
				t.Fatal("passive status caused a remote-control action")
			}
		})
	}
}

const appServerCompatThreadResponse = `{"id":3,"result":{"thread":{"id":"native-app-thread","turns":[],"ephemeral":false},"approvalPolicy":"never","sandbox":{"type":"readOnly","networkAccess":false},"cwd":"/synthetic/workspace"}}`

// Replay safe protocol shapes, not original account/config values. No CLI or
// model is invoked; the existing stdio fixtures cover unchanged process cleanup.
func appServerCompatThreadPending(t *testing.T) (*codexAppServerProtocol, func(string), func() []Event) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var events []Event
	p := &codexAppServerProtocol{
		ctx: ctx, cancel: cancel,
		request: ExecuteRequest{CWD: "/synthetic/workspace", Prompt: "synthetic prompt"},
		receipt: &ExecutionReceipt{Started: true, Outcome: OutcomeUnknown},
		items:   make(map[string]codexAppServerItem), requests: make(map[string]bool),
		send: func(_ int, method string, params any) bool {
			if method == "config/read" && params.(map[string]any)["includeLayers"] != true {
				t.Error("full config layer metadata was not requested")
			}
			return true
		},
		reply:   func(codexRPCMessage, any) bool { t.Error("passive metadata caused a reply or grant"); return false },
		summary: func(codexAppServerConfigSnapshot) {},
	}
	p.delivery = newExecutionDelivery(ctx, cancel, func(event Event) error { events = append(events, event); return nil })
	finish := sync.OnceFunc(func() { p.delivery.finish() })
	t.Cleanup(func() { cancel(); finish() })
	receive := func(raw string) {
		t.Helper()
		message, err := decodeCodexRPCFrame([]byte(raw))
		if err != nil {
			t.Fatal("invalid synthetic protocol frame")
		}
		p.receive(message)
	}
	p.next(1, "initialize", map[string]any{})
	for _, frame := range []string{
		`{"id":1,"result":{"userAgent":"synthetic","platformFamily":"unix","platformOs":"synthetic","codexHome":"/synthetic/home"}}`,
		`{"method":"configWarning","params":{"summary":"synthetic warning"}}`,
		`{"method":"remoteControl/status/changed","params":{"installationId":"synthetic","serverName":"synthetic","status":"disabled"}}`,
		`{"id":2,"result":{"config":{"mcp_servers":{}},"origins":{},"layers":[{"name":{"type":"sessionFlags"},"version":"flags-v1","config":{}}]}}`,
	} {
		receive(frame)
	}
	if p.damaged || p.awaiting != 3 || p.threadID != "" || p.receipt.Accepted {
		t.Fatal("synthetic handshake did not reach pending thread/start")
	}
	return p, receive, func() []Event { finish(); return events }
}

func TestCodexAppServerAccountUpdateDuringThreadStart(t *testing.T) {
	p, receive, finish := appServerCompatThreadPending(t)
	before := *p.receipt
	receive(`{"method":"account/updated","params":{"authMode":"chatgpt","planType":"plus"}}`)
	if p.damaged || p.ctx.Err() != nil || p.awaiting != 3 || *p.receipt != before {
		t.Fatal("passive account/updated interrupted pending thread/start or created execution evidence")
	}
	receive(appServerCompatThreadResponse)
	for _, frame := range []string{
		`{"id":4,"result":{"config":{"mcp_servers":{}},"origins":{},"layers":[{"name":{"type":"sessionFlags"},"version":"flags-v1","config":{}}]}}`,
		`{"id":5,"result":{"turn":{"id":"native-app-turn","status":"inProgress","items":[]}}}`,
		`{"method":"turn/started","params":{"threadId":"native-app-thread","turn":{"id":"native-app-turn","status":"inProgress","items":[]}}}`,
		`{"method":"item/completed","params":{"threadId":"native-app-thread","turnId":"native-app-turn","item":{"id":"answer","type":"agentMessage","text":"synthetic answer"}}}`,
		`{"method":"turn/completed","params":{"threadId":"native-app-thread","turn":{"id":"native-app-turn","status":"completed","items":[]}}}`,
	} {
		receive(frame)
	}
	events := finish()
	var kinds []EventKind
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	if p.damaged || !p.settled() || !p.receipt.Accepted || p.receipt.Terminal != OutcomeSucceeded ||
		!reflect.DeepEqual(kinds, []EventKind{SessionEvent, AcceptedEvent, ResponseEvent, TerminalEvent}) {
		t.Fatal("account update prevented the bound synthetic turn from completing")
	}
	if p.receipt.Outcome != OutcomeUnknown || p.receipt.DirectChildReaped || p.receipt.OutputComplete {
		t.Fatal("protocol replay substituted for a real process wait or EOF")
	}
}

func TestCodexAppServerTerminalItemStatusContradiction(t *testing.T) {
	p, receive, _ := appServerCompatThreadPending(t)
	for _, raw := range []string{
		appServerCompatThreadResponse,
		`{"id":4,"result":{"config":{"mcp_servers":{}},"origins":{},"layers":[{"name":{"type":"sessionFlags"},"version":"flags-v1","config":{}}]}}`,
		`{"id":5,"result":{"turn":{"id":"native-app-turn","status":"inProgress","items":[],"error":null}}}`,
		`{"method":"turn/started","params":{"threadId":"native-app-thread","turn":{"id":"native-app-turn","status":"inProgress","items":[],"error":null}}}`,
		`{"method":"item/completed","params":{"threadId":"native-app-thread","turnId":"native-app-turn","item":{"id":"command","type":"commandExecution","status":"completed","command":"synthetic","cwd":"/synthetic/work","commandActions":[],"processId":null,"aggregatedOutput":"","exitCode":0,"durationMs":1}}}`,
		`{"method":"turn/completed","params":{"threadId":"native-app-thread","turn":{"id":"native-app-turn","status":"completed","items":[{"id":"command","type":"commandExecution","status":"inProgress","command":"synthetic","cwd":"/synthetic/work","commandActions":[],"processId":null,"aggregatedOutput":"","exitCode":0,"durationMs":1}],"error":null}}}`,
	} {
		receive(raw)
	}
	if !p.damaged || p.receipt.Terminal != "" {
		t.Fatal("terminal snapshot contradicted a known item's native status")
	}
}

func TestCodexAppServerPassiveMetadataIsNotAuthority(t *testing.T) {
	for _, raw := range []string{
		`{"method":"account/updated","params":{}}`,
		`{"method":"account/updated","params":{"authMode":null,"planType":null}}`,
		`{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"usedPercent":10}}}}`,
		`{"method":"warning","params":{"threadId":null,"message":"synthetic warning"}}`,
		`{"method":"thread/name/updated","params":{"threadId":"native-app-thread","threadName":"synthetic name"}}`,
		`{"method":"thread/project/updated","params":{"threadId":"native-app-thread","projectId":null}}`,
	} {
		p, receive, finish := appServerCompatThreadPending(t)
		receive(raw)
		receive(appServerCompatThreadResponse)
		if p.damaged || p.ctx.Err() != nil || p.awaiting != 4 || p.receipt.Accepted || p.receipt.Terminal != "" {
			t.Fatal("passive metadata damaged the session or supplied acceptance/terminal evidence")
		}
		if events := finish(); len(events) != 1 || events[0].Kind != SessionEvent {
			t.Fatal("passive metadata produced a model wakeup or execution event")
		}
	}
	for _, raw := range []string{
		`{"id":99,"method":"account/updated","params":{"authMode":"chatgpt"}}`,
		`{"method":"account/updated","params":{"authMode":true}}`,
		`{"method":"account/updated","params":{"authMode":"futureAuth"}}`,
		`{"method":"account/updated","params":{"threadId":"other-thread"}}`,
		`{"method":"account/rateLimits/updated","params":{"rateLimits":null}}`,
		`{"method":"account/futureUpdated","params":{}}`,
		`{"id":99,"method":"thread/name/updated","params":{"threadId":"native-app-thread","threadName":"synthetic"}}`,
		`{"method":"thread/name/updated","params":{"threadId":"other-thread","threadName":null}}`,
		`{"method":"thread/project/updated","params":{"threadId":"native-app-thread","projectId":false}}`,
		`{"method":"thread/settings/updated","params":{"threadId":"native-app-thread","approvalPolicy":"never"}}`,
		`{"method":"warning","params":{"threadId":"other-thread","message":"synthetic warning"}}`,
	} {
		p, receive, _ := appServerCompatThreadPending(t)
		receive(raw)
		receive(appServerCompatThreadResponse)
		if !p.damaged || p.ctx.Err() == nil || p.receipt.Accepted || p.receipt.Terminal != "" {
			t.Fatal("request, invalid metadata, unknown method or cross-session identity was silently trusted")
		}
	}
}

func TestCodexAppServerStartupOverridesDenyExternalCapabilities(t *testing.T) {
	args := codexAppServerArguments()
	values := make(map[string]bool)
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-c" {
			values[args[i+1]] = true
		}
	}
	for _, restriction := range []string{
		"features.plugins=false", "features.apps=false", "features.hooks=false", "features.shell_tool=false",
		"features.multi_agent=false", "features.multi_agent_v2=false", "features.code_mode=false", "features.code_mode_only=false",
		"features.code_mode_host=false", "features.tool_suggest=false", "agents.enabled=false", "notify=[]",
		`web_search="disabled"`, `approval_policy="never"`, `sandbox_mode="read-only"`,
	} {
		if !values[restriction] {
			t.Errorf("startup restriction missing: %s", restriction)
		}
	}
	for _, arg := range args {
		if arg == "--ignore-user-config" || strings.HasPrefix(arg, "mcp_servers=") {
			t.Fatal("unsupported or recursively merged pseudo-isolation was used")
		}
	}
}
