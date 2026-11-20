package provider

import (
	"crypto/sha256"
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const PublishRunExplanationTool = "publish_run_explanation"

// PiRunExplanationJournalName is the only extra file the qualified fresh
// bridge may create. The runtime binds its path to the current scheduler run.
const PiRunExplanationJournalName = "run-explanation-bridge-attempt"

const (
	piRunExplanationEntrySHA = "0a98c13677666ba4c5e205ea7a2cf6987709125096becee1177692359054a9b2"
	piRunExplanationImplSHA  = "24efa725e336f4c819a0f42e90e852603f76db2283ae4bbdf9b9b3b40eaa1c94"
	piRunExplanationAck      = "agent_workspace_run_explanation_ack_v1"
	piRunExplanationUnknown  = "agent_workspace_run_explanation_unknown_v1"
	piRunExplanationMaxBytes = 1024
)

var (
	piRunExplanationCallID           = regexp.MustCompile(`^[!-~]{1,256}$`)
	piRunExplanationClaimFingerprint = regexp.MustCompile(`^[1-9][0-9]{0,2}:[0-9a-f]{64}$`)
)

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
	journal := piRunExplanationConfigJournal(path, issued.SHA256, runID)
	if bridge == "" || journal == "" {
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
	native.memberTools.configSHA256, native.memberTools.runExplanation, native.memberTools.runExplanationJournal = issued.SHA256, runID, journal
	return piDigestBoundExecutor{piExecutor: native, configuredBridge: extension}, nil
}

func piRunExplanationConfigJournal(path string, digest [sha256.Size]byte, runID string) string {
	if !executionID.MatchString(runID) {
		return ""
	}
	raw, _, err := readMemberToolConfigFile(path)
	if err != nil || sha256.Sum256(raw) != digest {
		return ""
	}
	value, err := nativeSessionJSON(raw, 3, false)
	fields, _ := value.(map[string]any)
	journal := piSessionString(fields, "run_explanation_journal")
	if err != nil || len(fields) != 5 || fields["run_explanation"] != runID ||
		memberToolConfigOrigin(piSessionString(fields, "endpoint"), piSessionString(fields, "token")) == "" ||
		!piRunExplanationClaimFingerprint.MatchString(piSessionString(fields, "run_explanation_claim_fingerprint")) ||
		!filepath.IsAbs(journal) || filepath.Clean(journal) != journal || strings.ContainsRune(journal, 0) ||
		filepath.Base(journal) != PiRunExplanationJournalName || filepath.Base(filepath.Dir(journal)) != runID {
		return ""
	}
	return journal
}

type piRunExplanationOutput struct {
	// Outstanding pairs, indexed by the derived key. Retain only digests;
	// the exact possibly-sent text lives in the independent bridge journal.
	pending            map[string]string
	ackKey, ackTextSHA string
	unknown            bool
}

func piRunExplanationKey(runID, id string) string {
	return piSessionBytesDigest([]byte(PublishRunExplanationTool + "\x00" + runID + "\x00" + id))
}

func (p *piRunExplanationOutput) observe(line []byte, kind, decodedTool, runID string, active bool) bool {
	fields, err := codexRPCObject(line, "toolName")
	var tool string
	if err != nil || json.Unmarshal(fields["toolName"], &tool) != nil || tool == "" || tool != decodedTool {
		return false // A duplicate/aliased discriminator must not hide an invocation.
	}
	if tool != PublishRunExplanationTool {
		return true
	}
	value, err := nativeSessionJSON(line, 12, false)
	event, _ := value.(map[string]any)
	id := piSessionString(event, "toolCallId")
	if err != nil || event["type"] != kind || !piRunExplanationCallID.MatchString(id) ||
		piSessionHasFieldAlias(event, "type", "toolName", "toolCallId", "args", "result", "isError") || !active {
		return false
	}
	key := piRunExplanationKey(runID, id)
	if kind == "tool_execution_start" {
		if _, present := event["args"]; !present {
			return false
		}
		if key == p.ackKey {
			// A later same-key invocation needs its own ACK. A generic error
			// or an omitted result cannot borrow this earlier receipt.
			p.ackKey, p.ackTextSHA = "", ""
		}
		if _, open := p.pending[key]; open || len(p.pending) >= executionFramesCountLimit {
			return false // Fail closed on ambiguity/overflow without evicting a pair.
		}
		if p.pending == nil {
			p.pending = make(map[string]string)
		}
		// Pi prints starts before validation. Even invalid args need closure.
		args, _ := event["args"].(map[string]any)
		text := piSessionString(args, "text")
		digest := ""
		if len(args) == 1 && len(text) <= piRunExplanationMaxBytes && strings.TrimSpace(text) != "" && !strings.ContainsRune(text, 0) {
			digest = piSessionBytesDigest([]byte(text))
		}
		p.pending[key] = digest
		return true
	}
	return p.end(event, key)
}

func (p *piRunExplanationOutput) end(event map[string]any, key string) bool {
	digest, open := p.pending[key]
	if !open {
		return false
	}
	result, _ := event["result"].(map[string]any)
	_, content := result["content"].([]any)
	isError, outer := event["isError"].(bool)
	inner, hasInner := result["isError"]
	detailValue, detailsPresent := result["details"]
	details, detailsObject := detailValue.(map[string]any)
	if !content || !outer || piSessionHasFieldAlias(result, "details", "isError", "content") {
		return false
	}
	switch {
	case !isError && (!hasInner || inner == false) && len(details) == 1 && details[piRunExplanationAck] == key:
		if digest == "" || p.ackKey != "" && (p.ackKey != key || p.ackTextSHA != digest) {
			return false
		}
		p.ackKey, p.ackTextSHA = key, digest
	case isError && inner == true && len(details) == 1:
		fields, _ := details[piRunExplanationUnknown].(map[string]any)
		if len(fields) != 2 || fields["idempotency_key"] != key || !nativeSessionSHA256(piSessionString(fields, "text_sha256")) {
			return false
		}
		p.unknown = true // Sticky even if later frames or the terminal report success.
	case isError && !hasInner && (!detailsPresent || detailsObject && len(details) == 0):
		// Native validation/denial can close a pair but conveys no ACK.
	default:
		return false
	}
	delete(p.pending, key)
	return true
}
