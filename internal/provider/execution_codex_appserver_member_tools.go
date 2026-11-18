package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const codexAppServerMemberConfigLimit = 4096

var memberToolEndpoint = regexp.MustCompile(`^http://(127\.0\.0\.1|\[::1\]):([0-9]{1,5})/api/v1/member-tools$`)
var memberToolBearer = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)

type memberToolConfig struct {
	endpoint string
	origin   string
	token    string
	file     os.FileInfo
	digest   [32]byte
}

// Trusted, stable ancestors are required. Descriptor and path checks detect
// replacement/metadata drift, not hostile ancestor mutation or ABA writes.
func readMemberToolConfig(path string) (memberToolConfig, error) {
	var config memberToolConfig
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return config, errPiMemberToolConfiguration
	}
	path = filepath.Clean(path)
	before, err := os.Lstat(path)
	if err != nil || before.Mode() != 0600 || before.Size() <= 0 || before.Size() > codexAppServerMemberConfigLimit {
		return config, errPiMemberToolConfiguration
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return config, errPiMemberToolConfiguration
	}
	opened, statErr := file.Stat()
	valid := func(info os.FileInfo) bool {
		if info == nil || info.Mode() != 0600 || !os.SameFile(before, info) || before.Size() != info.Size() || !before.ModTime().Equal(info.ModTime()) {
			return false
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		return ok && owner.Uid == uint32(os.Geteuid())
	}
	if statErr != nil || !valid(opened) {
		_ = file.Close()
		return config, errPiMemberToolConfiguration
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, codexAppServerMemberConfigLimit+1))
	finished, finishErr := file.Stat()
	closeErr := file.Close()
	after, pathErr := os.Lstat(path)
	if readErr != nil || finishErr != nil || closeErr != nil || pathErr != nil || !valid(finished) || !valid(after) || int64(len(raw)) != before.Size() || !utf8.Valid(raw) {
		return config, errPiMemberToolConfiguration
	}
	fields, err := codexAppServerMap(raw)
	// The optional Pi schema hint is not authority; Codex discovers its schema
	// from MCP under the same bound token rather than reading the hint.
	if err != nil || len(fields) != 2 && !(len(fields) == 3 && string(fields["continue_work"]) == "true") ||
		json.Unmarshal(fields["endpoint"], &config.endpoint) != nil || json.Unmarshal(fields["token"], &config.token) != nil {
		return memberToolConfig{}, errPiMemberToolConfiguration
	}
	parts := memberToolEndpoint.FindStringSubmatch(config.endpoint)
	if len(parts) != 3 || len(config.token) > 256 || !memberToolBearer.MatchString(config.token) {
		return memberToolConfig{}, errPiMemberToolConfiguration
	}
	port, err := strconv.Atoi(parts[2])
	if err != nil || port < 1 || port > 65535 {
		return memberToolConfig{}, errPiMemberToolConfiguration
	}
	// Native httpOrigin uses URL-origin serialization (canonical numeric port,
	// with HTTP's default port omitted), not the literal configured URL bytes.
	config.origin = "http://" + parts[1]
	if port != 80 {
		config.origin += ":" + strconv.Itoa(port)
	}
	config.file, config.digest = after, sha256.Sum256(raw)
	return config, nil
}

type codexAppServerMemberToolExecutor struct {
	codexAppServerExecutor
	configPath string
	configFile os.FileInfo
	digest     [32]byte
	names      []string
}

// NewCodexAppServerMemberToolExecutor is experimental and explicitly opt-in on
// verified Codex 0.166.0. Its restricted read-only native profile accepts only
// server-authorized text member tool evidence. Native patch/resource helpers may
// remain registered; unexpected native items stay unknown. This is not a
// directory-read sandbox or authorization for file tasks. Original CLI HOME
// and authentication are preserved. Config and ancestors must remain trusted and
// stable; no provider/model/auth/discovery probe is performed. Each Execute uses
// a fresh native thread and random MCP name; resume is not supported.
func NewCodexAppServerMemberToolExecutor(selected Provider, configPath string, toolNames []string, timeout time.Duration) (Executor, error) {
	if selected.Name != Codex || selected.Version != "0.166.0" {
		return nil, errExecutorConfiguration
	}
	if err := ValidateExecutor(selected, timeout); err != nil {
		return nil, err
	}
	names, err := memberToolProfile(toolNames)
	if err != nil {
		return nil, err
	}
	config, err := readMemberToolConfig(configPath)
	if err != nil {
		return nil, err
	}
	baseline, err := NewCodexAppServerExecutor(selected.Path, timeout)
	if err != nil {
		return nil, err
	}
	return codexAppServerMemberToolExecutor{codexAppServerExecutor: baseline.(codexAppServerExecutor),
		configPath: configPath, configFile: config.file, digest: config.digest, names: names}, nil
}

func (e codexAppServerMemberToolExecutor) Execute(ctx context.Context, request ExecuteRequest, onEvent func(Event) error) ExecutionReceipt {
	config, err := readMemberToolConfig(e.configPath)
	if err != nil || !os.SameFile(e.configFile, config.file) || config.digest != e.digest || !e.configFile.ModTime().Equal(config.file.ModTime()) {
		return ExecutionReceipt{Outcome: OutcomeNotStarted, ExitCode: -1, Reason: "成员工具配置无效或已变化，执行未启动"}
	}
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return ExecutionReceipt{Outcome: OutcomeNotStarted, ExitCode: -1, Reason: "无法创建成员工具执行配置"}
	}
	session := &codexAppServerMemberSession{endpoint: config.endpoint + "/mcp", origin: config.origin, names: e.names,
		tokenEnv: "AGENT_WORKSPACE_MCP_TOKEN_" + strings.ToUpper(hex.EncodeToString(nonce))}
	env := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, session.tokenEnv+"=") {
			env = append(env, value)
		}
	}
	env = append(env, session.tokenEnv+"="+config.token)
	return e.codexAppServerExecutor.execute(ctx, request, onEvent, session, env)
}

type codexAppServerMemberSession struct {
	endpoint string
	origin   string
	tokenEnv string
	names    []string
	server   string
	startup  string
	ready    bool
}

func codexAppServerMemberName(inherited []string, entropy io.Reader) (string, error) {
	for range 4 {
		nonce := make([]byte, 16)
		if _, err := io.ReadFull(entropy, nonce); err != nil {
			return "", errPiMemberToolConfiguration
		}
		name := "aw_member_" + hex.EncodeToString(nonce)
		if !slices.Contains(inherited, name) {
			return name, nil
		}
	}
	return "", errPiMemberToolConfiguration
}

// The trusted loopback endpoint does not redirect. Installed 0.166 additionally
// rejects cross-origin HTTP redirects; legacy same-origin redirects are possible.
func (m *codexAppServerMemberSession) configure(inherited []string, config map[string]any) error {
	name, err := codexAppServerMemberName(inherited, rand.Reader)
	if err != nil {
		return err
	}
	m.server = name
	// In 0.166 model-selected code mode can override false feature flags. Keep
	// only this server's issued tools direct, outside search and the disabled
	// code-mode host. Clear inherited global namespace overrides rather than
	// giving unrelated native/MCP namespaces direct-only exposure.
	features := config["features"].(map[string]any)
	features["code_mode"] = map[string]any{"enabled": false, "direct_only_tool_namespaces": []string{}}
	features["code_mode_only_strict_3p_tools"] = false
	// Native Approve removes the redundant prompt only for issued tools. The
	// platform still checks the original token, claim, purpose and grants per call.
	toolApprovals := make(map[string]map[string]string, len(m.names))
	for _, tool := range m.names {
		toolApprovals[tool] = map[string]string{"approval_mode": "approve"}
	}
	config["mcp_servers"].(map[string]any)[name] = map[string]any{
		"url": m.endpoint, "bearer_token_env_var": m.tokenEnv, "enabled": true, "enabled_tools": m.names,
		"omit_tools_from": []string{"code_mode", "deferred"}, "tools": toolApprovals,
	}
	return nil
}

func (p *codexAppServerProtocol) memberStatus(raw json.RawMessage) {
	fields, err := codexRPCObject(raw, "data", "nextCursor")
	var data []json.RawMessage
	if err != nil || json.Unmarshal(fields["data"], &data) != nil || len(data) != 1 || !codexAppServerNoError(fields["nextCursor"]) {
		p.fail()
		return
	}
	status, err := codexRPCObject(data[0], "name", "runtimeStatus", "httpOrigin", "authStatus", "toolsError", "pluginId", "tools")
	name, _ := codexAppServerString(status, "name")
	origin, _ := codexAppServerString(status, "httpOrigin")
	tools, toolsErr := codexAppServerMap(status["tools"])
	if err != nil || toolsErr != nil || name != p.member.server || string(status["runtimeStatus"]) != `"connected"` || string(status["authStatus"]) != `"bearerToken"` ||
		origin != p.member.origin || !codexAppServerNoError(status["toolsError"]) || !codexAppServerNoError(status["pluginId"]) {
		p.fail()
		return
	}
	// Status returns raw Tool definitions, not model-visible ToolInfo namespaces.
	// A catalog can include unissued tools; enabled_tools and memberItem retain
	// the exact allowlist. Every issued tool must exist with its raw name bound.
	for _, name := range p.member.names {
		tool, err := codexRPCObject(tools[name], "name")
		observed, valid := codexAppServerString(tool, "name")
		if err != nil || !valid || observed != name {
			p.fail()
			return
		}
	}
	p.member.ready = true
	p.startTurn()
}

// Startup notifications are bound to the selected thread and random server.
// Failed startup and any elicitation remain unknown; nothing grants authority.
func (p *codexAppServerProtocol) memberNotification(message codexRPCMessage) bool {
	method := *message.Method
	if method != "mcpServer/startupStatus/updated" && method != "mcpServer/elicitation/request" {
		return false
	}
	fields, err := codexRPCObject(message.Params, "threadId", "turnId", "name", "status", "serverName", "error", "failureReason")
	thread, _ := codexAppServerString(fields, "threadId")
	valid := err == nil && p.threadID != "" && thread == p.threadID && p.receipt.Terminal == ""
	if method == "mcpServer/elicitation/request" {
		server, _ := codexAppServerString(fields, "serverName")
		valid = valid && server == p.member.server && message.ID != nil
		if !codexAppServerNoError(fields["turnId"]) {
			turn, _ := codexAppServerString(fields, "turnId")
			valid = valid && p.turnID != "" && turn == p.turnID
		}
		if message.ID != nil {
			key := string(message.ID)
			_, duplicate := p.requests[key]
			valid = valid && !duplicate && len(key) <= 512 && len(p.requests) < 1024
			var result any
			if valid {
				p.requests[key] = true
				result = map[string]any{"action": "cancel", "content": nil}
			}
			if !p.reply(message, result) {
				valid = false
			}
		}
		p.external = true
		p.cancel()
		if !valid {
			p.fail()
		}
		return true
	}
	name, _ := codexAppServerString(fields, "name")
	status, _ := codexAppServerString(fields, "status")
	if !valid || message.ID != nil || name != p.member.server || !codexAppServerNoError(fields["error"]) || !codexAppServerNoError(fields["failureReason"]) ||
		!slices.Contains([]string{"starting", "ready"}, status) || status == "starting" && (p.member.ready || p.member.startup != "") {
		p.fail()
		return true
	}
	p.member.startup = status
	return true
}

func (p *codexAppServerProtocol) memberItem(raw json.RawMessage, completed bool) (codexAppServerItem, bool) {
	fields, err := codexRPCObject(raw, "id", "status", "server", "tool", "arguments", "result", "error", "appContext", "mcpAppUi", "pluginId", "mcpAppResourceUri")
	id, idOK := codexAppServerString(fields, "id")
	server, serverOK := codexAppServerString(fields, "server")
	tool, toolOK := codexAppServerString(fields, "tool")
	status, statusOK := codexAppServerString(fields, "status")
	item := codexAppServerItem{kind: "mcpToolCall", completed: completed, status: status, server: server, tool: tool}
	_, argumentsErr := codexAppServerMap(fields["arguments"])
	if err != nil || !idOK || !serverOK || !toolOK || !statusOK || argumentsErr != nil || !p.member.ready || server != p.member.server || !slices.Contains(p.member.names, tool) {
		return item, false
	}
	for _, key := range []string{"appContext", "mcpAppUi", "pluginId", "mcpAppResourceUri"} {
		if !codexAppServerNoError(fields[key]) {
			return item, false
		}
	}
	if !completed {
		return item, status == "inProgress" && codexAppServerNoError(fields["result"]) && codexAppServerNoError(fields["error"])
	}
	previous, exists := p.items[id]
	if !exists || previous.kind != item.kind || previous.server != server || previous.tool != tool || status != "completed" || !codexAppServerNoError(fields["error"]) {
		return item, false
	}
	result, err := codexRPCObject(fields["result"], "content")
	var content []json.RawMessage
	if err != nil || json.Unmarshal(result["content"], &content) != nil || content == nil {
		return item, false
	}
	for _, raw := range content {
		part, err := codexRPCObject(raw, "type", "text")
		var text string
		if err != nil || !bytes.Equal(bytes.TrimSpace(part["type"]), []byte(`"text"`)) || json.Unmarshal(part["text"], &text) != nil {
			return item, false
		}
	}
	return item, true
}
