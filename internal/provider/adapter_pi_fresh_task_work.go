package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const piFreshTaskWorkTools = "read_work_inputs,ack_work_input,hand_back_work_input,submit_result"

// Independent from the ordinary index.ts/bridge.mjs pair used by native resume.
// Update both digests together after offline Pi 1.4.0 loader/config checks.
const (
	piFreshTaskWorkEntrySHA = "0f42260dfd4ce255da065315a929cbc715128de0f82211f3762b8eadb41f2f81"
	piFreshTaskWorkImplSHA  = "228eeb89a4bb39ea1715e74625a88b3131763ae1566975d8efce4009a5bccb5f"
)

func PiFreshTaskWorkToolNames() []string { return strings.Split(piFreshTaskWorkTools, ",") }

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

func (a PiFreshTaskWorkAdapter) NewExecutor(ctx context.Context, selected Provider, binding FreshTaskWorkBinding) (Executor, error) {
	names := PiFreshTaskWorkToolNames()
	if ctx.Err() != nil || selected.Name != Pi || !executionID.MatchString(binding.RunID) || !slices.Equal(binding.Operations, names) {
		return nil, errPiMemberToolConfiguration
	}
	baseline, err := NewExecutor(selected, a.timeout)
	if err != nil {
		return nil, err
	}
	bridge := a.qualifiedBridge()
	if bridge == "" || binding.ConfigSHA256 == ([sha256.Size]byte{}) {
		return nil, errPiMemberToolConfiguration
	}
	config, err := readMemberToolConfig(binding.ConfigPath)
	if err != nil || config.digest != binding.ConfigSHA256 {
		return nil, errPiMemberToolConfiguration
	}
	if FreshTaskWorkConfigSHA256(config.endpoint, config.token) != config.digest || ctx.Err() != nil {
		return nil, errPiMemberToolConfiguration
	}
	pi := baseline.(piExecutor)
	pi.memberTools = &piMemberToolOptions{extensionPath: bridge, configPath: filepath.Clean(binding.ConfigPath),
		toolNames: names, configSHA256: binding.ConfigSHA256}
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
	if e.used.Swap(true) || ctx.Err() != nil || request.RunID != e.runID || e.adapter.qualifiedBridge() != opts.extensionPath {
		return unknown
	}
	config, err := readMemberToolConfig(opts.configPath)
	if err != nil || config.digest != opts.configSHA256 || !os.SameFile(e.configFile, config.file) || ctx.Err() != nil {
		return unknown
	}
	// Ordinary Pi argv construction already pins the fresh session ID and
	// exact allowlist. Only the child receives the issued config digest.
	return e.pi.Execute(ctx, request, onEvent)
}

func (a PiFreshTaskWorkAdapter) qualifiedBridge() string {
	if !filepath.IsAbs(a.extensionPath) {
		return ""
	}
	// Pass the physical entry so the loader resolves the same relative import.
	entry, err := filepath.EvalSymlinks(a.extensionPath)
	if err != nil || filepath.Base(entry) != "fresh-task-work.ts" || !freshTaskWorkBridgeBytesMatch(entry, piFreshTaskWorkEntrySHA) ||
		!freshTaskWorkBridgeBytesMatch(filepath.Join(filepath.Dir(entry), "fresh-task-work.mjs"), piFreshTaskWorkImplSHA) {
		return ""
	}
	return entry
}

func freshTaskWorkBridgeBytesMatch(path, digest string) bool {
	const maxSize = 64 << 10
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0444 == 0 || info.Size() > maxSize {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSize+1))
	sum := sha256.Sum256(data)
	return err == nil && len(data) <= maxSize && hex.EncodeToString(sum[:]) == digest
}
