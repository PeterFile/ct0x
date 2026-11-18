package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

func memberMCPFingerprint(name string, arguments []byte) ([sha256.Size]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	// Preserve exact numeric lexemes; Marshal sorts object keys recursively.
	// The original JSON, not this canonical form, still goes to the handler.
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return [sha256.Size]byte{}, err
	}
	if object == nil || decoder.Decode(new(any)) != io.EOF {
		return [sha256.Size]byte{}, store.ErrInvalid
	}
	canonical, err := json.Marshal([]any{name, object})
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(canonical), nil
}

// Only these native reportWork replies certify validation failure, denial,
// or confirmed rollback. In particular a context 408 or 5xx might be unknown.
func memberMCPReportRejected(buffer *memberMCPBuffer) bool {
	switch buffer.status {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict,
		http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType:
		return len(buffer.body) != 0
	default:
		return false
	}
}

func memberMCPBindingError(w http.ResponseWriter, id any, err error) {
	buffer := newMemberMCPBuffer()
	switch {
	case errors.Is(err, store.ErrConflict):
		WriteError(buffer, http.StatusConflict, "conflict", "同一 JSON-RPC id 已绑定其他工具或参数；此拒绝不确认原操作结果")
	case errors.Is(err, platform.ErrMemberMCPRequestInFlight):
		WriteError(buffer, http.StatusConflict, "mcp_request_in_flight", "相同 JSON-RPC id 的报告请求仍在处理；此拒绝不确认原操作结果")
	case errors.Is(err, platform.ErrMemberMCPRequestCapacity):
		WriteError(buffer, http.StatusTooManyRequests, "mcp_request_capacity", "当前运行的 MCP 请求标识数量已达上限；原请求重放仍可用，不会驱逐已有标识")
	default:
		memberToolError(buffer, err)
	}
	memberMCPToolReply(w, id, false, buffer)
}
