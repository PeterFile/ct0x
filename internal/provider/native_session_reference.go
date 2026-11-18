package provider

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

const NativeSessionReferenceVersion = 1
const NativeSessionBindingLimit = 16 << 10

const codexSessionAdapter = "codex-app-server-text-member/v1"
const codexSessionProtocol = "codex-app-server/v2@0.166.0"
const codexSessionHistoryTurnLimit = 256
const codexSessionHistoryItemLimit = 4096

// NativeSessionReference is a server-only, non-authorizing side channel. Binding
// is an optional frozen canonical JSON object owned and strictly parsed by Store;
// it must contain no credentials, raw claim, config or transcript. A binding-only
// value is a latest-run barrier, not a resumable native reference. Treat the value
// and its payloads as immutable; a receipt copy does not deep-copy this pointer.
// Future adapters can add their own small typed reference without changing Execute.
type NativeSessionReference struct {
	Version int                          `json:"version"`
	Binding json.RawMessage              `json:"binding,omitempty"`
	Codex   *CodexNativeSessionReference `json:"codex,omitempty"`
	Pi      *PiNativeSessionReference    `json:"pi,omitempty"`
}

// CodexNativeSessionReference records bounded public-history evidence captured
// by this adapter. It does not prove platform settlement, grant tools, or authorize
// arbitrary thread adoption. Store must independently qualify the latest receipt.
type CodexNativeSessionReference struct {
	Adapter            string `json:"adapter"`
	Protocol           string `json:"protocol"`
	CLIPath            string `json:"cli_path"`
	CLIVersion         string `json:"cli_version"`
	Provider           string `json:"provider"`
	Model              string `json:"model"`
	ReasoningEffort    string `json:"reasoning_effort,omitempty"`
	CWD                string `json:"cwd"`
	ToolsProfileSHA256 string `json:"tools_profile_sha256"`
	NativeHome         string `json:"native_home"`
	NativeThreadID     string `json:"native_thread_id"`
	NativeTurnID       string `json:"native_turn_id"`
	HistorySHA256      string `json:"history_sha256"`
	HistoryTurns       int    `json:"history_turns"`
	HistoryItems       int    `json:"history_items"`
}

// PiNativeSessionReference identifies one exact platform-owned native session
// file and completed leaf. It contains no transcript or authorization. The Pi
// adapter must validate ownership, identity and content before any append/resume;
// these fields alone never qualify an old session for execution.
type PiNativeSessionReference struct {
	Adapter                  string `json:"adapter"`
	Protocol                 string `json:"protocol"`
	CLIPath                  string `json:"cli_path"`
	CLIVersion               string `json:"cli_version"`
	Model                    string `json:"model"`
	Thinking                 string `json:"thinking"`
	CWD                      string `json:"cwd"`
	ToolsProfileSHA256       string `json:"tools_profile_sha256"`
	NativeSessionID          string `json:"native_session_id"`
	SessionFile              string `json:"session_file"`
	Device                   uint64 `json:"device"`
	Inode                    uint64 `json:"inode"`
	Size                     int64  `json:"size"`
	MTimeNS                  int64  `json:"mtime_ns"`
	FileSHA256               string `json:"file_sha256"`
	LeafID                   string `json:"leaf_id"`
	LastCompletedAssistantID string `json:"last_completed_assistant_id"`
}

// CodexAppServerSessionOptions is experimental and server supplied. Nil Resume
// explicitly selects fresh-and-capture. Non-nil Resume must be a qualified native
// reference; recovery errors never trigger a fresh fallback. Binding belongs to
// the current run, not the historical run referenced by Resume.
type CodexAppServerSessionOptions struct {
	Resume  *NativeSessionReference
	Binding json.RawMessage
}

func cloneNativeSessionReference(ref *NativeSessionReference) *NativeSessionReference {
	if ref == nil {
		return nil
	}
	copy := *ref
	copy.Binding = bytes.Clone(ref.Binding)
	if ref.Codex != nil {
		payload := *ref.Codex
		copy.Codex = &payload
	}
	if ref.Pi != nil {
		payload := *ref.Pi
		copy.Pi = &payload
	}
	return &copy
}

func nativeSessionPath(path string) bool {
	return len(path) <= 4096 && filepath.IsAbs(path) && filepath.Clean(path) == path &&
		!strings.ContainsAny(path, "\x00\r\n") && codexRPCStrictStrings(mustCodexAppServerJSON(path))
}

func nativeSessionSHA256(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

func validCodexNativeReference(ref *NativeSessionReference) bool {
	if ref == nil || ref.Version != NativeSessionReferenceVersion || ref.Codex == nil || ref.Pi != nil || !validNativeSessionBinding(ref.Binding) {
		return false
	}
	c := ref.Codex
	return c.Adapter == codexSessionAdapter && c.Protocol == codexSessionProtocol && c.CLIVersion == "0.166.0" &&
		nativeSessionPath(c.CLIPath) && nativeSessionPath(c.CWD) && nativeSessionPath(c.NativeHome) &&
		codexAppServerIdentity(c.Provider) && codexAppServerIdentity(c.Model) &&
		(c.ReasoningEffort == "" || codexAppServerIdentity(c.ReasoningEffort)) &&
		codexAppServerIdentity(c.NativeThreadID) && codexAppServerIdentity(c.NativeTurnID) &&
		nativeSessionSHA256(c.ToolsProfileSHA256) && nativeSessionSHA256(c.HistorySHA256) &&
		c.HistoryTurns > 0 && c.HistoryTurns <= codexSessionHistoryTurnLimit &&
		c.HistoryItems > 0 && c.HistoryItems <= codexSessionHistoryItemLimit
}

func validNativeSessionBinding(raw json.RawMessage) bool {
	if raw == nil {
		return true
	}
	if len(raw) == 0 || len(raw) > NativeSessionBindingLimit {
		return false
	}
	value, err := nativeSessionJSON(raw, 16, true)
	if err != nil {
		return false
	}
	if _, ok := value.(map[string]any); !ok {
		return false
	}
	canonical, err := json.Marshal(value)
	return err == nil && bytes.Equal(raw, canonical)
}

// Bound input before calling. Reject duplicate keys at every depth (including
// ignored metadata), lossy Unicode and trailing JSON. Numbers retain their exact
// JSON spelling; Binding accepts canonical int64 values only, never floats.
func nativeSessionJSON(raw []byte, depth int, integersOnly bool) (any, error) {
	if !codexRPCStrictStrings(raw) {
		return nil, errCodexRPCFrame
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var parse func(int) (any, error)
	parse = func(left int) (any, error) {
		if left <= 0 {
			return nil, errCodexRPCFrame
		}
		token, err := decoder.Token()
		if err != nil {
			return nil, errCodexRPCFrame
		}
		switch token {
		case json.Delim('{'):
			object := make(map[string]any)
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok {
					return nil, errCodexRPCFrame
				}
				if _, duplicate := object[name]; duplicate {
					return nil, errCodexRPCFrame
				}
				value, err := parse(left - 1)
				if err != nil {
					return nil, err
				}
				object[name] = value
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return nil, errCodexRPCFrame
			}
			return object, nil
		case json.Delim('['):
			array := make([]any, 0)
			for decoder.More() {
				value, err := parse(left - 1)
				if err != nil {
					return nil, err
				}
				array = append(array, value)
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return nil, errCodexRPCFrame
			}
			return array, nil
		default:
			switch value := token.(type) {
			case nil, bool, string:
				return value, nil
			case json.Number:
				if integersOnly {
					n, err := value.Int64()
					if err != nil || strconv.FormatInt(n, 10) != value.String() {
						return nil, errCodexRPCFrame
					}
				}
				return value, nil
			default:
				return nil, errCodexRPCFrame
			}
		}
	}
	value, err := parse(depth)
	if err != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		return nil, errCodexRPCFrame
	}
	return value, nil
}
