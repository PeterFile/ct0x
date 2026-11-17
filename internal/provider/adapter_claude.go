package provider

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	ClaudeSDKAdapterID           = "claude_sdk"
	ClaudeSDKProtocol   Protocol = "claude_sdk_stdio_v1"
	ClaudeSDKVersion             = "0.3.326"
	ClaudeNativeVersion          = "2.1.326"
)

var errClaudeSDKConfiguration = errors.New("Claude SDK 受限执行配置无效或版本未经确认")

// ClaudeSDKOptions is trusted host configuration, never model input. ConfigPath
// names the current run's existing private endpoint/token file. All ancestors
// and selected files must remain trusted and immutable for the executor lifetime.
// BridgePath points to the built official-SDK entry, not an arbitrary SDK module.
type ClaudeSDKOptions struct {
	NodePath   string
	BridgePath string
	ConfigPath string
	ToolNames  []string
}

type claudeSDKAdapter struct{ options ClaudeSDKOptions }

// NewClaudeSDKAdapter is experimental and CLOSED. It neither registers itself
// nor advertises any verified native capability. Managed startup isolation and
// native execution need separately authorized root acceptance before activation.
func NewClaudeSDKAdapter(options ClaudeSDKOptions) Adapter {
	options.ToolNames = append([]string(nil), options.ToolNames...)
	return claudeSDKAdapter{options}
}

func (a claudeSDKAdapter) Capabilities(selected Provider) (AdapterCapabilities, error) {
	if _, err := validateClaudeSelection(selected); err != nil {
		return AdapterCapabilities{}, err
	}
	if _, err := memberToolProfile(a.options.ToolNames); err != nil {
		return AdapterCapabilities{}, errClaudeSDKConfiguration
	}
	for _, file := range []struct {
		path       string
		executable bool
	}{{a.options.NodePath, true}, {a.options.BridgePath, false}} {
		if _, _, err := claudeTrustedFile(file.path, file.executable); err != nil {
			return AdapterCapabilities{}, err
		}
	}
	return AdapterCapabilities{
		PlatformTools: SupportUnknown, TerminalEvidence: SupportUnknown,
		PermissionInteraction: SupportUnknown, Cancel: SupportUnknown,
		NativeResume: SupportUnsupported, Steer: SupportUnsupported,
		Evidence: CapabilityEvidence{Source: BuiltinContractEvidence, ProviderID: selected.ID, Name: selected.Name,
			Path: selected.Path, Version: selected.Version, Protocol: ClaudeSDKProtocol},
	}, nil
}

func (a claudeSDKAdapter) NewExecutor(selected Provider, timeout time.Duration) (Executor, error) {
	return NewClaudeSDKExecutor(selected, a.options, timeout)
}

func validateClaudeSelection(selected Provider) (os.FileInfo, error) {
	if selected.Name != "claude" || selected.Version != ClaudeNativeVersion || strings.TrimSpace(selected.ID) == "" || selected.Installation != Installed {
		return nil, errClaudeSDKConfiguration
	}
	_, info, err := claudeTrustedFile(selected.Path, true)
	return info, err
}

// Metadata observes the trusted installation, not a version/auth/model probe.
// Physical paths plus file identity/size/mtime checks detect ordinary drift, not
// a hostile same-UID ancestor or an ABA replacement. This is not an OS sandbox.
func claudeTrustedFile(path string, executable bool) (string, os.FileInfo, error) {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return "", nil, errClaudeSDKConfiguration
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, errClaudeSDKConfiguration
	}
	info, err := os.Stat(real)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0444 == 0 || executable && info.Mode().Perm()&0111 == 0 {
		return "", nil, errClaudeSDKConfiguration
	}
	return real, info, nil
}
