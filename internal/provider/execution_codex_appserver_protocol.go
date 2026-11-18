package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

var codexAppServerDisabledFeatures = []string{
	"plugins", "apps", "hooks", "shell_tool", "view_image", "unified_exec", "multi_agent", "multi_agent_v2",
	"code_mode", "code_mode_only", "code_mode_host", "tool_suggest",
}

func codexAppServerConfig(names []string) map[string]any {
	features := make(map[string]any, len(codexAppServerDisabledFeatures))
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
		if kind == "system" {
			// A version claiming emptiness is not proof; inspect the complete object.
			fields, fieldsErr := codexRPCObject(layer, "config")
			config, configErr := codexAppServerMap(fields["config"])
			if fieldsErr != nil || configErr != nil || len(config) != 0 {
				return snapshot, errCodexRPCFrame
			}
		}
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

// Accept known local provenance only. readCodexAppServerConfig additionally
// requires every system layer's full config to be empty. Managed/unknown sources
// remain unsupported even when disabled or absent from winning origins.
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
	case "packagedDefaults", "user", "system":
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
	server    string
	tool      string
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
	member                *codexAppServerMemberSession
	session               *codexAppServerSession
	awaiting              int
	threadID              string
	turnID                string
	threadSeen            string
	before                codexAppServerConfigSnapshot
	buffer                []codexRPCMessage
	bufferBytes           int
	resumeUsageTurnIDs    []string
	resumeUsageBytes      int
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
		resumeMetadata := p.session != nil && p.session.resume != nil && (method == "thread/goal/cleared" || method == "thread/tokenUsage/updated")
		provisional := p.awaiting == 3 && (resumeMetadata || slices.Contains([]string{"thread/status/changed", "thread/name/updated", "thread/project/updated", "mcpServer/startupStatus/updated"}, method)) || p.awaiting == 5 &&
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
	if p.awaiting == 0 || !bytes.Equal(bytes.TrimSpace(message.ID), []byte(strconv.Itoa(p.awaiting))) || message.Error != nil && (p.session == nil || p.awaiting != 9) {
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
		if p.session != nil {
			if !nativeSessionPath(home) || p.session.resume != nil && p.session.resume.NativeHome != home {
				p.fail()
				return
			}
			p.session.home = home
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
		if p.session != nil && !p.session.currentConfig(message.Result, id == 2) {
			p.fail()
			return
		}
		p.summary(config)
		if id == 2 {
			p.before = config
			threadConfig := codexAppServerConfig(config.names)
			if p.member != nil && p.member.configure(config.names, threadConfig) != nil {
				p.fail()
				return
			}
			params := map[string]any{"cwd": p.request.CWD, "sandbox": "read-only", "approvalPolicy": "never", "config": threadConfig, "ephemeral": false}
			if p.request.Model != "" {
				params["model"] = p.request.Model
			}
			method := "thread/start"
			if p.session != nil {
				// Disable persisted autonomous goals before native thread initialization.
				threadConfig["features"].(map[string]any)["goals"] = false
				// Let Codex resolve an implicit provider; pinning the expected
				// default here could mask a configured provider in the response.
				if p.session.providerSet {
					params["modelProvider"] = p.session.provider
				}
				params["approvalsReviewer"] = "user"
				threadConfig["approvals_reviewer"] = "user"
				if p.session.effort != "" {
					threadConfig["model_reasoning_effort"] = p.session.effort
				}
				threadConfig["mcp_servers"].(map[string]any)[p.member.server].(map[string]any)["required"] = true
				if p.session.resume != nil {
					method = "thread/resume"
					delete(params, "ephemeral")
					params["threadId"], params["excludeTurns"] = p.session.resume.NativeThreadID, true
				}
			}
			p.next(3, method, params)
		} else {
			if config.origins != p.before.origins || config.layers != p.before.layers || !slices.Equal(config.names, p.before.names) {
				p.fail()
				return
			}
			if p.member != nil {
				// Both selectors are mandatory: omitting serverName discovers all
				// inherited servers using freshly loaded config in Codex 0.166.
				p.next(7, "mcpServerStatus/list", map[string]any{"threadId": p.threadID, "serverName": p.member.server, "detail": "toolsAndAuthOnly", "limit": 1})
			} else {
				p.startTurn()
			}
		}
	case 3:
		// In verified 0.166, the actual snapshot projects writable, external or
		// unrepresentable permissions away from readOnly. Profile identity and
		// runtime workspace roots alone do not grant writes.
		fields, err := codexRPCObject(message.Result, "thread", "approvalPolicy", "sandbox", "cwd", "model")
		thread, threadErr := codexRPCObject(fields["thread"], "id", "turns", "ephemeral", "parentThreadId", "forkedFromId")
		sandbox, sandboxErr := codexRPCObject(fields["sandbox"], "type", "networkAccess")
		threadID, valid := codexAppServerString(thread, "id")
		var cwd, approval, policy, model string
		if err != nil || threadErr != nil || sandboxErr != nil || !valid ||
			json.Unmarshal(fields["cwd"], &cwd) != nil || filepath.Clean(cwd) != filepath.Clean(p.request.CWD) ||
			json.Unmarshal(fields["approvalPolicy"], &approval) != nil || approval != "never" ||
			p.request.Model != "" && (json.Unmarshal(fields["model"], &model) != nil || model != p.request.Model) ||
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
		if p.session != nil && !p.session.threadResponse(message.Result, p.request, threadID) {
			p.fail()
			return
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
		if p.session != nil && p.session.priorTurnIDs[turnID] {
			p.fail()
			return
		}
		p.turnID = turnID
		p.replayBuffer()
	case 7:
		if p.member == nil {
			p.fail()
			return
		}
		p.memberStatus(message.Result)
	case 8:
		if p.session == nil || p.session.resume == nil || !p.verifySessionHistory(message.Result, false) {
			p.fail()
			return
		}
		p.startTurn()
	case 9:
		// Optional anchor unavailability drops only the reference. Positive
		// contradictory evidence instead fences settlement inside verification.
		if message.Error == nil {
			p.verifySessionHistory(message.Result, true)
		}
	default:
		p.fail()
	}
}

func (p *codexAppServerProtocol) startTurn() {
	params := map[string]any{"threadId": p.threadID, "input": []any{map[string]string{"type": "text", "text": p.request.Prompt}}}
	if p.session != nil {
		params["cwd"], params["model"], params["approvalPolicy"] = p.request.CWD, p.request.Model, "never"
		params["sandboxPolicy"] = map[string]any{"type": "readOnly", "networkAccess": false}
		if p.session.effort != "" {
			params["effort"] = p.session.effort
		}
	}
	p.next(5, "turn/start", params)
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
	if p.member != nil && p.memberNotification(message) {
		return
	}
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
	if p.session != nil && p.session.resume != nil {
		if method == "thread/goal/cleared" {
			// 0.166's empty goal snapshot is thread-only passive metadata, not
			// permission to adopt a goal or native autonomous work.
			goal, err := codexAppServerMap(message.Params)
			if err != nil || message.ID != nil || len(goal) != 1 {
				p.fail()
			}
			return
		}
		if method == "thread/tokenUsage/updated" {
			id, valid := codexAppServerString(fields, "turnId")
			if message.ID != nil || !valid {
				p.fail()
				return
			}
			if p.session.priorTurnIDs == nil {
				// Retain only bounded attribution IDs until the writer-held full
				// history matches the supplied anchor. No current identity is granted.
				size := len(method) + len(message.Params)
				if len(p.resumeUsageTurnIDs) >= executionFramesCountLimit || size > executionFramesByteLimit-p.resumeUsageBytes {
					p.fail()
					return
				}
				p.resumeUsageTurnIDs = append(p.resumeUsageTurnIDs, id)
				p.resumeUsageBytes += size
				return
			}
			if p.session.priorTurnIDs[id] {
				return // Delayed replay may race the new turn; only verified old IDs qualify.
			}
		}
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
		if p.session != nil && p.turnID == "" && (method == "thread/closed" || method == "thread/status/changed" && !codexSessionIdle(message.Params, "status")) {
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
	case "item/mcpToolCall/progress":
		id, valid := codexAppServerString(fields, "itemId")
		item, exists := p.items[id]
		if p.member == nil || !valid || !exists || item.kind != "mcpToolCall" || item.completed || item.server != p.member.server || !slices.Contains(p.member.names, item.tool) {
			p.fail()
		}
	case "model/rerouted":
		if p.request.Model != "" {
			p.fail() // A selected model is not permission for a silent fallback.
		}
	case "turn/plan/updated", "model/verification", "model/safetyBuffering/updated":
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
		} else if kind == "mcpToolCall" && p.member != nil {
			item, valid := p.memberItem(raw, true)
			if !valid || !exists || !known.completed || known.server != item.server || known.tool != item.tool {
				p.fail()
				return
			}
		} else if kind != "userMessage" && kind != "reasoning" && kind != "plan" && kind != "contextCompaction" {
			p.external = true
		}
	}
	p.receipt.Terminal = terminal
	p.emit(Event{Kind: TerminalEvent, Terminal: terminal})
	if p.session != nil && terminal == OutcomeSucceeded && p.settled() {
		p.next(9, "thread/read", map[string]any{"threadId": p.threadID, "includeTurns": true})
	}
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
	case "mcpToolCall":
		if p.member == nil {
			p.external = true
			p.cancel()
			break
		}
		var valid bool
		item, valid = p.memberItem(raw, completed)
		if !valid {
			p.fail()
			return
		}
	default:
		// Only explicitly bound platform MCP calls can be controlled. Even a tool's
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

// Per-invocation state only: never retain a native process, token or transcript.
// The original native writer is acquired by cold thread/resume and retained
// through the final history read and ordinary stdin shutdown.
type codexAppServerSession struct {
	cliPath      string
	home         string
	toolsProfile string
	provider     string
	providerSet  bool // The first config/read explicitly selected the provider.
	effort       string
	source       string // Thread.source bound by this invocation's start/resume response.
	binding      json.RawMessage
	resume       *CodexNativeSessionReference
	pending      *NativeSessionReference
	priorTurnIDs map[string]bool
}

func codexSessionToolsProfile(names []string) string {
	// Match Store.NativeToolsProfileSHA256 without changing issuance order.
	// The adapter/protocol versions separately bind the fixed native policy.
	canonical := slices.Clone(names)
	slices.Sort(canonical)
	canonical = slices.Compact(canonical)
	digest := sha256.Sum256(mustCodexAppServerJSON(canonical))
	return hex.EncodeToString(digest[:])
}

func (s *codexAppServerSession) currentConfig(raw json.RawMessage, first bool) bool {
	fields, err := codexRPCObject(raw, "config")
	config, configErr := codexRPCObject(fields["config"], "model_provider", "model_reasoning_effort")
	if err != nil || configErr != nil {
		return false
	}
	// In 0.166.0 native resolution is required provider > runtime override >
	// config setting > openai. This is an expectation checked on thread response.
	provider, valid := "openai", true
	providerSet := !codexAppServerNoError(config["model_provider"])
	if providerSet {
		provider, valid = codexAppServerString(config, "model_provider")
	}
	effort := ""
	if !codexAppServerNoError(config["model_reasoning_effort"]) {
		var effortOK bool
		effort, effortOK = codexAppServerString(config, "model_reasoning_effort")
		valid = valid && effortOK
	}
	if !valid || !first && (provider != s.provider || effort != s.effort) ||
		s.resume != nil && (provider != s.resume.Provider || effort != s.resume.ReasoningEffort) {
		return false
	}
	if first {
		s.provider, s.providerSet, s.effort = provider, providerSet, effort
	}
	return true
}

func codexSessionIdle(raw json.RawMessage, key string) bool {
	fields, err := codexRPCObject(raw, key)
	status, statusErr := codexAppServerMap(fields[key])
	return err == nil && statusErr == nil && len(status) == 1 && string(status["type"]) == `"idle"`
}

func (s *codexAppServerSession) threadResponse(raw json.RawMessage, request ExecuteRequest, threadID string) bool {
	fields, err := codexRPCObject(raw, "modelProvider", "reasoningEffort", "approvalsReviewer", "thread")
	provider, valid := codexAppServerString(fields, "modelProvider")
	effort := ""
	if !codexAppServerNoError(fields["reasoningEffort"]) {
		var effortOK bool
		effort, effortOK = codexAppServerString(fields, "reasoningEffort")
		valid = valid && effortOK
	}
	thread, threadErr := codexRPCObject(fields["thread"], "cliVersion", "source", "cwd", "modelProvider")
	// rust-v0.166.0's CLI app-server entrypoint passes SessionSource::VSCode
	// even over stdio. Keep the previously admitted AppServer variant, but
	// do not treat the labels as interchangeable within a connection.
	source, _ := codexAppServerString(thread, "source")
	var cwd string
	if err != nil || threadErr != nil || !valid || provider != s.provider || effort != s.effort ||
		string(fields["approvalsReviewer"]) != `"user"` || !codexSessionIdle(fields["thread"], "status") ||
		string(thread["cliVersion"]) != `"0.166.0"` || (source != "appServer" && source != "vscode") ||
		json.Unmarshal(thread["cwd"], &cwd) != nil || cwd != request.CWD ||
		string(thread["modelProvider"]) != string(mustCodexAppServerJSON(s.provider)) ||
		(s.resume != nil && threadID != s.resume.NativeThreadID) {
		return false
	}
	s.source = source
	return true
}

// This is a public-history anchor, not a CAS over hidden native settings or
// arbitrary file bytes. It excludes runtime timing/status metadata at thread
// level, but hashes actual stable turn/item contents, including MCP arguments and
// results. Full content is held only in this bounded stack-local snapshot.
type codexSessionPublicHistory struct {
	turns []any
	items int
}

func (h codexSessionPublicHistory) digest() string {
	digest := sha256.Sum256(mustCodexAppServerJSON(h.turns))
	return hex.EncodeToString(digest[:])
}

func (h codexSessionPublicHistory) matches(ref *CodexNativeSessionReference) bool {
	if len(h.turns) != ref.HistoryTurns || h.items != ref.HistoryItems || h.digest() != ref.HistorySHA256 {
		return false
	}
	last := h.turns[len(h.turns)-1].(map[string]any)
	return last["id"] == ref.NativeTurnID && last["status"] == "completed"
}

type codexSessionHistoryEvidence uint8

const (
	codexSessionHistoryUnavailable codexSessionHistoryEvidence = iota
	codexSessionHistoryComplete
	codexSessionHistoryConflict
)

func (p *codexAppServerProtocol) sessionHistory(raw json.RawMessage) (codexSessionPublicHistory, bool) {
	history, evidence := p.sessionHistoryEvidence(raw, false)
	return history, evidence == codexSessionHistoryComplete
}

func (p *codexAppServerProtocol) sessionHistoryEvidence(raw json.RawMessage, capture bool) (codexSessionPublicHistory, codexSessionHistoryEvidence) {
	// Uninspectable input cannot prove absence of external activity. This is
	// distinct from a safely inspected history whose optional anchor is unsupported.
	if len(raw) > codexRPCFrameLimit {
		return codexSessionPublicHistory{}, codexSessionHistoryConflict
	}
	decoded, err := nativeSessionJSON(raw, 64, false)
	response, ok := decoded.(map[string]any)
	if err != nil || !ok {
		return codexSessionPublicHistory{}, codexSessionHistoryConflict
	}
	thread, ok := response["thread"].(map[string]any)
	if !ok || !p.inspectSessionHistory(thread, capture) {
		return codexSessionPublicHistory{}, codexSessionHistoryConflict
	}
	return p.projectSessionHistory(thread, capture)
}

// Inspect raw execution evidence before any optional anchor projection. Safety
// budgets fence an uninspectable capture rather than making it binding-only success.
func (p *codexAppServerProtocol) inspectSessionHistory(thread map[string]any, capture bool) bool {
	if thread["id"] != p.threadID || thread["ephemeral"] != false || thread["parentThreadId"] != nil || thread["forkedFromId"] != nil ||
		thread["cliVersion"] != "0.166.0" || p.session.source == "" || thread["source"] != p.session.source ||
		thread["cwd"] != p.request.CWD || thread["modelProvider"] != p.session.provider {
		return false
	}
	// Active is a native projection, not proof of newly spawned work. It still
	// prevents proving absence of other activity after the observed terminal.
	status, ok := thread["status"].(map[string]any)
	if !ok || status["type"] != "idle" {
		return false
	}
	turns, ok := thread["turns"].([]any)
	if !ok || len(turns) == 0 || len(turns) > codexSessionHistoryTurnLimit {
		return false
	}
	if capture {
		expectedTurns := 1
		if p.session.resume != nil {
			expectedTurns += p.session.resume.HistoryTurns
		}
		last, ok := turns[len(turns)-1].(map[string]any)
		if len(turns) != expectedTurns || !ok || last["id"] != p.turnID {
			return false
		}
		if p.session.resume != nil {
			if len(turns) < 2 {
				return false
			}
			prior, ok := turns[len(turns)-2].(map[string]any)
			if !ok || prior["id"] != p.session.resume.NativeTurnID {
				return false
			}
		}
	}
	turnIDs, itemIDs := map[string]bool{}, map[string]bool{}
	itemCount := 0
	for index, value := range turns {
		turn, ok := value.(map[string]any)
		id, idOK := turn["id"].(string)
		items, itemsOK := turn["items"].([]any)
		if !ok || !idOK || !codexAppServerIdentity(id) || turnIDs[id] || turn["status"] != "completed" || turn["error"] != nil || !itemsOK ||
			len(items) > codexSessionHistoryItemLimit-itemCount {
			return false
		}
		if capture && index < len(turns)-1 && !p.session.priorTurnIDs[id] {
			return false
		}
		if root := turn["rootTurnId"]; root != nil {
			rootID, ok := root.(string)
			if !ok || !codexAppServerIdentity(rootID) {
				return false
			}
		}
		turnIDs[id], itemCount = true, itemCount+len(items)
		current := capture && index == len(turns)-1
		seen, users := map[string]bool{}, 0
		for _, value := range items {
			item, ok := value.(map[string]any)
			id, idOK := item["id"].(string)
			kind, kindOK := item["type"].(string)
			if !ok || !idOK || !codexAppServerIdentity(id) || itemIDs[id] || !kindOK ||
				!slices.Contains([]string{"userMessage", "agentMessage", "reasoning", "plan", "contextCompaction", "mcpToolCall"}, kind) ||
				kind == "mcpToolCall" && item["status"] != "completed" {
				return false
			}
			itemIDs[id] = true
			if current {
				seen[id] = true
				if !p.capturedSessionItemMatches(item) {
					return false
				}
				if kind == "userMessage" {
					users++
					if users > 1 {
						return false
					}
				}
			}
		}
		// The schema defaults an omitted itemsView to full. Partial views can
		// prove visible contradictions, but cannot prove that an item is absent.
		if current && (turn["itemsView"] == nil || turn["itemsView"] == "full") {
			if users != 1 {
				return false
			}
			for id := range p.items {
				if !seen[id] {
					return false
				}
			}
		}
	}
	return true
}

// Projection runs only after all bounded execution evidence has been inspected.
// Unsupported anchor fields must not bypass the independent capture checks.
func (p *codexAppServerProtocol) projectSessionHistory(thread map[string]any, capture bool) (codexSessionPublicHistory, codexSessionHistoryEvidence) {
	var history codexSessionPublicHistory
	evidence := codexSessionHistoryComplete
	if len(thread["status"].(map[string]any)) != 1 {
		evidence = codexSessionHistoryUnavailable
	}
	turns := thread["turns"].([]any)
	history.turns = make([]any, 0, len(turns))
	prefixComplete := true
	for index, value := range turns {
		if capture && index == len(turns)-1 && p.session.resume != nil && prefixComplete && !history.matches(p.session.resume) {
			return codexSessionPublicHistory{}, codexSessionHistoryConflict
		}
		turn := value.(map[string]any)
		items := turn["items"].([]any)
		if turn["itemsView"] != nil && turn["itemsView"] != "full" {
			evidence, prefixComplete = codexSessionHistoryUnavailable, false
		}
		projectedItems := make([]any, 0, len(items))
		for _, value := range items {
			item := value.(map[string]any)
			projected, valid := codexSessionPublicItem(item, p.member.names)
			if !valid {
				if item["type"] == "mcpToolCall" {
					return codexSessionPublicHistory{}, codexSessionHistoryConflict
				}
				evidence, prefixComplete = codexSessionHistoryUnavailable, false
				continue
			}
			projectedItems = append(projectedItems, projected)
		}
		history.items += len(items)
		history.turns = append(history.turns, map[string]any{"id": turn["id"], "rootTurnId": turn["rootTurnId"], "status": "completed", "items": projectedItems, "error": nil})
	}
	return history, evidence
}

func codexSessionPublicItem(item map[string]any, tools []string) (map[string]any, bool) {
	id, idOK := item["id"].(string)
	kind, kindOK := item["type"].(string)
	if !idOK || !kindOK || !codexAppServerIdentity(id) {
		return nil, false
	}
	projected := map[string]any{"id": id, "type": kind}
	switch kind {
	case "userMessage":
		content, ok := item["content"].([]any)
		if !ok || len(content) == 0 {
			return nil, false
		}
		for _, value := range content {
			part, ok := value.(map[string]any)
			if !ok || part["type"] != "text" {
				return nil, false
			}
			if _, ok := part["text"].(string); !ok {
				return nil, false
			}
		}
		projected["content"], projected["clientId"] = content, item["clientId"]
	case "agentMessage":
		text, ok := item["text"].(string)
		if !ok || item["delivery"] != nil || item["questions"] != nil {
			return nil, false
		}
		projected["text"], projected["phase"], projected["memoryCitation"] = text, item["phase"], item["memoryCitation"]
	case "reasoning":
		for _, key := range []string{"summary", "content"} {
			parts := []any{}
			if value, exists := item[key]; exists {
				var ok bool
				parts, ok = value.([]any)
				if !ok {
					return nil, false
				}
			}
			for _, part := range parts {
				if _, ok := part.(string); !ok {
					return nil, false
				}
			}
			projected[key] = parts
		}
	case "mcpToolCall":
		server, serverOK := item["server"].(string)
		tool, toolOK := item["tool"].(string)
		_, argsOK := item["arguments"].(map[string]any)
		result, resultOK := item["result"].(map[string]any)
		content, contentOK := result["content"].([]any)
		if !serverOK || !codexAppServerIdentity(server) || !toolOK || !slices.Contains(tools, tool) || !argsOK || !resultOK || !contentOK || item["status"] != "completed" || item["error"] != nil {
			return nil, false
		}
		for _, key := range []string{"appContext", "mcpAppUi", "pluginId", "mcpAppResourceUri"} {
			if item[key] != nil {
				return nil, false
			}
		}
		for _, value := range content {
			part, ok := value.(map[string]any)
			if !ok || part["type"] != "text" {
				return nil, false
			}
			if _, ok := part["text"].(string); !ok {
				return nil, false
			}
		}
		if hint := item["readOnlyHint"]; hint != nil {
			if _, ok := hint.(bool); !ok {
				return nil, false
			}
		}
		for _, key := range []string{"server", "tool", "arguments", "result", "readOnlyHint"} {
			projected[key] = item[key]
		}
		projected["status"], projected["error"] = "completed", nil
	default:
		// No file/resource/dynamic/compaction/experimental plan adoption.
		return nil, false
	}
	return projected, true
}

func (p *codexAppServerProtocol) capturedSessionItemMatches(item map[string]any) bool {
	id, kind := item["id"].(string), item["type"].(string)
	known, exists := p.items[id]
	if exists && (known.kind != kind || !known.completed) {
		return false
	}
	switch kind {
	case "userMessage":
		content, ok := item["content"].([]any)
		if !ok || len(content) != 1 {
			return false
		}
		part, ok := content[0].(map[string]any)
		text, textOK := part["text"].(string)
		return ok && textOK && part["type"] == "text" && text == p.request.Prompt
	case "mcpToolCall":
		_, valid := p.memberItem(mustCodexAppServerJSON(item), true)
		return valid
	case "agentMessage":
		text, ok := item["text"].(string)
		return exists && ok && known.textHash == sha256.Sum256([]byte(text))
	case "reasoning":
		return exists && known.kind == kind
	case "plan", "contextCompaction":
		return true // Unsupported passive anchors still require identity and coverage checks.
	}
	return false
}

func (p *codexAppServerProtocol) verifySessionHistory(raw json.RawMessage, capture bool) bool {
	history, evidence := p.sessionHistoryEvidence(raw, capture)
	if evidence != codexSessionHistoryComplete {
		if capture && evidence == codexSessionHistoryConflict {
			p.external = true
		}
		return false
	}
	s := p.session
	if !capture {
		if s.resume == nil || !history.matches(s.resume) {
			return false
		}
		s.priorTurnIDs = make(map[string]bool, len(history.turns))
		for _, value := range history.turns {
			s.priorTurnIDs[value.(map[string]any)["id"].(string)] = true
		}
		for _, id := range p.resumeUsageTurnIDs {
			if !s.priorTurnIDs[id] {
				return false
			}
		}
		p.resumeUsageTurnIDs, p.resumeUsageBytes = nil, 0
		return true
	}
	s.pending = &NativeSessionReference{Version: NativeSessionReferenceVersion, Binding: bytes.Clone(s.binding),
		Codex: &CodexNativeSessionReference{Adapter: codexSessionAdapter, Protocol: codexSessionProtocol, CLIPath: s.cliPath, CLIVersion: "0.166.0",
			Provider: s.provider, Model: p.request.Model, ReasoningEffort: s.effort, CWD: p.request.CWD, ToolsProfileSHA256: s.toolsProfile,
			NativeHome: s.home, NativeThreadID: p.threadID, NativeTurnID: p.turnID, HistorySHA256: history.digest(), HistoryTurns: len(history.turns), HistoryItems: history.items}}
	return true
}
