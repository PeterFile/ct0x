package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"agentworkspace/internal/domain"
)

// Only this entry (and its pinned pure bridge validators) implements the typed
// investigation profile. It does not qualify either ordinary Pi entry point.
const piFreshInvestigationEntrySHA = "ba79d6f902bd3289f5697c4cbd8912338663848716fee6fed85469eb473a4f2c"

// PiFreshInvestigationProfile is the launcher-selected profile of an issued
// investigation capability. Operation names describe transport, not grants.
// Supports does not infer a source kind or widen a partial profile.
type PiFreshInvestigationProfile struct {
	Action     string                    `json:"action"`
	SourceKind domain.TaskWorkSourceKind `json:"source_kind"`
	Operations []string                  `json:"operations"`
}

func (p PiFreshInvestigationProfile) valid() bool {
	if p.Action != domain.ChatActionReadOnlyInvestigation {
		return false
	}
	names := p.Operations
	// Consume the two optional reads in C's order, at most once each.
	for _, op := range []string{"read_investigation_channel", "read_investigation_artifact"} {
		if len(names) > 0 && names[0] == op {
			names = names[1:]
		}
	}
	switch p.SourceKind {
	case domain.TaskWorkSourceFollowup:
		return slices.Equal(names, PiFreshTaskWorkToolNames())
	case domain.TaskWorkSourceDelegation:
		return slices.Equal(names, []string{"report_work"})
	case domain.TaskWorkSourceReport:
		return len(names) != 0 && names[0] == "read_work_report" && slices.Equal(names[1:], PiFreshTaskWorkToolNames())
	}
	return false
}

// PiFreshInvestigationBinding pairs the launcher's issued digest with the
// exact claimed Turn and profile. It is never a wire or model input.
type PiFreshInvestigationBinding struct {
	ConfigPath   string                      `json:"-"`
	ConfigSHA256 [sha256.Size]byte           `json:"-"`
	RunID        string                      `json:"-"`
	Profile      PiFreshInvestigationProfile `json:"-"`
}

type piFreshInvestigationWire struct {
	Endpoint string `json:"endpoint"`
	Token    string `json:"token"`
	RunID    string `json:"run_id"`
	PiFreshInvestigationProfile
}

// PiFreshInvestigationConfig encodes and hashes the exact typed bootstrap from
// trusted issuance facts. The caller owns the private 0600 file, lifetime and
// revocation. It must pass this digest, not compute it from a path on readback.
func PiFreshInvestigationConfig(endpoint, token, runID string, profile PiFreshInvestigationProfile) ([]byte, [sha256.Size]byte, error) {
	if !profile.valid() || !executionID.MatchString(runID) || memberToolConfigOrigin(endpoint, token) == "" {
		return nil, [sha256.Size]byte{}, errPiMemberToolConfiguration
	}
	raw, _ := json.Marshal(piFreshInvestigationWire{endpoint, token, runID, profile}) // Only strings and a string slice.
	raw = append(raw, '\n')
	return raw, sha256.Sum256(raw), nil
}

// PiFreshInvestigationAdapter is an experimental, fresh-only transport. It
// samples installed Pi metadata and configured bytes without discovery or CLI
// probes. Use only a qualified Pi 1.4.0 executable, loader and dependencies in
// trusted, stable paths and environment. This is not a host filesystem sandbox.
type PiFreshInvestigationAdapter struct {
	extensionPath string
	timeout       time.Duration
}

func NewPiFreshInvestigationAdapter(extensionPath string, timeout time.Duration) PiFreshInvestigationAdapter {
	return PiFreshInvestigationAdapter{extensionPath: extensionPath, timeout: timeout}
}

func (a PiFreshInvestigationAdapter) Supports(selected Provider, profile PiFreshInvestigationProfile) bool {
	return profile.valid() && selected.Name == Pi && ValidateExecutor(selected, a.timeout) == nil && a.qualifiedBridge() != ""
}

func (a PiFreshInvestigationAdapter) NewExecutor(ctx context.Context, selected Provider, binding PiFreshInvestigationBinding) (Executor, error) {
	if ctx.Err() != nil || selected.Name != Pi || !binding.Profile.valid() || !executionID.MatchString(binding.RunID) ||
		binding.ConfigSHA256 == ([sha256.Size]byte{}) {
		return nil, errPiMemberToolConfiguration
	}
	baseline, err := NewExecutor(selected, a.timeout)
	if err != nil {
		return nil, err
	}
	bridge := a.qualifiedBridge()
	if bridge == "" {
		return nil, errPiMemberToolConfiguration
	}
	raw, file, err := readMemberToolConfigFile(binding.ConfigPath)
	var issued piFreshInvestigationWire
	if err != nil || sha256.Sum256(raw) != binding.ConfigSHA256 || json.Unmarshal(raw, &issued) != nil {
		return nil, errPiMemberToolConfiguration
	}
	// Compare canonical issuance bytes, not just decoded values. No duplicate
	// fields, alternate profiles, or legacy/intake hints can pass readback.
	want, _, err := PiFreshInvestigationConfig(issued.Endpoint, issued.Token, binding.RunID, binding.Profile)
	if err != nil || !bytes.Equal(raw, want) || ctx.Err() != nil {
		return nil, errPiMemberToolConfiguration
	}
	pi := baseline.(piExecutor)
	pi.memberTools = &piMemberToolOptions{extensionPath: bridge, configPath: filepath.Clean(binding.ConfigPath),
		toolNames: issued.Operations, configSHA256: binding.ConfigSHA256, investigationConfigFile: file, typedWorkRole: string(issued.SourceKind)}
	return &piFreshInvestigationExecutor{pi: pi, adapter: a, runID: binding.RunID}, nil
}

func (a PiFreshInvestigationAdapter) qualifiedBridge() string {
	if !filepath.IsAbs(a.extensionPath) {
		return ""
	}
	entry, err := filepath.EvalSymlinks(a.extensionPath)
	// Give Pi the checked physical entry so its relative import agrees with us.
	if err != nil || filepath.Base(entry) != "fresh-investigation-work.ts" || !piBridgeBytesMatch(entry, piFreshInvestigationEntrySHA) ||
		!piBridgeBytesMatch(filepath.Join(filepath.Dir(entry), "fresh-task-work.mjs"), piFreshTaskWorkImplSHA) {
		return ""
	}
	return entry
}

type piFreshInvestigationExecutor struct {
	pi      piExecutor
	adapter PiFreshInvestigationAdapter
	runID   string
	used    atomic.Bool
}

func (e *piFreshInvestigationExecutor) Execute(ctx context.Context, request ExecuteRequest, onEvent func(Event) error) ExecutionReceipt {
	opts := e.pi.memberTools
	if e.used.Swap(true) || request.RunID != e.runID || ctx.Err() != nil ||
		e.adapter.qualifiedBridge() != opts.extensionPath || !opts.launchConfigMatches() || ctx.Err() != nil {
		return ExecutionReceipt{Outcome: OutcomeUnknown, ExitCode: -1, Reason: "调查传输绑定无法确认，未调用进程，需对账"}
	}
	return e.pi.Execute(ctx, request, onEvent)
}
