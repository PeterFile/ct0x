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
	endpoint           string
	origin             string
	token              string
	file               os.FileInfo
	digest             [32]byte
	investigationTools []string // Pi strict mode only.
	discussionRelay    bool     // Digest-bound Pi intake only.
}

// Trusted, stable ancestors are required. Descriptor and path checks detect
// replacement/metadata drift, not hostile ancestor mutation or ABA writes.
func readMemberToolConfigFile(path string) ([]byte, os.FileInfo, error) {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return nil, nil, errPiMemberToolConfiguration
	}
	path = filepath.Clean(path)
	before, err := os.Lstat(path)
	if err != nil || before.Mode() != 0600 || before.Size() <= 0 || before.Size() > codexAppServerMemberConfigLimit {
		return nil, nil, errPiMemberToolConfiguration
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, errPiMemberToolConfiguration
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
		return nil, nil, errPiMemberToolConfiguration
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, codexAppServerMemberConfigLimit+1))
	finished, finishErr := file.Stat()
	closeErr := file.Close()
	after, pathErr := os.Lstat(path)
	if readErr != nil || finishErr != nil || closeErr != nil || pathErr != nil || !valid(finished) || !valid(after) || int64(len(raw)) != before.Size() || !utf8.Valid(raw) {
		return nil, nil, errPiMemberToolConfiguration
	}
	return raw, after, nil
}

func readMemberToolConfig(path string) (memberToolConfig, error) {
	return readMemberToolConfigMode(path, false, false)
}

func readPiMemberToolConfig(path string, strict bool) (memberToolConfig, error) {
	return readMemberToolConfigMode(path, true, strict)
}

func readMemberToolConfigMode(path string, pi, strict bool) (memberToolConfig, error) {
	raw, file, err := readMemberToolConfigFile(path)
	if err != nil {
		return memberToolConfig{}, err
	}
	var config memberToolConfig
	fields, err := codexAppServerMap(raw)
	count := 2
	if strict {
		count++
		if json.Unmarshal(fields["investigation_tools"], &config.investigationTools) != nil || len(config.investigationTools) == 0 {
			return memberToolConfig{}, errPiMemberToolConfiguration
		}
	} else {
		// Optional schema hints are not authority. Codex discovers its schema
		// through MCP, and never accepts the strict config or investigation hint.
		if string(fields["continue_work"]) == "true" {
			count++
		}
		if pi && string(fields["discussion_only_excerpt"]) == "true" {
			count++
		}
		if pi && string(fields["discussion_relay"]) == "true" {
			count++
			config.discussionRelay = true
		}
		if pi && string(fields["intake_assessment_action"]) == `"read_only_investigation"` {
			count++
		}
	}
	if err != nil || len(fields) != count ||
		json.Unmarshal(fields["endpoint"], &config.endpoint) != nil || json.Unmarshal(fields["token"], &config.token) != nil {
		return memberToolConfig{}, errPiMemberToolConfiguration
	}
	config.origin = memberToolConfigOrigin(config.endpoint, config.token)
	if config.origin == "" {
		return memberToolConfig{}, errPiMemberToolConfiguration
	}
	config.file, config.digest = file, sha256.Sum256(raw)
	return config, nil
}

func memberToolConfigOrigin(endpoint, token string) string {
	parts := memberToolEndpoint.FindStringSubmatch(endpoint)
	if len(parts) != 3 || len(token) > 256 || !memberToolBearer.MatchString(token) {
		return ""
	}
	port, err := strconv.Atoi(parts[2])
	if err != nil || port < 1 || port > 65535 {
		return ""
	}
	// Native httpOrigin uses URL-origin serialization (canonical numeric port,
	// with HTTP's default port omitted), not the literal configured URL bytes.
	origin := "http://" + parts[1]
	if port != 80 {
		origin += ":" + strconv.Itoa(port)
	}
	return origin
}

type codexAppServerMemberToolExecutor struct {
	codexAppServerExecutor
	configPath     string
	configFile     os.FileInfo
	digest         [32]byte
	names          []string
	sessionOptions *CodexAppServerSessionOptions
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

// NewCodexAppServerSessionExecutor experimentally opts into bounded capture and
// exact-thread cold resume on Codex 0.166.0. The caller must qualify the latest
// platform receipt and freeze current grants before supplying Resume. A reference
// is not authorization. Nil Resume means fresh-and-capture; errors never fall back.
// Each Execute retains the original HOME/auth and starts a new process. The
// caller must supply the current turn's MCP token via its issued config.
func NewCodexAppServerSessionExecutor(selected Provider, configPath string, toolNames []string, options CodexAppServerSessionOptions, timeout time.Duration) (Executor, error) {
	if !validNativeSessionBinding(options.Binding) || options.Resume != nil && !validCodexNativeReference(options.Resume) {
		return nil, errExecutorConfiguration
	}
	base, err := NewCodexAppServerMemberToolExecutor(selected, configPath, toolNames, timeout)
	if err != nil {
		return nil, err
	}
	e := base.(codexAppServerMemberToolExecutor)
	if !nativeSessionPath(selected.Path) || options.Resume != nil &&
		(options.Resume.Codex.CLIPath != selected.Path || options.Resume.Codex.ToolsProfileSHA256 != codexSessionToolsProfile(e.names)) {
		return nil, errExecutorConfiguration
	}
	options.Binding = bytes.Clone(options.Binding)
	options.Resume = cloneNativeSessionReference(options.Resume)
	e.sessionOptions = &options
	return e, nil
}

func (e codexAppServerMemberToolExecutor) Execute(ctx context.Context, request ExecuteRequest, onEvent func(Event) error) (receipt ExecutionReceipt) {
	var native *codexAppServerSession
	if e.sessionOptions != nil {
		options := e.sessionOptions
		defer func() {
			if receipt.NativeRef == nil && options.Binding != nil {
				receipt.NativeRef = &NativeSessionReference{Version: NativeSessionReferenceVersion, Binding: bytes.Clone(options.Binding)}
			}
		}()
		if !codexAppServerIdentity(request.Model) || !nativeSessionPath(request.CWD) || options.Resume != nil &&
			(options.Resume.Codex.Model != request.Model || options.Resume.Codex.CWD != request.CWD) {
			return ExecutionReceipt{Outcome: OutcomeNotStarted, ExitCode: -1, Reason: "原生会话模型或目录不匹配，执行未启动"}
		}
		native = &codexAppServerSession{cliPath: e.path, toolsProfile: codexSessionToolsProfile(e.names), binding: bytes.Clone(options.Binding)}
		if options.Resume != nil {
			native.resume = cloneNativeSessionReference(options.Resume).Codex
		}
	}
	config, err := readMemberToolConfig(e.configPath)
	if err != nil || !os.SameFile(e.configFile, config.file) || config.digest != e.digest || !e.configFile.ModTime().Equal(config.file.ModTime()) {
		return ExecutionReceipt{Outcome: OutcomeNotStarted, ExitCode: -1, Reason: "成员工具配置无效或已变化，执行未启动"}
	}
	return e.executeMember(ctx, request, onEvent, config, &codexAppServerMemberSession{names: e.names, native: native})
}

func (e codexAppServerExecutor) executeMember(ctx context.Context, request ExecuteRequest, onEvent func(Event) error, config memberToolConfig, session *codexAppServerMemberSession) ExecutionReceipt {
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return ExecutionReceipt{Outcome: OutcomeNotStarted, ExitCode: -1, Reason: "无法创建成员工具执行配置"}
	}
	session.endpoint, session.origin = config.endpoint+"/mcp", config.origin
	const tokenPrefix = "AGENT_WORKSPACE_MCP_TOKEN_"
	session.tokenEnv = tokenPrefix + strings.ToUpper(hex.EncodeToString(nonce))
	strip := session.tokenEnv + "="
	if session.threadDecision != nil {
		strip = tokenPrefix // This profile never inherits another platform MCP bearer.
	}
	env := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, strip) {
			env = append(env, value)
		}
	}
	env = append(env, session.tokenEnv+"="+config.token)
	return e.execute(ctx, request, onEvent, session, env)
}

type codexAppServerMemberSession struct {
	endpoint string
	origin   string
	tokenEnv string
	names    []string
	server   string
	startup  string
	ready    bool
	native   *codexAppServerSession
	// Non-nil only for the fresh, single-tool Thread decision continuation.
	threadDecision *codexAppServerThreadDecisionSession
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
	server := map[string]any{
		"url": m.endpoint, "bearer_token_env_var": m.tokenEnv, "enabled": true, "enabled_tools": m.names,
		"omit_tools_from": []string{"code_mode", "deferred"}, "tools": toolApprovals,
	}
	if m.threadDecision != nil {
		features["goals"] = false
		server["required"] = true
	}
	config["mcp_servers"].(map[string]any)[name] = server
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
		origin != p.member.origin || !codexAppServerNoError(status["toolsError"]) || !codexAppServerNoError(status["pluginId"]) ||
		p.member.threadDecision != nil && len(tools) != len(p.member.names) {
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
	if p.session != nil && p.session.resume != nil {
		p.next(8, "thread/read", map[string]any{"threadId": p.threadID, "includeTurns": true})
	} else {
		p.startTurn()
	}
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
			p.failResumeNotification(message, "notification_not_allowed")
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
	fields, err := codexRPCObject(raw, "id", "status", "server", "tool", "arguments", "result", "error", "readOnlyHint", "appContext", "mcpAppUi", "pluginId", "mcpAppResourceUri")
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
	// Fingerprint the completion notification, not the later terminal or
	// history copy. Neither tool payload survives in per-item state.
	item.mcp = &codexSessionMCPContent{
		arguments:    codexSessionHashField(fields["arguments"]),
		result:       codexSessionHashField(fields["result"]),
		readOnlyHint: codexSessionHashField(fields["readOnlyHint"]),
	}
	return item, true
}
