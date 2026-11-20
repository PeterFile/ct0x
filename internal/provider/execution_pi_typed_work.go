package provider

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	"agentworkspace/internal/domain"
)

const piTypedWriteUnknown = "agent_workspace_write_unknown_v1"
const piTypedWriteUnverified = "agent_workspace_write_unknown_unverified_v1"
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
	role  string
	tools []string
	// Pi can end parallel calls out of order. Keep only bounded native write
	// IDs and their names; a closed ID retains an empty name to reject reuse.
	writes  map[string]string
	unknown *piTypedWriteEvidence
}

// After recordPi validates the JSON, require one exact top-level discriminator
// before dispatch. Leave nested payloads opaque to this check.
func piTypedWorkHasExactType(line []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(line))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return false
	}
	var seen bool
	var value json.RawMessage
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return false
		}
		if strings.EqualFold(key, "type") {
			if key != "type" || seen {
				return false
			}
			seen = true
		}
		if decoder.Decode(&value) != nil {
			return false
		}
	}
	return seen
}

func (p *piTypedWorkOutput) boundWrite(name string) bool {
	if !slices.Contains(p.tools, name) {
		return false
	}
	switch name {
	case "ack_work_input", "hand_back_work_input", "submit_result", "report_work":
		return true
	case ProposeTaskDecisionTool:
		return p.role == "followup"
	case DelegateWorkTool:
		return domain.TaskWorkSourceKind(p.role) == PiInitialInvestigationOwnerRole || domain.TaskWorkSourceKind(p.role).IsOwnerContinuation()
	case HandbackWorkTool:
		return domain.TaskWorkSourceKind(p.role).IsOwnerContinuation()
	default:
		return false
	}
}

func (p *piTypedWorkOutput) observeStart(line []byte, active bool) bool {
	// Pi prints args before tool validation. Read only the exact envelope
	// fields, rejecting duplicates there without interpreting raw model args.
	fields, err := codexRPCObject(line, "type", "toolName", "toolCallId")
	var kind, name, id string
	if err != nil || json.Unmarshal(fields["type"], &kind) != nil || kind != "tool_execution_start" ||
		json.Unmarshal(fields["toolName"], &name) != nil || name == "" ||
		json.Unmarshal(fields["toolCallId"], &id) != nil || id == "" {
		return false
	}
	if _, seen := p.writes[id]; seen {
		return false
	}
	if !p.boundWrite(name) {
		return true
	}
	if !active || !piTypedWriteCallID.MatchString(id) || len(p.writes) >= executionFramesCountLimit {
		return false
	}
	if p.writes == nil {
		p.writes = make(map[string]string)
	}
	p.writes[id] = name
	return true
}

func (p *piTypedWorkOutput) hasOpenWrite() bool {
	for _, name := range p.writes {
		if name != "" {
			return true
		}
	}
	return false
}

func (p *piTypedWorkOutput) observeEnd(line []byte, runID string, active bool) bool {
	value, err := nativeSessionJSON(line, 16, false)
	event, _ := value.(map[string]any)
	name, id := piSessionString(event, "toolName"), piSessionString(event, "toolCallId")
	result, ok := event["result"].(map[string]any)
	isError, hasErrorFlag := event["isError"].(bool)
	if err != nil || event["type"] != "tool_execution_end" || !ok || !hasErrorFlag || name == "" || id == "" ||
		piSessionHasFieldAlias(event, "type", "toolName", "toolCallId", "result", "isError") ||
		piSessionHasFieldAlias(result, "details", "isError") {
		return false
	}
	bound := p.boundWrite(name)
	start, seen := p.writes[id]
	if bound && (!active || start != name) || !bound && seen {
		return false
	}
	if _, hasContent := result["content"].([]any); bound && !hasContent {
		return false
	}
	details, _ := result["details"].(map[string]any)
	if result["details"] == nil || details != nil && len(details) == 0 {
		// Pi sets only the outer flag for a thrown denial. Without a marker,
		// an explicit inner flag must be false and agree with the outer flag.
		if flag, present := result["isError"]; present && (flag != false || isError) {
			return false
		}
	} else if !bound || !isError || result["isError"] != true || !p.observeMarker(details, runID, name, id) {
		return false
	}
	if bound {
		p.writes[id] = ""
	}
	return true
}

// The pinned bridge exclusively owns result.details. A marker can only be
// retained from the matched native end, never from args, messages or text.
func (p *piTypedWorkOutput) observeMarker(details map[string]any, runID, name, id string) bool {
	fields, ok := details[piTypedWriteUnknown].(map[string]any)
	if !ok || len(details) != 1 || len(fields) != 6 || p.unknown != nil {
		return false
	}
	e := piTypedWriteEvidence{
		Role: piSessionString(fields, "role"), RunID: piSessionString(fields, "run_id"),
		Operation: piSessionString(fields, "operation"), ToolCallID: piSessionString(fields, "tool_call_id"),
		IdempotencyKey: piSessionString(fields, "idempotency_key"), RequestSHA256: piSessionString(fields, "request_sha256"),
	}
	if e.Role != p.role || e.RunID != runID || e.Operation != name || e.ToolCallID != id ||
		!piTypedWriteRequestSHA.MatchString(e.RequestSHA256) {
		return false
	}
	var key string
	if name == ProposeTaskDecisionTool {
		key = PiTaskDecisionWriteKey(runID, id)
	} else {
		digest := sha256.Sum256([]byte(name + id))
		key = hex.EncodeToString(digest[:])
	}
	if key != e.IdempotencyKey {
		return false
	}
	p.unknown = &e
	return true
}

// After Wait and parser join the normal private log already has the native
// frame unless it was clipped. In that case use the reserved tail for just the
// checked identity, not unbounded native bytes or any tool payload. This extra
// log record is parent-authored and never parsed as a live provider frame.
func (e *piTypedWriteEvidence) retain(log *executionLog, kind string, bearer *piInvestigationBearerFingerprint) bool {
	if log.failed {
		return false
	}
	if log.truncated {
		record, _ := json.Marshal(struct {
			Type string `json:"type"`
			piTypedWriteEvidence
		}{kind, *e}) // Only bounded strings.
		data := append(append([]byte{'\n'}, record...), '\n')
		// JSON escaping can introduce a match absent from the decoded fields.
		if len(data) > log.tailReserve || bearer != nil && bearer.inBytes(data) {
			return false
		}
		n, err := log.file.Write(data)
		if err != nil || n != len(data) {
			return false
		}
	}
	return log.file.Sync() == nil
}
