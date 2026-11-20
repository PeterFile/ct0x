package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const PublishRunExplanationTool = "publish_run_explanation"

const (
	piRunExplanationEntrySHA = "5e22fde2267ac407312f7d67ca5cc713615961187fd9731a5d9dfbbc0710d6ac"
	piRunExplanationImplSHA  = "821a7ce8184c46864e51e1da23426ee1ae32e25726074f49001db1fc48cc43ea"
	piRunExplanationAck      = "agent_workspace_run_explanation_ack_v1"
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

// Inspect only the exact tool-end acknowledgement inserted into result.details
// by the pinned bridge. Text from the model (including tool args) is not proof.
func piRunExplanationAckKey(line []byte, runID string) string {
	value, err := nativeSessionJSON(line, 12, false)
	event, _ := value.(map[string]any)
	if err != nil || event["type"] != "tool_execution_end" || event["toolName"] != PublishRunExplanationTool || event["isError"] != false ||
		piSessionHasFieldAlias(event, "type", "toolName", "toolCallId", "result", "isError") {
		return ""
	}
	id := piSessionString(event, "toolCallId")
	result, _ := event["result"].(map[string]any)
	details, _ := result["details"].(map[string]any)
	if !piRunExplanationCallID.MatchString(id) || len(details) != 1 || piSessionHasFieldAlias(result, "details", "isError") {
		return ""
	}
	if flag, present := result["isError"]; present && flag != false {
		return ""
	}
	key := sha256.Sum256([]byte(PublishRunExplanationTool + "\x00" + runID + "\x00" + id))
	encoded := hex.EncodeToString(key[:])
	if details[piRunExplanationAck] == encoded {
		return encoded
	}
	return ""
}
