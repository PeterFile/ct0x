package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const piMemberToolFrameLimit = 1 << 20
const piMemberToolConfigEnv = "AGENT_WORKSPACE_MEMBER_TOOL_CONFIG"
const piMemberToolConfigSHAEnv = "AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256"
const piMemberToolNames = "list_members,read_channel,send_message,create_member"
const workspaceWorkerToolNames = "read_channel,search_history,read_history,submit_result,workspace_list,workspace_read,workspace_write,publish_workspace_artifact"

// WorkspaceWorkerToolNames is the complete server-selected file-worker profile.
// Names describe implementation support, never filesystem or task authority.
func WorkspaceWorkerToolNames() []string {
	return strings.Split(workspaceWorkerToolNames, ",")
}

var errPiMemberToolConfiguration = errors.New("成员工具执行配置无效")

type piMemberToolOptions struct {
	// Trusted only from the exact pinned fresh typed adapters, never inferred
	// from tool names, ordinary digest options, the child stream or the prompt.
	typedWorkRole string

	extensionPath string
	configPath    string
	toolNames     []string
	configSHA256  [sha256.Size]byte // Nonzero only for a runtime-bound profile.
	investigation bool
}

// NewPiMemberToolExecutor opts into four native member tools on verified Pi 1.4.0.
// It reuses NewExecutor's metadata/path/version gate without probing or changing
// selected. The caller must keep the trusted extension, config and their ancestors
// immutable for the executor's lifetime. Config must be a private 0600 JSON file
// containing endpoint, token and an optional issued continuation schema hint.
// Only the config path enters the child environment; no token is issued here.
// The trusted extension is not a host filesystem sandbox. Tools can perform only
// server-authorized platform operations. Default Pi executors remain tool-disabled.
func NewPiMemberToolExecutor(selected Provider, extensionPath, configPath string, timeout time.Duration) (Executor, error) {
	return NewPiMemberToolExecutorWithTools(selected, extensionPath, configPath, strings.Split(piMemberToolNames, ","), timeout)
}

// NewPiMemberToolExecutorWithTools freezes a trusted, nonempty subset of the six
// ordinary tools or a complete role profile in canonical order. Worker profiles
// can include issued work-input operations. Names do not grant authority.
// It preserves the legacy constructor's file/version gates and does not launch Pi.
func NewPiMemberToolExecutorWithTools(selected Provider, extensionPath, configPath string, toolNames []string, timeout time.Duration) (Executor, error) {
	native, err := newPiMemberToolExecutor(selected, extensionPath, configPath, toolNames, timeout, false)
	if err != nil {
		return nil, err
	}
	return native, nil
}

func newPiMemberToolExecutor(selected Provider, extensionPath, configPath string, toolNames []string, timeout time.Duration, investigation bool) (piExecutor, error) {
	if selected.Name != Pi {
		return piExecutor{}, errExecutorConfiguration
	}
	baseline, err := NewExecutor(selected, timeout)
	if err != nil {
		return piExecutor{}, err
	}
	var names []string
	if investigation {
		names, err = piInvestigationProfile(toolNames)
	} else {
		names, err = canonicalMemberToolProfile(toolNames, true)
	}
	if err != nil {
		return piExecutor{}, err
	}
	if !filepath.IsAbs(extensionPath) || strings.ContainsRune(extensionPath, 0) ||
		!filepath.IsAbs(configPath) || strings.ContainsRune(configPath, 0) {
		return piExecutor{}, errPiMemberToolConfiguration
	}
	info, err := os.Stat(extensionPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0444 == 0 {
		return piExecutor{}, errPiMemberToolConfiguration
	}
	extension, err := os.Open(extensionPath)
	if err != nil {
		return piExecutor{}, errPiMemberToolConfiguration
	}
	if extension.Close() != nil {
		return piExecutor{}, errPiMemberToolConfiguration
	}
	// Clean first so a trailing separator cannot hide a final symlink from Lstat.
	info, err = os.Lstat(filepath.Clean(configPath))
	if err != nil || !info.Mode().IsRegular() || info.Mode() != 0600 {
		return piExecutor{}, errPiMemberToolConfiguration
	}
	e := baseline.(piExecutor)
	e.memberTools = &piMemberToolOptions{extensionPath: extensionPath, configPath: configPath, toolNames: names, investigation: investigation}
	return e, nil
}

type piWorkspaceMemberToolAdapter struct {
	extensionPath, configPath string
	toolNames                 []string
}

// NewPiWorkspaceMemberToolAdapter is a separate verified file profile, not an
// upgrade of text-only SupportSupported evidence. A blank config samples only.
func NewPiWorkspaceMemberToolAdapter(extensionPath, configPath string, workInputs ...string) Adapter {
	return piWorkspaceMemberToolAdapter{extensionPath: extensionPath, configPath: configPath,
		toolNames: append(WorkspaceWorkerToolNames(), workInputs...)}
}

func (a piWorkspaceMemberToolAdapter) Capabilities(selected Provider) (AdapterCapabilities, error) {
	if _, err := canonicalMemberToolProfile(a.toolNames, true); err != nil {
		return AdapterCapabilities{}, err
	}
	// Reuse the exact executable/version/extension gates, not a model probe.
	return (memberToolAdapter{name: Pi, extensionPath: a.extensionPath,
		toolNames: []string{"read_channel", "search_history", "read_history", "submit_result"}}).Capabilities(selected)
}

func (a piWorkspaceMemberToolAdapter) NewExecutor(selected Provider, timeout time.Duration) (Executor, error) {
	if timeout < 0 {
		return nil, errExecutorConfiguration
	}
	if _, err := a.Capabilities(selected); err != nil {
		return nil, err
	}
	if _, err := readMemberToolConfig(a.configPath); err != nil {
		return nil, err
	}
	return NewPiMemberToolExecutorWithTools(selected, a.extensionPath, filepath.Clean(a.configPath), a.toolNames, timeout)
}

func (o *piMemberToolOptions) environment(inherited []string) []string {
	env := make([]string, 0, len(inherited)+2)
	for _, value := range inherited {
		if !strings.HasPrefix(value, piMemberToolConfigEnv+"=") && !strings.HasPrefix(value, piMemberToolConfigSHAEnv+"=") {
			env = append(env, value)
		}
	}
	env = append(env, piMemberToolConfigEnv+"="+o.configPath)
	if o.boundConfig() {
		env = append(env, piMemberToolConfigSHAEnv+"="+hex.EncodeToString(o.configSHA256[:]))
	}
	return env
}

func (o *piMemberToolOptions) boundConfig() bool {
	return o != nil && o.configSHA256 != ([sha256.Size]byte{})
}
