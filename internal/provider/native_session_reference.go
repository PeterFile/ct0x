package provider

import "encoding/json"

const NativeSessionReferenceVersion = 1
const NativeSessionBindingLimit = 16 << 10

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
