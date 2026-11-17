package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func memberAppRoutingThread(t *testing.T, cwd string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cwd, "client.jsonl"))
	if err != nil {
		t.Fatal("missing controlled native transcript")
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		message, err := decodeCodexRPCFrame(line)
		if err == nil && message.Method != nil && *message.Method == "thread/start" {
			fields, err := codexAppServerMap(message.Params)
			if err != nil {
				t.Fatal("invalid controlled thread request")
			}
			return fields
		}
	}
	t.Fatal("missing controlled thread request")
	return nil
}

func TestCodexAppServerMemberToolsDirectRouting(t *testing.T) {
	for _, test := range []struct {
		name, mode string
		names      []string
	}{
		{"ordinary", "direct-routing", []string{"read_channel"}},
		{"worker-inherited-pollution", "direct-routing-polluted", []string{"read_channel", "search_history", "read_history", "submit_result"}},
		{"coordinator-inherited-pollution", "direct-routing-polluted", []string{"list_members", "read_channel", "search_history", "read_history", "decide_dispatch"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			selected, configPath, request := appServerMemberFixture(t, test.mode)
			e, err := NewCodexAppServerMemberToolExecutor(selected, configPath, test.names, 5*time.Second)
			if err != nil {
				t.Fatal("valid direct member profile rejected")
			}
			r := e.Execute(context.Background(), request, nil)
			if r.Outcome != OutcomeSucceeded || !r.OutputComplete || !r.DirectChildReaped || r.ExitCode != 0 {
				t.Fatal("controlled direct MCP turn did not settle after drain and real Wait")
			}
			if _, err := os.Stat(filepath.Join(request.CWD, "direct-mcp-invoked")); err != nil {
				t.Error("nativeSuccess/Wait0 was text-only: selected MCP tools were still routed through the disabled host")
			}
			params := memberAppRoutingThread(t, request.CWD)
			if string(params["sandbox"]) != `"read-only"` || string(params["approvalPolicy"]) != `"never"` ||
				string(params["model"]) != string(mustAppServerFixtureJSON(request.Model)) {
				t.Fatal("direct routing weakened the native permission or selected-model binding")
			}
			config, err := codexAppServerMap(params["config"])
			if err != nil || params["dynamicTools"] != nil {
				t.Fatal("invalid direct routing configuration")
			}
			features, err := codexAppServerMap(config["features"])
			var codeMode struct {
				Enabled    *bool    `json:"enabled"`
				DirectOnly []string `json:"direct_only_tool_namespaces"`
			}
			if err != nil || json.Unmarshal(features["code_mode"], &codeMode) != nil || codeMode.Enabled == nil || *codeMode.Enabled ||
				codeMode.DirectOnly == nil || len(codeMode.DirectOnly) != 0 {
				t.Error("structured disabled code mode did not explicitly clear inherited global direct namespaces")
			}
			if string(features["code_mode_only_strict_3p_tools"]) != "false" {
				t.Error("inherited strict third-party deferral was not explicitly disabled")
			}
			for _, name := range codexAppServerDisabledFeatures {
				if name != "code_mode" && string(features[name]) != "false" {
					t.Error("direct routing enabled a restricted native feature")
				}
			}
			servers, err := codexAppServerMap(config["mcp_servers"])
			if err != nil || len(servers) != 3 {
				t.Fatal("direct routing changed the inherited server set")
			}
			active := 0
			for name, raw := range servers {
				var settings struct {
					Enabled *bool    `json:"enabled"`
					Omit    []string `json:"omit_tools_from"`
					Names   []string `json:"enabled_tools"`
				}
				if json.Unmarshal(raw, &settings) != nil || settings.Enabled == nil {
					t.Fatal("server activation was not explicit")
				}
				if !*settings.Enabled {
					if settings.Omit != nil || settings.Names != nil {
						t.Error("inherited server was given a direct routing policy or tool grant")
					}
					continue
				}
				active++
				if !strings.HasPrefix(name, "aw_member_") || name == "aw_member_tools" ||
					!reflect.DeepEqual(settings.Omit, []string{"code_mode", "deferred"}) || !reflect.DeepEqual(settings.Names, test.names) {
					t.Error("direct-only exposure escaped the unique selected server and issued profile")
				}
			}
			if active != 1 {
				t.Error("direct routing activated an inherited server")
			}
			argv, err := os.ReadFile(filepath.Join(request.CWD, "argv"))
			if err != nil || !strings.Contains(string(argv), "features.code_mode=false") || !strings.Contains(string(argv), "features.code_mode_host=false") ||
				strings.Contains(string(argv), "omit_tools_from") || strings.Contains(string(argv), "direct_only_tool_namespaces") {
				t.Error("startup overrides replaced session-only routing or enabled the host")
			}
		})
	}
}

func TestCodexAppServerMemberToolsInventoryBindsIssuedTools(t *testing.T) {
	for _, mode := range []string{"status-tools-missing", "status-tools-name", "status-tools-null"} {
		t.Run(mode, func(t *testing.T) {
			selected, configPath, request := appServerMemberFixture(t, mode)
			e, err := NewCodexAppServerMemberToolExecutor(selected, configPath, []string{"read_channel", "list_members"}, 5*time.Second)
			if err != nil {
				t.Fatal("valid member profile rejected")
			}
			r := e.Execute(context.Background(), request, nil)
			data, _ := os.ReadFile(filepath.Join(request.CWD, "client.jsonl"))
			if r.Outcome != OutcomeUnknown || r.Accepted || !r.DirectChildReaped || strings.Contains(string(data), `"method":"turn/start"`) {
				t.Error("model turn started without matching selected-server raw tool inventory")
			}
		})
	}
}

func TestCodexAppServerPlainDoesNotGainDirectNamespaces(t *testing.T) {
	raw, _ := json.Marshal(codexAppServerConfig([]string{"workspace"}))
	config, err := codexAppServerMap(raw)
	features, featureErr := codexAppServerMap(config["features"])
	servers, serverErr := codexAppServerMap(config["mcp_servers"])
	if err != nil || featureErr != nil || serverErr != nil || string(features["code_mode"]) != "false" ||
		features["code_mode_only_strict_3p_tools"] != nil || string(servers["workspace"]) != `{"enabled":false}` {
		t.Fatal("plain executor acquired member-specific direct routing")
	}
	for _, arg := range codexAppServerArguments() {
		if strings.Contains(arg, "omit_tools_from") || strings.Contains(arg, "direct_only_tool_namespaces") {
			t.Fatal("session-only direct routing entered plain startup overrides")
		}
	}
}
