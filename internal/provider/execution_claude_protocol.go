package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strconv"
	"unicode/utf8"
)

var claudeUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var claudeToolIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var claudeDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var errClaudeFrame = errors.New("Claude SDK 桥协议校验失败")

type claudeFrame struct {
	V         int             `json:"v"`
	RequestID string          `json:"request_id"`
	Seq       int             `json:"seq"`
	Kind      string          `json:"kind"`
	SessionID string          `json:"session_id"`
	InputUUID string          `json:"input_uuid"`
	Body      json.RawMessage `json:"body"`
}

type claudeRead struct {
	raw                     []byte
	damaged, done, complete bool
}

// On overflow/damage preserve the queued prefix, then drain without parsing.
// Pipe ownership and real Wait remain with execute, not this bounded reader.
func readClaudeFrames(input io.Reader, output chan<- claudeRead) {
	reader := bufio.NewReaderSize(input, claudeFrameLimit+1)
	for {
		line, err := reader.ReadSlice('\n')
		if err != nil {
			if err == io.EOF && len(line) == 0 {
				output <- claudeRead{done: true, complete: true}
				return
			}
			output <- claudeRead{damaged: true}
			_, drainErr := io.Copy(io.Discard, reader)
			output <- claudeRead{done: true, complete: drainErr == nil && (err == io.EOF || err == bufio.ErrBufferFull)}
			return
		}
		if len(line)-1 > claudeFrameLimit {
			output <- claudeRead{damaged: true}
			_, drainErr := io.Copy(io.Discard, reader)
			output <- claudeRead{done: true, complete: drainErr == nil}
			return
		}
		copy := append([]byte(nil), line[:len(line)-1]...)
		select {
		case output <- claudeRead{raw: copy}:
		default:
			output <- claudeRead{damaged: true}
			_, drainErr := io.Copy(io.Discard, reader)
			output <- claudeRead{done: true, complete: drainErr == nil}
			return
		}
	}
}

// Reject lone surrogate escapes before encoding/json can replace them. Ordinary
// non-escaped strings are checked by utf8.Valid and the standard JSON decoder.
func claudeUnicode(raw []byte) bool {
	inside := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			inside = !inside
			continue
		}
		if !inside || raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		n, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			n, err = strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if err != nil || n < 0xdc00 || n > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return !inside
}

func claudeStrictJSON(raw []byte) error {
	if !utf8.Valid(raw) || !claudeUnicode(raw) {
		return errClaudeFrame
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 {
			return errClaudeFrame
		}
		token, err := decoder.Token()
		if err != nil {
			return errClaudeFrame
		}
		delim, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				token, err := decoder.Token()
				key, ok := token.(string)
				if err != nil || !ok || seen[key] {
					return errClaudeFrame
				}
				seen[key] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return errClaudeFrame
			}
		case '[':
			for decoder.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return errClaudeFrame
			}
		default:
			return errClaudeFrame
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errClaudeFrame
	}
	return nil
}

func claudeDecode(raw []byte, destination any, fields ...string) error {
	if claudeStrictJSON(raw) != nil {
		return errClaudeFrame
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil || len(object) != len(fields) {
		return errClaudeFrame
	}
	for _, key := range fields {
		value, exists := object[key]
		if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) && key != "signal" && key != "exit_code" {
			return errClaudeFrame
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(destination) != nil {
		return errClaudeFrame
	}
	return nil
}

func mustClaudeJSON(value any) json.RawMessage { raw, _ := json.Marshal(value); return raw }

type claudeProtocol struct {
	request                                                                                   ExecuteRequest
	cwd, cliPath, session, input, server                                                      string
	names                                                                                     []string
	receipt                                                                                   *ExecutionReceipt
	delivery                                                                                  *executionDelivery
	cancel                                                                                    context.CancelFunc
	send                                                                                      func(string, any) bool
	log                                                                                       *executionLog
	seq, nativePID                                                                            int
	damaged, initialized, submitted, terminal, closed, nativeClean, stopping, terminalPending bool
	permissions, uses                                                                         map[string]bool
}

func (p *claudeProtocol) fail() { p.damaged = true; p.cancel() }
func (p *claudeProtocol) summary(value any) {
	if p.log != nil {
		_, _ = p.log.Write(append(mustClaudeJSON(value), '\n'))
	}
}
func (p *claudeProtocol) event(event Event) {
	p.summary(map[string]any{"kind": event.Kind, "session_id": event.NativeSessionID, "input_uuid": p.input, "terminal": event.Terminal})
	p.delivery.enqueue(event)
}

func (p *claudeProtocol) receive(raw []byte, cancelled bool) {
	if p.damaged {
		return
	}
	var frame claudeFrame
	if len(raw) > claudeFrameLimit || claudeDecode(raw, &frame, "v", "request_id", "seq", "kind", "session_id", "input_uuid", "body") != nil ||
		frame.V != 1 || frame.RequestID != p.request.RunID || frame.SessionID != p.session || frame.InputUUID != p.input || frame.Seq != p.seq+1 || frame.Seq > 2048 || p.closed {
		p.fail()
		return
	}
	p.seq++
	switch frame.Kind {
	case "submitted":
		var body struct{}
		if claudeDecode(frame.Body, &body) != nil || p.submitted || p.terminal {
			p.fail()
			return
		}
		p.submitted = true
	case "initialized":
		p.initialize(frame.Body)
	case "permission_request":
		p.permission(frame.Body, cancelled)
	case "terminal":
		p.result(frame.Body)
	case "native_closed":
		p.nativeClosed(frame.Body)
	case "error":
		var body struct {
			Code string `json:"code"`
		}
		if claudeDecode(frame.Body, &body, "code") != nil {
			p.fail()
			return
		}
		// Never copy free-form native errors, arguments, argv or account data.
		p.summary(map[string]string{"kind": "bridge_error", "reason": "bridge_rejected"})
		p.fail()
	default:
		p.fail()
	}
}

func (p *claudeProtocol) initialize(raw []byte) {
	var body struct {
		SDKVersion string   `json:"sdk_version"`
		CLIVersion string   `json:"cli_version"`
		CWD        string   `json:"cwd"`
		Model      string   `json:"model"`
		Mode       string   `json:"permission_mode"`
		Tools      []string `json:"tools"`
		Server     string   `json:"mcp_server"`
		Guards     bool     `json:"guards_applied"`
		NativePID  int      `json:"native_pid"`
		CLIPath    string   `json:"cli_path"`
	}
	if claudeDecode(raw, &body, "sdk_version", "cli_version", "cwd", "model", "permission_mode", "tools", "mcp_server", "guards_applied", "native_pid", "cli_path") != nil || p.initialized || p.terminal ||
		body.SDKVersion != ClaudeSDKVersion || body.CLIVersion != ClaudeNativeVersion || body.CWD != p.cwd || body.CLIPath != p.cliPath || body.Model != p.request.Model || body.Mode != "default" || !body.Guards || body.NativePID <= 0 || body.Server != p.server || len(body.Tools) != len(p.names) {
		p.fail()
		return
	}
	seen := map[string]bool{}
	for _, name := range p.names {
		seen["mcp__"+p.server+"__"+name] = true
	}
	for _, name := range body.Tools {
		if !seen[name] {
			p.fail()
			return
		}
		delete(seen, name)
	}
	p.initialized, p.nativePID, p.receipt.NativeSessionID = true, body.NativePID, p.session
	p.event(Event{Kind: SessionEvent, NativeSessionID: p.session})
}

func (p *claudeProtocol) permission(raw []byte, cancelled bool) {
	var body struct {
		ID     string `json:"permission_id"`
		ToolID string `json:"tool_use_id"`
		Name   string `json:"tool_name"`
		Digest string `json:"input_sha256"`
		Server string `json:"mcp_server"`
		Source string `json:"mcp_source"`
		Agent  string `json:"agent_id"`
		Phase  string `json:"phase"`
	}
	if claudeDecode(raw, &body, "permission_id", "tool_use_id", "tool_name", "input_sha256", "mcp_server", "mcp_source", "agent_id", "phase") != nil || !claudeUUIDPattern.MatchString(body.ID) || !claudeToolIDPattern.MatchString(body.ToolID) || !claudeDigestPattern.MatchString(body.Digest) || len(body.Name) > 256 || len(body.Server) > 256 || len(body.Source) > 32 || len(body.Agent) > 128 || p.permissions[body.ID] || p.uses[body.ToolID+":"+body.Phase] || len(p.permissions) >= 256 || !slices.Contains([]string{"pre_tool_use", "can_use_tool"}, body.Phase) {
		p.fail()
		return
	}
	p.permissions[body.ID], p.uses[body.ToolID+":"+body.Phase] = true, true
	allowed := p.initialized && p.submitted && !p.terminal && !cancelled && !p.stopping && body.Agent == "" && body.Server == p.server && body.Source == "dynamic"
	issued := false
	for _, name := range p.names {
		issued = issued || body.Name == "mcp__"+p.server+"__"+name
	}
	allowed = allowed && issued
	decision, reason, loggedName := "deny", "outside_scope", "native_or_unknown"
	if issued {
		loggedName = body.Name
	}
	if allowed {
		decision, reason = "allow", "issued_tool"
	}
	if cancelled || p.stopping {
		decision, reason = "deny", "cancelled"
	}
	p.summary(map[string]string{"kind": "permission_decision", "permission_id": body.ID, "tool_use_id": body.ToolID, "tool_name": loggedName, "input_sha256": body.Digest, "decision": decision, "reason": reason})
	if !p.send("permission_result", map[string]string{"permission_id": body.ID, "decision": decision, "reason_code": reason}) {
		p.fail()
	}
	// A denied unsupported native request is not proof the limited profile held.
	if !issued || body.Agent != "" || body.Source != "dynamic" || body.Server != p.server || !p.initialized || !p.submitted || p.terminal {
		p.fail()
	}
}

func (p *claudeProtocol) result(raw []byte) {
	var body struct {
		UUID        string `json:"result_uuid"`
		Subtype     string `json:"subtype"`
		IsError     bool   `json:"is_error"`
		Reason      string `json:"terminal_reason"`
		Input       string `json:"user_message_uuid"`
		Queued      int    `json:"queued_turn_count"`
		Tools       int    `json:"pending_tools"`
		Background  int    `json:"pending_background"`
		Permissions int    `json:"pending_permissions"`
		Text        string `json:"text"`
	}
	if claudeDecode(raw, &body, "result_uuid", "subtype", "is_error", "terminal_reason", "user_message_uuid", "queued_turn_count", "pending_tools", "pending_background", "pending_permissions", "text") != nil || !p.initialized || !p.submitted || p.terminal || body.Input != p.input || !claudeUUIDPattern.MatchString(body.UUID) || body.UUID == p.input || body.UUID == p.session || len(body.Text) > 64<<10 || !utf8.ValidString(body.Text) || body.Queued < 0 || body.Tools < 0 || body.Background < 0 || body.Permissions < 0 {
		p.fail()
		return
	}
	p.terminal, p.receipt.Accepted = true, true
	p.event(Event{Kind: AcceptedEvent, NativeSessionID: p.session})
	var outcome Outcome
	switch {
	case body.Subtype == "success" && !body.IsError && body.Reason == "completed":
		outcome = OutcomeSucceeded
	case body.Subtype == "success" && body.IsError && slices.Contains([]string{"api_error", "model_error"}, body.Reason):
		outcome = OutcomeFailed
	case body.Subtype == "error_during_execution" && body.IsError && slices.Contains([]string{"api_error", "model_error", "turn_setup_failed"}, body.Reason):
		outcome = OutcomeFailed
	case body.Subtype == "error_during_execution" && body.IsError && slices.Contains([]string{"aborted_streaming", "aborted_tools"}, body.Reason):
		outcome = OutcomeAborted
	case body.Subtype == "error_max_turns" && body.IsError && body.Reason == "max_turns", body.Subtype == "error_max_budget_usd" && body.IsError && body.Reason == "budget_exhausted", body.Subtype == "error_max_structured_output_retries" && body.IsError && body.Reason == "structured_output_retry_exhausted":
		outcome = OutcomeTruncated
	default:
		p.fail()
		return
	}
	if outcome != OutcomeSucceeded && body.Text != "" {
		p.fail()
		return
	}
	p.terminalPending = body.Queued != 0 || body.Tools != 0 || body.Background != 0 || body.Permissions != 0
	p.receipt.Terminal = outcome
	if outcome == OutcomeSucceeded {
		p.event(Event{Kind: ResponseEvent, NativeSessionID: p.session, Text: body.Text})
	}
	p.event(Event{Kind: TerminalEvent, NativeSessionID: p.session, Terminal: outcome})
}

func (p *claudeProtocol) nativeClosed(raw []byte) {
	var body struct {
		PID         int     `json:"pid"`
		ExitCode    *int    `json:"exit_code"`
		Signal      *string `json:"signal"`
		Stdout      bool    `json:"stdout_drained"`
		Stderr      bool    `json:"stderr_drained"`
		Tools       int     `json:"pending_tools"`
		Background  int     `json:"pending_background"`
		Permissions int     `json:"pending_permissions"`
	}
	if claudeDecode(raw, &body, "pid", "exit_code", "signal", "stdout_drained", "stderr_drained", "pending_tools", "pending_background", "pending_permissions") != nil || !p.initialized || body.PID != p.nativePID || body.PID <= 0 || !body.Stdout || !body.Stderr || body.Tools < 0 || body.Background < 0 || body.Permissions < 0 || body.ExitCode == nil && body.Signal == nil || body.ExitCode != nil && body.Signal != nil {
		p.fail()
		return
	}
	p.closed = true
	p.nativeClean = p.terminal && !p.terminalPending && body.ExitCode != nil && *body.ExitCode == 0 && body.Signal == nil && body.Tools == 0 && body.Background == 0 && body.Permissions == 0
	p.summary(map[string]any{"kind": "native_closed", "pid": body.PID, "clean": p.nativeClean})
}
