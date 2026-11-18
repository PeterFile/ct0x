package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Native profile versions are distinct from the registry transport protocol.
const (
	CodexNativeSessionAdapterVersion  = codexSessionAdapter
	CodexNativeSessionProtocolVersion = codexSessionProtocol
	PiNativeSessionAdapterVersion     = piSessionAdapter
	PiNativeSessionProtocolVersion    = piSessionProtocol
)

// PiNativeTextSessionProfileSupported checks declared Pi metadata only, not
// account readiness, authority, or eligibility of any historical reference.
func PiNativeTextSessionProfileSupported(selected Provider, model, thinking string) bool {
	return selected.Name == Pi && nativeTextSessionProviderSupported(selected) &&
		utf8.ValidString(model) && len(model) <= 256 && piSessionModel.MatchString(model) && validPiThinking(thinking)
}

func nativeTextSessionProviderSupported(selected Provider) bool {
	return utf8.ValidString(selected.Path) && strings.TrimSpace(selected.Path) == selected.Path && nativeSessionPath(selected.Path) &&
		(selected.Name == Pi && selected.Version == "1.4.0" || selected.Name == Codex && selected.Version == "0.166.0")
}

// NativeMemberToolConfig carries issuance facts across the trusted builder only.
// It is not a run profile, receipt or model argument. A zero value samples capabilities only.
type NativeMemberToolConfig struct {
	Path   string            `json:"-"`
	SHA256 [sha256.Size]byte `json:"-"`
}

// This release pair is the audited index.ts (passes the digest before registering
// tools) and its exact ./bridge.mjs (reads, verifies and consumes the same bytes).
// Refresh both together after review and offline loader/config tests:
// shasum -a 256 extensions/pi-bridge/{index.ts,bridge.mjs}.
const (
	piDigestBridgeEntrySHA = "321fd2bc30c71322a140f5e022d96b2c005e90c0b5b4468697b59978decc8356"
	piDigestBridgeImplSHA  = "f983651f525fb411d9e8f50843b15447558f6d6b1f9f1a13563d96f3528a3061"
)

// PiNativeSessionBridgeSupported qualifies only the actual trusted native release
// pair. It never reads declarations from the extension, or probes the CLI.
func PiNativeSessionBridgeSupported(extensionPath string) bool {
	return qualifyPiNativeBridge(extensionPath) != ""
}

func qualifyPiNativeBridge(extensionPath string) string {
	if !filepath.IsAbs(extensionPath) {
		return ""
	}
	// Pass the physical entry to Pi so the loader and this check agree on the
	// base of the relative import, even with a configured symlink alias.
	entry, err := filepath.EvalSymlinks(extensionPath)
	if err != nil || filepath.Base(entry) != "index.ts" || !piBridgeBytesMatch(entry, piDigestBridgeEntrySHA) ||
		!piBridgeBytesMatch(filepath.Join(filepath.Dir(entry), "bridge.mjs"), piDigestBridgeImplSHA) {
		return ""
	}
	return entry
}

func piBridgeBytesMatch(path, digest string) bool {
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

// NewPiNativeSessionMemberToolAdapter is experimental and opt-in. It supports
// text tool profiles only, with exact model, explicit thinking and a qualified
// digest bridge. Legacy member tool execution does not require this bridge pair.
func NewPiNativeSessionMemberToolAdapter(extensionPath string, config NativeMemberToolConfig, toolNames []string, options PiSessionOptions) Adapter {
	return nativeSessionMemberToolAdapter{
		memberToolAdapter: NewPiMemberToolAdapter(extensionPath, config.Path, toolNames).(memberToolAdapter),
		configSHA256:      config.SHA256,
		pi:                &PiSessionOptions{Resume: cloneNativeSessionReference(options.Resume), Binding: bytes.Clone(options.Binding)},
	}
}

// NewCodexAppServerNativeSessionMemberToolAdapter is experimental and used only
// in offline tests. The service selects the fresh-only AppServer adapter.
func NewCodexAppServerNativeSessionMemberToolAdapter(config NativeMemberToolConfig, toolNames []string, options CodexAppServerSessionOptions) Adapter {
	return nativeSessionMemberToolAdapter{
		memberToolAdapter: NewCodexAppServerMemberToolAdapter(config.Path, toolNames).(memberToolAdapter),
		configSHA256:      config.SHA256,
		codex:             &CodexAppServerSessionOptions{Resume: cloneNativeSessionReference(options.Resume), Binding: bytes.Clone(options.Binding)},
	}
}

type nativeSessionMemberToolAdapter struct {
	memberToolAdapter
	configSHA256 [sha256.Size]byte
	pi           *PiSessionOptions
	codex        *CodexAppServerSessionOptions
}

func (a nativeSessionMemberToolAdapter) Capabilities(selected Provider) (AdapterCapabilities, error) {
	bridge := ""
	if a.pi != nil {
		bridge = qualifyPiNativeBridge(a.extensionPath)
	}
	return a.capabilities(selected, bridge)
}

func (a nativeSessionMemberToolAdapter) capabilities(selected Provider, bridge string) (AdapterCapabilities, error) {
	if !nativeTextSessionProviderSupported(selected) {
		return AdapterCapabilities{}, errExecutorConfiguration
	}
	caps, err := a.memberToolAdapter.Capabilities(selected)
	if err != nil {
		return AdapterCapabilities{}, err
	}
	if a.pi != nil && bridge == "" {
		return AdapterCapabilities{}, errPiMemberToolConfiguration
	}
	caps.NativeResume = SupportSupported
	return caps, nil
}

func (a nativeSessionMemberToolAdapter) NewExecutor(selected Provider, timeout time.Duration) (Executor, error) {
	if timeout < 0 {
		return nil, errExecutorConfiguration
	}
	bridge := ""
	if a.pi != nil {
		bridge = qualifyPiNativeBridge(a.extensionPath)
	}
	if _, err := a.capabilities(selected, bridge); err != nil {
		return nil, err
	}
	if a.configSHA256 == ([sha256.Size]byte{}) {
		return nil, errPiMemberToolConfiguration
	}
	if a.pi != nil {
		path := filepath.Clean(a.configPath)
		if !memberToolConfigMatches(path, a.configSHA256) {
			return nil, errPiMemberToolConfiguration
		}
		native, err := NewPiSessionExecutor(selected, bridge, path, a.toolNames, *a.pi, timeout)
		if err != nil {
			return nil, err
		}
		// The bridge reopens the path; it must verify its own bytes before use.
		pi := native.(piExecutor)
		pi.memberTools.configSHA256 = a.configSHA256
		return piDigestBoundExecutor{piExecutor: pi, configuredBridge: a.extensionPath}, nil
	}
	native, err := NewCodexAppServerSessionExecutor(selected, a.configPath, a.toolNames, *a.codex, timeout)
	if err != nil {
		return nil, err
	}
	// Codex already verifies this constructor snapshot again in Execute and
	// uses its validated bytes to form the child transport, without a reopen.
	if native.(codexAppServerMemberToolExecutor).digest != a.configSHA256 {
		return nil, errPiMemberToolConfiguration
	}
	return native, nil
}

type piDigestBoundExecutor struct {
	piExecutor
	configuredBridge string
}

func (e piDigestBoundExecutor) Execute(ctx context.Context, request ExecuteRequest, onEvent func(Event) error) ExecutionReceipt {
	// Reconfirm both bytes and the exact import location at invocation. The
	// selected CLI/loader, its environment and the trusted extension ancestry
	// must still be stable through load; this is not a same-UID code sandbox.
	if qualifyPiNativeBridge(e.configuredBridge) != e.memberTools.extensionPath {
		return ExecutionReceipt{Outcome: OutcomeUnknown, ExitCode: -1, Reason: "原生会话成员工具扩展无法确认，未调用进程，需对账"}
	}
	return e.piExecutor.Execute(ctx, request, onEvent)
}

func memberToolConfigMatches(path string, digest [sha256.Size]byte) bool {
	config, err := readMemberToolConfig(path)
	return err == nil && config.digest == digest
}
