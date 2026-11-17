package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This process replays installed-0.166 shapes without a CLI, model or network.
func TestCodexAppServerMemberProcessHelper(t *testing.T) {
	mode := os.Getenv("AW_MEMBER_APP_FIXTURE")
	if mode == "" {
		return
	}
	defer os.Exit(0)
	input := bufio.NewReader(os.Stdin)
	readFrame := func() codexRPCMessage {
		line, err := input.ReadBytes('\n')
		if err != nil {
			os.Exit(0)
		}
		message, err := decodeCodexRPCFrame(line)
		if err != nil {
			os.Exit(8)
		}
		file, err := os.OpenFile("client.jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(8)
		}
		_, _ = file.Write(line)
		_ = file.Close()
		return message
	}
	read := func(method string) codexRPCMessage {
		message := readFrame()
		if message.Method == nil || *message.Method != method {
			os.Exit(8)
		}
		return message
	}
	send := func(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }
	respond := func(request codexRPCMessage, value any) { send(map[string]any{"id": request.ID, "result": value}) }
	notify := func(method string, params any) { send(map[string]any{"method": method, "params": params}) }
	config := func() any {
		inherited := map[string]any{"mcp_servers": map[string]any{
			"workspace":       map[string]any{"command": "must-not-execute", "env": map[string]string{"TOKEN": memberToolTestToken}},
			"aw_member_tools": map[string]any{"url": "https://synthetic.invalid", "bearer_token": memberToolTestToken},
		}}
		if mode == "direct-routing-polluted" {
			inherited["features"] = map[string]any{
				"code_mode":                      map[string]any{"enabled": true, "direct_only_tool_namespaces": []string{"functions", "mcp__workspace", "*"}},
				"code_mode_only_strict_3p_tools": true, "non_prefixed_mcp_tool_names": true,
			}
		}
		return map[string]any{"config": inherited, "origins": map[string]any{"mcp_servers": map[string]any{"name": map[string]string{"type": "sessionFlags"}, "version": "flags-v1"}},
			"layers": []any{map[string]any{"name": map[string]string{"type": "sessionFlags"}, "version": "flags-v1", "config": map[string]any{}}}}
	}
	respond(read("initialize"), map[string]string{"userAgent": "synthetic", "platformFamily": "unix", "platformOs": "synthetic", "codexHome": "/synthetic/original-home"})
	notify("configWarning", map[string]any{"summary": "synthetic", "details": memberToolTestToken})
	notify("remoteControl/status/changed", map[string]any{"environmentId": nil, "installationId": "synthetic", "serverName": "synthetic", "status": "disabled"})
	read("initialized")
	respond(read("config/read"), config())
	threadRequest := read("thread/start")
	var params struct {
		ApprovalPolicy string `json:"approvalPolicy"`
		Sandbox        string `json:"sandbox"`
		Config         struct {
			Features map[string]json.RawMessage `json:"features"`
			Servers  map[string]struct {
				Enabled   bool     `json:"enabled"`
				URL       string   `json:"url"`
				TokenEnv  string   `json:"bearer_token_env_var"`
				Tools     []string `json:"enabled_tools"`
				Omit      []string `json:"omit_tools_from"`
				Approvals map[string]struct {
					Mode string `json:"approval_mode"`
				} `json:"tools"`
			} `json:"mcp_servers"`
		} `json:"config"`
	}
	_ = json.Unmarshal(threadRequest.Params, &params)
	server := "workspace"
	for name, settings := range params.Config.Servers {
		if settings.Enabled {
			server = name
			if os.Getenv(settings.TokenEnv) == memberToolTestToken {
				_ = os.WriteFile("child-token-bound", nil, 0600)
			}
		}
	}
	_ = os.WriteFile("inherited-home", []byte(os.Getenv("HOME")+"\n"+os.Getenv("CODEX_HOME")), 0600)
	_ = os.WriteFile("argv", mustAppServerFixtureJSON(os.Args[stringsIndexAppServerFixtureArgs(os.Args):]), 0600)
	cwd, _ := os.Getwd()
	model := "synthetic/model"
	if mode == "model-fallback" {
		model = "synthetic/fallback"
	} else if mode == "missing-model" {
		model = ""
	}
	notify("account/updated", map[string]any{"authMode": "chatgpt", "planType": "plus"})
	if mode == "early-startup" {
		notify("mcpServer/startupStatus/updated", map[string]any{"threadId": appServerFixtureThread, "name": server, "status": "starting", "error": nil, "failureReason": nil})
		notify("mcpServer/startupStatus/updated", map[string]any{"threadId": appServerFixtureThread, "name": server, "status": "ready", "error": nil, "failureReason": nil})
	}
	respond(threadRequest, map[string]any{"thread": map[string]any{"id": appServerFixtureThread, "turns": []any{}, "ephemeral": false},
		"approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "cwd": cwd, "model": model, "activePermissionProfile": nil, "runtimeWorkspaceRoots": []string{cwd}})
	notify("thread/started", map[string]any{"thread": map[string]string{"id": appServerFixtureThread}})
	notify("warning", map[string]any{"threadId": appServerFixtureThread, "message": memberToolTestToken})
	if mode != "early-startup" {
		notify("mcpServer/startupStatus/updated", map[string]any{"threadId": appServerFixtureThread, "name": server, "status": "starting", "error": nil, "failureReason": nil})
	}
	respond(read("config/read"), config())
	// A selected-server request must never initialize inherited servers.
	request := read("mcpServerStatus/list")
	var selection map[string]any
	_ = json.Unmarshal(request.Params, &selection)
	if selection["serverName"] != server || selection["threadId"] != appServerFixtureThread {
		os.Exit(8)
	}
	if mode == "elicitation" || mode == "foreign-elicitation" {
		name := server
		if mode == "foreign-elicitation" {
			name = "workspace"
		}
		send(map[string]any{"id": "elicit", "method": "mcpServer/elicitation/request", "params": map[string]any{"threadId": appServerFixtureThread, "turnId": nil, "serverName": name, "mode": "url", "url": "https://synthetic.invalid/" + memberToolTestToken, "message": memberToolTestToken, "elicitationId": "synthetic"}})
		denial := readFrame()
		if string(denial.ID) != `"elicit"` || mode == "elicitation" && string(denial.Result) != `{"action":"cancel","content":null}` || mode == "foreign-elicitation" && denial.Error == nil {
			os.Exit(8)
		}
		_ = os.WriteFile("elicitation-denied", nil, 0600)
		_, _ = io.Copy(io.Discard, input)
		return
	}
	tools := make(map[string]any)
	for _, name := range params.Config.Servers[server].Tools {
		tools[name] = map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}}
	}
	status := map[string]any{"name": server, "runtimeStatus": "connected", "httpOrigin": "http://127.0.0.1:1", "authStatus": "bearerToken", "toolsError": nil,
		"tools": tools, "resources": []any{}, "resourceTemplates": []any{}}
	switch mode {
	case "status-origin":
		status["httpOrigin"] = "http://127.0.0.1:2"
	case "status-name":
		status["name"] = "workspace"
	case "status-failed":
		status["runtimeStatus"] = "failed"
	case "status-tools-error":
		status["toolsError"] = memberToolTestToken
	case "status-tools-missing":
		delete(tools, "read_channel")
	case "status-tools-name":
		tools["read_channel"] = map[string]string{"name": "send_message"}
	case "status-tools-null":
		status["tools"] = nil
	}
	if mode == "startup-failed" || mode == "startup-foreign" {
		name, state := server, "failed"
		if mode == "startup-foreign" {
			name, state = "workspace", "ready"
		}
		notify("mcpServer/startupStatus/updated", map[string]any{"threadId": appServerFixtureThread, "name": name, "status": state, "error": memberToolTestToken})
	}
	respond(request, map[string]any{"data": []any{status}, "nextCursor": nil})
	if mode != "early-startup" {
		notify("mcpServer/startupStatus/updated", map[string]any{"threadId": appServerFixtureThread, "name": server, "status": "ready", "error": nil, "failureReason": nil})
	}
	turn := func(status string, items []any) map[string]any {
		return map[string]any{"id": appServerFixtureTurn, "status": status, "items": items, "error": nil}
	}
	respond(read("turn/start"), map[string]any{"turn": turn("inProgress", []any{})})
	notify("turn/started", map[string]any{"threadId": appServerFixtureThread, "turn": turn("inProgress", []any{})})
	if strings.HasPrefix(mode, "direct-routing") {
		// Replay a model-selected code-mode/search surface with the host disabled.
		// A native text terminal can succeed without any actual MCP invocation.
		direct := string(params.Config.Features["code_mode_only_strict_3p_tools"]) == "false" &&
			reflect.DeepEqual(params.Config.Servers[server].Omit, []string{"code_mode", "deferred"})
		if !direct {
			answer := map[string]string{"id": "answer", "type": "agentMessage", "text": "code-mode host is disabled"}
			notify("item/completed", map[string]any{"threadId": appServerFixtureThread, "turnId": appServerFixtureTurn, "item": answer})
			notify("turn/completed", map[string]any{"threadId": appServerFixtureThread, "turn": turn("completed", []any{answer})})
			_, _ = io.Copy(io.Discard, input)
			_ = os.WriteFile("stdin-eof", nil, 0600)
			return
		}
		_ = os.WriteFile("direct-mcp-invoked", nil, 0600)
	}
	if strings.HasPrefix(mode, "native-approval") {
		if params.ApprovalPolicy != "never" || params.Sandbox != "read-only" {
			os.Exit(8)
		}
		selected := params.Config.Servers[server]
		names := selected.Tools
		if mode == "native-approval-unknown-tool" {
			names = []string{"future_member_tool"}
		}
		approved := []string{}
		items := []any{}
		failed := false
		for _, name := range names {
			// Replay 0.166's per-tool Approve branch with Never/ManagedReadOnly.
			// Missing approval stops before the synthetic platform invocation.
			failure := ""
			if selected.Approvals[name].Mode != "approve" {
				failure = "MCP tool call requires approval, but approval policy is never"
				_ = os.WriteFile("native-approval-denied", nil, 0600)
			} else {
				approved = append(approved, name)
				_ = os.WriteFile("native-approved-tools.json", mustAppServerFixtureJSON(approved), 0600)
				if mode == "native-approval-platform-rejected" {
					failure = "synthetic platform token/claim rejected"
					_ = os.WriteFile("platform-rejected", nil, 0600)
				}
			}
			item := map[string]any{"id": "approval-call-" + name, "type": "mcpToolCall", "server": server, "tool": name,
				"status": "inProgress", "arguments": map[string]any{}, "result": nil, "error": nil}
			notify("item/started", map[string]any{"threadId": appServerFixtureThread, "turnId": appServerFixtureTurn, "item": item})
			if failure != "" {
				item["status"], item["error"] = "failed", map[string]string{"message": failure}
			} else {
				item["status"], item["result"] = "completed", map[string]any{"content": []any{map[string]string{"type": "text", "text": "synthetic approved member result"}}}
			}
			notify("item/completed", map[string]any{"threadId": appServerFixtureThread, "turnId": appServerFixtureTurn, "item": item})
			items = append(items, item)
			if failure != "" {
				failed = true
				break
			}
		}
		if !failed {
			answer := map[string]string{"id": "answer", "type": "agentMessage", "text": "synthetic approved member answer"}
			notify("item/completed", map[string]any{"threadId": appServerFixtureThread, "turnId": appServerFixtureTurn, "item": answer})
			items = append(items, answer)
		}
		notify("turn/completed", map[string]any{"threadId": appServerFixtureThread, "turn": turn("completed", items)})
		_, _ = io.Copy(io.Discard, input)
		_ = os.WriteFile("stdin-eof", nil, 0600)
		return
	}
	item := map[string]any{"id": "member-call", "type": "mcpToolCall", "server": server, "tool": "read_channel", "status": "inProgress", "arguments": map[string]int{"limit": 1}, "result": nil, "error": nil}
	itemEvent := func(method string) {
		thread, turn := appServerFixtureThread, appServerFixtureTurn
		if mode == "cross-thread" {
			thread = "foreign-thread"
		} else if mode == "cross-turn" {
			turn = "foreign-turn"
		}
		notify(method, map[string]any{"threadId": thread, "turnId": turn, "item": item})
	}
	if mode == "foreign-server" {
		item["server"] = "workspace"
	} else if mode == "foreign-tool" {
		item["tool"] = "send_message"
	} else if mode == "native-external" {
		item["type"] = "commandExecution"
	}
	if mode != "no-start" {
		itemEvent("item/started")
	}
	progressID := "member-call"
	if mode == "orphan-progress" {
		progressID = "unrequested-item"
	}
	notify("item/mcpToolCall/progress", map[string]any{"threadId": appServerFixtureThread, "turnId": appServerFixtureTurn, "itemId": progressID, "message": memberToolTestToken})
	if mode == "unknown-notification" {
		notify("mcpServer/futureNotification", map[string]any{"threadId": appServerFixtureThread, "turnId": appServerFixtureTurn, "token": memberToolTestToken})
	}
	item["status"], item["result"] = "completed", map[string]any{"content": []any{map[string]string{"type": "text", "text": memberToolTestToken}}}
	switch mode {
	case "pending":
		item["status"] = "inProgress"
	case "failed":
		item["status"], item["result"], item["error"] = "failed", nil, map[string]string{"message": memberToolTestToken}
	case "completed-error":
		item["error"] = map[string]string{"message": memberToolTestToken}
	case "nontext-result":
		item["result"] = map[string]any{"content": []any{map[string]string{"type": "image", "data": "synthetic", "mimeType": "image/png"}}}
	case "changed-tool":
		item["tool"] = "list_members"
	case "changed-id":
		item["id"] = "different-item"
	}
	if mode != "pending" {
		itemEvent("item/completed")
	}
	answer := map[string]string{"id": "answer", "type": "agentMessage", "text": "合成成员工具回答"}
	notify("item/completed", map[string]any{"threadId": appServerFixtureThread, "turnId": appServerFixtureTurn, "item": answer})
	// Native diagnostics may split a token over arbitrary writes.
	_, _ = os.Stderr.Write([]byte(memberToolTestToken[:9]))
	_, _ = os.Stderr.Write([]byte(memberToolTestToken[9:]))
	if mode == "missing-terminal" {
		return
	}
	if mode == "terminal-status" {
		item["status"] = "failed"
	} else if mode == "terminal-server" {
		item["server"] = "workspace"
	} else if mode == "terminal-tool" {
		item["tool"] = "list_members"
	}
	notify("turn/completed", map[string]any{"threadId": appServerFixtureThread, "turn": turn("completed", []any{item, answer})})
	_, _ = io.Copy(io.Discard, input)
	_ = os.WriteFile("stdin-eof", nil, 0600)
}

func appServerMemberFixture(t *testing.T, mode string) (Provider, string, ExecuteRequest) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("export AW_MEMBER_APP_FIXTURE=%s\nexec '%s' -test.run='^TestCodexAppServerMemberProcessHelper$' -- \"$@\"\n", mode, strings.ReplaceAll(self, "'", "'\\''"))
	selected, _, config := memberToolFiles(t, body)
	selected.Name, selected.Version = Codex, "0.166.0"
	request := ExecuteRequest{RunID: "member1", CWD: t.TempDir(), RuntimeDir: t.TempDir(), Prompt: "synthetic member prompt", Model: "synthetic/model"}
	if os.Chmod(request.RuntimeDir, 0700) != nil {
		t.Fatal("private runtime unavailable")
	}
	return selected, config, request
}

func TestCodexAppServerMemberToolsControlledTurn(t *testing.T) {
	selected, config, request := appServerMemberFixture(t, "normal")
	e, err := NewCodexAppServerMemberToolExecutor(selected, config, []string{"read_channel"}, 5*time.Second)
	if err != nil {
		t.Fatal("valid member profile rejected")
	}
	home, codexHome := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	var kinds []EventKind
	r := e.Execute(context.Background(), request, func(event Event) error { kinds = append(kinds, event.Kind); return nil })
	if r.Outcome != OutcomeSucceeded || !r.OutputComplete || !r.DirectChildReaped || r.ExitCode != 0 || !reflect.DeepEqual(kinds, []EventKind{SessionEvent, AcceptedEvent, ResponseEvent, TerminalEvent}) {
		t.Fatal("selected native MCP lifecycle did not settle after terminal, drain and real Wait")
	}
	for _, marker := range []string{"child-token-bound", "stdin-eof"} {
		if _, err := os.Stat(filepath.Join(request.CWD, marker)); err != nil {
			t.Error("child environment or stdin shutdown was not observed")
		}
	}
	data, _ := os.ReadFile(filepath.Join(request.CWD, "inherited-home"))
	if string(data) != home+"\n"+codexHome {
		t.Error("original CLI authentication homes changed")
	}
	visible, _ := json.Marshal(r)
	for _, path := range []string{r.StdoutPath, r.StderrPath, filepath.Join(request.CWD, "argv"), filepath.Join(request.CWD, "client.jsonl")} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("missing native transcript or private log")
		}
		if (path == r.StdoutPath || path == r.StderrPath) && (strings.Contains(string(data), "/api/v1/member-tools") || strings.Contains(string(data), "合成成员工具回答")) {
			t.Error("raw native response or config entered lifecycle logs")
		}
		visible = append(visible, data...)
	}
	if strings.Contains(string(visible), memberToolTestToken) {
		t.Fatal("platform token leaked outside the child environment")
	}
}

func memberAppThreadConfig(t *testing.T, cwd string) (string, string) {
	t.Helper()
	file, err := os.Open(filepath.Join(cwd, "client.jsonl"))
	if err != nil {
		t.Fatal("missing synthetic client transcript")
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		message, err := decodeCodexRPCFrame(scanner.Bytes())
		if err != nil || message.Method == nil || *message.Method != "thread/start" {
			continue
		}
		var params map[string]json.RawMessage
		_ = json.Unmarshal(message.Params, &params)
		if params["dynamicTools"] != nil {
			t.Fatal("native dynamic tools were registered")
		}
		var config struct {
			Servers map[string]struct {
				Enabled  *bool    `json:"enabled"`
				URL      string   `json:"url"`
				TokenEnv string   `json:"bearer_token_env_var"`
				Names    []string `json:"enabled_tools"`
			} `json:"mcp_servers"`
		}
		if json.Unmarshal(params["config"], &config) != nil || len(config.Servers) != 3 {
			t.Fatal("inherited MCP names were dropped or merged with the platform server")
		}
		for _, inherited := range []string{"workspace", "aw_member_tools"} {
			settings := config.Servers[inherited]
			if settings.Enabled == nil || *settings.Enabled {
				t.Fatal("inherited server was not explicitly disabled")
			}
		}
		for name, settings := range config.Servers {
			if settings.Enabled != nil && *settings.Enabled {
				if name == "workspace" || name == "aw_member_tools" || !strings.HasPrefix(name, "aw_member_") || settings.URL != "http://127.0.0.1:1/api/v1/member-tools/mcp" ||
					!strings.HasPrefix(settings.TokenEnv, "AGENT_WORKSPACE_MCP_TOKEN_") || !reflect.DeepEqual(settings.Names, []string{"read_channel"}) {
					t.Fatal("platform server transport/profile was not uniquely bound")
				}
				return name, settings.TokenEnv
			}
		}
	}
	t.Fatal("missing platform server configuration")
	return "", ""
}

func TestCodexAppServerMemberToolsUniqueInheritanceIsolation(t *testing.T) {
	selected, config, request := appServerMemberFixture(t, "early-startup")
	names := []string{"read_channel"}
	e, err := NewCodexAppServerMemberToolExecutor(selected, config, names, 5*time.Second)
	if err != nil {
		t.Fatal("valid member profile rejected")
	}
	names[0] = "send_message"
	var lastName, lastEnv string
	for i := range 2 {
		request.RunID = fmt.Sprintf("member%d", i+1)
		request.CWD = t.TempDir()
		r := e.Execute(context.Background(), request, nil)
		if r.Outcome != OutcomeSucceeded || !r.OutputComplete || !r.DirectChildReaped {
			t.Fatal("early selected startup did not remain bound to the eventual thread response")
		}
		name, env := memberAppThreadConfig(t, request.CWD)
		if name == lastName || env == lastEnv || os.Getenv(env) != "" {
			t.Fatal("per-run server/environment identity was reused or leaked into the parent")
		}
		lastName, lastEnv = name, env
	}
	zero := "aw_member_" + strings.Repeat("0", 32)
	entropy := bytes.NewReader(append(make([]byte, 16), bytes.Repeat([]byte{1}, 16)...))
	name, err := codexAppServerMemberName([]string{zero}, entropy)
	if err != nil || name == zero {
		t.Fatal("random server name inherited collision was not retried")
	}
	if _, err := codexAppServerMemberName([]string{zero}, bytes.NewReader(make([]byte, 64))); err == nil {
		t.Fatal("colliding entropy silently enabled an inherited name")
	}
}

func TestCodexAppServerMemberToolsUnprovenWorkStaysUnknown(t *testing.T) {
	for _, mode := range []string{"status-origin", "status-name", "status-failed", "status-tools-error", "startup-failed", "startup-foreign", "foreign-server", "foreign-tool", "cross-thread", "cross-turn", "no-start", "orphan-progress", "changed-id", "changed-tool", "pending", "failed", "completed-error", "nontext-result", "terminal-status", "terminal-server", "terminal-tool", "native-external", "unknown-notification", "missing-terminal"} {
		t.Run(mode, func(t *testing.T) {
			selected, config, request := appServerMemberFixture(t, mode)
			e, err := NewCodexAppServerMemberToolExecutor(selected, config, []string{"read_channel", "list_members"}, 5*time.Second)
			if err != nil {
				t.Fatal("valid member profile rejected")
			}
			r := e.Execute(context.Background(), request, nil)
			if r.Outcome != OutcomeUnknown || !r.Started || !r.DirectChildReaped {
				t.Fatal("unproven native MCP lifecycle was settled")
			}
			visible, _ := json.Marshal(r)
			for _, path := range []string{r.StdoutPath, r.StderrPath} {
				data, _ := os.ReadFile(path)
				visible = append(visible, data...)
			}
			if strings.Contains(string(visible), memberToolTestToken) {
				t.Fatal("native MCP error/config/diagnostic leaked the platform token")
			}
			if strings.HasPrefix(mode, "status-") || strings.HasPrefix(mode, "startup-") {
				data, _ := os.ReadFile(filepath.Join(request.CWD, "client.jsonl"))
				if r.Accepted || strings.Contains(string(data), `"method":"turn/start"`) {
					t.Fatal("model turn started without selected MCP readiness")
				}
			}
		})
	}
}

func TestCodexAppServerMemberToolsElicitationIsDeniedBeforeTurn(t *testing.T) {
	for _, mode := range []string{"elicitation", "foreign-elicitation"} {
		t.Run(mode, func(t *testing.T) {
			selected, config, request := appServerMemberFixture(t, mode)
			e, err := NewCodexAppServerMemberToolExecutor(selected, config, []string{"read_channel"}, 5*time.Second)
			if err != nil {
				t.Fatal("valid member config rejected")
			}
			r := e.Execute(context.Background(), request, nil)
			if r.Outcome != OutcomeUnknown || r.Accepted || !r.DirectChildReaped {
				t.Fatal("elicitation granted authority or substituted for a terminal")
			}
			if _, err := os.Stat(filepath.Join(request.CWD, "elicitation-denied")); err != nil {
				t.Fatal("startup elicitation was not explicitly denied")
			}
		})
	}
}

func TestCodexAppServerRequiresObservedSelectedModel(t *testing.T) {
	for _, mode := range []string{"model-fallback", "missing-model"} {
		for _, member := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/member-%t", mode, member), func(t *testing.T) {
				e, request := appServerFixture(t, mode)
				if member {
					selected, config, memberRequest := appServerMemberFixture(t, mode)
					var err error
					e, err = NewCodexAppServerMemberToolExecutor(selected, config, []string{"read_channel"}, 5*time.Second)
					if err != nil {
						t.Fatal("valid member profile rejected")
					}
					request = memberRequest
				}
				r := e.Execute(context.Background(), request, nil)
				data, _ := os.ReadFile(filepath.Join(request.CWD, "client.jsonl"))
				if r.Outcome != OutcomeUnknown || r.Accepted || !r.DirectChildReaped || strings.Contains(string(data), `"method":"turn/start"`) {
					t.Fatal("missing/mismatched native model was replaced by requested-model evidence")
				}
			})
		}
	}
}
