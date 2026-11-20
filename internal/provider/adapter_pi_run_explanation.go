package provider

import (
	"crypto/sha256"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const PublishRunExplanationTool = "publish_run_explanation"

const (
	piRunExplanationEntrySHA = "dc5c17fe9f5d783d1be2becb1c47e38b01443d2f85e23e8243ddb3100853902b"
	piRunExplanationImplSHA  = "85bfc85b5ebda50b7bcddb64966f1f730838c5437c4e2d9caef4251f0fe274d5"
	piRunExplanationAck      = "agent_workspace_run_explanation_ack_v1"
	piRunExplanationUnknown  = "agent_workspace_run_explanation_unknown_v1"
	piRunExplanationMaxBytes = 1024
)

var piRunExplanationCallID = regexp.MustCompile(`^[!-~]{1,256}$`)

func PiRunExplanationBridgeSupported(extensionPath string) bool {
	return qualifyPiRunExplanationBridge(extensionPath) != ""
}

func qualifyPiRunExplanationBridge(extensionPath string) string {
	return qualifyPiBridgePair(extensionPath, piRunExplanationImplSHA, piRunExplanationEntrySHA)
}

// NewPiRunExplanationExecutor adds one operation to a complete ordinary, fresh
// Pi profile. The caller owns the live claim and issued config. Neither this
// constructor nor the tool name grants authority or elects native resume.
func NewPiRunExplanationExecutor(selected Provider, extension string, issued NativeMemberToolConfig, runID string, base []string, timeout time.Duration) (Executor, error) {
	bridge, path := qualifyPiRunExplanationBridge(extension), filepath.Clean(issued.Path)
	if bridge == "" || !piRunExplanationConfigMatches(path, issued.SHA256, runID) {
		return nil, errPiMemberToolConfiguration
	}
	native, err := newPiMemberToolExecutor(selected, bridge, path, base, timeout, false)
	if err != nil {
		return nil, err
	}
	ordinary := strings.Split(piMemberToolNames, ",")
	names := native.memberTools.toolNames
	if !slices.Equal(names, ordinary) && !slices.Equal(names, append(ordinary, "search_history", "read_history")) {
		return nil, errPiMemberToolConfiguration
	}
	native.memberTools.toolNames = append(names, PublishRunExplanationTool)
	native.memberTools.configSHA256, native.memberTools.runExplanation = issued.SHA256, runID
	return piDigestBoundExecutor{piExecutor: native, configuredBridge: extension}, nil
}

func piRunExplanationConfigMatches(path string, digest [sha256.Size]byte, runID string) bool {
	config, err := readMemberToolConfigMode(path, true, false, true)
	return err == nil && config.runExplanation == runID && config.digest == digest
}

// RunExplanationUncertainty is private evidence from the pinned explanation
// bridge, not a publication acknowledgement. The optional text is never part
// of the marker or a serialized execution receipt.
type RunExplanationUncertainty struct {
	IdempotencyKey string `json:"idempotency_key"`
	TextSHA256     string `json:"text_sha256"`
	text           string
}

// ReconciliationText returns exact text only if a native start matched the
// key and hash of the sent request. An absent start cannot disprove a write.
func (e *RunExplanationUncertainty) ReconciliationText() string { return e.text }

type piRunExplanationOutput struct {
	// A bounded set of recent candidates, not a uniqueness constraint on
	// native IDs. Parallel starts can share an ID and exact replay reuses it.
	starts        [executionFramesCountLimit]struct{ id, text string }
	next          int
	ackConflicted bool
}

func (p *piRunExplanationOutput) observeStart(line []byte) {
	value, err := nativeSessionJSON(line, 12, false)
	event, _ := value.(map[string]any)
	if err != nil || event["type"] != "tool_execution_start" || event["toolName"] != PublishRunExplanationTool ||
		piSessionHasFieldAlias(event, "type", "toolName", "toolCallId", "args") {
		return
	}
	id := piSessionString(event, "toolCallId")
	args, _ := event["args"].(map[string]any)
	text := piSessionString(args, "text")
	if piRunExplanationCallID.MatchString(id) && len(args) == 1 && len(text) <= piRunExplanationMaxBytes &&
		strings.TrimSpace(text) != "" && !strings.ContainsRune(text, 0) {
		p.starts[p.next].id, p.starts[p.next].text = id, text
		p.next = (p.next + 1) % len(p.starts)
	}
}

func (p *piRunExplanationOutput) observeEnd(line []byte, runID string, r *ExecutionReceipt, allowAck bool) bool {
	if r.RunExplanationUnknown != nil {
		return true // No later frame can clear the first uncertainty.
	}
	id, key, unknown := parsePiRunExplanationEnd(line, runID)
	if unknown != nil {
		for _, start := range p.starts {
			if start.id == id && piSessionBytesDigest([]byte(start.text)) == unknown.TextSHA256 {
				unknown.text = start.text
				break
			}
		}
		clear(p.starts[:])
		r.RunExplanationUnknown, r.RunExplanationAckKey = unknown, ""
	} else if key != "" && allowAck && !p.ackConflicted {
		if r.RunExplanationAckKey != "" && r.RunExplanationAckKey != key {
			// Conflicting ACKs cannot validate the attempt, but must not disable
			// observation of a subsequent unknown result.
			r.RunExplanationAckKey, p.ackConflicted = "", true
			return false
		}
		r.RunExplanationAckKey = key
	}
	return true
}

// Inspect only the exact native tool end. The pinned bridge owns its
// result.details; args, tool/assistant text and other tools are not evidence.
func parsePiRunExplanationEnd(line []byte, runID string) (id, ackKey string, unknown *RunExplanationUncertainty) {
	value, err := nativeSessionJSON(line, 12, false)
	event, _ := value.(map[string]any)
	if err != nil || event["type"] != "tool_execution_end" || event["toolName"] != PublishRunExplanationTool ||
		piSessionHasFieldAlias(event, "type", "toolName", "toolCallId", "result", "isError") {
		return "", "", nil
	}
	id = piSessionString(event, "toolCallId")
	result, _ := event["result"].(map[string]any)
	details, _ := result["details"].(map[string]any)
	if !piRunExplanationCallID.MatchString(id) || len(details) != 1 || piSessionHasFieldAlias(result, "details", "isError") {
		return "", "", nil
	}
	key := piSessionBytesDigest([]byte(PublishRunExplanationTool + "\x00" + runID + "\x00" + id))
	flag, present := result["isError"]
	if event["isError"] == false && (!present || flag == false) && details[piRunExplanationAck] == key {
		return id, key, nil
	}
	if event["isError"] == true && flag == true {
		fields, _ := details[piRunExplanationUnknown].(map[string]any)
		textSHA256 := piSessionString(fields, "text_sha256")
		if _, hasContent := result["content"].([]any); hasContent && len(fields) == 2 &&
			fields["idempotency_key"] == key && nativeSessionSHA256(textSHA256) {
			return id, "", &RunExplanationUncertainty{IdempotencyKey: key, TextSHA256: textSHA256}
		}
	}
	return "", "", nil
}
