package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
)

const piTypedWriteUnknown = "agent_workspace_write_unknown_v1"
const piTypedWriteLogReserve = 2048 // One bounded identity record, even at the raw stdout cap.

var piTypedWriteCallID = regexp.MustCompile(`^[!-~]{1,256}$`)
var piTypedWriteRequestSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)

type piTypedWriteEvidence struct {
	Role           string `json:"role"`
	RunID          string `json:"run_id"`
	Operation      string `json:"operation"`
	ToolCallID     string `json:"tool_call_id"`
	IdempotencyKey string `json:"idempotency_key"`
	RequestSHA256  string `json:"request_sha256"`
}

type piTypedWorkOutput struct {
	role    string
	tools   []string
	unknown *piTypedWriteEvidence
}

// Only called on a native tool_execution_end from the exact typed launch. The
// pinned bridge exclusively owns result.details. Assistant messages, tool args
// and result text are never marker carriers. The control can only deny settlement.
func (p *piTypedWorkOutput) observe(line []byte, runID string, active bool) bool {
	value, err := nativeSessionJSON(line, 16, false)
	event, _ := value.(map[string]any)
	result, ok := event["result"].(map[string]any)
	if err != nil || event["type"] != "tool_execution_end" || !ok ||
		piSessionHasFieldAlias(event, "type", "toolName", "toolCallId", "result", "isError") ||
		piSessionHasFieldAlias(result, "details", "isError") {
		return false
	}
	details, empty := result["details"].(map[string]any)
	if result["details"] == nil || (empty && len(details) == 0) {
		// Pi puts isError on the outer frame for a thrown error, including
		// definite business denials. A bridge-returned error needs its marker.
		return result["isError"] != true
	}
	fields, ok := details[piTypedWriteUnknown].(map[string]any)
	if !active || !ok || len(details) != 1 || len(fields) != 6 || p.unknown != nil ||
		event["isError"] != true || result["isError"] != true {
		return false
	}
	e := piTypedWriteEvidence{
		Role: piSessionString(fields, "role"), RunID: piSessionString(fields, "run_id"),
		Operation: piSessionString(fields, "operation"), ToolCallID: piSessionString(fields, "tool_call_id"),
		IdempotencyKey: piSessionString(fields, "idempotency_key"), RequestSHA256: piSessionString(fields, "request_sha256"),
	}
	switch e.Operation {
	case "ack_work_input", "hand_back_work_input", "submit_result", "report_work":
	default:
		return false
	}
	if e.Role != p.role || e.RunID != runID || e.Operation != piSessionString(event, "toolName") ||
		!slices.Contains(p.tools, e.Operation) || e.ToolCallID != piSessionString(event, "toolCallId") ||
		!piTypedWriteCallID.MatchString(e.ToolCallID) || !piTypedWriteRequestSHA.MatchString(e.RequestSHA256) {
		return false
	}
	key := sha256.Sum256([]byte(e.Operation + e.ToolCallID))
	if hex.EncodeToString(key[:]) != e.IdempotencyKey {
		return false
	}
	p.unknown = &e
	return true
}

// After Wait and parser join the normal private log already has the native
// frame unless it was clipped. In that case use the reserved tail for just the
// checked identity, not unbounded native bytes or any tool payload. This extra
// log record is parent-authored and never parsed as a live provider frame.
func (p *piTypedWorkOutput) retain(log *executionLog) {
	if log.failed {
		return
	}
	if log.truncated {
		record, _ := json.Marshal(struct {
			Type string `json:"type"`
			piTypedWriteEvidence
		}{piTypedWriteUnknown, *p.unknown}) // Only bounded strings.
		data := append(append([]byte{'\n'}, record...), '\n')
		if len(data) > log.tailReserve {
			log.failed = true
			return
		}
		n, err := log.file.Write(data)
		if err != nil || n != len(data) {
			log.failed = true
			return
		}
	}
	log.failed = log.file.Sync() != nil
}
