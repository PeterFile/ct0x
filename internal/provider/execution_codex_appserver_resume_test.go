package provider

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This child is the compiled Go test binary. All native history, writer locks and
// protocol errors below are synthetic; no Codex, model, auth or network is used.
func TestCodexAppServerSessionProcessHelper(t *testing.T) {
	if os.Getenv("AW_SESSION_FIXTURE") == "" {
		return
	}
	defer os.Exit(0)
	mode := os.Getenv("AW_SESSION_MODE")
	earlyStatus := strings.HasPrefix(mode, "early-status-")
	pid := strconv.Itoa(os.Getpid())
	_ = os.WriteFile("argv-"+pid+".json", mustCodexAppServerJSON(os.Args[1:]), 0600)
	input := bufio.NewReader(os.Stdin)
	read := func() codexRPCMessage {
		line, err := input.ReadBytes('\n')
		if err != nil {
			if earlyStatus && err == io.EOF {
				_ = os.WriteFile("eof-"+pid, nil, 0600)
			}
			os.Exit(0)
		}
		message, err := decodeCodexRPCFrame(line)
		if err != nil {
			os.Exit(8)
		}
		log, err := os.OpenFile("client-"+pid+".jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(8)
		}
		_, _ = log.Write(line)
		_ = log.Close()
		return message
	}
	expect := func(method string) codexRPCMessage {
		message := read()
		if message.Method == nil || *message.Method != method {
			os.Exit(8)
		}
		return message
	}
	send := func(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }
	respond := func(request codexRPCMessage, value any) { send(map[string]any{"id": request.ID, "result": value}) }
	notify := func(method string, params any) { send(map[string]any{"method": method, "params": params}) }
	fail := func(request codexRPCMessage) {
		send(map[string]any{"id": request.ID, "error": map[string]any{"code": -32000, "message": memberToolTestToken}})
		_, _ = io.Copy(io.Discard, input)
	}
	config := func(overrideEnv string) any {
		if override := os.Getenv(overrideEnv); override != "" {
			return json.RawMessage(override)
		}
		return map[string]any{"config": map[string]any{"model_provider": "synthetic", "model_reasoning_effort": "high", "mcp_servers": map[string]any{
			"workspace": map[string]any{"enabled": true}, "previous_run": map[string]any{"enabled": true}}},
			"origins": map[string]any{"mcp_servers": map[string]any{"name": map[string]string{"type": "sessionFlags"}, "version": "flags-v1"}},
			"layers":  []any{map[string]any{"name": map[string]string{"type": "sessionFlags"}, "version": "flags-v1", "config": map[string]any{}}}}
	}
	cwd, _ := os.Getwd()
	home := "/synthetic/original-home"
	if mode == "wrong-home" {
		home = "/synthetic/other-home"
	}
	respond(expect("initialize"), map[string]string{"userAgent": "synthetic", "platformFamily": "unix", "platformOs": "synthetic", "codexHome": home})
	expect("initialized")
	respond(expect("config/read"), config("AW_SESSION_CONFIG_BEFORE"))
	threadRequest := read()
	if threadRequest.Method == nil || (*threadRequest.Method != "thread/start" && *threadRequest.Method != "thread/resume") {
		os.Exit(8)
	}
	if wire := os.Getenv("AW_SESSION_PREBIND_WIRE"); wire != "" {
		// Handwritten v0.166 wire shapes, not original native frames.
		_, _ = fmt.Fprintln(os.Stdout, wire)
	}
	switch mode {
	case "bootstrap-close":
		return
	case "bootstrap-pending":
		if *threadRequest.Method != "thread/resume" {
			os.Exit(8)
		}
		_, _ = fmt.Fprintln(os.Stderr, codexBootstrapPrivatePayload, memberToolTestToken)
		if os.WriteFile("resume-pending-"+pid, nil, 0600) != nil {
			os.Exit(8)
		}
		// No response. The executor must close stdin and reap on context completion.
		if _, err := input.ReadByte(); err != io.EOF {
			os.Exit(8)
		}
		return
	case "bootstrap-incomplete-frame":
		_, _ = fmt.Fprint(os.Stdout, `{"id":3,"result":{}}`)
		return
	case "bootstrap-oversize-frame":
		notify("warning", map[string]string{"message": strings.Repeat("x", codexRPCFrameLimit)})
		return
	case "bootstrap-warning-count":
		for range executionFramesCountLimit + 1 {
			_, _ = fmt.Fprintln(os.Stdout, codexFixtureScopedWarning)
		}
	case "bootstrap-warning-bytes":
		for range executionFramesByteLimit / (codexRPCFrameLimit / 2) {
			notify("warning", map[string]string{"threadId": "session-native-thread", "message": strings.Repeat("x", codexRPCFrameLimit/2)})
		}
	}
	lock, _ := os.OpenFile("synthetic-writer.lock", os.O_CREATE|os.O_RDWR, 0600)
	if lock == nil || syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		fail(threadRequest)
		return
	}
	defer lock.Close()
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	var history []any
	state, _ := os.ReadFile("synthetic-public.json")
	_ = json.Unmarshal(state, &history)
	if history == nil {
		history = []any{}
	}
	threadID := "session-native-thread"
	if *threadRequest.Method == "thread/start" {
		history = []any{}
	}
	var params map[string]json.RawMessage
	_ = json.Unmarshal(threadRequest.Params, &params)
	var configParams struct {
		Servers map[string]struct {
			Enabled  bool     `json:"enabled"`
			TokenEnv string   `json:"bearer_token_env_var"`
			Names    []string `json:"enabled_tools"`
		} `json:"mcp_servers"`
		Features map[string]json.RawMessage `json:"features"`
	}
	_ = json.Unmarshal(params["config"], &configParams)
	if os.Getenv("AW_SESSION_ENFORCE_GOALS_DISABLED") != "" {
		if enabled, present := configParams.Features["goals"]; !present || string(enabled) != "false" {
			fail(threadRequest)
			return
		}
	}
	server := ""
	names := []string{}
	for name, entry := range configParams.Servers {
		if entry.Enabled {
			server, names = name, entry.Names
			tokenHash := sha256.Sum256([]byte(os.Getenv(entry.TokenEnv)))
			_ = os.WriteFile("token-"+pid, []byte(hex.EncodeToString(tokenHash[:])), 0600)
		}
	}
	nativeProvider, nativeEffort := "synthetic", "high"
	if os.Getenv("AW_SESSION_NATIVE_DEFAULT") != "" {
		nativeProvider, nativeEffort = "openai", "max"
	}
	thread := func(turns []any) map[string]any {
		status := "idle"
		if mode == "loaded-active" {
			status = "active"
		}
		return map[string]any{"id": threadID, "turns": turns, "ephemeral": false, "status": map[string]any{"type": status},
			"cliVersion": "0.166.0", "cwd": cwd, "modelProvider": nativeProvider, "source": "vscode", "historyMode": "legacy"}
	}
	model, provider, effort, sandbox := "synthetic/model", nativeProvider, nativeEffort, "readOnly"
	switch mode {
	case "wrong-model":
		model = "synthetic/other"
	case "wrong-provider":
		provider = "other"
	case "wrong-effort":
		effort = "low"
	case "writable":
		sandbox = "workspaceWrite"
	}
	responseThread := thread([]any{})
	switch mode {
	case "wrong-thread":
		responseThread["id"] = "other-thread"
	case "wrong-thread-provider":
		responseThread["modelProvider"] = "other"
	}
	// Inject resume-only metadata at deterministic wire boundaries, including
	// before identity binding and on both sides of the new turn response.
	replay := func(phase string) {
		if *threadRequest.Method != "thread/resume" || os.Getenv("AW_SESSION_REPLAY_PHASE") != phase {
			return
		}
		kind, replayThread := os.Getenv("AW_SESSION_REPLAY_KIND"), threadID
		if os.Getenv("AW_SESSION_REPLAY_FOREIGN") != "" {
			replayThread = "foreign-thread"
		}
		if strings.HasPrefix(kind, "goal") {
			method, fields := "thread/goal/cleared", map[string]any{"threadId": replayThread}
			if kind == "goal-nonempty" || kind == "goal-cleared-with-goal" {
				fields["goal"] = map[string]any{"objective": "unauthorized autonomous work", "status": "active"}
				if kind == "goal-nonempty" {
					method = "thread/goal/updated"
				}
			}
			if kind == "goal-cleared-with-turn" {
				fields["turnId"] = "session-turn-1"
			}
			notify(method, fields)
		}
		if kind == "usage" || kind == "goal-and-usage" || strings.HasPrefix(kind, "usage-") {
			oldTurn := history[0].(map[string]any)["id"]
			if kind == "usage-unverified" {
				oldTurn = "unverified-turn"
			}
			if kind == "usage-future-turn" {
				oldTurn = fmt.Sprintf("session-turn-%d", len(history)+1)
			}
			usage := map[string]any{"totalTokens": 2, "inputTokens": 1, "cachedInputTokens": 0, "outputTokens": 1, "reasoningOutputTokens": 0}
			fields := map[string]any{"threadId": replayThread, "turnId": oldTurn, "tokenUsage": map[string]any{"total": usage, "last": usage, "modelContextWindow": 1024}}
			if kind == "usage-without-turn" {
				delete(fields, "turnId")
			}
			count := 1
			if kind == "usage-overflow" {
				count = executionFramesCountLimit + 1
			}
			if kind == "usage-byte-overflow" {
				fields["tokenUsage"].(map[string]any)["opaque"] = strings.Repeat("x", codexRPCFrameLimit/2)
				count = executionFramesByteLimit/(codexRPCFrameLimit/2) + 1
			}
			for range count {
				notify("thread/tokenUsage/updated", fields)
			}
		}
	}
	response := map[string]any{"thread": responseThread, "approvalPolicy": "never", "approvalsReviewer": "user",
		"sandbox": map[string]any{"type": sandbox, "networkAccess": false}, "cwd": cwd, "model": model, "modelProvider": provider, "reasoningEffort": effort}
	if *threadRequest.Method == "thread/resume" {
		// v0.166 metadata-only response with extra passive fields.
		maps.Copy(response, map[string]any{"disabledPluginIds": []string{}, "instructionSources": []string{}, "serviceTier": nil,
			"turnsBackwardsCursor": nil, "itemsBackwardsCursor": nil,
			"collaborationMode": map[string]any{"mode": "default", "settings": map[string]any{"model": model, "reasoning_effort": effort}}})
		maps.Copy(responseThread, map[string]any{"sessionId": "synthetic-session", "preview": codexBootstrapPrivatePayload,
			"projectId": nil, "forkedFromId": nil, "parentThreadId": nil, "createdAt": 1, "updatedAt": 2})
		notify("thread/status/changed", map[string]any{"threadId": threadID, "status": map[string]string{"type": "idle"}})
	}
	replay("before-response")
	respond(threadRequest, response)
	if mode == "bootstrap-bound-thread-closed" {
		notify("thread/closed", map[string]string{"threadId": threadID})
		_, _ = io.Copy(io.Discard, input)
		return
	}
	replay("after-response")
	respond(expect("config/read"), config("AW_SESSION_CONFIG_AFTER"))
	statusRequest := expect("mcpServerStatus/list")
	tools := make(map[string]any)
	for _, name := range names {
		tools[name] = map[string]any{"name": name}
	}
	origin, auth := "http://127.0.0.1:1", "bearerToken"
	switch mode {
	case "bootstrap-wrong-mcp-origin":
		origin = "http://127.0.0.1:2"
	case "bootstrap-wrong-mcp-auth":
		auth = "notLoggedIn"
	}
	respond(statusRequest, map[string]any{"data": []any{map[string]any{"name": server, "runtimeStatus": "connected", "authStatus": auth,
		"httpOrigin": origin, "tools": tools, "toolsError": nil, "pluginId": nil}}, "nextCursor": nil})
	request := read()
	if request.Method != nil && *request.Method == "thread/read" {
		before := history
		switch mode {
		case "extra-turn":
			before = append(append([]any{}, history...), map[string]any{"id": "foreign-turn", "status": "completed", "items": []any{}, "error": nil})
		case "missing-turn":
			before = []any{}
		case "changed-content":
			items := history[0].(map[string]any)["items"].([]any)
			items[len(items)-1].(map[string]any)["text"] = "changed without updatedAt"
		case "changed-turn-id":
			history[len(history)-1].(map[string]any)["id"] = "foreign-turn"
		case "unfinished-history":
			history[len(history)-1].(map[string]any)["status"] = "inProgress"
		case "unknown-tool-history":
			history[0].(map[string]any)["items"].([]any)[1].(map[string]any)["type"] = "dynamicToolCall"
		case "old-server-drift":
			history[0].(map[string]any)["items"].([]any)[1].(map[string]any)["server"] = "foreign-server"
		}
		replay("before-history")
		if mode == "duplicate-history" {
			raw := string(mustCodexAppServerJSON(map[string]any{"id": request.ID, "result": map[string]any{"thread": thread(before)}}))
			_, _ = fmt.Fprintln(os.Stdout, strings.Replace(raw, `"status":"completed"`, `"status":"completed","status":"completed"`, 1))
		} else {
			respond(request, map[string]any{"thread": thread(before)})
		}
		replay("after-history")
		request = read()
	}
	if request.Method == nil || *request.Method != "turn/start" {
		os.Exit(8)
	}
	turnID := fmt.Sprintf("session-turn-%d", len(history)+1)
	if mode == "reused-turn" && len(history) != 0 {
		turnID = history[len(history)-1].(map[string]any)["id"].(string)
	}
	turn := func(status string, items []any) map[string]any {
		return map[string]any{"id": turnID, "status": status, "items": items, "error": nil}
	}
	replyTurn := func() {
		respond(request, map[string]any{"turn": turn("inProgress", []any{})})
		replay("after-turn-response")
	}
	replay("before-turn-response")
	if !earlyStatus {
		replyTurn()
	}
	notify("turn/started", map[string]any{"threadId": threadID, "turn": turn("inProgress", []any{})})
	replay("after-turn-started")
	_ = json.Unmarshal(request.Params, &params)
	var currentInput []any
	_ = json.Unmarshal(params["input"], &currentInput)
	user := map[string]any{"id": "user-" + turnID, "type": "userMessage", "content": currentInput}
	call := map[string]any{"id": "mcp-" + turnID, "type": "mcpToolCall", "server": server, "tool": "read_channel", "status": "inProgress", "arguments": map[string]any{"limit": 1}, "result": nil, "error": nil}
	switch mode {
	case "capture-mcp-equivalent-json":
		call["arguments"] = json.RawMessage(`{"nested":{"b":[true],"a":3},"limit":1}`)
	case "terminal-mcp-hint-drift":
		call["readOnlyHint"] = true
	}
	notify("item/started", map[string]any{"threadId": threadID, "turnId": turnID, "item": call})
	call["status"], call["result"] = "completed", map[string]any{"content": []any{map[string]any{"type": "text", "text": "synthetic historical text"}}, "_meta": nil, "structuredContent": nil}
	if mode != "pending-tool" {
		notify("item/completed", map[string]any{"threadId": threadID, "turnId": turnID, "item": call})
	}
	answer := map[string]any{"id": "answer-" + turnID, "type": "agentMessage", "text": "synthetic session answer", "phase": "final_answer"}
	if mode == "empty-answer" {
		answer["text"] = ""
	}
	observedAnswerText := answer["text"]
	notify("item/completed", map[string]any{"threadId": threadID, "turnId": turnID, "item": answer})
	items := []any{user, call, answer}
	history = append(history, turn("completed", items))
	_ = os.WriteFile("synthetic-public.json", mustCodexAppServerJSON(history), 0600)
	if mode == "no-terminal" {
		return
	}
	terminal := turn("completed", items)
	switch mode {
	case "failed-turn", "failed-turn-late-active":
		terminal["status"], terminal["error"] = "failed", map[string]any{"message": "synthetic failure"}
	case "interrupted-turn-late-ambiguous-active":
		terminal["status"] = "interrupted"
	case "terminal-mcp-omitted":
		terminal["items"] = []any{}
	case "terminal-mcp-arguments-drift", "terminal-mcp-result-drift", "terminal-mcp-hint-drift", "capture-mcp-equivalent-json":
		// Only change the terminal copy; a later capture agrees with item/completed.
		duplicate := maps.Clone(call)
		switch mode {
		case "terminal-mcp-arguments-drift":
			duplicate["arguments"] = map[string]any{"limit": 2}
		case "terminal-mcp-result-drift":
			duplicate["result"] = json.RawMessage(`{"content":[{"type":"text","text":"different result in terminal"}],"_meta":null,"structuredContent":null}`)
		case "terminal-mcp-hint-drift":
			duplicate["readOnlyHint"] = false
		case "capture-mcp-equivalent-json":
			duplicate["arguments"] = json.RawMessage(`{"limit":1,"nested":{"a":3,"b":[true]}}`)
			duplicate["result"] = json.RawMessage(`{"content":[{"text":"synthetic historical text","type":"text"}],"structuredContent":null,"_meta":null}`)
		}
		terminal["items"] = []any{user, duplicate, answer}
	}
	threadStatus := func(thread, kind string, flags []any) {
		status := map[string]any{"type": kind}
		if flags != nil {
			status["activeFlags"] = flags
		}
		notify("thread/status/changed", map[string]any{"threadId": thread, "status": status})
	}
	if mode == "early-status-active-first" {
		threadStatus(threadID, "active", []any{})
	}
	notify("turn/completed", map[string]any{"threadId": threadID, "turn": terminal})
	switch mode {
	case "failed-turn-late-active", "early-status-terminal-first":
		threadStatus(threadID, "active", []any{})
	case "early-status-foreign":
		threadStatus("foreign-thread", "active", []any{})
	case "interrupted-turn-late-ambiguous-active":
		threadStatus(threadID, "idle", []any{"waitingOnApproval"})
	}
	if earlyStatus {
		replyTurn()
	}
	capture := read()
	if capture.Method == nil || *capture.Method != "thread/read" {
		os.Exit(8)
	}
	switch mode {
	case "capture-active-before-error", "capture-active-before-missing":
		threadStatus(threadID, "active", []any{})
		if mode == "capture-active-before-missing" {
			return
		}
		threadStatus(threadID, "idle", nil) // Later idle must not erase the earlier contradiction.
		fallthrough
	case "capture-error":
		fail(capture)
		return
	}
	if mode == "capture-wrong-turn" {
		history[len(history)-1].(map[string]any)["id"] = "foreign-turn"
	}
	if mode == "capture-prefix-drift" || mode == "capture-prefix-drift-unsupported" {
		oldItems := history[0].(map[string]any)["items"].([]any)
		oldItems[len(oldItems)-1].(map[string]any)["text"] = "changed prefix after new Turn"
	}
	last := history[len(history)-1].(map[string]any)
	argumentsNesting := 0
	switch mode {
	case "capture-mcp-arguments-drift", "capture-mcp-arguments-drift-no-result":
		call["arguments"] = map[string]any{"limit": 2}
		if mode == "capture-mcp-arguments-drift-no-result" {
			delete(call, "result")
		}
	case "capture-mcp-result-drift":
		call["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] = "different result after terminal"
	case "capture-mcp-missing-result":
		delete(call, "result")
	case "capture-mcp-server-drift":
		call["server"] = "different-server"
	case "capture-mcp-equivalent-json":
		call["arguments"] = map[string]any{"limit": 1, "nested": map[string]any{"a": 3, "b": []any{true}}}
		call["result"] = json.RawMessage(`{"content":[{"type":"text","text":"synthetic historical text"}],"structuredContent":null,"_meta":null}`)
	case "capture-prefix-extra-plan", "capture-prefix-questions-text-drift", "capture-prefix-questions-only", "capture-prefix-partial-only", "capture-prefix-missing-result", "capture-prefix-local-image-text-drift":
		old := history[0].(map[string]any)
		items := old["items"].([]any)
		oldAnswer := items[len(items)-1].(map[string]any)
		switch mode {
		case "capture-prefix-extra-plan":
			old["items"] = append(items, map[string]any{"id": "new-old-plan", "type": "plan", "text": "not in verified history"})
		case "capture-prefix-partial-only":
			old["itemsView"], old["items"] = "summary", []any{oldAnswer}
		case "capture-prefix-missing-result":
			delete(items[1].(map[string]any), "result")
		case "capture-prefix-local-image-text-drift":
			items[0].(map[string]any)["content"] = []any{
				map[string]any{"type": "text", "text": "different verified old input"},
				map[string]any{"type": "localImage", "path": "/synthetic/local-image.png"},
			}
		default:
			oldAnswer["questions"] = []any{}
			if mode == "capture-prefix-questions-text-drift" {
				oldAnswer["text"] = "different verified old answer"
			}
		}
	case "capture-budget-users":
		last["items"] = []any{}
		for i := 0; i <= codexSessionHistoryItemLimit; i++ {
			last["items"] = append(last["items"].([]any), map[string]any{"id": fmt.Sprintf("budget-user-%d", i), "type": "userMessage", "content": currentInput})
		}
		last["items"] = append(last["items"].([]any), call, answer)
	case "capture-questions-text-drift", "capture-questions-no-drift", "capture-partial-text-drift":
		answer["questions"] = []any{}
		if mode != "capture-questions-no-drift" {
			answer["text"] = "contradicts the observed answer with valid questions"
		}
		if mode == "capture-partial-text-drift" {
			last["itemsView"] = "summary"
		}
	case "capture-plan-missing-observed", "capture-plan-complete", "capture-plan-kind-drift", "capture-late-external-safe-prefix":
		plan := map[string]any{"id": "unsupported-plan", "type": "plan", "text": "synthetic plan"}
		switch mode {
		case "capture-plan-missing-observed":
			last["items"] = []any{user, answer, plan}
		case "capture-plan-kind-drift":
			answer["type"] = "plan"
		case "capture-late-external-safe-prefix":
			for i := 0; i < 16; i++ {
				last["items"] = append(last["items"].([]any), map[string]any{"id": fmt.Sprintf("unsupported-plan-%d", i), "type": "plan", "text": "synthetic plan"})
			}
			last["items"] = append(last["items"].([]any), map[string]any{"id": "late-command", "type": "commandExecution", "status": "completed", "command": "synthetic", "cwd": cwd, "commandActions": []any{}, "aggregatedOutput": "", "exitCode": 0})
		default:
			last["items"] = append(last["items"].([]any), plan)
		}
	case "capture-partial-missing-observed", "capture-partial-multi-user":
		last["itemsView"] = "summary"
		last["items"] = []any{user, answer}
		if mode == "capture-partial-multi-user" {
			last["items"] = append(last["items"].([]any), map[string]any{"id": "second-user", "type": "userMessage", "content": currentInput})
		}
	case "capture-deep-active", "capture-deep-idle", "capture-deep-external":
		var nested any = "synthetic"
		argumentsNesting = 70
		for i := 0; i < argumentsNesting; i++ {
			nested = map[string]any{"nested": nested}
		}
		call["arguments"] = map[string]any{"deep": nested}
		if mode == "capture-deep-external" {
			last["items"] = append(last["items"].([]any), map[string]any{"id": "deep-command", "type": "commandExecution", "status": "completed", "command": "synthetic", "cwd": cwd, "commandActions": []any{}, "aggregatedOutput": "", "exitCode": 0})
		}
	case "capture-external-item", "capture-external-unsupported", "capture-external-oversized-anchor":
		if mode == "capture-external-unsupported" {
			last["itemsView"] = "notLoaded"
		}
		if mode == "capture-external-oversized-anchor" {
			for i := len(last["items"].([]any)); i <= codexSessionHistoryItemLimit; i++ {
				last["items"] = append(last["items"].([]any), map[string]any{"id": fmt.Sprintf("passive-%d", i), "type": "reasoning", "summary": []any{}, "content": []any{}})
			}
		}
		last["items"] = append(last["items"].([]any), map[string]any{"id": "unobserved-command", "type": "commandExecution", "status": "completed", "command": "synthetic", "cwd": cwd, "commandActions": []any{}, "aggregatedOutput": "", "exitCode": 0})
	case "capture-unobserved-mcp", "capture-unobserved-mcp-invalid-tool":
		unobserved := make(map[string]any, len(call))
		for key, value := range call {
			unobserved[key] = value
		}
		unobserved["id"] = "unobserved-mcp"
		if mode == "capture-unobserved-mcp-invalid-tool" {
			unobserved["tool"] = "unissued-tool"
		}
		last["items"] = append(last["items"].([]any), unobserved)
	case "capture-in-progress":
		last["status"] = "inProgress"
	case "capture-extra-turn":
		history = append(history, map[string]any{"id": "unobserved-turn", "status": "completed", "items": []any{}, "error": nil})
	case "capture-unsupported-anchor", "capture-wrong-turn-unsupported", "capture-answer-drift-unsupported", "capture-prefix-drift-unsupported":
		last["itemsView"] = "notLoaded"
		if mode == "capture-wrong-turn-unsupported" {
			last["id"] = "foreign-turn"
		}
		if mode == "capture-answer-drift-unsupported" {
			answer["text"] = "contradicts the observed completed item"
		}
	}
	captureThread := thread(history)
	switch mode {
	case "capture-active", "capture-deep-active":
		captureThread["status"] = map[string]any{"type": "active", "activeFlags": []any{}}
	case "capture-idle-active-flags":
		captureThread["status"] = map[string]any{"type": "idle", "activeFlags": []any{"waitingOnApproval"}}
	case "capture-wrong-thread":
		captureThread["id"] = "foreign-thread"
	}
	captureResult := map[string]any{"thread": captureThread}
	frameBytes := len(mustCodexAppServerJSON(map[string]any{"id": capture.ID, "result": captureResult}))
	if frameBytes >= codexRPCFrameLimit {
		os.Exit(8)
	}
	counts, observed := map[string]int{}, map[string]bool{}
	commandIndex := -1
	for i, value := range last["items"].([]any) {
		item := value.(map[string]any)
		counts[item["type"].(string)]++
		observed[item["id"].(string)] = true
		if item["type"] == "commandExecution" {
			commandIndex = i
		}
	}
	questions, hasQuestions := answer["questions"].([]any)
	_ = os.WriteFile("capture-meta-"+pid+".json", mustCodexAppServerJSON(map[string]any{
		"mode": mode, "frame_bytes": frameBytes, "items": len(last["items"].([]any)), "counts": counts,
		"observed_mcp_present": observed[call["id"].(string)], "observed_answer_present": observed["answer-"+turnID],
		"answer_text_matches": answer["text"] == observedAnswerText, "questions_empty": hasQuestions && len(questions) == 0,
		"arguments_nesting": argumentsNesting, "command_index": commandIndex,
	}), 0600)
	respond(capture, captureResult)
	var lateStatus map[string]any
	switch mode {
	case "capture-late-active":
		lateStatus = map[string]any{"type": "active", "activeFlags": []any{}}
	case "capture-late-unknown-status":
		lateStatus = map[string]any{"type": "futureStatus"}
	case "capture-late-idle":
		lateStatus = map[string]any{"type": "idle"}
	}
	if lateStatus != nil {
		notify("thread/status/changed", map[string]any{"threadId": threadID, "status": lateStatus})
	}
	_, _ = io.Copy(io.Discard, input)
	_ = os.WriteFile("eof-"+pid, nil, 0600)
	if mode == "nonzero-exit" {
		os.Exit(7)
	}
}

func appServerSessionFixture(t *testing.T) (Provider, string, ExecuteRequest) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("export AW_SESSION_FIXTURE=1\nexec '%s' -test.run='^TestCodexAppServerSessionProcessHelper$' -- \"$@\"\n", strings.ReplaceAll(self, "'", "'\\''"))
	selected, _, config := memberToolFiles(t, body)
	selected.Name, selected.Version = Codex, "0.166.0"
	request := ExecuteRequest{RunID: "session1", CWD: t.TempDir(), RuntimeDir: t.TempDir(), Prompt: "new input one", Model: "synthetic/model"}
	if os.Chmod(request.RuntimeDir, 0700) != nil {
		t.Fatal("private runtime unavailable")
	}
	return selected, config, request
}

func sessionClientFrames(t *testing.T, cwd string) [][]codexRPCMessage {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(cwd, "client-*.jsonl"))
	result := make([][]codexRPCMessage, 0, len(files))
	for _, path := range files {
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(file)
		var messages []codexRPCMessage
		for scanner.Scan() {
			message, err := decodeCodexRPCFrame(scanner.Bytes())
			if err != nil {
				t.Fatal("invalid fixture client frame")
			}
			messages = append(messages, message)
		}
		_ = file.Close()
		if scanner.Err() != nil {
			t.Fatal(scanner.Err())
		}
		result = append(result, messages)
	}
	return result
}

func TestCodexAppServerEarlyThreadStatusOrder(t *testing.T) {
	const threadID = "session-native-thread"
	for _, profile := range []string{"legacy", "session"} {
		for _, tc := range []struct {
			mode  string
			want  Outcome
			owned bool
		}{
			{"early-status-terminal-first", OutcomeUnknown, true},
			{"early-status-active-first", OutcomeSucceeded, true},
			{"early-status-foreign", OutcomeUnknown, false},
		} {
			t.Run(profile+"/"+tc.mode, func(t *testing.T) {
				t.Setenv("AW_SESSION_MODE", tc.mode)
				selected, config, request := appServerSessionFixture(t)
				binding := json.RawMessage(`{"purpose":"generic"}`)
				var executor Executor
				var err error
				if profile == "session" {
					executor, err = NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, CodexAppServerSessionOptions{Binding: binding}, 5*time.Second)
				} else {
					executor, err = NewCodexAppServerMemberToolExecutor(selected, config, []string{"read_channel"}, 5*time.Second)
				}
				if err != nil {
					t.Fatal(err)
				}
				var events []Event
				r := executor.Execute(context.Background(), request, func(event Event) error {
					events = append(events, event)
					return nil
				})
				var terminal Outcome
				wantEvents := []Event{{Kind: SessionEvent, NativeSessionID: threadID}}
				if tc.owned {
					terminal = OutcomeSucceeded
					wantEvents = append(wantEvents,
						Event{Kind: AcceptedEvent, NativeSessionID: threadID},
						Event{Kind: ResponseEvent, NativeSessionID: threadID, Text: "synthetic session answer"},
						Event{Kind: TerminalEvent, NativeSessionID: threadID, Terminal: terminal},
					)
				}
				if r.Outcome != tc.want || !r.Started || r.Accepted != tc.owned || r.Terminal != terminal || r.NativeSessionID != threadID || !r.DirectChildReaped || r.OutputComplete != tc.owned || r.ExitCode != 0 || r.DeliveryFailed {
					t.Fatalf("want outcome %s with owned evidence=%v, got %+v", tc.want, tc.owned, r)
				}
				if !reflect.DeepEqual(events, wantEvents) {
					t.Fatalf("want events %+v, got %+v", wantEvents, events)
				}
				if files, err := filepath.Glob(filepath.Join(request.CWD, "eof-*")); err != nil || len(files) != 1 {
					t.Fatal("child did not observe stdin shutdown")
				}
				wantCaptures := 0
				if profile == "session" {
					if tc.want == OutcomeSucceeded {
						wantCaptures = 1
					}
					if r.NativeRef == nil || string(r.NativeRef.Binding) != string(binding) || (r.NativeRef.Codex != nil) != (wantCaptures == 1) {
						t.Fatal("session reference did not reflect the settled outcome")
					}
				} else if r.NativeRef != nil {
					t.Fatal("legacy path issued a session reference")
				}
				if files, err := filepath.Glob(filepath.Join(request.CWD, "capture-meta-*.json")); err != nil || len(files) != wantCaptures {
					t.Fatalf("want %d capture reads, got %d (err=%v)", wantCaptures, len(files), err)
				}
			})
		}
	}
}

func TestCodexSessionRejectsDriftBeforeNewPrompt(t *testing.T) {
	selected, config, request := appServerSessionFixture(t)
	options := CodexAppServerSessionOptions{Binding: json.RawMessage(`{"purpose":"generic","version":1}`)}
	e, err := NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, options, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	first := e.Execute(context.Background(), request, nil)
	if first.Outcome != OutcomeSucceeded || first.NativeRef == nil || first.NativeRef.Codex == nil {
		t.Fatal("fixture baseline not captured")
	}
	options.Resume = first.NativeRef
	for _, mode := range []string{"wrong-home", "wrong-thread", "wrong-model", "wrong-provider", "wrong-effort", "writable", "loaded-active", "extra-turn", "missing-turn", "changed-content", "changed-turn-id", "unfinished-history", "unknown-tool-history", "old-server-drift", "duplicate-history"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("AW_SESSION_MODE", mode)
			e, err := NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, options, 5*time.Second)
			if err != nil {
				t.Fatal("valid historical reference rejected before handshake")
			}
			request.RunID = "reject-" + mode
			request.Prompt = "must never be submitted"
			r := e.Execute(context.Background(), request, nil)
			if r.Outcome != OutcomeUnknown || !r.Started || r.Accepted || r.Terminal != "" || !r.DirectChildReaped || r.NativeRef == nil || r.NativeRef.Codex != nil || string(r.NativeRef.Binding) != string(options.Binding) {
				t.Fatal("unqualified history/config was replayed, settled or lost its binding-only barrier")
			}
			for _, frames := range sessionClientFrames(t, request.CWD) {
				for _, frame := range frames {
					if frame.Method == nil || *frame.Method != "turn/start" {
						continue
					}
					if strings.Contains(string(frame.Params), request.Prompt) {
						t.Fatal("new prompt sent across a rejected resume")
					}
				}
			}
		})
	}
	// One initial fresh call, never an implicit recovery fallback.
	fresh := 0
	for _, frames := range sessionClientFrames(t, request.CWD) {
		for _, frame := range frames {
			if *frame.Method == "thread/start" {
				fresh++
			}
		}
	}
	if fresh != 1 {
		t.Fatal("resume failure silently created a fresh thread")
	}
}

func TestCodexSessionHeldNativeWriterDoesNotStartTurn(t *testing.T) {
	selected, config, request := appServerSessionFixture(t)
	e, err := NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, CodexAppServerSessionOptions{}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	first := e.Execute(context.Background(), request, nil)
	if first.NativeRef == nil || first.NativeRef.Codex == nil {
		t.Fatal("fixture baseline not captured")
	}
	lock, err := os.OpenFile(filepath.Join(request.CWD, "synthetic-writer.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		t.Fatal("fixture writer ownership unavailable")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	e, err = NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, CodexAppServerSessionOptions{Resume: first.NativeRef}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	request.RunID, request.Prompt = "held-writer", "must not cross writer conflict"
	r := e.Execute(context.Background(), request, nil)
	if r.Outcome != OutcomeUnknown || r.Accepted || r.Terminal != "" || !r.DirectChildReaped || r.NativeRef != nil {
		t.Fatal("writer conflict invented execution or triggered recovery")
	}
	for _, frames := range sessionClientFrames(t, request.CWD) {
		for _, frame := range frames {
			if *frame.Method == "turn/start" && strings.Contains(string(frame.Params), request.Prompt) {
				t.Fatal("turn started while native writer was held")
			}
		}
	}
}

func TestCodexSessionLatestWithoutSuccessfulReferenceIsBarrier(t *testing.T) {
	for _, mode := range []string{"no-terminal", "failed-turn", "pending-tool", "nonzero-exit", "capture-error", "capture-wrong-turn"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("AW_SESSION_MODE", mode)
			selected, config, request := appServerSessionFixture(t)
			binding := json.RawMessage(`{"purpose":"generic","version":1}`)
			e, err := NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, CodexAppServerSessionOptions{Binding: binding}, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			r := e.Execute(context.Background(), request, nil)
			want := OutcomeUnknown
			if mode == "capture-error" {
				want = OutcomeSucceeded
			}
			if mode == "failed-turn" {
				want = OutcomeFailed
			}
			if r.Outcome != want || !r.DirectChildReaped || r.NativeRef == nil || r.NativeRef.Codex != nil || string(r.NativeRef.Binding) != string(binding) {
				t.Fatal("latest non-success/noRef execution exposed resumable evidence or lost barrier")
			}
			if (mode == "capture-error" || mode == "capture-wrong-turn") && (!r.OutputComplete || r.Terminal != OutcomeSucceeded) {
				t.Fatal("optional capture failure rewrote actual execution evidence")
			}
			// This exercises provider qualification, not the separate Store latest selector.
			if _, err := NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, CodexAppServerSessionOptions{Resume: r.NativeRef}, 5*time.Second); err == nil {
				t.Fatal("binding-only latest barrier was accepted as a resume reference")
			}
		})
	}
}

func TestCodexSessionPendingReferenceWaitsForDeliveryFence(t *testing.T) {
	selected, config, request := appServerSessionFixture(t)
	binding := json.RawMessage(`{"purpose":"generic","version":1}`)
	e, err := NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, CodexAppServerSessionOptions{Binding: binding}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	done := make(chan struct{})
	var receipt ExecutionReceipt
	go func() {
		receipt = e.Execute(context.Background(), request, func(Event) error { <-release; return errors.New("synthetic delivery failure") })
		close(done)
	}()
	deadline := time.After(4 * time.Second)
	for {
		files, _ := filepath.Glob(filepath.Join(request.CWD, "eof-*"))
		if len(files) != 0 {
			break
		}
		select {
		case <-done:
			t.Fatal("returned before capture/drain marker")
		case <-deadline:
			close(release)
			<-done
			t.Fatal("native output did not progress independently of callback")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-done:
		t.Fatal("pending reference escaped an in-flight callback")
	default:
	}
	close(release)
	<-done
	if receipt.Outcome != OutcomeUnknown || !receipt.DeliveryFailed || !receipt.DirectChildReaped || receipt.Terminal != OutcomeSucceeded || receipt.NativeRef == nil || receipt.NativeRef.Codex != nil {
		t.Fatal("pending native reference survived failed delivery")
	}
}

func TestCodexSessionReusedTurnIDAndCapturePrefixDrift(t *testing.T) {
	for _, mode := range []string{"reused-turn", "capture-prefix-drift", "capture-prefix-drift-unsupported"} {
		t.Run(mode, func(t *testing.T) {
			selected, config, request := appServerSessionFixture(t)
			e, err := NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, CodexAppServerSessionOptions{}, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			first := e.Execute(context.Background(), request, nil)
			if first.NativeRef == nil || first.NativeRef.Codex == nil {
				t.Fatal("fixture baseline not captured")
			}
			t.Setenv("AW_SESSION_MODE", mode)
			e, err = NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, CodexAppServerSessionOptions{Resume: first.NativeRef}, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			request.RunID, request.Prompt = "second", "second independent input"
			r := e.Execute(context.Background(), request, nil)
			if r.Outcome != OutcomeUnknown || r.NativeRef != nil || !r.DirectChildReaped {
				t.Fatal("old turn acceptance or changed history prefix was adopted")
			}
		})
	}
}

func TestCodexSessionDefaultConstructorsDoNotCaptureOrResume(t *testing.T) {
	selected, config, request := appServerSessionFixture(t)
	t.Setenv("AW_SESSION_PREBIND_WIRE", codexFixtureUnscopedWarnings)
	e, err := NewCodexAppServerMemberToolExecutor(selected, config, []string{"read_channel"}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{"old1", "old2"} {
		request.RunID = run
		r := e.Execute(context.Background(), request, nil)
		if r.Outcome != OutcomeSucceeded || r.NativeRef != nil {
			t.Fatal("legacy constructor changed behavior")
		}
	}
	for _, frames := range sessionClientFrames(t, request.CWD) {
		for _, frame := range frames {
			if *frame.Method == "thread/read" || *frame.Method == "thread/resume" {
				t.Fatal("legacy constructor opted into native sessions")
			}
		}
	}
}

func TestCodexSessionFreshCaptureThenColdResume(t *testing.T) {
	selected, config, request := appServerSessionFixture(t)
	tools := []string{"read_channel"}
	options := CodexAppServerSessionOptions{Binding: json.RawMessage(`{"turn":1}`)}
	e, err := NewCodexAppServerNativeSessionMemberToolAdapter(testIssuedMemberConfig(t, config), tools, options).NewExecutor(selected, 5*time.Second)
	if err != nil {
		t.Fatal("valid fresh session rejected")
	}
	t.Setenv("AW_SESSION_PREBIND_WIRE", codexFixtureUnscopedWarnings)
	first := e.Execute(context.Background(), request, nil)
	if first.Outcome != OutcomeSucceeded || first.NativeRef == nil || first.NativeRef.Codex == nil {
		t.Fatal("successful fresh native Turn did not capture a settled reference")
	}
	ref := first.NativeRef.Codex
	if ref.NativeThreadID != first.NativeSessionID || ref.NativeTurnID != "session-turn-1" || ref.HistoryTurns != 1 || ref.HistoryItems != 3 ||
		ref.CLIPath != selected.Path || ref.CLIVersion != selected.Version || ref.Provider != "synthetic" || ref.Model != request.Model || ref.CWD != request.CWD ||
		ref.NativeHome != "/synthetic/original-home" || !nativeSessionSHA256(ref.HistorySHA256) {
		t.Fatal("reference is missing actual identity, namespace or full-public-history evidence")
	}
	secondConfig := filepath.Join(t.TempDir(), "current-config.json")
	const secondToken = "synthetic-current-token-two"
	if os.WriteFile(secondConfig, mustCodexAppServerJSON(map[string]string{"endpoint": "http://127.0.0.1:1/api/v1/member-tools", "token": secondToken}), 0600) != nil {
		t.Fatal("fixture config unavailable")
	}
	options.Resume, options.Binding = new(NativeSessionReference), json.RawMessage(`{"turn":2}`)
	if err := json.Unmarshal(mustCodexAppServerJSON(first.NativeRef), options.Resume); err != nil || !reflect.DeepEqual(options.Resume, first.NativeRef) {
		t.Fatal("typed reference did not round-trip before construction")
	}
	e, err = NewCodexAppServerNativeSessionMemberToolAdapter(testIssuedMemberConfig(t, secondConfig), tools, options).NewExecutor(selected, 5*time.Second)
	if err != nil {
		t.Fatal("qualified reference rejected")
	}
	// Constructor input is not retained by alias.
	options.Resume.Codex.NativeThreadID = "caller-mutated-thread"
	options.Binding[0] = ' '
	t.Setenv("AW_SESSION_PREBIND_WIRE", codexFixtureUnscopedWarnings+"\n"+codexFixtureScopedWarning+"\n"+codexFixtureThreadAnnouncement)
	request.RunID, request.Prompt = "session2", "new input two"
	var events []EventKind
	second := e.Execute(context.Background(), request, func(event Event) error {
		events = append(events, event.Kind)
		return nil
	})
	if second.Outcome != OutcomeSucceeded || !second.Accepted || second.Terminal != OutcomeSucceeded || !second.OutputComplete || !second.DirectChildReaped ||
		second.NativeRef == nil || second.NativeRef.Codex == nil || second.NativeSessionID != ref.NativeThreadID || string(second.NativeRef.Binding) != `{"turn":2}` ||
		second.NativeRef.Codex.NativeTurnID != "session-turn-2" || second.NativeRef.Codex.HistoryTurns != 2 || second.NativeRef.Codex.HistoryItems != 6 || second.NativeRef.Codex.HistorySHA256 == ref.HistorySHA256 {
		t.Fatalf("matching pending warning blocked same-thread cold resume: outcome=%s accepted=%t terminal=%s", second.Outcome, second.Accepted, second.Terminal)
	}
	if !reflect.DeepEqual(events, []EventKind{SessionEvent, AcceptedEvent, ResponseEvent, TerminalEvent}) {
		t.Fatal("passive warning changed the verified lifecycle")
	}
	frames := sessionClientFrames(t, request.CWD)
	if len(frames) != 2 {
		t.Fatal("resume did not use a new child process")
	}
	servers, envs := map[string]bool{}, map[string]bool{}
	for _, messages := range frames {
		var methods []string
		for _, frame := range messages {
			methods = append(methods, *frame.Method)
			var params map[string]json.RawMessage
			_ = json.Unmarshal(frame.Params, &params)
			if *frame.Method == "thread/start" || *frame.Method == "thread/resume" {
				if params["history"] != nil || params["path"] != nil || params["dynamicTools"] != nil || string(params["approvalPolicy"]) != `"never"` || string(params["sandbox"]) != `"read-only"` || string(params["modelProvider"]) != `"synthetic"` {
					t.Fatal("native selection or current isolation bundle widened")
				}
				if *frame.Method == "thread/resume" && (string(params["excludeTurns"]) != "true" || params["ephemeral"] != nil || params["initialTurnsPage"] != nil ||
					string(params["threadId"]) != string(mustCodexAppServerJSON(ref.NativeThreadID)) || string(params["cwd"]) != string(mustCodexAppServerJSON(request.CWD)) ||
					string(params["model"]) != string(mustCodexAppServerJSON(request.Model)) || string(params["approvalsReviewer"]) != `"user"`) {
					t.Fatal("resume changed the exact metadata-only selection or profile")
				}
				var config struct {
					Servers map[string]struct {
						Enabled bool   `json:"enabled"`
						Env     string `json:"bearer_token_env_var"`
					} `json:"mcp_servers"`
				}
				_ = json.Unmarshal(params["config"], &config)
				for name, entry := range config.Servers {
					if entry.Enabled {
						if servers[name] || envs[entry.Env] {
							t.Fatal("native MCP server or token env reused")
						}
						servers[name], envs[entry.Env] = true, true
					} else if name != "workspace" && name != "previous_run" {
						t.Fatal("unexpected inherited server")
					}
				}
			}
			if *frame.Method == "thread/read" && string(params["includeTurns"]) != "true" {
				t.Fatal("partial history requested")
			}
			if *frame.Method == "turn/start" && (string(params["cwd"]) != string(mustCodexAppServerJSON(request.CWD)) || string(params["approvalPolicy"]) != `"never"` || params["sandboxPolicy"] == nil) {
				t.Fatal("turn did not retain explicit current policy")
			}
		}
		fresh := []string{"initialize", "initialized", "config/read", "thread/start", "config/read", "mcpServerStatus/list", "turn/start", "thread/read"}
		resume := []string{"initialize", "initialized", "config/read", "thread/resume", "config/read", "mcpServerStatus/list", "thread/read", "turn/start", "thread/read"}
		if !reflect.DeepEqual(methods, fresh) && !reflect.DeepEqual(methods, resume) {
			t.Fatal("unexpected cold resume protocol sequence")
		}
	}
	if len(servers) != 2 || len(envs) != 2 {
		t.Fatal("MCP identity did not rotate")
	}
	tokens, _ := filepath.Glob(filepath.Join(request.CWD, "token-*"))
	observed := map[string]bool{}
	for _, path := range tokens {
		data, _ := os.ReadFile(path)
		observed[string(data)] = true
	}
	oldHash, newHash := sha256.Sum256([]byte(memberToolTestToken)), sha256.Sum256([]byte(secondToken))
	if !observed[hex.EncodeToString(oldHash[:])] || !observed[hex.EncodeToString(newHash[:])] {
		t.Fatal("current child token not rebound")
	}
	private := []string{"new input one", request.Prompt, memberToolTestToken, secondToken, "synthetic historical text", "synthetic session answer", codexBootstrapPrivatePayload}
	for _, receipt := range []ExecutionReceipt{first, second} {
		out := readCodexFixtureProjection(t, receipt.StdoutPath, private...)
		err := readCodexFixtureProjection(t, receipt.StderrPath, private...)
		if len(out["bootstrap_failure"]) != 0 || len(err["bootstrap_failure"]) != 0 {
			t.Fatal("successful run logged a bootstrap failure")
		}
		life := out["lifecycle_summary"]
		if len(life) != 4 || life[0]["thread_id"] != ref.NativeThreadID || life[3]["turn_id"] != receipt.NativeRef.Codex.NativeTurnID || life[3]["terminal"] != string(OutcomeSucceeded) {
			t.Fatal("sanitized logging dropped verified reconciliation IDs")
		}
	}
}
