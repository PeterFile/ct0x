package provider

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const piMemberToolFrameLimit = 1 << 20
const piMemberToolConfigEnv = "AGENT_WORKSPACE_MEMBER_TOOL_CONFIG"
const piMemberToolNames = "list_members,read_channel,send_message,create_member"
const workspaceWorkerToolNames = "read_channel,search_history,read_history,submit_result,workspace_list,workspace_read,workspace_write,publish_workspace_artifact"

// WorkspaceWorkerToolNames is the complete server-selected file-worker profile.
// Names describe implementation support, never filesystem or task authority.
func WorkspaceWorkerToolNames() []string {
	return strings.Split(workspaceWorkerToolNames, ",")
}

var errPiMemberToolConfiguration = errors.New("成员工具执行配置无效")

type piMemberToolOptions struct {
	extensionPath string
	configPath    string
	toolNames     []string
}

// NewPiMemberToolExecutor opts into four native member tools on verified Pi 1.4.0.
// It reuses NewExecutor's metadata/path/version gate without probing or changing
// selected. The caller must keep the trusted extension, config and their ancestors
// immutable for the executor's lifetime. Config must be a private 0600 JSON file
// containing exactly endpoint and token; only the bridge reads/validates its bytes.
// Only the config path enters the child environment; no token is issued here.
// The trusted extension is not a host filesystem sandbox. Tools can perform only
// server-authorized platform operations. Default Pi executors remain tool-disabled.
func NewPiMemberToolExecutor(selected Provider, extensionPath, configPath string, timeout time.Duration) (Executor, error) {
	return NewPiMemberToolExecutorWithTools(selected, extensionPath, configPath, strings.Split(piMemberToolNames, ","), timeout)
}

// NewPiMemberToolExecutorWithTools freezes a trusted, nonempty subset of the six
// ordinary member tools or a complete coordinator/text-worker/file-worker profile in canonical
// order. Names never come from model or HTTP input and do not grant authority.
// It preserves the legacy constructor's file/version gates and does not launch Pi.
func NewPiMemberToolExecutorWithTools(selected Provider, extensionPath, configPath string, toolNames []string, timeout time.Duration) (Executor, error) {
	if selected.Name != Pi {
		return nil, errExecutorConfiguration
	}
	baseline, err := NewExecutor(selected, timeout)
	if err != nil {
		return nil, err
	}
	canonical := strings.Split(piMemberToolNames+",search_history,read_history", ",")
	requireAll := false
	switch {
	case slices.Contains(toolNames, "workspace_write"):
		canonical = WorkspaceWorkerToolNames()
		requireAll = true
	case slices.Contains(toolNames, "decide_dispatch"):
		canonical = []string{"list_members", "read_channel", "search_history", "read_history", "decide_dispatch"}
		requireAll = true
	case slices.Contains(toolNames, "submit_result"):
		canonical = []string{"read_channel", "search_history", "read_history", "submit_result"}
		requireAll = true
	}
	if len(toolNames) == 0 || requireAll && len(toolNames) != len(canonical) {
		return nil, errPiMemberToolConfiguration
	}
	for i, name := range toolNames {
		if !slices.Contains(canonical, name) || slices.Contains(toolNames[:i], name) {
			return nil, errPiMemberToolConfiguration
		}
	}
	names := make([]string, 0, len(toolNames))
	for _, name := range canonical {
		if slices.Contains(toolNames, name) {
			names = append(names, name)
		}
	}
	if !filepath.IsAbs(extensionPath) || strings.ContainsRune(extensionPath, 0) ||
		!filepath.IsAbs(configPath) || strings.ContainsRune(configPath, 0) {
		return nil, errPiMemberToolConfiguration
	}
	info, err := os.Stat(extensionPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0444 == 0 {
		return nil, errPiMemberToolConfiguration
	}
	extension, err := os.Open(extensionPath)
	if err != nil {
		return nil, errPiMemberToolConfiguration
	}
	if extension.Close() != nil {
		return nil, errPiMemberToolConfiguration
	}
	// Clean first so a trailing separator cannot hide a final symlink from Lstat.
	info, err = os.Lstat(filepath.Clean(configPath))
	if err != nil || !info.Mode().IsRegular() || info.Mode() != 0600 {
		return nil, errPiMemberToolConfiguration
	}
	e := baseline.(piExecutor)
	e.memberTools = &piMemberToolOptions{extensionPath: extensionPath, configPath: configPath, toolNames: names}
	return e, nil
}

type piWorkspaceMemberToolAdapter struct {
	extensionPath, configPath string
}

// NewPiWorkspaceMemberToolAdapter is a separate verified file profile, not an
// upgrade of text-only SupportSupported evidence. A blank config samples only.
// Codex's experimental text/readOnly selector and allowlist stay unchanged.
func NewPiWorkspaceMemberToolAdapter(extensionPath, configPath string) Adapter {
	return piWorkspaceMemberToolAdapter{extensionPath: extensionPath, configPath: configPath}
}

func (a piWorkspaceMemberToolAdapter) Capabilities(selected Provider) (AdapterCapabilities, error) {
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
	return NewPiMemberToolExecutorWithTools(selected, a.extensionPath, filepath.Clean(a.configPath), WorkspaceWorkerToolNames(), timeout)
}

func piMemberToolEnvironment(inherited []string, configPath string) []string {
	env := make([]string, 0, len(inherited)+1)
	for _, value := range inherited {
		if !strings.HasPrefix(value, piMemberToolConfigEnv+"=") {
			env = append(env, value)
		}
	}
	return append(env, piMemberToolConfigEnv+"="+configPath)
}
