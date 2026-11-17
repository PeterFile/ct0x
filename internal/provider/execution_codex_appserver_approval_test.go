package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCodexAppServerMemberToolsNativeApproval(t *testing.T) {
	for _, test := range []struct {
		name  string
		names []string
	}{
		{"ordinary", []string{"list_members", "read_channel", "send_message", "create_member", "search_history", "read_history"}},
		{"ordinary-subset", []string{"read_channel"}},
		{"coordinator", []string{"list_members", "read_channel", "search_history", "read_history", "decide_dispatch"}},
		{"worker", []string{"read_channel", "search_history", "read_history", "submit_result"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			selected, configPath, request := appServerMemberFixture(t, "native-approval")
			e, err := NewCodexAppServerMemberToolExecutor(selected, configPath, test.names, 5*time.Second)
			if err != nil {
				t.Fatal("valid issued member profile rejected")
			}
			r := e.Execute(context.Background(), request, nil)
			if r.Outcome != OutcomeSucceeded || !r.OutputComplete || !r.DirectChildReaped || r.ExitCode != 0 {
				t.Error("issued tools were blocked by native Never approval instead of reaching the synthetic platform")
			}
			var approved []string
			data, err := os.ReadFile(filepath.Join(request.CWD, "native-approved-tools.json"))
			if err != nil || json.Unmarshal(data, &approved) != nil || !reflect.DeepEqual(approved, test.names) {
				t.Error("not every issued tool crossed the native per-tool approval branch")
			}
			params := memberAppRoutingThread(t, request.CWD)
			config, err := codexAppServerMap(params["config"])
			if err != nil || string(params["approvalPolicy"]) != `"never"` || string(params["sandbox"]) != `"read-only"` ||
				string(config["approval_policy"]) != `"never"` || string(config["sandbox_mode"]) != `"read-only"` {
				t.Fatal("native approval mapping broadened the global approval or sandbox policy")
			}
			servers, err := codexAppServerMap(config["mcp_servers"])
			if err != nil || len(servers) != 3 {
				t.Fatal("native approval mapping changed the inherited server set")
			}
			active := 0
			for name, raw := range servers {
				settings, err := codexAppServerMap(raw)
				if err != nil || settings["default_tools_approval_mode"] != nil || settings["approval_mode"] != nil {
					t.Fatal("native approval mapping used a server-wide approval policy")
				}
				if string(settings["enabled"]) != "true" {
					if string(settings["enabled"]) != "false" || settings["tools"] != nil {
						t.Error("native approval mapping granted an inherited server tool policy")
					}
					continue
				}
				active++
				if !strings.HasPrefix(name, "aw_member_") || name == "aw_member_tools" {
					t.Error("native approval mapping was not bound to the unique selected server")
				}
				tools, err := codexAppServerMap(settings["tools"])
				if err != nil || len(tools) != len(test.names) {
					t.Error("native approvals were missing, wildcarded or extended beyond the issued tool set")
				}
				for _, tool := range test.names {
					policy, err := codexAppServerMap(tools[tool])
					if err != nil || len(policy) != 1 || string(policy["approval_mode"]) != `"approve"` {
						t.Error("issued raw tool identity did not have an exact per-tool Approve policy")
					}
				}
			}
			if active != 1 {
				t.Error("native approvals activated more than the selected server")
			}
			argv, err := os.ReadFile(filepath.Join(request.CWD, "argv"))
			if err != nil || strings.Contains(string(argv), "approval_mode") || !strings.Contains(string(argv), "features.code_mode_host=false") {
				t.Error("session-only native approval mapping entered CLI overrides or enabled the host")
			}
		})
	}
}

func TestCodexAppServerMemberToolsNativeApprovalKeepsUnknownOrRejectedUnknown(t *testing.T) {
	for _, test := range []struct{ mode, marker string }{
		{"native-approval-unknown-tool", "native-approval-denied"},
		{"native-approval-platform-rejected", "platform-rejected"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			selected, configPath, request := appServerMemberFixture(t, test.mode)
			e, err := NewCodexAppServerMemberToolExecutor(selected, configPath, []string{"read_channel"}, 5*time.Second)
			if err != nil {
				t.Fatal("valid issued member profile rejected")
			}
			r := e.Execute(context.Background(), request, nil)
			if r.Outcome != OutcomeUnknown || !r.Started || !r.DirectChildReaped || r.Terminal != "" {
				t.Error("native approval substituted for a valid issued tool or successful platform result")
			}
			if _, err := os.Stat(filepath.Join(request.CWD, test.marker)); err != nil {
				t.Error("native approval denial or synthetic platform rejection branch was not observed")
			}
			data, err := os.ReadFile(filepath.Join(request.CWD, "native-approved-tools.json"))
			if test.mode == "native-approval-unknown-tool" {
				if !os.IsNotExist(err) {
					t.Error("an unissued raw tool crossed native approval")
				}
			} else {
				var approved []string
				if err != nil || json.Unmarshal(data, &approved) != nil || !reflect.DeepEqual(approved, []string{"read_channel"}) {
					t.Error("synthetic platform refusal did not occur after native approval")
				}
			}
		})
	}
}

func TestCodexAppServerPlainDoesNotGainNativeApprovals(t *testing.T) {
	raw, _ := json.Marshal(codexAppServerConfig([]string{"workspace"}))
	config, err := codexAppServerMap(raw)
	servers, serverErr := codexAppServerMap(config["mcp_servers"])
	if err != nil || serverErr != nil || string(servers["workspace"]) != `{"enabled":false}` ||
		string(config["approval_policy"]) != `"never"` || string(config["sandbox_mode"]) != `"read-only"` {
		t.Fatal("plain executor gained member-native approvals")
	}
	for _, arg := range codexAppServerArguments() {
		if strings.Contains(arg, "approval_mode") {
			t.Fatal("native tool approval escaped into plain startup overrides")
		}
	}
}
