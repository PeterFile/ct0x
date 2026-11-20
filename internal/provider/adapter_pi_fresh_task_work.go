package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

const piFreshTaskWorkTools = "read_work_inputs,ack_work_input,hand_back_work_input,submit_result"
const ProposeTaskDecisionTool = "propose_task_decision"

// Independent from the ordinary index.ts/bridge.mjs pair used by native resume.
// Qualify complete pairs, not the Cartesian product of accepted file digests.
const (
	piFreshTaskWorkEntrySHA = "834dced6bdfbcd6bae9ca04ca9a1501a980407d02995efbaa9743ca6e701c866"
	piFreshTaskWorkImplSHA  = "0da78b82aa74e0d3f2cdd38131dc62997b4d67d6041a2efe8b247bc3d789fa6f"
	piTaskDecisionEntrySHA  = "106638ddee12a5efbf653853070ee123a6614f1ed84907026e13338558cb5ab6"
	piTaskDecisionImplSHA   = "aca7d329999f26cc9bf1e1c4900ee8cdf6d5f4f33ede4ba584b9463e6d067f5f"
)

func PiFreshTaskWorkToolNames() []string { return strings.Split(piFreshTaskWorkTools, ",") }

func PiFreshTaskDecisionToolNames() []string {
	return append(PiFreshTaskWorkToolNames(), ProposeTaskDecisionTool)
}

// FreshTaskWorkBinding comes only from the claimed runtime, never tool input.
// Only the private file contains the endpoint and capability.
type FreshTaskWorkBinding struct {
	ConfigPath   string            `json:"-"`
	ConfigSHA256 [sha256.Size]byte `json:"-"`
	RunID        string            `json:"-"`
	Operations   []string          `json:"-"`
}

// PiFreshTaskWorkAdapter selects only the exact text continuation profile;
// it never probes a CLI or selects native resume. The executable, loader and
// extension ancestry must remain trusted and stable through invocation.
type PiFreshTaskWorkAdapter struct {
	extensionPath string
	timeout       time.Duration
}

func NewPiFreshTaskWorkAdapter(extensionPath string, timeout time.Duration) PiFreshTaskWorkAdapter {
	return PiFreshTaskWorkAdapter{extensionPath: extensionPath, timeout: timeout}
}

func (a PiFreshTaskWorkAdapter) Supports(selected Provider) bool {
	return selected.Name == Pi && ValidateExecutor(selected, a.timeout) == nil && a.qualifiedBridge() != ""
}

// SupportsTaskDecision qualifies the optional schema on the same typed path.
// Old releases and ordinary/native-resume adapters do not understand it.
func (a PiFreshTaskWorkAdapter) SupportsTaskDecision(selected Provider) bool {
	return selected.Name == Pi && ValidateExecutor(selected, a.timeout) == nil && a.bridgeForProfile(true) != ""
}

func (a PiFreshTaskWorkAdapter) NewExecutor(ctx context.Context, selected Provider, binding FreshTaskWorkBinding) (Executor, error) {
	return a.newExecutor(ctx, selected, binding, false)
}

func (a PiFreshTaskWorkAdapter) NewTaskDecisionExecutor(ctx context.Context, selected Provider, binding FreshTaskWorkBinding) (Executor, error) {
	return a.newExecutor(ctx, selected, binding, true)
}

func (a PiFreshTaskWorkAdapter) newExecutor(ctx context.Context, selected Provider, binding FreshTaskWorkBinding, decision bool) (Executor, error) {
	names := PiFreshTaskWorkToolNames()
	if decision {
		names = PiFreshTaskDecisionToolNames()
	}
	if ctx.Err() != nil || selected.Name != Pi || !executionID.MatchString(binding.RunID) || !slices.Equal(binding.Operations, names) {
		return nil, errPiMemberToolConfiguration
	}
	baseline, err := NewExecutor(selected, a.timeout)
	if err != nil {
		return nil, err
	}
	bridge := a.bridgeForProfile(decision)
	if bridge == "" || binding.ConfigSHA256 == ([sha256.Size]byte{}) {
		return nil, errPiMemberToolConfiguration
	}
	config, err := readFreshTaskWorkConfig(binding.ConfigPath, decision)
	if err != nil || config.digest != binding.ConfigSHA256 || ctx.Err() != nil {
		return nil, errPiMemberToolConfiguration
	}
	pi := baseline.(piExecutor)
	pi.memberTools = &piMemberToolOptions{extensionPath: bridge, configPath: filepath.Clean(binding.ConfigPath),
		toolNames: names, configSHA256: binding.ConfigSHA256, typedWorkRole: "followup", taskDecision: decision}
	return &piFreshTaskWorkExecutor{pi: pi, adapter: a, runID: binding.RunID, configFile: config.file}, nil
}

// FreshTaskWorkConfigSHA256 hashes the canonical bytes issued by the runtime
// for this profile, which never advertises the ordinary intake schema hint.
func FreshTaskWorkConfigSHA256(endpoint, token string) [sha256.Size]byte {
	data, _ := json.Marshal(struct {
		Endpoint string `json:"endpoint"`
		Token    string `json:"token"`
	}{endpoint, token}) // Encoding two strings cannot fail.
	return sha256.Sum256(append(data, '\n'))
}

// FreshTaskWorkConfigMatches checks the issued bytes without exposing a token.
func FreshTaskWorkConfigMatches(path string, digest [sha256.Size]byte) bool {
	config, err := readMemberToolConfig(path)
	return err == nil && config.digest == digest
}

type piFreshTaskDecisionConfig struct {
	Endpoint string `json:"endpoint"`
	Token    string `json:"token"`
	Proof    string `json:"task_decision_proof"`
}

// PiFreshTaskDecisionConfig encodes an issued credential, not model input.
// The additional proof is never set by the ordinary configuration codec.
func PiFreshTaskDecisionConfig(endpoint, token, proof string) ([]byte, error) {
	if memberToolConfigOrigin(endpoint, token) == "" || !piTypedWriteRequestSHA.MatchString(proof) || token == proof {
		return nil, errPiMemberToolConfiguration
	}
	data, err := json.Marshal(piFreshTaskDecisionConfig{Endpoint: endpoint, Token: token, Proof: proof})
	return append(data, '\n'), err
}

func readFreshTaskWorkConfig(path string, decision bool) (memberToolConfig, error) {
	if !decision {
		config, err := readMemberToolConfig(path)
		if err != nil || FreshTaskWorkConfigSHA256(config.endpoint, config.token) != config.digest {
			return memberToolConfig{}, errPiMemberToolConfiguration
		}
		return config, nil
	}
	raw, file, err := readMemberToolConfigFile(path)
	var issued piFreshTaskDecisionConfig
	if err != nil || json.Unmarshal(raw, &issued) != nil {
		return memberToolConfig{}, errPiMemberToolConfiguration
	}
	canonical, err := PiFreshTaskDecisionConfig(issued.Endpoint, issued.Token, issued.Proof)
	if err != nil || !bytes.Equal(raw, canonical) {
		return memberToolConfig{}, errPiMemberToolConfiguration
	}
	return memberToolConfig{digest: sha256.Sum256(raw), file: file}, nil
}

type piFreshTaskWorkExecutor struct {
	pi         piExecutor
	adapter    PiFreshTaskWorkAdapter
	runID      string
	configFile os.FileInfo
	used       atomic.Bool
}

func (e *piFreshTaskWorkExecutor) Execute(ctx context.Context, request ExecuteRequest, onEvent func(Event) error) ExecutionReceipt {
	unknown := ExecutionReceipt{Outcome: OutcomeUnknown, ExitCode: -1, Reason: "任务续办传输绑定无法确认，未调用进程，需对账"}
	opts := e.pi.memberTools
	if e.used.Swap(true) || ctx.Err() != nil || request.RunID != e.runID || e.adapter.bridgeForProfile(opts.taskDecision) != opts.extensionPath {
		return unknown
	}
	config, err := readFreshTaskWorkConfig(opts.configPath, opts.taskDecision)
	if err != nil || config.digest != opts.configSHA256 || !os.SameFile(e.configFile, config.file) || ctx.Err() != nil {
		return unknown
	}
	// Ordinary Pi argv construction already pins the fresh session ID and
	// exact allowlist. Only the child receives the issued config digest.
	return e.pi.Execute(ctx, request, onEvent)
}

func (a PiFreshTaskWorkAdapter) qualifiedBridge() string { return a.bridgeForProfile(false) }

func (a PiFreshTaskWorkAdapter) bridgeForProfile(decision bool) string {
	if !filepath.IsAbs(a.extensionPath) {
		return ""
	}
	// Pass the physical entry so the loader resolves the same relative import.
	entry, err := filepath.EvalSymlinks(a.extensionPath)
	if err != nil || filepath.Base(entry) != "fresh-task-work.ts" {
		return ""
	}
	impl := filepath.Join(filepath.Dir(entry), "fresh-task-work.mjs")
	if piBridgeBytesMatch(entry, piTaskDecisionEntrySHA) && piBridgeBytesMatch(impl, piTaskDecisionImplSHA) ||
		!decision && piBridgeBytesMatch(entry, piFreshTaskWorkEntrySHA) && piBridgeBytesMatch(impl, piFreshTaskWorkImplSHA) {
		return entry
	}
	return ""
}
