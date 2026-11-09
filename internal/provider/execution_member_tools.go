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
// member tools in canonical order. Names never come from model or HTTP input.
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
	if len(toolNames) == 0 {
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

func piMemberToolEnvironment(inherited []string, configPath string) []string {
	env := make([]string, 0, len(inherited)+1)
	for _, value := range inherited {
		if !strings.HasPrefix(value, piMemberToolConfigEnv+"=") {
			env = append(env, value)
		}
	}
	return append(env, piMemberToolConfigEnv+"="+configPath)
}
