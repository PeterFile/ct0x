package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

var threadDecisionAckMCPTool = memberMCPTool{
	Name:        platform.AckThreadDecisionOperation,
	Description: "显式确认已处理本轮绑定的原话题答复，不接受任何参数。未知时按原话题、待决编号和版本核对持久回执，不自动重试。",
	InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
}

// Only MCP can call this handler. Neither an Idempotency-Key header nor model
// arguments can supply a selector or override the server-derived call identity.
func (a memberToolAPI) ackThreadDecisionMCP(w http.ResponseWriter, r *http.Request, token string, id any) {
	buffer := newMemberMCPBuffer()
	view, err := a.tools.AcknowledgeThreadDecision(r.Context(), token, memberMCPKey(token, id))
	switch {
	case errors.Is(err, store.ErrTaskWorkMutationUnknown):
		// Check this before cancellation, stale claim or business errors in the cause.
		WriteError(buffer, http.StatusInternalServerError, "thread_decision_ack_unknown", "话题答复处理确认结果未知；请按原话题、待决编号和版本核对持久回执")
	case errors.Is(err, store.ErrStaleClaim):
		memberToolError(buffer, store.ErrMemberToolDenied)
	case errors.Is(err, store.ErrConflict):
		WriteError(buffer, http.StatusConflict, "conflict", "话题答复确认调用冲突；请保留原 JSON-RPC id。此拒绝不确认原操作结果")
	case err != nil:
		memberToolError(buffer, err)
	default:
		WriteJSON(buffer, http.StatusOK, map[string]any{"handling": view.Handling})
	}
	memberMCPToolReply(w, id, true, buffer)
}
