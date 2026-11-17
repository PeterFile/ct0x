package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const appServerFixtureThread = "native-app-thread"
const appServerFixtureTurn = "native-app-turn"
const appServerFixtureSecret = "SYNTHETIC-CONFIG-SECRET"

// The fixture is the local test binary, not a CLI/model or an authentication probe.
func TestCodexAppServerProcessHelper(t *testing.T) {
	mode := os.Getenv("AW_APP_SERVER_FIXTURE")
	if mode == "" {
		return
	}
	defer os.Exit(0)
	input := bufio.NewReader(os.Stdin)
	readFrame := func() codexRPCMessage {
		line, err := input.ReadBytes('\n')
		if err != nil {
			panic("synthetic fixture: missing client frame")
		}
		var message codexRPCMessage
		if json.Unmarshal(line, &message) != nil {
			panic("synthetic fixture: invalid client JSON")
		}
		file, err := os.OpenFile("client.jsonl", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			panic("synthetic fixture: transcript unavailable")
		}
		_, _ = file.Write(line)
		_ = file.Close()
		return message
	}
	read := func(method string) codexRPCMessage {
		message := readFrame()
		if message.Method == nil || *message.Method != method {
			panic("synthetic fixture: unexpected client method")
		}
		return message
	}
	waitFile := func(name string) {
		for {
			if _, err := os.Stat(name); err == nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	send := func(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }
	respond := func(request codexRPCMessage, result any) {
		send(map[string]any{"id": request.ID, "result": result})
	}
	notify := func(method string, params any) { send(map[string]any{"method": method, "params": params}) }
	turn := func(status string) map[string]any {
		return map[string]any{"id": appServerFixtureTurn, "status": status, "items": []any{}, "error": nil}
	}
	started := func() {
		notify("turn/started", map[string]any{"threadId": appServerFixtureThread, "turn": turn("inProgress")})
	}
	answer := func() {
		notify("item/completed", map[string]any{"threadId": appServerFixtureThread, "turnId": appServerFixtureTurn,
			"item": map[string]any{"id": "answer", "type": "agentMessage", "text": "合成\u2028完整回答"}})
	}
	terminal := func(status string) {
		notify("turn/completed", map[string]any{"threadId": appServerFixtureThread, "turn": turn(status)})
	}
	config := func(changed bool) any {
		version := "stable-v1"
		if changed {
			version = "changed-v2"
		}
		origins := map[string]any{"name": map[string]any{"type": "user", "file": "/synthetic/config.toml", "profile": nil}, "version": version}
		if mode == "unknown-origin" {
			origins["name"] = map[string]any{"type": "futureLayer"}
		}
		servers := map[string]any{
			"inherited.local":  map[string]any{"command": "must-not-execute", "env": map[string]string{"TOKEN": appServerFixtureSecret}},
			"inherited remote": map[string]any{"url": "https://synthetic.invalid", "bearer_token": appServerFixtureSecret},
		}
		if changed && mode == "config-names" {
			servers["late-inherited"] = map[string]bool{"enabled": true}
			origins["version"] = "stable-v1"
		}
		if mode == "managed" {
			origins["name"] = map[string]any{"type": "legacyManagedConfigTomlFromFile", "file": "/synthetic/managed.toml"}
		}
		layers := []any{
			map[string]any{"name": map[string]string{"type": "sessionFlags"}, "version": "flags-v1", "config": map[string]string{"private": appServerFixtureSecret}},
			map[string]any{"name": origins["name"], "version": origins["version"], "config": map[string]string{"private": appServerFixtureSecret}},
		}
		if mode == "hidden-managed" {
			layers = append(layers, map[string]any{"name": map[string]string{"type": "legacyManagedConfigTomlFromFile", "file": "/synthetic/managed.toml"}, "version": "managed-v1", "config": map[string]bool{"enabled": true}})
		}
		if mode == "missing-layers" {
			layers = nil
		}
		return map[string]any{
			"config":  map[string]any{"mcp_servers": servers, "model_provider": appServerFixtureSecret},
			"origins": map[string]any{"mcp_servers": origins}, "layers": layers,
		}
	}
	_ = os.WriteFile("pid", []byte(strconv.Itoa(os.Getpid())), 0600)
	_ = os.WriteFile("inherited-home", []byte(os.Getenv("HOME")+"\n"+os.Getenv("CODEX_HOME")), 0600)
	args := os.Args[stringsIndexAppServerFixtureArgs(os.Args):]
	if !reflect.DeepEqual(args, codexAppServerArguments()) {
		panic("synthetic fixture: unsafe startup restrictions")
	}
	_ = os.WriteFile("startup-restricted", nil, 0600)
	init := read("initialize")
	if mode == "wrong-initialize-id" {
		init.ID = json.RawMessage(`"unrelated"`)
	}
	respond(init, map[string]any{"userAgent": "synthetic", "platformFamily": "unix", "platformOs": "synthetic", "codexHome": "/synthetic/original-home"})
	if mode == "wrong-initialize-id" {
		_, _ = io.Copy(io.Discard, input)
		return
	}
	if mode == "remote-status" || mode == "remote-bad-status" || mode == "unknown-connection" {
		method, status := "remoteControl/status/changed", "disabled"
		if mode == "remote-bad-status" {
			status = "unsupported"
		}
		if mode == "unknown-connection" {
			method = "remoteControl/futureChanged"
		}
		notify(method, map[string]any{"installationId": "synthetic-installation", "serverName": "synthetic-server", "environmentId": nil, "status": status})
		if mode != "remote-status" {
			_, _ = io.Copy(io.Discard, input)
			return
		}
	}
	read("initialized")
	before := read("config/read")
	respond(before, config(false))
	if mode == "unknown-origin" || mode == "managed" || mode == "hidden-managed" || mode == "missing-layers" {
		tail, _ := io.ReadAll(io.LimitReader(input, 2<<20))
		_ = os.WriteFile("after-rejected-config", tail, 0600)
		return
	}
	thread := read("thread/start")
	if mode == "account-update" {
		notify("account/updated", map[string]any{"authMode": "chatgpt", "planType": "plus"})
	}
	if mode == "account-rate-limits" {
		notify("account/rateLimits/updated", map[string]any{"rateLimits": map[string]any{"primary": map[string]int{"usedPercent": 20}}})
	}
	if mode == "global-warning" {
		notify("warning", map[string]any{"threadId": nil, "message": appServerFixtureSecret})
	}
	if mode == "thread-name" || mode == "cross-thread-name" {
		id := appServerFixtureThread
		if mode == "cross-thread-name" {
			id = "unrelated-thread"
		}
		notify("thread/name/updated", map[string]any{"threadId": id, "threadName": appServerFixtureSecret})
	}
	cwd, _ := os.Getwd()
	threadValue := map[string]any{"id": appServerFixtureThread, "cwd": cwd, "ephemeral": false, "turns": []any{}, "path": "/synthetic/original-home/session.jsonl"}
	notify("thread/started", map[string]any{"thread": threadValue})
	if mode == "early-thread-status" {
		notify("thread/status/changed", map[string]any{"threadId": appServerFixtureThread, "status": map[string]string{"type": "idle"}})
	}
	respond(thread, map[string]any{"thread": threadValue, "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "cwd": cwd, "model": "synthetic/model", "modelProvider": "synthetic", "approvalsReviewer": "user"})
	after := read("config/read")
	respond(after, config(mode == "config-change" || mode == "config-names"))
	if mode == "config-change" || mode == "config-names" {
		_, _ = io.Copy(io.Discard, input)
		return
	}
	if mode == "blocked-stdin" {
		time.Sleep(10 * time.Second)
		return
	}
	request := read("turn/start")
	if mode == "rpc-error" {
		send(map[string]any{"id": request.ID, "error": map[string]any{"code": -32000, "message": appServerFixtureSecret, "data": appServerFixtureSecret}})
		_, _ = io.Copy(io.Discard, input)
		return
	}
	if mode == "early" {
		started()
		answer()
		terminal("completed")
		_ = os.WriteFile("early-notifications", nil, 0600)
		waitFile("release-response")
	}
	if mode == "wrong-turn-response-id" {
		request.ID = json.RawMessage(`"unrelated"`)
	}
	responseTurn := turn("inProgress")
	if mode == "wrong-turn-id" {
		responseTurn["id"] = "unrelated-turn"
	}
	respond(request, map[string]any{"turn": responseTurn})
	if mode != "early" {
		if mode == "terminal-before-start" {
			terminal("completed")
		}
		started()
		if mode == "duplicate-start" {
			started()
		}
		if mode == "slow-delivery" || mode == "overflow" {
			waitFile("emit-tail")
		}
		switch mode {
		case "cross-thread", "cross-turn":
			threadID, turnID := appServerFixtureThread, appServerFixtureTurn
			if mode == "cross-thread" {
				threadID = "unrelated-thread"
			} else {
				turnID = "unrelated-turn"
			}
			notify("item/completed", map[string]any{"threadId": threadID, "turnId": turnID, "item": map[string]any{"id": "answer", "type": "agentMessage", "text": "not this session"}})
		case "terminal-only":
		case "pending-tool", "background-tool", "subagent":
			kind := "commandExecution"
			if mode == "subagent" {
				kind = "collabAgentToolCall"
			}
			notify("item/started", map[string]any{"threadId": appServerFixtureThread, "turnId": appServerFixtureTurn,
				"item": map[string]any{"id": "pending-external", "type": kind, "status": "inProgress"}})
			if mode == "background-tool" {
				notify("item/completed", map[string]any{"threadId": appServerFixtureThread, "turnId": appServerFixtureTurn,
					"item": map[string]any{"id": "pending-external", "type": kind, "status": "completed", "processId": "background-process", "exitCode": nil}})
			}
			answer()
		case "overflow", "large-logs":
			text, count := "x", 128
			if mode == "large-logs" {
				text, count = strings.Repeat("x", 96<<10), 16
				_, _ = os.Stderr.Write([]byte(strings.Repeat("s", 2<<20)))
			}
			for i := 0; i < count; i++ {
				notify("item/completed", map[string]any{"threadId": appServerFixtureThread, "turnId": appServerFixtureTurn,
					"item": map[string]any{"id": fmt.Sprintf("answer-%d", i), "type": "agentMessage", "text": text}})
			}
		default:
			answer()
		}
		if mode == "duplicate-item" {
			answer()
		}
		if mode == "orphan-delta" {
			notify("item/agentMessage/delta", map[string]any{"threadId": appServerFixtureThread, "turnId": appServerFixtureTurn, "itemId": "unfinished", "delta": "unfinished answer"})
		}
		if mode == "active-delta" || mode == "delta-kind-mismatch" {
			kind := "agentMessage"
			if mode == "delta-kind-mismatch" {
				kind = "reasoning"
			}
			notify("item/started", map[string]any{"threadId": appServerFixtureThread, "turnId": appServerFixtureTurn, "item": map[string]string{"id": "active", "type": kind, "text": ""}})
			notify("item/agentMessage/delta", map[string]any{"threadId": appServerFixtureThread, "turnId": appServerFixtureTurn, "itemId": "active", "delta": "partial answer"})
			notify("item/completed", map[string]any{"threadId": appServerFixtureThread, "turnId": appServerFixtureTurn, "item": map[string]string{"id": "active", "type": kind, "text": "complete answer"}})
		}
		if mode == "cancel" || mode == "interrupt-ack-only" || mode == "approval" {
			if mode == "approval" {
				notifyRequest := map[string]any{"id": "approval", "method": "item/commandExecution/requestApproval", "params": map[string]any{
					"threadId": appServerFixtureThread, "turnId": appServerFixtureTurn, "itemId": "external", "command": appServerFixtureSecret}}
				send(notifyRequest)
				denial := readFrame()
				if string(denial.ID) != `"approval"` || string(denial.Result) != `{"decision":"cancel"}` {
					panic("synthetic fixture: permissions were not denied")
				}
			}
			interrupt := read("turn/interrupt")
			respond(interrupt, map[string]any{})
			if mode != "interrupt-ack-only" {
				terminal("interrupted")
			}
		} else if mode == "failed" {
			failed := turn("failed")
			failed["error"] = map[string]string{"message": appServerFixtureSecret, "additionalDetails": appServerFixtureSecret}
			notify("turn/completed", map[string]any{"threadId": appServerFixtureThread, "turn": failed})
		} else if mode == "interrupted" {
			terminal("interrupted")
		} else if mode == "malformed-drain" {
			_, _ = os.Stdout.Write([]byte("invalid-frame\n" + strings.Repeat("x", 2<<20) + "\n"))
			_, _ = os.Stderr.Write([]byte(strings.Repeat("s", 2<<20)))
			_ = os.WriteFile("drained", nil, 0600)
		} else if mode == "partial" {
			_, _ = os.Stdout.Write([]byte(`{"method":"turn/completed","params":{"threadId":"` + appServerFixtureThread + `","turn":{"id":"` + appServerFixtureTurn + `","status":"completed","items":[]}}}`))
			return
		} else if mode == "terminal-item-mismatch" || mode == "terminal-item-duplicate" {
			completed := turn("completed")
			item := map[string]string{"id": "answer", "type": "reasoning"}
			items := []any{item}
			if mode == "terminal-item-duplicate" {
				item["type"], item["text"] = "agentMessage", "合成\u2028完整回答"
				items = append(items, item)
			}
			completed["items"] = items
			notify("turn/completed", map[string]any{"threadId": appServerFixtureThread, "turn": completed})
		} else if mode != "missing-terminal" {
			terminal("completed")
		}
		if mode == "contradiction" {
			terminal("interrupted")
		}
		if mode == "late-item" {
			notify("item/completed", map[string]any{"threadId": appServerFixtureThread, "turnId": appServerFixtureTurn, "item": map[string]any{"id": "late", "type": "agentMessage", "text": "late"}})
		}
	}
	if mode == "missing-terminal" {
		return
	}
	if mode == "inherited-pipe" {
		child := exec.Command("/bin/sleep", "10")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if child.Start() != nil {
			panic("synthetic fixture: descendant did not start")
		}
		_ = os.WriteFile("descendant", []byte(strconv.Itoa(child.Process.Pid)), 0600)
		_ = child.Process.Release()
	}
	_, _ = io.Copy(io.Discard, input)
	_ = os.WriteFile("stdin-eof", nil, 0600)
	if mode == "wait-exit" {
		waitFile("release-exit")
	}
	if mode == "refuse-exit" {
		time.Sleep(10 * time.Second)
	}
	if mode == "nonzero-exit" {
		os.Exit(7)
	}
}

func stringsIndexAppServerFixtureArgs(args []string) int {
	for i, arg := range args {
		if arg == "--" {
			return i + 1
		}
	}
	return len(args)
}

func mustAppServerFixtureJSON(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic("synthetic fixture: invalid JSON")
	}
	return data
}

func appServerFixture(t *testing.T, mode string) (Executor, ExecuteRequest) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("export AW_APP_SERVER_FIXTURE=%s\nexec '%s' -test.run='^TestCodexAppServerProcessHelper$' -- \"$@\"\n", mode, strings.ReplaceAll(self, "'", "'\\''"))
	path := executable(t, filepath.Join(t.TempDir(), "synthetic app server ; literal"), body)
	e, err := NewCodexAppServerExecutor(path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	request := ExecuteRequest{RunID: "app1", CWD: t.TempDir(), RuntimeDir: t.TempDir(), Model: "synthetic/model", Prompt: "synthetic prompt"}
	if err := os.Chmod(request.RuntimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	return e, request
}

func appServerWaitFile(t *testing.T, path string, done <-chan struct{}) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-done:
			t.Fatalf("execution returned before synthetic marker %s", filepath.Base(path))
		case <-deadline:
			t.Fatalf("missing synthetic marker %s", filepath.Base(path))
		case <-time.After(time.Millisecond):
		}
	}
}

func TestCodexAppServerLiteralHandshakeAndConfiguration(t *testing.T) {
	e, request := appServerFixture(t, "normal")
	request.Prompt, request.Model = "@literal-file\n$(touch prompt-marker) ; --help\n", "synthetic/model;$(touch model-marker)"
	home, codexHome := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	var events []Event
	receipt := e.Execute(context.Background(), request, func(event Event) error { events = append(events, event); return nil })
	want := []Event{{Kind: SessionEvent, NativeSessionID: appServerFixtureThread}, {Kind: AcceptedEvent, NativeSessionID: appServerFixtureThread},
		{Kind: ResponseEvent, NativeSessionID: appServerFixtureThread, Text: "合成\u2028完整回答"}, {Kind: TerminalEvent, NativeSessionID: appServerFixtureThread, Terminal: OutcomeSucceeded}}
	if receipt.Outcome != OutcomeSucceeded || !receipt.Started || !receipt.Accepted || !receipt.DirectChildReaped || !receipt.OutputComplete || receipt.ExitCode != 0 || !reflect.DeepEqual(events, want) {
		t.Fatalf("incomplete native execution: %+v events=%+v", receipt, events)
	}
	data, err := os.ReadFile(filepath.Join(request.CWD, "inherited-home"))
	if err != nil || string(data) != home+"\n"+codexHome {
		t.Fatal("original CLI HOME/CODEX_HOME was not inherited")
	}
	file, err := os.Open(filepath.Join(request.CWD, "client.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var methods []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		message, err := decodeCodexRPCFrame(scanner.Bytes())
		if err != nil || message.Method == nil {
			t.Fatal("invalid client request")
		}
		methods = append(methods, *message.Method)
		var params map[string]json.RawMessage
		_ = json.Unmarshal(message.Params, &params)
		switch *message.Method {
		case "config/read":
			if string(params["includeLayers"]) != "true" || string(params["cwd"]) != string(mustAppServerFixtureJSON(request.CWD)) {
				t.Error("configuration must request complete source metadata for the explicit workspace")
			}
		case "thread/start":
			var model string
			_ = json.Unmarshal(params["model"], &model)
			if model != request.Model || string(params["sandbox"]) != `"read-only"` || string(params["approvalPolicy"]) != `"never"` || string(params["ephemeral"]) == "true" {
				t.Error("unsafe thread settings or nonliteral model")
			}
			var config struct {
				Features  map[string]bool                    `json:"features"`
				Agents    struct{ Enabled *bool }            `json:"agents"`
				MCP       map[string]struct{ Enabled *bool } `json:"mcp_servers"`
				WebSearch string                             `json:"web_search"`
				Notify    []string                           `json:"notify"`
			}
			if json.Unmarshal(params["config"], &config) != nil || config.Agents.Enabled == nil || *config.Agents.Enabled || config.WebSearch != "disabled" || config.Notify == nil {
				t.Error("missing restrictions")
			}
			for _, feature := range []string{"plugins", "apps", "hooks", "shell_tool", "multi_agent", "multi_agent_v2", "code_mode", "code_mode_only", "code_mode_host", "tool_suggest"} {
				if enabled, ok := config.Features[feature]; !ok || enabled {
					t.Errorf("feature %s was not explicitly disabled", feature)
				}
			}
			for _, name := range []string{"inherited.local", "inherited remote"} {
				if config.MCP[name].Enabled == nil || *config.MCP[name].Enabled {
					t.Error("inherited MCP was not disabled by literal name")
				}
			}
		case "turn/start":
			var input []struct{ Type, Text string }
			if json.Unmarshal(params["input"], &input) != nil || len(input) != 1 || input[0].Type != "text" || input[0].Text != request.Prompt || string(params["threadId"]) != `"`+appServerFixtureThread+`"` {
				t.Error("prompt was not submitted literally to the bound thread")
			}
		}
	}
	if scanner.Err() != nil || !reflect.DeepEqual(methods, []string{"initialize", "initialized", "config/read", "thread/start", "config/read", "turn/start"}) {
		t.Fatalf("unexpected handshake: %v", methods)
	}
	for _, path := range []string{receipt.StdoutPath, receipt.StderrPath} {
		data, err := os.ReadFile(path)
		info, statErr := os.Stat(path)
		if err != nil || statErr != nil || info.Mode().Perm()&0077 != 0 || len(data) > 1<<20 || strings.Contains(string(data), appServerFixtureSecret) {
			t.Fatal("nonprivate/unbounded log or raw config response persisted")
		}
	}
	for _, name := range []string{"prompt-marker", "model-marker"} {
		if _, err := os.Stat(filepath.Join(request.CWD, name)); !os.IsNotExist(err) {
			t.Fatal("shell interpreted input")
		}
	}
	if again := e.Execute(context.Background(), request, nil); again.Outcome != OutcomeUnknown || again.Started {
		t.Fatalf("existing run was replayed: %+v", again)
	}
}

func TestCodexAppServerDoesNotAcceptUnacknowledgedNotifications(t *testing.T) {
	e, request := appServerFixture(t, "early")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var receipt ExecutionReceipt
	var events []Event
	var mu sync.Mutex
	go func() {
		receipt = e.Execute(ctx, request, func(event Event) error { mu.Lock(); events = append(events, event); mu.Unlock(); return nil })
		close(done)
	}()
	appServerWaitFile(t, filepath.Join(request.CWD, "early-notifications"), done)
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	for _, event := range events {
		if event.Kind != SessionEvent {
			t.Error("notification was accepted without correlated response")
		}
	}
	mu.Unlock()
	if err := os.WriteFile(filepath.Join(request.CWD, "release-response"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("buffered native lifecycle did not complete")
	}
	if receipt.Outcome != OutcomeSucceeded || !receipt.Accepted || len(events) != 4 {
		t.Fatalf("early bound notifications were lost: %+v events=%+v", receipt, events)
	}
}

func TestCodexAppServerRequiresActualWaitAndTerminal(t *testing.T) {
	t.Run("actual-wait", func(t *testing.T) {
		e, request := appServerFixture(t, "wait-exit")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		var receipt ExecutionReceipt
		go func() { receipt = e.Execute(ctx, request, nil); close(done) }()
		appServerWaitFile(t, filepath.Join(request.CWD, "stdin-eof"), done)
		select {
		case <-done:
			t.Fatal("native terminal was substituted for process Wait")
		case <-time.After(30 * time.Millisecond):
		}
		_ = os.WriteFile(filepath.Join(request.CWD, "release-exit"), nil, 0600)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("process was not joined")
		}
		if receipt.Outcome != OutcomeSucceeded || !receipt.DirectChildReaped || !receipt.OutputComplete {
			t.Fatalf("actual exit was not trusted: %+v", receipt)
		}
	})
	for _, mode := range []string{"missing-terminal", "config-change"} {
		t.Run(mode, func(t *testing.T) {
			e, request := appServerFixture(t, mode)
			receipt := e.Execute(context.Background(), request, nil)
			if receipt.Outcome != OutcomeUnknown || receipt.Terminal != "" || !receipt.DirectChildReaped || mode == "config-change" && receipt.Accepted {
				t.Fatalf("incomplete/changed run was trusted: %+v", receipt)
			}
			data, _ := os.ReadFile(filepath.Join(request.CWD, "client.jsonl"))
			if mode == "config-change" && strings.Contains(string(data), `"method":"turn/start"`) {
				t.Fatal("turn started across a configuration change")
			}
		})
	}
}
