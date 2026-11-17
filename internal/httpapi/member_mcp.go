package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"agentworkspace/internal/platform"
)

const memberMCPVersion = "2025-06-18"
const memberMCPReconcile = "\n请保留相同 JSON-RPC id、服务器派生的原幂等键和原请求内容；结果未确认时先对账，不要自动重试。"

// MemberMCPRoutes is opt-in; mount on the same NewServer and registry as native
// routes. NewServer rejects GET with 405/Allow: POST: this endpoint has no SSE.
// No session ID, initialization state, execution queue or additional authority.
func MemberMCPRoutes(tools *platform.MemberTools) []Route {
	a := memberToolAPI{tools}
	return []Route{{http.MethodPost, "/api/v1/member-tools/mcp", memberToolNative(a.memberMCP)}}
}

func (a memberToolAPI) memberMCP(w http.ResponseWriter, r *http.Request, token string) {
	if !memberMCPAccept(r.Header) {
		WriteError(w, http.StatusNotAcceptable, "not_acceptable", "Accept 必须同时支持 application/json 和 text/event-stream；本接口仅返回 JSON")
		return
	}
	var frame map[string]json.RawMessage
	buffer := newMemberMCPBuffer()
	if !decodeMemberToolJSON(buffer, r, &frame) {
		if buffer.status == http.StatusBadRequest {
			memberMCPError(w, 400, nil, -32700, "请求体必须是有效的单个 JSON-RPC 对象，不得含有重复字段")
		} else {
			buffer.forward(w)
		}
		return
	}
	id, validID := memberMCPID(frame["id"])
	method := memberMCPString(frame["method"])
	if !memberMCPFields(frame, "jsonrpc", "id", "method", "params") || memberMCPString(frame["jsonrpc"]) != "2.0" || !validID || method == "" || len(method) > 128 ||
		(id == nil) != (method == "notifications/initialized") {
		memberMCPError(w, 400, nil, -32600, "JSON-RPC 请求格式或 id 无效")
		return
	}
	versions := r.Header.Values("MCP-Protocol-Version")
	// Without session state, only initialize can establish a version without the
	// header. We do not silently assume the unsupported 2025-03-26 batch protocol.
	if !(len(versions) == 0 && method == "initialize") && (len(versions) != 1 || versions[0] != memberMCPVersion) {
		memberMCPError(w, 400, id, -32600, "MCP-Protocol-Version 必须为 "+memberMCPVersion)
		return
	}
	operations, err := a.tools.TransportOperations(r.Context(), token)
	if err != nil && method != "tools/call" {
		buffer = newMemberMCPBuffer()
		memberToolError(buffer, err)
		buffer.forward(w)
		return
	}
	switch method {
	case "initialize":
		if !memberMCPInitializeParams(frame["params"]) {
			memberMCPError(w, 200, id, -32602, "initialize 参数无效，必须提供 protocolVersion、capabilities 和 clientInfo")
			return
		}
		memberMCPReply(w, id, map[string]any{
			"protocolVersion": memberMCPVersion, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo":   map[string]string{"name": "agent-workspace-member-tools", "version": "1"},
			"instructions": "调用身份与权限由当前运行能力绑定。写入幂等键由能力与 JSON-RPC id 派生。" + memberMCPReconcile,
		})
	case "notifications/initialized", "ping":
		if _, ok := memberMCPParams(frame["params"]); !ok {
			memberMCPError(w, 400, id, -32602, "请求参数无效")
		} else if method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
		} else {
			memberMCPReply(w, id, map[string]any{})
		}
	case "tools/list":
		p, ok := memberMCPParams(frame["params"], "cursor")
		if cursor, exists := p["cursor"]; !ok || exists && !bytes.Equal(bytes.TrimSpace(cursor), []byte(`""`)) {
			memberMCPError(w, 200, id, -32602, "工具列表无需游标")
			return
		}
		tools := make([]memberMCPTool, 0, len(memberMCPTools))
		for _, tool := range memberMCPTools {
			if slices.Contains(operations, tool.Name) {
				tools = append(tools, tool)
			}
		}
		memberMCPReply(w, id, map[string]any{"tools": tools})
	case "tools/call":
		params, paramsOK := memberMCPParams(frame["params"], "name", "arguments")
		name := memberMCPString(params["name"])
		arguments := params["arguments"]
		if len(arguments) == 0 {
			arguments = json.RawMessage(`{}`)
		}
		var object map[string]json.RawMessage
		if !paramsOK || name == "" || len(name) > 64 || json.Unmarshal(arguments, &object) != nil || object == nil {
			memberMCPError(w, 400, id, -32602, "工具名称和 arguments 对象无效")
			return
		}
		for _, tool := range memberMCPTools {
			if tool.Name != name {
				continue
			}
			// Discovery preflight is not command outcome evidence: its generic
			// validator sanitizes SQL errors that role writes must keep unknown.
			// Always use the fixed native handler's own token/claim/purpose and
			// same-tx fences, even after a preflight error. No URL dispatch or
			// model headers/metadata can bypass those checks.
			native := r.Clone(r.Context())
			native.Header = http.Header{"Content-Type": {"application/json"}}
			native.Body = io.NopCloser(bytes.NewReader(arguments))
			native.ContentLength = int64(len(arguments))
			if tool.write {
				native.Header.Set("Idempotency-Key", memberMCPKey(token, id))
			}
			buffer = newMemberMCPBuffer()
			tool.invoke(a, buffer, native, token)
			memberMCPToolReply(w, id, tool.write, buffer)
			return
		}
		if err != nil {
			buffer = newMemberMCPBuffer()
			memberToolError(buffer, err)
			memberMCPToolReply(w, id, false, buffer)
			return
		}
		memberMCPError(w, 200, id, -32602, "工具不存在")
	default:
		memberMCPError(w, 200, id, -32601, "JSON-RPC 方法不存在")
	}
}

func memberMCPAccept(header http.Header) bool {
	jsonOK, streamOK, size := false, false, 0
	for _, value := range header.Values("Accept") {
		size += len(value)
		if size > 2048 {
			return false
		}
		for _, part := range strings.Split(value, ",") {
			media, parameters, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err != nil {
				return false
			}
			quality := 1.0
			if raw, exists := parameters["q"]; exists {
				quality, err = strconv.ParseFloat(raw, 64)
				if err != nil || !(quality >= 0 && quality <= 1) {
					return false
				}
			}
			if quality > 0 {
				jsonOK = jsonOK || media == "application/json"
				streamOK = streamOK || media == "text/event-stream"
			}
		}
	}
	return jsonOK && streamOK
}

func memberMCPString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

// Keep IDs type-distinct and canonically encoded for stable idempotency. Integers
// are restricted to the interoperable exact JSON/JavaScript range; strings to 128 bytes.
func memberMCPID(raw json.RawMessage) (any, bool) {
	if raw == nil {
		return nil, true
	}
	if len(raw) > 0 && raw[0] == '"' {
		var value string
		err := json.Unmarshal(raw, &value)
		return value, err == nil && len(value) <= 128
	}
	value, err := strconv.ParseInt(string(raw), 10, 64)
	return value, err == nil && value >= -9007199254740991 && value <= 9007199254740991
}

func memberMCPFields(object map[string]json.RawMessage, names ...string) bool {
	if object == nil {
		return false
	}
	for name := range object {
		if !slices.Contains(names, name) {
			return false
		}
	}
	return true
}

func memberMCPParams(raw json.RawMessage, names ...string) (map[string]json.RawMessage, bool) {
	if raw == nil {
		raw = json.RawMessage(`{}`)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || !memberMCPFields(object, append(names, "_meta")...) {
		return nil, false
	}
	if meta, exists := object["_meta"]; exists {
		var fields map[string]json.RawMessage
		if json.Unmarshal(meta, &fields) != nil || !memberMCPFields(fields, "progressToken") {
			return nil, false
		}
		if progress, exists := fields["progressToken"]; exists {
			if _, ok := memberMCPID(progress); !ok {
				return nil, false
			}
		}
	}
	return object, true
}

func memberMCPInitializeParams(raw json.RawMessage) bool {
	p, ok := memberMCPParams(raw, "protocolVersion", "capabilities", "clientInfo")
	version := memberMCPString(p["protocolVersion"])
	var capabilities, info map[string]json.RawMessage
	if !ok || version == "" || len(version) > 64 || json.Unmarshal(p["capabilities"], &capabilities) != nil || capabilities == nil ||
		json.Unmarshal(p["clientInfo"], &info) != nil || !memberMCPFields(info, "name", "title", "version") {
		return false
	}
	for _, key := range []string{"name", "version"} {
		value := memberMCPString(info[key])
		if value == "" || len(value) > 256 {
			return false
		}
	}
	if title, exists := info["title"]; exists {
		var text string
		if json.Unmarshal(title, &text) != nil || bytes.Equal(bytes.TrimSpace(title), []byte("null")) || len(text) > 256 {
			return false
		}
	}
	return true
}

func memberMCPKey(token string, id any) string {
	digest := sha256.Sum256([]byte(token))
	encoded, _ := json.Marshal(id)
	hash := sha256.New()
	_, _ = hash.Write([]byte("member-mcp/idempotency/v1\x00"))
	_, _ = hash.Write(digest[:])
	_, _ = hash.Write(encoded)
	return fmt.Sprintf("mcp-%x", hash.Sum(nil))
}

func memberMCPReply(w http.ResponseWriter, id, result any) {
	WriteJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func memberMCPError(w http.ResponseWriter, status int, id any, code int, message string) {
	WriteJSON(w, status, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

// A bounded sink, not httptest. Write consumes excess bytes without propagating
// an error back across a handler's already-committed write boundary.
type memberMCPBuffer struct {
	header   http.Header
	body     []byte
	status   int
	overflow bool
}

func newMemberMCPBuffer() *memberMCPBuffer     { return &memberMCPBuffer{header: make(http.Header)} }
func (b *memberMCPBuffer) Header() http.Header { return b.header }
func (b *memberMCPBuffer) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}
func (b *memberMCPBuffer) Write(body []byte) (int, error) {
	b.WriteHeader(http.StatusOK)
	count := min(len(body), memberToolJSONBytes-len(b.body))
	b.body = append(b.body, body[:count]...)
	b.overflow = b.overflow || count != len(body)
	return len(body), nil
}
func (b *memberMCPBuffer) forward(w http.ResponseWriter) {
	for key, values := range b.header {
		w.Header()[key] = values
	}
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body)
}

func memberMCPToolReply(w http.ResponseWriter, id any, write bool, buffer *memberMCPBuffer) {
	failed, text := buffer.status >= 400, string(buffer.body)
	if failed {
		text += memberMCPReconcile
	}
	result := func() map[string]any {
		return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": failed}
	}
	encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result()})
	if buffer.overflow || len(encoded)+1 > memberToolJSONBytes {
		switch {
		case write && !failed:
			// Success is durable even when its projection cannot fit the envelope.
			text = `{"result_omitted":true,"message":"写入已完成，响应内容过大已省略"}` + memberMCPReconcile
		case failed:
			text = `{"error":{"code":"internal_error","message":"操作结果尚未确认，错误响应过大"}}` + memberMCPReconcile
		default:
			failed = true
			text = `{"error":{"code":"tool_result_too_large","message":"工具结果过大，请减小 limit 或 length"}}`
		}
	}
	memberMCPReply(w, id, result())
}
