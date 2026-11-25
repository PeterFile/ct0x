package provider

import (
	"os"
	"strings"
	"time"
)

// ACPV1Protocol names stable-v1 stdio, not an enabled registry integration.
const (
	ACPV1Protocol   Protocol = "acp_v1"
	AgyACPAdapterID          = "agy_acp"
)

type ACPVendorProfile string

const (
	ACPGemini068   ACPVendorProfile = "gemini_0_68"
	ACPOpenCode119 ACPVendorProfile = "opencode_1_19"
	ACPAgy15       ACPVendorProfile = "agy_1_5"
)

// ACPOpenCodeProfile pins a caller-prepared private dependency directory D,
// whose basename must be opencode. Child XDG_CONFIG_HOME points to D's parent;
// the cache is private per run. HOME/XDG_DATA_HOME are not redirected.
type ACPOpenCodeProfile struct {
	ConfigDir  string
	LockSHA256 string
}

// ACPGeminiContextFile pins one host-selected instruction file, not a file grant.
type ACPGeminiContextFile struct {
	Path   string
	SHA256 string
}

// ACPGeminiContextManifest describes the complete ambient instruction closure
// for one canonical CWD and installed version. Active imports are not supported.
type ACPGeminiContextManifest struct {
	Version string
	CWD     string
	Files   []ACPGeminiContextFile
}

// ACPConfig is trusted, immutable host configuration, never model input.
// This experimental integration is not registered by default. File/command/
// subagent/background tasks are not authorized by a text profile or fs=false.
type ACPConfig struct {
	AgentPath            string
	Version              string
	VendorProfile        ACPVendorProfile
	Model                string
	MemberToolConfigPath string
	ToolNames            []string
	OpenCode             *ACPOpenCodeProfile
	GeminiContext        *ACPGeminiContextManifest
}

type acpAdapter struct{ config ACPConfig }

// NewACPAdapter does not probe the executable or read authentication. Empty
// MemberToolConfigPath allows capability sampling, not executor construction.
func NewACPAdapter(config ACPConfig) Adapter {
	config = cloneACPConfig(config)
	return acpAdapter{config: config}
}

func (a acpAdapter) Capabilities(p Provider) (AdapterCapabilities, error) {
	name := "gemini"
	if a.config.VendorProfile == ACPOpenCode119 {
		name = "opencode"
	}
	if p.ID == "" || p.Name != name || p.Path != a.config.AgentPath || p.Version != a.config.Version || p.Installation != Installed {
		return AdapterCapabilities{}, ErrAdapterConfiguration
	}
	if err := validateACPConfig(a.config); err != nil {
		return AdapterCapabilities{}, err
	}
	// Fixture evidence is implementation evidence, not native CLI acceptance.
	return AdapterCapabilities{PlatformTools: SupportUnknown, PermissionInteraction: SupportUnknown, Cancel: SupportUnknown, TerminalEvidence: SupportUnknown,
		NativeResume: SupportUnsupported, Steer: SupportUnsupported,
		Evidence: CapabilityEvidence{Source: BuiltinContractEvidence, ProviderID: p.ID, Name: p.Name, Path: p.Path, Version: p.Version, Protocol: ACPV1Protocol}}, nil
}

func (a acpAdapter) NewExecutor(p Provider, timeout time.Duration) (Executor, error) {
	if _, err := a.Capabilities(p); err != nil {
		return nil, err
	}
	return NewACPExecutor(a.config, timeout)
}

type agyACPAdapter struct {
	configPath string
	toolNames  []string
}

// NewAgyACPMemberToolAdapter is experimental and explicitly opt-in for
// verified Agy 1.5.x text member tools via ACP v1 stdio + session HTTP MCP.
// It uses per-run containment (sandbox, isolated --gemini-dir, strict
// settings, and PreToolUse hook whitelist) while preserving original HOME/keyring
// auth. Resume and live steering remain unsupported.
func NewAgyACPMemberToolAdapter(configPath string, toolNames []string) Adapter {
	return agyACPAdapter{configPath: configPath, toolNames: append([]string(nil), toolNames...)}
}

func (a agyACPAdapter) acpConfig(p Provider) ACPConfig {
	return ACPConfig{
		AgentPath: p.Path, Version: p.Version, VendorProfile: ACPAgy15,
		MemberToolConfigPath: a.configPath, ToolNames: append([]string(nil), a.toolNames...),
	}
}

func (a agyACPAdapter) Capabilities(p Provider) (AdapterCapabilities, error) {
	if p.ID == "" || p.Name != Agy || p.Installation != Installed || !acpAgySupportedVersion(p.Version) {
		return AdapterCapabilities{}, ErrAdapterConfiguration
	}
	cfg := a.acpConfig(p)
	if err := validateACPConfig(cfg); err != nil {
		return AdapterCapabilities{}, err
	}
	return AdapterCapabilities{
		PlatformTools: SupportSupported, TerminalEvidence: SupportSupported,
		NativeResume: SupportUnsupported, Steer: SupportUnsupported,
		PermissionInteraction: SupportUnknown, Cancel: SupportUnknown,
		Evidence: CapabilityEvidence{
			Source: BuiltinContractEvidence, ProviderID: p.ID, Name: p.Name,
			Path: p.Path, Version: p.Version, Protocol: ACPV1Protocol,
		},
	}, nil
}

func (a agyACPAdapter) NewExecutor(p Provider, timeout time.Duration) (Executor, error) {
	if _, err := a.Capabilities(p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.configPath) == "" {
		return nil, ErrAdapterConfiguration
	}
	info, err := os.Stat(a.configPath)
	if err != nil || !info.Mode().IsRegular() {
		return nil, ErrAdapterConfiguration
	}
	return NewACPExecutor(a.acpConfig(p), timeout)
}
