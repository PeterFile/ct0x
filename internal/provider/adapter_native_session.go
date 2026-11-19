package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"slices"
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

// CodexNativeTextSessionProfileSupported checks declared metadata only, not
// installation, account readiness, authority, or eligibility of a reference.
func CodexNativeTextSessionProfileSupported(selected Provider, model, thinking string) bool {
	return selected.Name == Codex && nativeTextSessionProviderSupported(selected) &&
		utf8.ValidString(model) && codexAppServerIdentity(model) && thinking == ""
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

// Keep #193's exact pair eligible for its existing native text profile.
// Only the investigation releases understand the strict config. Each entry
// point is pinned to its matching parser.
const (
	piDigestBridgeEntrySHA        = "321fd2bc30c71322a140f5e022d96b2c005e90c0b5b4468697b59978decc8356"
	piDigestBridgeImplSHA         = "f983651f525fb411d9e8f50843b15447558f6d6b1f9f1a13563d96f3528a3061"
	piInvestigationBridgeEntrySHA = "84cca2d9f0ac4125a917841df4b15e62768269218a6d0e5f968e93c7bc6b71d2"
	piInvestigationBridgeImplSHA  = "c6e571fbeed7b6f78ea1a8b7b044babca691923297d1803a3aaaeb5b6c6eeb2b"
	// #200 used the same parser and tool contracts, with older descriptions.
	piInvestigationBridgeEntry200SHA = "39d5528abee0b6b39d0088d265dbe4d51c9c00eb9f43fe1d4d6be9d2383a7a82"
	piDiscussionRelayBridgeEntrySHA  = "a9178ae8ad54ea8ce834c9594dbbf407222be70f60ccb35d45506cb7f9855737"
	piDiscussionRelayBridgeImplSHA   = "17c987936c185fd11ea8c5c26855aa15c9707d393fac1152c1bbb2d1eaa035d4"
)

// PiNativeSessionBridgeSupported qualifies only the actual trusted native release
// pair. It never reads declarations from the extension, or probes the CLI.
func PiNativeSessionBridgeSupported(extensionPath string) bool {
	return qualifyPiNativeBridge(extensionPath) != ""
}

func qualifyPiNativeBridge(extensionPath string) string {
	if entry := qualifyPiInvestigationBridge(extensionPath); entry != "" {
		return entry
	}
	return qualifyPiBridgePair(extensionPath, piDigestBridgeImplSHA, piDigestBridgeEntrySHA)
}

func PiInvestigationBridgeSupported(extensionPath string) bool {
	return qualifyPiInvestigationBridge(extensionPath) != ""
}

// PiCombinedIntakeBridgeSupported requires the pair whose tool descriptions
// distinguish investigation from fresh Task Work continuation.
func PiCombinedIntakeBridgeSupported(extensionPath string) bool {
	return PiDiscussionRelayBridgeSupported(extensionPath) || qualifyPiBridgePair(extensionPath, piInvestigationBridgeImplSHA, piInvestigationBridgeEntrySHA) != ""
}

// PiDiscussionRelayBridgeSupported qualifies the installed relay-aware pair,
// not the per-turn permission to use it.
func PiDiscussionRelayBridgeSupported(extensionPath string) bool {
	return qualifyPiDiscussionRelayBridge(extensionPath) != ""
}

func qualifyPiDiscussionRelayBridge(extensionPath string) string {
	return qualifyPiBridgePair(extensionPath, piDiscussionRelayBridgeImplSHA, piDiscussionRelayBridgeEntrySHA)
}

func qualifyPiInvestigationBridge(extensionPath string) string {
	if entry := qualifyPiDiscussionRelayBridge(extensionPath); entry != "" {
		return entry
	}
	return qualifyPiBridgePair(extensionPath, piInvestigationBridgeImplSHA, piInvestigationBridgeEntrySHA, piInvestigationBridgeEntry200SHA)
}

func qualifyPiBridgePair(extensionPath, implSHA string, entrySHAs ...string) string {
	if !filepath.IsAbs(extensionPath) {
		return ""
	}
	// Pass the physical entry to Pi so the loader and this check agree on the
	// base of the relative import, even with a configured symlink alias.
	entry, err := filepath.EvalSymlinks(extensionPath)
	if err != nil || filepath.Base(entry) != "index.ts" || !piBridgeBytesMatch(entry, entrySHAs...) ||
		!piBridgeBytesMatch(filepath.Join(filepath.Dir(entry), "bridge.mjs"), implSHA) {
		return ""
	}
	return entry
}

func piBridgeBytesMatch(path string, digests ...string) bool {
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
	return err == nil && len(data) <= maxSize && slices.Contains(digests, hex.EncodeToString(sum[:]))
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

// NewCodexAppServerNativeSessionMemberToolAdapter constructs the explicit
// native text path. Capability sampling does not validate a claim or a reference.
// Execution requires the current issued config digest and, on resume, a qualified
// reference supplied by the caller. This does not register or enable the path.
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
		config, err := readPiMemberToolConfig(path, false)
		if err != nil || config.digest != a.configSHA256 || config.discussionRelay {
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
	configuredBridge    string
	investigationBridge bool
	discussionRelay     bool
}

func (e piDigestBoundExecutor) Execute(ctx context.Context, request ExecuteRequest, onEvent func(Event) error) ExecutionReceipt {
	// Reconfirm both bytes and the exact import location at invocation. The
	// selected CLI/loader, its environment and the trusted extension ancestry
	// must still be stable through load; this is not a same-UID code sandbox.
	qualify := qualifyPiNativeBridge
	if e.discussionRelay {
		qualify = qualifyPiDiscussionRelayBridge
	} else if e.investigationBridge {
		qualify = qualifyPiInvestigationBridge
	}
	if qualify(e.configuredBridge) != e.memberTools.extensionPath {
		return ExecutionReceipt{Outcome: OutcomeUnknown, ExitCode: -1, Reason: "成员工具扩展无法确认，未调用进程，需对账"}
	}
	return e.piExecutor.Execute(ctx, request, onEvent)
}

func piMemberToolConfigMatches(path string, digest [sha256.Size]byte, investigation bool) bool {
	config, err := readPiMemberToolConfig(path, investigation)
	return err == nil && config.digest == digest
}
