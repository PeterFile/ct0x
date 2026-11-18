package provider

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"path/filepath"
)

const piInvestigationReady = "agent_workspace_investigation_ready_v1"

// The pinned typed investigation bridge owns this customType. Pi 1.4.0
// serializes it outside message/tool payloads, after the in-memory session
// header and before the first agent_start.
type piInvestigationStartup struct {
	role, configSHA256     string
	header, ready, invalid bool
	response               Event // Only the latest snapshot, bounded by the native frame limit.

	unverifiedWrites piTypedWorkOutput // Never consulted for settlement or delivery.
	bearer           piInvestigationBearerFingerprint
}

// A plausible native call ID (or digest) can contain the issued bearer. Keep
// only its fingerprint and compare against windows of the bounded fields.
type piInvestigationBearerFingerprint struct {
	size   int
	digest [sha256.Size]byte
}

func (b piInvestigationBearerFingerprint) inEvidence(e *piTypedWriteEvidence) bool {
	for _, field := range [...]string{e.Role, e.RunID, e.Operation, e.ToolCallID, e.IdempotencyKey, e.RequestSHA256} {
		for i := 0; i+b.size <= len(field); i++ {
			if sha256.Sum256([]byte(field[i:i+b.size])) == b.digest {
				return true
			}
		}
	}
	return false
}

// Reuse the bounded native pair checks on frames rejected before acceptance.
// Matching here makes the identity plausible, not attributable to startup.
func (s *piInvestigationStartup) observeUnverifiedWrite(kind string, line []byte, runID string) {
	w := &s.unverifiedWrites
	if w.unknown != nil || s.bearer.size == 0 {
		return
	}
	switch kind {
	case "tool_execution_start":
		w.observeStart(line, true)
	case "tool_execution_end":
		if len(w.writes) != 0 && w.observeEnd(line, runID, true) && w.unknown != nil && s.bearer.inEvidence(w.unknown) {
			w.unknown = nil
		}
	}
}

// allow consumes the startup prefix and later entry frames. Do not expose the
// session or let the common parser accept the turn before the whole prefix
// matches. After acceptance, keep observing write evidence even if the startup
// proof is later contradicted (e.g. by a duplicate).
func (s *piInvestigationStartup) allow(o *executionOutput, kind string, line []byte) bool {
	if o.receipt.Accepted {
		switch kind {
		case "session":
			s.invalid = true
			return false
		case "entry_appended":
			entry := piInvestigationEntry(line)
			if entry == nil || entry["customType"] == piInvestigationReady {
				s.invalid = true
				return false
			}
		}
		return true
	}
	if s.invalid {
		return false
	}
	switch {
	case !s.header && kind == "session" && piInvestigationHeader(line, o.runID):
		s.header = true
	case s.header && !s.ready && kind == "entry_appended" && s.matches(piInvestigationEntry(line), o.runID):
		s.ready = true
	case s.ready && kind == "agent_start" && bytes.Equal(line, []byte(`{"type":"agent_start"}`)):
		o.receipt.NativeSessionID = o.runID
		o.emit(Event{Kind: SessionEvent})
		return true
	default:
		s.invalid = true
	}
	return false
}

func piInvestigationHeader(line []byte, runID string) bool {
	value, err := nativeSessionJSON(line, 8, false)
	header, _ := value.(map[string]any)
	return err == nil && len(header) == 5 && header["type"] == "session" && header["id"] == runID && header["version"] == json.Number("3") &&
		filepath.IsAbs(piSessionString(header, "cwd")) && piSessionTimestamp(header)
}

func piInvestigationEntry(line []byte) map[string]any {
	value, err := nativeSessionJSON(line, 16, false)
	frame, _ := value.(map[string]any)
	entry, _ := frame["entry"].(map[string]any)
	if err != nil || len(frame) != 2 || frame["type"] != "entry_appended" || piSessionString(entry, "type") == "" ||
		piSessionHasFieldAlias(entry, "type", "customType") {
		return nil
	}
	return entry
}

// The server-issued digest commits to the exact action and ordered tools as
// well as the run and role. The proof does not copy config or tool data.
func (s *piInvestigationStartup) matches(entry map[string]any, runID string) bool {
	data, _ := entry["data"].(map[string]any)
	parent, hasParent := entry["parentId"]
	return len(entry) == 6 && entry["type"] == "custom" && entry["customType"] == piInvestigationReady &&
		piSessionEntryID.MatchString(piSessionString(entry, "id")) && hasParent && (parent == nil || piSessionEntryID.MatchString(piSessionString(entry, "parentId"))) &&
		piSessionTimestamp(entry) && len(data) == 3 && data["run_id"] == runID && data["source_kind"] == s.role && data["config_sha256"] == s.configSHA256
}

// Acceptance must be live for scoped tools, but response and terminal can
// wait for the whole framed stream. An invalid or incomplete proof must not
// have left a trusted response/terminal even when the duplicate arrives last.
func (s *piInvestigationStartup) finish(o *executionOutput, stdoutComplete bool) {
	r := o.receipt
	if !r.Accepted || !stdoutComplete || s.invalid {
		s.invalid, o.damaged, r.Terminal = true, true, ""
		return
	}
	if s.response.Kind != "" {
		o.delivery.enqueue(s.response)
	}
	if r.Terminal != "" {
		o.delivery.enqueue(Event{Kind: TerminalEvent, NativeSessionID: r.NativeSessionID, Terminal: r.Terminal})
	}
}
