package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

const (
	acpToolLimit        = 256
	acpProvisionalLimit = 16
	acpProvisionalBytes = 64 << 10
	acpAnswerLimit      = 1 << 19
)

type acpTool struct {
	status   string
	identity string
	unsafe   bool
}

func (t acpTool) ended() bool { return t.status == "completed" || t.status == "failed" }

type acpProtocol struct {
	ctx                                                        context.Context
	cancel                                                     context.CancelFunc
	config                                                     ACPConfig
	request                                                    ExecuteRequest
	launch                                                     acpLaunch
	receipt                                                    *ExecutionReceipt
	delivery                                                   *executionDelivery
	send                                                       func(acpMessage) bool
	summary                                                    func(string, map[string]any)
	awaiting                                                   int
	promptID                                                   int
	agyCurrentModel                                            string
	damaged, external, interrupted, promptSent, promptReturned bool
	selectionConfirmed                                         bool
	effort                                                     string
	tools                                                      map[string]acpTool
	requests                                                   map[string]bool
	provisional                                                []json.RawMessage
	provisionalBytes                                           int
	answer                                                     strings.Builder
}

func (p *acpProtocol) fail() { p.damaged = true; p.cancel() }

func (p *acpProtocol) event(event Event) {
	p.delivery.enqueue(event)
}

func (p *acpProtocol) next(id int, method string, params any) {
	p.awaiting = id
	if !p.send(acpMessage{JSONRPC: "2.0", ID: json.RawMessage(strconv.Itoa(id)), Method: method, Params: acpJSON(params)}) {
		p.fail()
	}
}

func (p *acpProtocol) initialize() {
	p.next(1, "initialize", map[string]any{"protocolVersion": 1, "clientInfo": map[string]string{"name": "agent-workspace", "version": "0.1.0"}, "clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false}})
}

func (p *acpProtocol) receive(m acpMessage) {
	if m.Method != "" {
		if m.ID != nil {
			p.agentRequest(m)
			return
		}
		if m.Method != "session/update" {
			p.fail()
			return
		}
		if p.awaiting == 2 && p.receipt.NativeSessionID == "" {
			if len(p.provisional) >= acpProvisionalLimit || len(m.Params) > acpProvisionalBytes-p.provisionalBytes {
				p.fail()
				return
			}
			p.provisional = append(p.provisional, m.Params)
			p.provisionalBytes += len(m.Params)
			return
		}
		p.update(m.Params)
		return
	}
	id, ok := acpID(m.ID)
	if !ok || p.awaiting == 0 || id != "n:"+strconv.Itoa(p.awaiting) {
		p.fail()
		return
	}
	awaiting := p.awaiting
	p.awaiting = 0
	if m.Error != nil {
		p.fail()
		return
	}
	fields, ok := acpObject(m.Result)
	if !ok {
		p.fail()
		return
	}
	switch awaiting {
	case 1:
		caps, capOK := acpObject(fields["agentCapabilities"])
		mcp, mcpOK := acpObject(caps["mcpCapabilities"])
		if string(fields["protocolVersion"]) != "1" || !capOK {
			p.fail()
			return
		}
		if p.config.VendorProfile != ACPAgy15 && (!mcpOK || string(mcp["http"]) != "true") {
			p.fail()
			return
		}
		info, infoOK := acpObject(fields["agentInfo"])
		name, nameOK := acpString(info, "name")
		version, versionOK := acpString(info, "version")
		if p.config.VendorProfile == ACPAgy15 {
			if !infoOK || !nameOK || name != "Antigravity ACP Server" || !versionOK || !acpAgySupportedVersion(version) || version != p.config.Version {
				p.fail()
				return
			}
		} else if !infoOK || !nameOK || !acpReference(name) || !versionOK || version != p.config.Version {
			p.fail()
			return
		}
		// Free-form native identity is persisted only as a hash, not a capability
		// upgrade or a token-bearing raw log field.
		infoHash := sha256.Sum256(fields["agentInfo"])
		p.summary("initialize", map[string]any{"request_id": 1, "version": 1, "http_mcp": true, "agent_info_sha256": hex.EncodeToString(infoHash[:])})
		if p.ctx.Err() != nil || p.damaged {
			return
		}
		p.next(2, "session/new", map[string]any{"cwd": p.request.CWD, "mcpServers": []any{map[string]any{"type": "http", "name": p.launch.server, "url": p.launch.member.endpoint + "/mcp", "headers": []any{map[string]string{"name": "Authorization", "value": "Bearer " + p.launch.member.token}}}}})
	case 2:
		session, valid := acpString(fields, "sessionId")
		if !valid || !acpReference(session) {
			p.fail()
			return
		}
		p.receipt.NativeSessionID = session
		p.summary("session_new", map[string]any{"request_id": 2, "server": p.launch.server, "issued_tool_count": len(p.config.ToolNames)})
		p.event(Event{Kind: SessionEvent, NativeSessionID: session})
		// Creation is observed even when the selected model/agent is rejected.
		// Keep its reference for reconciliation without accepting/sending a prompt.
		if !p.selection(fields, false) {
			p.fail()
			return
		}
		p.selectionConfirmed = true
		for _, raw := range p.provisional {
			p.update(raw)
		}
		p.provisional = nil
		if p.damaged || p.ctx.Err() != nil {
			return
		}
		if p.launch.verifyModelCache != nil {
			if !p.launch.verifyModelCache() {
				p.fail()
				return
			}
			if p.ctx.Err() != nil {
				return
			}
		}
		if p.promptID == 0 {
			p.promptID = 3
		}
		if p.config.VendorProfile == ACPAgy15 && p.agyCurrentModel != p.config.Model {
			p.promptID = 4
			p.next(3, "session/set_model", map[string]any{"sessionId": session, "modelId": p.config.Model})
			return
		}
		p.sendPrompt(p.promptID)
	case 3, 4:
		promptID := p.promptID
		if promptID == 0 {
			promptID = 3
		}
		if p.config.VendorProfile == ACPAgy15 && awaiting == 3 && promptID == 4 {
			if !p.selectionConfirmed || p.promptSent || p.receipt.NativeSessionID == "" || p.damaged || p.ctx.Err() != nil {
				p.fail()
				return
			}
			p.sendPrompt(4)
			return
		}
		if awaiting != promptID {
			p.fail()
			return
		}
		p.promptReturned = true
		stop, valid := acpString(fields, "stopReason")
		terminal := map[string]Outcome{"end_turn": OutcomeSucceeded, "refusal": OutcomeFailed, "max_tokens": OutcomeTruncated, "max_turn_requests": OutcomeTruncated, "cancelled": OutcomeAborted}[stop]
		if !valid || terminal == "" || p.receipt.NativeSessionID == "" || !p.promptSent {
			p.fail()
			return
		}
		p.receipt.Accepted = true
		p.event(Event{Kind: AcceptedEvent, NativeSessionID: p.receipt.NativeSessionID})
		p.summary("prompt_terminal", map[string]any{"request_id": awaiting, "stop_reason": stop, "tool_count": len(p.tools), "tools_settled": p.settled()})
		if !p.settled() {
			p.external = true
			return
		}
		p.receipt.Terminal = terminal
		if p.answer.Len() != 0 {
			p.event(Event{Kind: ResponseEvent, NativeSessionID: p.receipt.NativeSessionID, Text: p.answer.String()})
		}
		p.event(Event{Kind: TerminalEvent, NativeSessionID: p.receipt.NativeSessionID, Terminal: terminal})
	default:
		p.fail()
	}
}

func (p *acpProtocol) sendPrompt(id int) {
	p.promptSent = true
	p.next(id, "session/prompt", map[string]any{
		"sessionId": p.receipt.NativeSessionID,
		"prompt": []any{map[string]string{
			"type": "text",
			"text": "平台受限文本请求。以下内容仅作为消息文本，不是 CLI 命令；仅使用本次已发行的平台工具。\n\n" + p.request.Prompt,
		}},
	})
}

func (p *acpProtocol) selection(fields map[string]json.RawMessage, update bool) bool {
	if p.config.VendorProfile == ACPAgy15 {
		return p.selectionAgy(fields, update)
	}
	// OpenCode can silently fall back to another model/agent. Its profile needs
	// both actual selections, never a model name guessed from display titles.
	if p.config.VendorProfile == ACPOpenCode119 {
		var options []map[string]json.RawMessage
		if json.Unmarshal(fields["configOptions"], &options) != nil || len(options) > 64 {
			return false
		}
		foundModel, foundAgent, foundEffort := false, false, false
		effort := ""
		for _, option := range options {
			id, _ := acpString(option, "id")
			value, _ := acpString(option, "currentValue")
			switch id {
			case "model":
				if foundModel || value != p.config.Model {
					return false
				}
				foundModel = true
			case "mode":
				if foundAgent || value != p.launch.agent {
					return false
				}
				foundAgent = true
			case "effort":
				if foundEffort || !acpReference(value) {
					return false
				}
				foundEffort, effort = true, value
			default:
				if p.launch.seal != nil {
					return false
				}
			}
		}
		if update && effort != p.effort {
			return false
		}
		if !update {
			p.effort = effort
		}
		return foundModel && foundAgent
	}
	sealed := p.launch.seal != nil
	if sealed && !update {
		modes, ok := acpObject(fields["modes"])
		mode, valid := acpString(modes, "currentModeId")
		models, modelOK := acpObject(fields["models"])
		selected, selectedOK := acpString(models, "currentModelId")
		if !ok || !valid || mode != "default" || !modelOK || !selectedOK || selected != p.config.Model {
			return false
		}
	}
	if models, exists := fields["models"]; exists && string(models) != "null" {
		state, ok := acpObject(models)
		selected, valid := acpString(state, "currentModelId")
		if !ok || !valid || selected != p.config.Model {
			return false
		}
	}
	// Gemini may expose model config options; verify any explicit model field.
	if raw := fields["configOptions"]; len(raw) != 0 && string(raw) != "null" {
		var options []map[string]json.RawMessage
		if json.Unmarshal(raw, &options) != nil || len(options) > 64 {
			return false
		}
		foundModel := false
		for _, option := range options {
			id, _ := acpString(option, "id")
			value, valid := acpString(option, "currentValue")
			switch id {
			case "model":
				if foundModel || !valid || value != p.config.Model {
					return false
				}
				foundModel = true
			case "mode":
				if !valid || value != "default" {
					return false
				}
			default:
				if sealed {
					return false
				}
			}
		}
		if update {
			return foundModel
		}
	}
	return !update
}

func (p *acpProtocol) selectionAgy(fields map[string]json.RawMessage, update bool) bool {
	sealed := p.launch.seal != nil
	if !update {
		if sealed || fields["modes"] != nil && string(fields["modes"]) != "null" {
			modes, ok := acpObject(fields["modes"])
			mode, valid := acpString(modes, "currentModeId")
			if !ok || !valid || mode != "default" {
				return false
			}
		}
		models, modelOK := acpObject(fields["models"])
		selected, selectedOK := acpString(models, "currentModelId")
		if !modelOK || !selectedOK || !acpReference(selected) {
			return false
		}
		p.agyCurrentModel = selected
		if rawAvail := models["availableModels"]; len(rawAvail) != 0 && string(rawAvail) != "null" {
			var available []map[string]json.RawMessage
			if json.Unmarshal(rawAvail, &available) != nil {
				return false
			}
			listed := false
			for _, item := range available {
				if id, ok := acpString(item, "modelId"); ok && id == p.config.Model {
					listed = true
					break
				}
			}
			if !listed {
				return false
			}
		}
	}
	if raw := fields["configOptions"]; len(raw) != 0 && string(raw) != "null" {
		var options []map[string]json.RawMessage
		if json.Unmarshal(raw, &options) != nil || len(options) > 64 {
			return false
		}
		for _, option := range options {
			id, idOK := acpString(option, "id")
			value, valOK := acpString(option, "currentValue")
			if !idOK || !valOK {
				return false
			}
			switch id {
			case "model":
				if !acpReference(value) {
					return false
				}
				if update && value != p.config.Model && !strings.HasPrefix(p.config.Model, value+"-") {
					return false
				}
			case "thought_level":
				if !acpReference(value) {
					return false
				}
			case "mode":
				if value != "default" {
					return false
				}
			default:
				if sealed {
					return false
				}
			}
		}
		if update {
			return len(options) > 0
		}
	}
	return !update
}

func (p *acpProtocol) update(raw json.RawMessage) {
	fields, ok := acpObject(raw)
	session, valid := acpString(fields, "sessionId")
	u, updateOK := acpObject(fields["update"])
	kind, kindOK := acpString(u, "sessionUpdate")
	if !ok || !valid || session != p.receipt.NativeSessionID || session == "" || !updateOK || !kindOK {
		p.fail()
		return
	}
	switch kind {
	case "agent_message_chunk":
		if !p.promptSent || p.promptReturned {
			p.fail()
			return
		}
		content, ok := acpObject(u["content"])
		typ, _ := acpString(content, "type")
		text, valid := acpString(content, "text")
		if !ok || typ != "text" || !valid || len(text) > acpAnswerLimit-p.answer.Len() {
			p.fail()
			return
		}
		p.answer.WriteString(text)
	case "tool_call", "tool_call_update":
		if !p.promptSent || p.promptReturned {
			p.fail()
			return
		}
		p.tool(u, kind == "tool_call")
	case "config_option_update":
		if !p.selection(u, true) {
			p.fail()
		}
	case "current_mode_update":
		mode, ok := acpString(u, "currentModeId")
		expected := "default"
		if p.config.VendorProfile == ACPOpenCode119 {
			expected = p.launch.agent
		}
		if !ok || mode != expected {
			p.fail()
		}
	case "agent_thought_chunk", "plan", "available_commands_update", "session_info_update", "usage_update", "user_message_chunk":
		// Passive updates cannot supply final text, acceptance, or tool evidence.
	default:
		// An unclassified extension could describe background/external work.
		// Do not infer harmlessness from its name or silently settle it.
		p.external = true
	}
}

func (p *acpProtocol) exactTool(fields map[string]json.RawMessage) (string, bool) {
	if p.config.VendorProfile == ACPAgy15 {
		if p.launch.seal == nil || !p.launch.seal.valid(p.config, p.request.CWD, p.launch) || acpToolNativeFields(fields) {
			return "", false
		}
		for _, key := range []string{"name", "title"} {
			if value, ok := acpString(fields, key); ok {
				for _, tool := range p.config.ToolNames {
					if value == "mcp_"+p.launch.server+"_"+tool {
						return tool, true
					}
				}
			}
		}
		return "", false
	}
	if p.config.VendorProfile != ACPGemini068 {
		return "", false
	}
	meta, ok := acpObject(fields["_meta"])
	gemini, gOK := acpObject(meta["gemini-cli"])
	mcp, mOK := acpObject(gemini["mcp"])
	server, sOK := acpString(mcp, "server_name")
	name, nOK := acpString(mcp, "tool_name")
	return name, ok && gOK && mOK && sOK && nOK && server == p.launch.server && slices.Contains(p.config.ToolNames, name)
}

func (p *acpProtocol) tool(fields map[string]json.RawMessage, initial bool) {
	id, ok := acpString(fields, "toolCallId")
	previous, exists := p.tools[id]
	if !ok || !acpReference(id) || initial && (exists || len(p.tools) >= acpToolLimit) || !initial && !exists || previous.ended() {
		p.fail()
		return
	}
	if initial {
		if _, ok := acpString(fields, "title"); !ok {
			p.fail()
			return
		}
	}
	if raw := fields["status"]; raw != nil && string(raw) != "null" {
		status, valid := acpString(fields, "status")
		if !valid || !slices.Contains([]string{"pending", "in_progress", "completed", "failed"}, status) ||
			p.config.VendorProfile != ACPAgy15 && previous.status == "in_progress" && status == "pending" {
			p.fail()
			return
		}
		previous.status = status
	}
	if acpToolNativeFields(fields) {
		previous.unsafe = true
	}
	if _, hasMeta := fields["_meta"]; hasMeta && string(fields["_meta"]) != "null" {
		identity, known := p.exactTool(fields)
		if !known || previous.identity != "" && previous.identity != identity {
			previous.unsafe = true
		} else {
			previous.identity = identity
		}
	}
	// Terminal-backed/native diff content is outside this text MCP profile.
	var content []map[string]json.RawMessage
	if raw := fields["content"]; raw != nil && string(raw) != "null" {
		if json.Unmarshal(raw, &content) != nil {
			p.fail()
			return
		}
		for _, part := range content {
			typ, _ := acpString(part, "type")
			if typ != "content" {
				previous.unsafe = true
			}
		}
	}
	p.tools[id] = previous
}

func acpToolNativeFields(fields map[string]json.RawMessage) bool {
	if raw := fields["kind"]; raw != nil && string(raw) != "null" {
		kind, ok := acpString(fields, "kind")
		if !ok || kind != "other" {
			return true
		}
	}
	if raw := fields["locations"]; raw != nil && string(raw) != "null" {
		var locations []json.RawMessage
		if json.Unmarshal(raw, &locations) != nil || len(locations) != 0 {
			return true
		}
	}
	if raw := fields["content"]; raw != nil && string(raw) != "null" {
		var content []map[string]json.RawMessage
		if json.Unmarshal(raw, &content) != nil {
			return true
		}
		for _, part := range content {
			typ, _ := acpString(part, "type")
			if typ != "content" {
				return true
			}
			inner, ok := acpObject(part["content"])
			kind, valid := acpString(inner, "type")
			if !ok || !valid || kind != "text" && kind != "image" {
				return true
			}
		}
	}
	return false
}

func (p *acpProtocol) settled() bool {
	if p.damaged || p.external {
		return false
	}
	sealed := p.selectionConfirmed && p.launch.seal != nil && p.launch.seal.valid(p.config, p.request.CWD, p.launch)
	for _, tool := range p.tools {
		if !tool.ended() || tool.identity == "" && !sealed || tool.unsafe {
			return false
		}
	}
	return true
}

func (p *acpProtocol) agentRequest(m acpMessage) {
	id, valid := acpID(m.ID)
	if !valid || p.requests[id] || len(p.requests) >= acpToolLimit {
		p.fail()
		return
	}
	p.requests[id] = true
	fields, ok := acpObject(m.Params)
	session, sessionOK := acpString(fields, "sessionId")
	bound := ok && sessionOK && session != "" && session == p.receipt.NativeSessionID && p.promptSent && !p.promptReturned
	if m.Method != "session/request_permission" {
		// No fs/terminal/extension request may be fulfilled by this host.
		p.external = true
		p.reply(m, nil)
		if !bound {
			p.fail()
		}
		return
	}
	if !bound {
		p.reply(m, map[string]any{"outcome": map[string]string{"outcome": "cancelled"}})
		p.fail()
		return
	}
	toolFields, toolOK := acpObject(fields["toolCall"])
	toolID, toolIDOK := acpString(toolFields, "toolCallId")
	tool, exists := p.tools[toolID]
	name, known := p.exactTool(toolFields)
	known = known && toolOK && toolIDOK && exists && !tool.ended() && !tool.unsafe && (tool.identity == "" || tool.identity == name)
	if acpToolNativeFields(toolFields) {
		known = false
	}
	if known {
		// Structured identity is lifecycle evidence even when approval is denied.
		tool.identity = name
		p.tools[toolID] = tool
	} else {
		p.external = true
	}
	var options []map[string]json.RawMessage
	if json.Unmarshal(fields["options"], &options) != nil || len(options) > 32 {
		p.reply(m, map[string]any{"outcome": map[string]string{"outcome": "cancelled"}})
		p.fail()
		return
	}
	allow, reject := "", ""
	seen := make(map[string]bool)
	for _, option := range options {
		optionID, idOK := acpString(option, "optionId")
		kind, kindOK := acpString(option, "kind")
		if !idOK || !acpReference(optionID) || !kindOK || seen[optionID] {
			p.reply(m, map[string]any{"outcome": map[string]string{"outcome": "cancelled"}})
			p.fail()
			return
		}
		seen[optionID] = true
		switch kind {
		case "allow_once":
			if allow != "" {
				p.fail()
				return
			}
			allow = optionID
		case "reject_once":
			if reject != "" {
				p.fail()
				return
			}
			reject = optionID
		case "allow_always", "reject_always":
		default:
			p.fail()
			return
		}
	}
	outcome := map[string]string{"outcome": "cancelled"}
	choice := "cancelled"
	if p.ctx.Err() == nil && !p.interrupted && !p.damaged {
		selected := reject
		if known && allow != "" {
			selected = allow
			tool.identity = name
			p.tools[toolID] = tool
			choice = "allow_once"
		} else {
			choice = "reject_once"
		}
		if selected != "" {
			outcome = map[string]string{"outcome": "selected", "optionId": selected}
		}
	}
	p.summary("permission", map[string]any{"decision": choice, "exact_issued_mcp": known})
	p.reply(m, map[string]any{"outcome": outcome})
}

func (p *acpProtocol) reply(request acpMessage, result any) {
	message := acpMessage{JSONRPC: "2.0", ID: request.ID}
	if result == nil {
		message.Error = acpJSON(map[string]any{"code": -32601, "message": "受限执行器不支持该请求"})
	} else {
		message.Result = acpJSON(result)
	}
	if !p.send(message) {
		p.fail()
	}
}

func (p *acpProtocol) interrupt() {
	if p.interrupted {
		return
	}
	p.interrupted = true
	if p.promptSent && !p.promptReturned && p.receipt.NativeSessionID != "" {
		if !p.send(acpMessage{JSONRPC: "2.0", Method: "session/cancel", Params: acpJSON(map[string]string{"sessionId": p.receipt.NativeSessionID})}) {
			p.fail()
		}
	}
}
