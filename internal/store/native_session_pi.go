package store

import (
	"regexp"

	"agentworkspace/internal/provider"
)

// These bounds and identity grammars match the frozen Pi 1.4.0 adapter. Store
// validates metadata only; file ownership, integrity and one-turn append proof
// remain the provider's responsibility, not filesystem work in a transaction.
var nativePiModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._/-]*$`)
var nativePiSessionID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var nativePiEntryID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func validNativeAdapterProfile(p NativeRunProfile) bool {
	switch p.Adapter {
	case "codex-app-server-text-member/v1":
		return p.Protocol == "codex-app-server/v2@0.166.0" && p.CLIVersion == "0.166.0" && p.Thinking == "" &&
			p.NativePolicy == (NativeTextPolicy{Sandbox: "read-only", ApprovalPolicy: "never"})
	case "pi-json-text-member-session/v1":
		return p.Protocol == "pi-json-session/v3@1.4.0" && p.CLIVersion == "1.4.0" && nativePiModel.MatchString(p.Model) &&
			validNativePiThinking(p.Thinking) && p.NativePolicy == (NativeTextPolicy{Sandbox: "tools-only", ApprovalPolicy: "never"})
	}
	return false
}

func validNativePiThinking(thinking string) bool {
	switch thinking {
	case "off", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

func validNativePiReference(pi *provider.PiNativeSessionReference, p NativeRunProfile, tools, nativeID string) bool {
	return pi.Adapter == p.Adapter && pi.Protocol == p.Protocol && pi.CLIPath == p.CLIPath && pi.CLIVersion == p.CLIVersion &&
		pi.Model == p.Model && pi.Thinking == p.Thinking && pi.CWD == p.CWD && pi.ToolsProfileSHA256 == tools &&
		pi.NativeSessionID == nativeID && nativePiSessionID.MatchString(pi.NativeSessionID) && nativePath(pi.SessionFile) &&
		pi.Device > 0 && pi.Device <= 1<<63-1 && pi.Inode > 0 && pi.Inode <= 1<<63-1 &&
		pi.Size > 0 && pi.Size <= 32<<20 && pi.MTimeNS > 0 && nativeHexSHA256(pi.FileSHA256) &&
		nativePiEntryID.MatchString(pi.LeafID) && nativePiEntryID.MatchString(pi.LastCompletedAssistantID)
}

func nativePiAppendMetadata(actual, prior *provider.PiNativeSessionReference) bool {
	return actual.NativeSessionID == prior.NativeSessionID && actual.SessionFile == prior.SessionFile &&
		actual.Device == prior.Device && actual.Inode == prior.Inode && actual.Size > prior.Size && actual.MTimeNS >= prior.MTimeNS &&
		actual.FileSHA256 != prior.FileSHA256 && actual.LeafID != prior.LeafID && actual.LastCompletedAssistantID != prior.LastCompletedAssistantID
}
