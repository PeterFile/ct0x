package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
