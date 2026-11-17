package provider

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	PiMemberToolAdapterID   = "pi_member_tools"
	CodexAppServerAdapterID = "codex_app_server"
)

type memberToolAdapter struct {
	name          string
	extensionPath string
	configPath    string
	toolNames     []string
}

// NewPiMemberToolAdapter selects the existing verified Pi 1.4.0 implementation.
// A blank configPath permits capability sampling only, never execution. Paths,
// extension and config ancestors must remain trusted and stable for each run.
func NewPiMemberToolAdapter(extensionPath, configPath string, toolNames []string) Adapter {
	return memberToolAdapter{name: Pi, extensionPath: extensionPath, configPath: configPath, toolNames: append([]string(nil), toolNames...)}
}

// NewCodexAppServerMemberToolAdapter is experimental and explicitly opt-in for
// verified Codex 0.166.0 text member tools in a restricted read-only native
// profile. It does not alter the default registry, authorize file tasks, or
// promise directory-read isolation or native resume.
func NewCodexAppServerMemberToolAdapter(configPath string, toolNames []string) Adapter {
	return memberToolAdapter{name: Codex, configPath: configPath, toolNames: append([]string(nil), toolNames...)}
}

func (a memberToolAdapter) Capabilities(selected Provider) (AdapterCapabilities, error) {
	if selected.Name != a.name || a.name == Codex && selected.Version != "0.166.0" {
		return AdapterCapabilities{}, errExecutorConfiguration
	}
	if err := ValidateExecutor(selected, 0); err != nil {
		return AdapterCapabilities{}, err
	}
	if _, err := memberToolProfile(a.toolNames); err != nil {
		return AdapterCapabilities{}, err
	}
	protocol := CodexAppServerProtocol
	if a.name == Pi {
		protocol = PiExtensionProtocol
		if !filepath.IsAbs(a.extensionPath) || strings.ContainsRune(a.extensionPath, 0) {
			return AdapterCapabilities{}, errPiMemberToolConfiguration
		}
		info, err := os.Stat(a.extensionPath)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0444 == 0 {
			return AdapterCapabilities{}, errPiMemberToolConfiguration
		}
		file, err := os.Open(a.extensionPath)
		if err != nil {
			return AdapterCapabilities{}, errPiMemberToolConfiguration
		}
		if file.Close() != nil {
			return AdapterCapabilities{}, errPiMemberToolConfiguration
		}
	}
	return AdapterCapabilities{
		PlatformTools: SupportSupported, TerminalEvidence: SupportSupported,
		NativeResume: SupportUnsupported, Steer: SupportUnsupported,
		PermissionInteraction: SupportUnsupported, Cancel: SupportUnsupported,
		Evidence: CapabilityEvidence{Source: BuiltinContractEvidence, ProviderID: selected.ID, Name: selected.Name,
			Path: selected.Path, Version: selected.Version, Protocol: protocol},
	}, nil
}

func (a memberToolAdapter) NewExecutor(selected Provider, timeout time.Duration) (Executor, error) {
	if timeout < 0 {
		return nil, errExecutorConfiguration
	}
	if _, err := a.Capabilities(selected); err != nil {
		return nil, err
	}
	if a.name == Pi {
		if _, err := readMemberToolConfig(a.configPath); err != nil {
			return nil, err
		}
		return NewPiMemberToolExecutorWithTools(selected, a.extensionPath, filepath.Clean(a.configPath), a.toolNames, timeout)
	}
	return NewCodexAppServerMemberToolExecutor(selected, a.configPath, a.toolNames, timeout)
}
