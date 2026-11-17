package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

var codexAppServerDisabledFeatures = []string{
	"plugins", "apps", "hooks", "shell_tool", "unified_exec", "multi_agent", "multi_agent_v2",
	"code_mode", "code_mode_only", "code_mode_host", "tool_suggest",
}

func codexAppServerConfig(names []string) map[string]any {
	features := make(map[string]bool, len(codexAppServerDisabledFeatures))
	for _, name := range codexAppServerDisabledFeatures {
		features[name] = false
	}
	servers := make(map[string]any, len(names))
	for _, name := range names {
		servers[name] = map[string]bool{"enabled": false}
	}
	return map[string]any{
		"features": features, "agents": map[string]bool{"enabled": false}, "mcp_servers": servers,
		"web_search": "disabled", "notify": []string{}, "approval_policy": "never", "sandbox_mode": "read-only",
	}
}

// Only the selected names and a canonical provenance digest survive config/read.
// This does not freeze configuration: trusted, stable local config is a precondition.
// No config values, raw responses, layer paths or native errors enter logs/reasons.
type codexAppServerConfigSnapshot struct {
	names   []string
	origins [32]byte
	layers  [32]byte
}

func readCodexAppServerConfig(raw json.RawMessage) (codexAppServerConfigSnapshot, error) {
	var snapshot codexAppServerConfigSnapshot
	fields, err := codexRPCObject(raw, "config", "origins", "layers")
	if err != nil || fields["config"] == nil || fields["origins"] == nil {
		return snapshot, errCodexRPCFrame
	}
	// Winning origins can omit a shadowed managed layer. Require the complete
	// includeLayers metadata before creating a thread; never retain layer config.
	var layers []json.RawMessage
	if json.Unmarshal(fields["layers"], &layers) != nil || len(layers) == 0 || len(layers) > 256 {
		return snapshot, errCodexRPCFrame
	}
	layerNames := make(map[string]bool, len(layers))
	ordered := make([]string, 0, len(layers))
	sessionFlags := false
	for _, layer := range layers {
		metadata, err := codexAppServerConfigMetadata(layer)
		if err != nil || layerNames[metadata] {
			return snapshot, errCodexRPCFrame
		}
		name, _ := codexRPCObject(layer, "name")
		source, _ := codexRPCObject(name["name"], "type")
		var kind string
		_ = json.Unmarshal(source["type"], &kind)
		sessionFlags = sessionFlags || kind == "sessionFlags"
		layerNames[metadata] = true
		ordered = append(ordered, metadata)
	}
	if !sessionFlags {
		return snapshot, errCodexRPCFrame
	}
	encodedLayers, _ := json.Marshal(ordered)
	snapshot.layers = sha256.Sum256(encodedLayers)
	config, err := codexRPCObject(fields["config"], "mcp_servers")
	if err != nil {
		return snapshot, errCodexRPCFrame
	}
	if servers := config["mcp_servers"]; servers != nil {
		names, err := codexAppServerMap(servers)
		if err != nil || len(names) > 1024 {
			return snapshot, errCodexRPCFrame
		}
		for name, value := range names {
			if !codexAppServerIdentity(name) {
				return snapshot, errCodexRPCFrame
			}
			// Check the container, never interpret command/env/token values.
			if _, err := codexRPCObject(value); err != nil {
				return snapshot, errCodexRPCFrame
			}
			snapshot.names = append(snapshot.names, name)
		}
	}
	slices.Sort(snapshot.names)
	origins, err := codexAppServerMap(fields["origins"])
	if err != nil {
		return snapshot, errCodexRPCFrame
	}
	canonical := make(map[string]bool, len(origins))
	for _, raw := range origins {
		metadata, err := codexAppServerConfigMetadata(raw)
		if err != nil || !layerNames[metadata] {
			return snapshot, errCodexRPCFrame
		}
		canonical[metadata] = true
	}
	if len(snapshot.names) != 0 && len(origins) == 0 {
		return snapshot, errCodexRPCFrame
	}
	encoded, _ := json.Marshal(canonical)
	snapshot.origins = sha256.Sum256(encoded)
	return snapshot, nil
}

// Only understood, non-managed local sources can be overridden safely. Reject
// managed sources even when they are disabled or absent from winning origins.
func codexAppServerConfigMetadata(raw json.RawMessage) (string, error) {
	metadata, err := codexRPCObject(raw, "name", "version")
	if err != nil {
		return "", errCodexRPCFrame
	}
	var version string
	if json.Unmarshal(metadata["version"], &version) != nil || version == "" {
		return "", errCodexRPCFrame
	}
	name, err := codexAppServerMap(metadata["name"])
	if err != nil {
		return "", errCodexRPCFrame
	}
	var kind string
	if json.Unmarshal(name["type"], &kind) != nil {
		return "", errCodexRPCFrame
	}
	var pathKey string
	switch kind {
	case "packagedDefaults", "user":
		pathKey = "file"
	case "project":
		pathKey = "dotCodexFolder"
	case "sessionFlags":
	default:
		return "", errCodexRPCFrame
	}
	for key := range name {
		if key != "type" && key != pathKey && !(kind == "user" && key == "profile") {
			return "", errCodexRPCFrame
		}
	}
	if pathKey != "" {
		var path string
		if json.Unmarshal(name[pathKey], &path) != nil || !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
			return "", errCodexRPCFrame
		}
	}
	if profile := name["profile"]; profile != nil {
		var value *string
		if json.Unmarshal(profile, &value) != nil {
			return "", errCodexRPCFrame
		}
		if value == nil {
			delete(name, "profile")
		}
	}
	encoded, _ := json.Marshal(map[string]any{"name": name, "version": version})
	return string(encoded), nil
}

// Dynamic config keys must reject duplicates without quadratic key matching.
func codexAppServerMap(raw json.RawMessage) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errCodexRPCFrame
	}
	values := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, errCodexRPCFrame
		}
		if _, exists := values[key]; exists {
			return nil, errCodexRPCFrame
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, errCodexRPCFrame
		}
		values[key] = value
	}
	if _, err := decoder.Token(); err != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		return nil, errCodexRPCFrame
	}
	return values, nil
}

func codexAppServerIdentity(value string) bool {
	return value != "" && len(value) <= 256 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n\t")
}

func codexAppServerString(fields map[string]json.RawMessage, key string) (string, bool) {
	var value string
	if json.Unmarshal(fields[key], &value) != nil || !codexAppServerIdentity(value) {
		return "", false
	}
	return value, true
}

type codexAppServerItem struct {
	kind      string
	status    string
	completed bool
	textHash  [32]byte
}

// One owner handles a single connection, thread and turn. Provisional frames are
// bounded and replayed only after the corresponding start response binds identity.
type codexAppServerProtocol struct {
	ctx                   context.Context
	cancel                context.CancelFunc
	request               ExecuteRequest
	receipt               *ExecutionReceipt
	delivery              *executionDelivery
	lifecycle             func(Event)
	send                  func(int, string, any) bool
	reply                 func(codexRPCMessage, any) bool
	summary               func(codexAppServerConfigSnapshot)
	awaiting              int
	threadID              string
	turnID                string
	threadSeen            string
	before                codexAppServerConfigSnapshot
	buffer                []codexRPCMessage
	bufferBytes           int
	items                 map[string]codexAppServerItem
	requests              map[string]bool
	damaged               bool
	external              bool
	interruptSent         bool
	interruptAcknowledged bool
}

func (p *codexAppServerProtocol) fail() {
	p.damaged = true
	p.cancel()
}

func (p *codexAppServerProtocol) next(id int, method string, params any) {
	if p.ctx.Err() != nil {
		return
	}
	p.awaiting = id
	if !p.send(id, method, params) {
		p.fail()
	}
}

func (p *codexAppServerProtocol) emit(event Event) {
	event.NativeSessionID = p.threadID
	if p.lifecycle != nil {
		p.lifecycle(event)
	}
	p.delivery.enqueue(event)
}

func (p *codexAppServerProtocol) receive(message codexRPCMessage) {
	if p.damaged {
		return
	}
	if message.Method != nil {
		method := *message.Method
		provisional := p.awaiting == 3 && slices.Contains([]string{"thread/status/changed", "thread/name/updated", "thread/project/updated"}, method) || p.awaiting == 5 &&
			(strings.HasPrefix(method, "turn/") || strings.HasPrefix(method, "item/") || strings.HasPrefix(method, "hook/") || strings.HasPrefix(method, "model/") || method == "thread/tokenUsage/updated" || method == "error")
		if provisional {
			size := len(method) + len(message.Params) + len(message.ID)
			if len(p.buffer) >= executionFramesCountLimit || size > executionFramesByteLimit-p.bufferBytes {
				p.fail()
				return
			}
			p.buffer = append(p.buffer, message)
			p.bufferBytes += size
			return
		}
		p.notification(message)
		return
	}
	if p.interruptSent && !p.interruptAcknowledged && bytes.Equal(bytes.TrimSpace(message.ID), []byte("6")) {
		// An interrupt response acknowledges only the request, never completion.
		if message.Error != nil {
			p.fail()
			return
		}
		p.interruptAcknowledged = true
		return
	}
	if p.awaiting == 0 || !bytes.Equal(bytes.TrimSpace(message.ID), []byte(strconv.Itoa(p.awaiting))) || message.Error != nil {
		p.fail()
		return
	}
	id := p.awaiting
	p.awaiting = 0
	switch id {
	case 1:
		fields, err := codexRPCObject(message.Result, "userAgent", "platformFamily", "platformOs", "codexHome")
		var home string
		_, agent := codexAppServerString(fields, "userAgent")
		_, family := codexAppServerString(fields, "platformFamily")
		_, platform := codexAppServerString(fields, "platformOs")
		if err != nil || !agent || !family || !platform || json.Unmarshal(fields["codexHome"], &home) != nil || !filepath.IsAbs(home) {
			p.fail()
			return
		}
		if !p.send(0, "initialized", nil) {
			p.fail()
			return
		}
		p.next(2, "config/read", map[string]any{"cwd": p.request.CWD, "includeLayers": true})
	case 2, 4:
		config, err := readCodexAppServerConfig(message.Result)
		if err != nil {
			p.fail()
			return
		}
		p.summary(config)
		if id == 2 {
			p.before = config
			params := map[string]any{"cwd": p.request.CWD, "sandbox": "read-only", "approvalPolicy": "never", "config": codexAppServerConfig(config.names), "ephemeral": false}
			if p.request.Model != "" {
				params["model"] = p.request.Model
			}
			p.next(3, "thread/start", params)
		} else {
			if config.origins != p.before.origins || config.layers != p.before.layers || !slices.Equal(config.names, p.before.names) {
				p.fail()
				return
			}
			p.next(5, "turn/start", map[string]any{"threadId": p.threadID, "input": []any{map[string]string{"type": "text", "text": p.request.Prompt}}})
		}
	case 3:
		fields, err := codexRPCObject(message.Result, "thread", "approvalPolicy", "sandbox", "cwd")
		thread, threadErr := codexRPCObject(fields["thread"], "id", "turns", "ephemeral", "parentThreadId", "forkedFromId")
		sandbox, sandboxErr := codexRPCObject(fields["sandbox"], "type", "networkAccess")
		threadID, valid := codexAppServerString(thread, "id")
		var cwd, approval, policy string
		if err != nil || threadErr != nil || sandboxErr != nil || !valid ||
			json.Unmarshal(fields["cwd"], &cwd) != nil || filepath.Clean(cwd) != filepath.Clean(p.request.CWD) ||
			json.Unmarshal(fields["approvalPolicy"], &approval) != nil || approval != "never" ||
			json.Unmarshal(sandbox["type"], &policy) != nil || policy != "readOnly" ||
			!bytes.Equal(bytes.TrimSpace(thread["turns"]), []byte("[]")) || string(thread["ephemeral"]) != "false" ||
			(p.threadSeen != "" && threadID != p.threadSeen) {
			p.fail()
			return
		}
		if access := sandbox["networkAccess"]; access != nil && string(access) != "false" {
			p.fail()
			return
		}
		for _, key := range []string{"parentThreadId", "forkedFromId"} {
			if value := thread[key]; value != nil && string(value) != "null" {
				p.fail()
				return
			}
		}
		p.threadID, p.receipt.NativeSessionID = threadID, threadID
		p.emit(Event{Kind: SessionEvent})
		p.replayBuffer()
		p.next(4, "config/read", map[string]any{"cwd": p.request.CWD, "includeLayers": true})
	case 5:
		fields, err := codexRPCObject(message.Result, "turn")
		turn, turnErr := codexRPCObject(fields["turn"], "id", "status", "items", "error")
		turnID, valid := codexAppServerString(turn, "id")
		if err != nil || turnErr != nil || !valid || string(turn["status"]) != `"inProgress"` || !codexAppServerEmptyItems(turn["items"]) || !codexAppServerNoError(turn["error"]) {
			p.fail()
			return
		}
		p.turnID = turnID
		p.replayBuffer()
	default:
		p.fail()
	}
}

func (p *codexAppServerProtocol) replayBuffer() {
	buffer := p.buffer
	p.buffer, p.bufferBytes = nil, 0
	for _, notification := range buffer {
		if p.damaged {
			break
		}
		p.notification(notification)
	}
}

func codexAppServerEmptyItems(raw json.RawMessage) bool {
	var items []json.RawMessage
	return json.Unmarshal(raw, &items) == nil && items != nil && len(items) == 0
}

func codexAppServerNoError(raw json.RawMessage) bool {
	return raw == nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func (p *codexAppServerProtocol) notification(message codexRPCMessage) {
	method := *message.Method
	if method == "remoteControl/status/changed" {
		// Installed 0.166 emits this connection-level notification during
		// initialization. It is passive status, never execution or authority.
		fields, err := codexAppServerMap(message.Params)
		valid := err == nil && message.ID == nil
		for key := range fields {
			if key != "installationId" && key != "serverName" && key != "environmentId" && key != "status" {
				valid = false
			}
		}
		for _, key := range []string{"installationId", "serverName", "status"} {
			var value *string
			if json.Unmarshal(fields[key], &value) != nil || value == nil {
				valid = false
			}
		}
		if environment := fields["environmentId"]; environment != nil {
			var value *string
			if json.Unmarshal(environment, &value) != nil {
				valid = false
			}
		}
		var status string
		_ = json.Unmarshal(fields["status"], &status)
		if !valid || !slices.Contains([]string{"disabled", "connecting", "connected", "errored"}, status) {
			p.fail()
		}
		return
	}
	if method == "account/updated" || method == "account/rateLimits/updated" {
		// Passive connection metadata is not platform account readiness or a grant.
		fields, err := codexAppServerMap(message.Params)
		if err != nil || message.ID != nil {
			p.fail()
			return
		}
		if method == "account/rateLimits/updated" {
			if _, err := codexAppServerMap(fields["rateLimits"]); err != nil || len(fields) != 1 {
				p.fail()
			}
			return // Quota values are opaque; do not retain, log or act on them.
		}
		for key, raw := range fields {
			if key != "authMode" && key != "planType" {
				p.fail()
				return
			}
			var value *string
			if json.Unmarshal(raw, &value) != nil {
				p.fail()
				return
			}
			if value == nil {
				continue
			}
			allowed := []string{"apikey", "chatgpt", "chatgptAuthTokens", "headers", "agentIdentity", "personalAccessToken", "bedrockApiKey", "bedrockAccessKeys"}
			if key == "planType" {
				allowed = []string{"free", "go", "plus", "pro", "prolite", "promax", "team", "self_serve_business_prolite", "self_serve_business_usage_based", "business", "ent26", "enterprise_cbp_automation", "enterprise_cbp_usage_based", "enterprise", "edu", "edu_plus", "edu_pro", "sci", "sci_plus", "unknown"}
			}
			if !slices.Contains(allowed, *value) {
				p.fail()
				return
			}
		}
		return
	}
	fields, err := codexRPCObject(message.Params, "thread", "threadId", "turn", "turnId", "item", "itemId", "requestId", "willRetry", "message", "threadName", "projectId")
	if err != nil {
		p.fail()
		return
	}
	if method == "thread/started" && message.ID == nil {
		thread, err := codexRPCObject(fields["thread"], "id")
		id, valid := codexAppServerString(thread, "id")
		if err != nil || !valid || p.threadSeen != "" || (p.awaiting != 3 && p.threadID == "") || (p.threadID != "" && id != p.threadID) {
			p.fail()
			return
		}
		p.threadSeen = id
		return
	}
	// Missing and null threadId both mean an unscoped warning in 0.166.
	if method == "warning" {
		var text *string
		if message.ID != nil || json.Unmarshal(fields["message"], &text) != nil || text == nil {
			p.fail()
			return
		}
		if codexAppServerNoError(fields["threadId"]) {
			return
		}
	}
	if message.ID == nil && (method == "configWarning" || method == "deprecationNotice") && fields["threadId"] == nil {
		return
	}
	threadID, valid := codexAppServerString(fields, "threadId")
	if !valid || threadID != p.threadID || p.threadID == "" {
		p.fail()
		return
	}
	if method == "thread/name/updated" || method == "thread/project/updated" {
		key := "threadName"
		if method == "thread/project/updated" {
			key = "projectId"
		}
		var value *string
		if message.ID != nil || (fields[key] != nil || key == "projectId") && json.Unmarshal(fields[key], &value) != nil || fields["turnId"] != nil {
			p.fail()
		}
		return // Bound native metadata never changes platform identity or policy.
	}
	if method == "thread/status/changed" || method == "thread/closed" || method == "thread/tokenUsage/updated" || method == "serverRequest/resolved" || method == "warning" {
		if message.ID != nil {
			p.fail()
		}
		if id := fields["turnId"]; id != nil && (p.turnID == "" || string(id) != string(mustCodexAppServerJSON(p.turnID))) {
			p.fail()
		}
		if method == "serverRequest/resolved" {
			key := string(fields["requestId"])
			if !p.requests[key] {
				p.fail()
			}
			p.requests[key] = false
		}
		return
	}
	if p.turnID == "" {
		p.fail()
		return
	}
	if method == "turn/started" || method == "turn/completed" {
		if message.ID != nil {
			p.fail()
			return
		}
		p.turnNotification(method, fields["turn"])
		return
	}
	turnID, valid := codexAppServerString(fields, "turnId")
	if !valid || turnID != p.turnID || !p.receipt.Accepted || p.receipt.Terminal != "" {
		p.fail()
		return
	}
	if message.ID != nil {
		key := string(message.ID)
		if _, duplicate := p.requests[key]; duplicate || len(key) > 512 || len(p.requests) >= 1024 {
			p.fail()
			return
		}
		p.requests[key] = true
		var result any
		switch method {
		case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
			result = map[string]string{"decision": "cancel"}
		case "item/permissions/requestApproval":
			result = map[string]any{"permissions": map[string]any{}, "scope": "turn"}
		case "mcpServer/elicitation/request":
			result = map[string]any{"action": "cancel", "content": nil}
		}
		if !p.reply(message, result) {
			p.fail()
		}
		p.external = true
		p.cancel()
		return
	}
	switch method {
	case "item/started", "item/completed":
		p.item(fields["item"], method == "item/completed")
	case "item/agentMessage/delta", "item/reasoning/summaryTextDelta", "item/reasoning/textDelta", "item/reasoning/summaryPartAdded", "item/plan/delta":
		kind := map[string]string{"item/agentMessage/delta": "agentMessage", "item/reasoning/summaryTextDelta": "reasoning", "item/reasoning/textDelta": "reasoning", "item/reasoning/summaryPartAdded": "reasoning", "item/plan/delta": "plan"}[method]
		id, valid := codexAppServerString(fields, "itemId")
		item, exists := p.items[id]
		if !valid || !exists || item.completed || item.kind != kind {
			p.fail()
		}
	case "turn/plan/updated", "model/rerouted", "model/verification", "model/safetyBuffering/updated":
	case "error":
		// A diagnostic (including a retry) is not a terminal receipt.
	default:
		p.external = true
		p.cancel()
	}
}

func (p *codexAppServerProtocol) turnNotification(method string, raw json.RawMessage) {
	turn, err := codexRPCObject(raw, "id", "status", "items", "error")
	id, valid := codexAppServerString(turn, "id")
	var status string
	var items []json.RawMessage
	if err != nil || !valid || id != p.turnID || json.Unmarshal(turn["status"], &status) != nil || json.Unmarshal(turn["items"], &items) != nil || items == nil {
		p.fail()
		return
	}
	if method == "turn/started" {
		if p.receipt.Accepted || p.receipt.Terminal != "" || status != "inProgress" || len(items) != 0 || !codexAppServerNoError(turn["error"]) {
			p.fail()
			return
		}
		p.receipt.Accepted = true
		p.emit(Event{Kind: AcceptedEvent})
		return
	}
	terminal := map[string]Outcome{"completed": OutcomeSucceeded, "failed": OutcomeFailed, "interrupted": OutcomeAborted}[status]
	if !p.receipt.Accepted || p.receipt.Terminal != "" || terminal == "" {
		p.fail()
		return
	}
	if !codexAppServerNoError(turn["error"]) {
		fields, err := codexRPCObject(turn["error"], "message")
		var text string
		if err != nil || json.Unmarshal(fields["message"], &text) != nil || text == "" || status == "completed" {
			p.fail()
			return
		}
	} else if status == "failed" {
		p.fail()
		return
	}
	seen := make(map[string]bool, len(items))
	for _, raw := range items {
		fields, err := codexRPCObject(raw, "id", "type", "text", "status")
		id, idOK := codexAppServerString(fields, "id")
		kind, kindOK := codexAppServerString(fields, "type")
		status, statusOK := "", true
		if fields["status"] != nil {
			status, statusOK = codexAppServerString(fields, "status")
		}
		known, exists := p.items[id]
		if err != nil || !idOK || !kindOK || !statusOK || seen[id] || exists && (known.kind != kind || known.status != status || !known.completed) {
			p.fail()
			return
		}
		seen[id] = true
		if kind == "agentMessage" {
			var text string
			item := p.items[id]
			if json.Unmarshal(fields["text"], &text) != nil || !item.completed || item.kind != kind || item.textHash != sha256.Sum256([]byte(text)) {
				p.fail()
				return
			}
		} else if kind != "userMessage" && kind != "reasoning" && kind != "plan" && kind != "contextCompaction" {
			p.external = true
		}
	}
	p.receipt.Terminal = terminal
	p.emit(Event{Kind: TerminalEvent, Terminal: terminal})
}

func (p *codexAppServerProtocol) item(raw json.RawMessage, completed bool) {
	fields, err := codexRPCObject(raw, "id", "type", "text", "status")
	id, idOK := codexAppServerString(fields, "id")
	kind, kindOK := codexAppServerString(fields, "type")
	status, statusOK := "", true
	if fields["status"] != nil {
		status, statusOK = codexAppServerString(fields, "status")
	}
	previous, exists := p.items[id]
	if err != nil || !idOK || !kindOK || !statusOK || len(p.items) >= 4096 || (exists && (previous.completed || previous.kind != kind || !completed)) {
		p.fail()
		return
	}
	item := codexAppServerItem{kind: kind, status: status, completed: completed}
	switch kind {
	case "agentMessage":
		if completed {
			var text *string
			if json.Unmarshal(fields["text"], &text) != nil || text == nil {
				p.fail()
				return
			}
			item.textHash = sha256.Sum256([]byte(*text))
			p.emit(Event{Kind: ResponseEvent, Text: *text})
		}
	case "userMessage", "reasoning", "plan", "contextCompaction":
	default:
		// No external tools are permitted in this text-only slice. Even a tool's
		// completed item does not prove its background work or agents stopped.
		p.external = true
		p.cancel()
	}
	p.items[id] = item
}

func (p *codexAppServerProtocol) settled() bool {
	if p.awaiting != 0 || p.damaged || p.external {
		return false
	}
	response := false
	for _, item := range p.items {
		if !item.completed {
			return false
		}
		response = response || item.kind == "agentMessage"
	}
	return p.receipt.Terminal != OutcomeSucceeded || response
}

func (p *codexAppServerProtocol) interrupt() {
	if p.threadID != "" && p.turnID != "" && !p.interruptSent && p.receipt.Terminal == "" {
		p.interruptSent = true
		if !p.send(6, "turn/interrupt", map[string]string{"threadId": p.threadID, "turnId": p.turnID}) {
			p.fail()
		}
	}
}

func mustCodexAppServerJSON(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}
