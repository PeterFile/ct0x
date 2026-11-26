package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"agentworkspace/internal/preflight"
)

const acpOpenCodeRestrictedReason = "OpenCode 模型目录、私有依赖、配置或匿名账户前提未经确认，受限执行未启动"

func cloneACPConfig(c ACPConfig) ACPConfig {
	c.ToolNames = append([]string(nil), c.ToolNames...)
	if c.freshTaskWork != nil {
		binding := *c.freshTaskWork
		binding.Operations = slices.Clone(binding.Operations)
		c.freshTaskWork = &binding
	}
	if c.GeminiContext != nil {
		manifest := *c.GeminiContext
		manifest.Files = append([]ACPGeminiContextFile(nil), manifest.Files...)
		c.GeminiContext = &manifest
	}
	if c.OpenCode != nil {
		profile := *c.OpenCode
		c.OpenCode = &profile
	}
	return c
}

func acpMemberToolProfile(c ACPConfig) ([]string, error) {
	if binding := c.freshTaskWork; binding != nil {
		if c.VendorProfile != ACPAgy15 || !slices.Equal(binding.Operations, PiFreshTaskWorkToolNames()) ||
			!slices.Equal(c.ToolNames, binding.Operations) || c.MemberToolConfigPath != binding.ConfigPath ||
			!executionID.MatchString(binding.RunID) {
			return nil, ErrAdapterConfiguration
		}
		return slices.Clone(c.ToolNames), nil
	}
	if c.VendorProfile == ACPAgy15 && validPiProjectExperienceToolNames(c.ToolNames) {
		return slices.Clone(c.ToolNames), nil
	}
	return memberToolProfile(c.ToolNames)
}

func readACPMemberToolConfig(ctx context.Context, c ACPConfig) (memberToolConfig, error) {
	if binding := c.freshTaskWork; binding != nil {
		config, err := readMemberToolConfigContext(ctx, c.MemberToolConfigPath)
		if err != nil {
			return memberToolConfig{}, err
		}
		if config.digest != binding.ConfigSHA256 || config.digest != FreshTaskWorkConfigSHA256(config.endpoint, config.token) {
			return memberToolConfig{}, errPiMemberToolConfiguration
		}
		return config, nil
	}
	if c.VendorProfile != ACPAgy15 || !slices.Contains(c.ToolNames, LookupProjectExperiencesTool) {
		return readMemberToolConfigContext(ctx, c.MemberToolConfigPath)
	}
	config, err := readMemberToolConfigModeContext(ctx, c.MemberToolConfigPath, false, false, true)
	if err != nil {
		return memberToolConfig{}, err
	}
	if !slices.Equal(config.projectExperienceTools, c.ToolNames) {
		return memberToolConfig{}, errPiMemberToolConfiguration
	}
	return config, nil
}

func validateACPConfig(c ACPConfig) error {
	if !filepath.IsAbs(c.AgentPath) || strings.ContainsRune(c.AgentPath, 0) {
		return ErrAdapterConfiguration
	}
	if c.VendorProfile == ACPAgy15 {
		if !acpAgySupportedVersion(c.Version) || c.OpenCode != nil || c.GeminiContext != nil {
			return ErrAdapterConfiguration
		}
		if c.Model != "" && !acpValidModel(c.Model) {
			return ErrAdapterConfiguration
		}
	} else {
		if !acpValidModel(c.Model) {
			return ErrAdapterConfiguration
		}
		switch c.VendorProfile {
		case ACPGemini068:
			if c.Version != "0.68.0" || c.OpenCode != nil {
				return ErrAdapterConfiguration
			}
		case ACPOpenCode119:
			if c.Version != "1.19.6" || c.Model != "opencode/big-pickle" || c.GeminiContext != nil {
				return ErrAdapterConfiguration
			}
		default:
			return ErrAdapterConfiguration
		}
	}
	info, err := os.Stat(c.AgentPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return ErrAdapterConfiguration
	}
	if _, err := acpMemberToolProfile(c); err != nil {
		return err
	}
	return nil
}

func (e acpExecutor) prepare(ctx context.Context, request ExecuteRequest, dir string, member memberToolConfig) (acpLaunch, string) {
	return e.prepareWithEnvironment(ctx, request, dir, member, os.Environ())
}

func (e acpExecutor) prepareWithEnvironment(ctx context.Context, request ExecuteRequest, dir string, member memberToolConfig, env []string) (acpLaunch, string) {
	var launch acpLaunch
	var reason string
	if !preflight.Check(ctx, "acp_prepare", func() bool {
		launch, reason = e.prepareProfile(ctx, request, dir, member, env)
		return reason == ""
	}) && reason == "" {
		return acpLaunch{}, "执行在启动前已取消或超时"
	}
	return launch, reason
}

func (e acpExecutor) prepareProfile(ctx context.Context, request ExecuteRequest, dir string, member memberToolConfig, env []string) (acpLaunch, string) {
	if err := validateACPConfig(e.config); err != nil {
		return acpLaunch{}, "ACP 可执行文件或版本配置无效，执行未启动"
	}
	if ctx.Err() != nil {
		return acpLaunch{}, "执行在启动前已取消或超时"
	}
	if e.agentFile != nil {
		info, err := os.Stat(e.config.AgentPath)
		if err != nil || ctx.Err() != nil {
			return acpLaunch{}, "ACP 可执行来源已变化，执行未启动"
		}
		real, realErr := filepath.EvalSymlinks(e.config.AgentPath)
		if realErr != nil || real != e.agentReal || !acpStableInfo(e.agentFile, info) {
			return acpLaunch{}, "ACP 可执行来源已变化，执行未启动"
		}
	}
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return acpLaunch{}, "无法创建 ACP 成员工具配置"
	}
	server := "aw" + hex.EncodeToString(nonce)
	switch e.config.VendorProfile {
	case ACPGemini068:
		digest, ok := acpGeminiContextDigest(e.config, request.CWD, env)
		if !ok {
			return acpLaunch{}, "Gemini 指令来源、继承配置或管理策略未经核实，受限执行未启动"
		}
		prepared, reason := acpGeminiLaunch(e.config, dir, server, member, env)
		if reason != "" {
			return acpLaunch{}, reason
		}
		settingsPath := acpEnv(prepared.env, "GEMINI_CLI_SYSTEM_SETTINGS_PATH")
		settings, settingsInfo, stable := acpReadStable(settingsPath, acpFrameLimit)
		if !stable {
			return acpLaunch{}, "Gemini 受限配置不稳定，执行未启动"
		}
		prepared.verify = func(_ context.Context) bool {
			current, ok := acpGeminiContextDigest(e.config, request.CWD, env)
			raw, info, stable := acpReadStable(settingsPath, acpFrameLimit)
			return ok && current == digest && stable && acpStableInfo(settingsInfo, info) && string(raw) == string(settings)
		}
		prepared.seal = acpSeal(e.config, request.CWD, prepared)
		if prepared.seal == nil {
			return acpLaunch{}, "ACP 执行来源无法封存，执行未启动"
		}
		return prepared, ""
	case ACPOpenCode119:
		if e.config.OpenCode == nil || e.openCodeAgentSHA == "" || !acpOpenCodePreflightContext(ctx, request.CWD, env) {
			return acpLaunch{}, acpOpenCodeRestrictedReason
		}
		if !preflight.Check(ctx, "opencode_prepare_dependencies", func() bool {
			digest, err := acpOpenCodeDependenciesContext(ctx, *e.config.OpenCode)
			return err == nil && digest == e.openCodeDigest
		}) {
			return acpLaunch{}, acpOpenCodeRestrictedReason
		}
		var account acpAccountSource
		if !preflight.Check(ctx, "opencode_prepare_account_pin", func() bool {
			var ok bool
			account, ok = acpOpenCodeAccountSource(ctx, env)
			return ok
		}) {
			return acpLaunch{}, acpOpenCodeRestrictedReason
		}
		var cacheHome string
		var verifyCache func() bool
		if !preflight.Check(ctx, "opencode_prepare_private_cache", func() bool {
			cacheHome, verifyCache = acpOpenCodePrivateCache(dir)
			return verifyCache != nil
		}) {
			return acpLaunch{}, acpOpenCodeRestrictedReason
		}
		prepared, ok := acpOpenCodeLaunch(e.config, request.CWD, server, cacheHome, env)
		if !ok {
			return acpLaunch{}, acpOpenCodeRestrictedReason
		}
		prepared.member = member
		prepared.verifyModelCache = verifyCache
		prepared.verify = func(ctx context.Context) bool {
			return preflight.Check(ctx, "opencode_verify_private_cache", verifyCache) &&
				preflight.Check(ctx, "opencode_verify_image_hash", func() bool {
					return acpOpenCodeExecutableSHA256Context(ctx, e.agentReal, e.agentFile) == e.openCodeAgentSHA
				}) && preflight.Check(ctx, "opencode_verify_account", func() bool {
				current, ok := acpOpenCodeAccountSource(ctx, env)
				return ok && account.compatible(current)
			}) && preflight.Check(ctx, "opencode_verify_dependencies", func() bool {
				deps, err := acpOpenCodeDependenciesContext(ctx, *e.config.OpenCode)
				return err == nil && deps == e.openCodeDigest
			}) && preflight.Check(ctx, "opencode_verify_sources", func() bool {
				return acpOpenCodeSources(ctx, env)
			})
		}
		prepared.seal = acpSeal(e.config, request.CWD, prepared)
		if prepared.seal == nil {
			return acpLaunch{}, "ACP 执行来源无法封存，执行未启动"
		}
		return prepared, ""
	case ACPAgy15:
		prepared, reason := acpAgyLaunch(e.config, dir, server, member, env)
		if reason != "" {
			return acpLaunch{}, reason
		}
		prepared.seal = acpSeal(e.config, request.CWD, prepared)
		if prepared.seal == nil {
			return acpLaunch{}, "ACP 执行来源无法封存，执行未启动"
		}
		return prepared, ""
	}
	return acpLaunch{}, "ACP 厂商受限配置无效"
}

func acpGeminiLaunch(c ACPConfig, dir, server string, member memberToolConfig, inherited []string) (acpLaunch, string) {
	settings, ok := acpGeminiSettings(server, c.ToolNames)
	if !ok {
		return acpLaunch{}, "Gemini 工具规范名无效或冲突，执行未启动"
	}
	path := filepath.Join(dir, "gemini-restricted-settings.json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return acpLaunch{}, "无法保存 Gemini 受限配置"
	}
	raw := acpJSON(settings)
	n, writeErr := file.Write(raw)
	closeErr := file.Close()
	if writeErr != nil || n != len(raw) || closeErr != nil {
		return acpLaunch{}, "无法保存 Gemini 受限配置"
	}
	return acpLaunch{server: server, agent: server, member: member,
		args: []string{"--acp", "--extensions", "none", "--allowed-mcp-server-names", server, "--approval-mode", "default", "--model", c.Model},
		env:  acpEnvironment(inherited, map[string]string{"GEMINI_CLI_NO_RELAUNCH": "1", "GEMINI_CLI_SYSTEM_SETTINGS_PATH": path})}, ""
}

// Installed Gemini 0.68.0 generateValidName. This profile intentionally refuses
// truncation or sanitization rather than admitting canonical-name collisions.
func acpGeminiToolName(server, tool string) (string, bool) {
	name := server + "_" + tool
	if !strings.HasPrefix(name, "mcp_") {
		name = "mcp_" + name
	}
	if len(name) > 63 {
		return "", false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_-.:", c)) {
			return "", false
		}
	}
	return name, true
}

func acpGeminiSettings(server string, names []string) (map[string]any, bool) {
	if server == "" || len(names) == 0 {
		return nil, false
	}
	core := make([]string, 0, len(names))
	for _, tool := range names {
		name, ok := acpGeminiToolName(server, tool)
		if !ok || slices.Contains(core, name) {
			return nil, false
		}
		core = append(core, name)
	}
	return map[string]any{
		"tools":   map[string]any{"core": core, "allowed": []string{}, "discoveryCommand": "", "callCommand": "", "useRipgrep": false, "sandbox": false},
		"context": map[string]bool{"includeDirectoryTree": false}, "mcp": map[string]string{"serverCommand": ""},
		"ide":          map[string]bool{"enabled": false},
		"hooksConfig":  map[string]bool{"enabled": false},
		"skills":       map[string]bool{"enabled": false},
		"experimental": map[string]bool{"enableAgents": false, "autoMemory": false},
		"security":     map[string]bool{"enablePermanentToolApproval": false},
		"general":      map[string]bool{"enableAutoUpdate": false, "enableAutoUpdateNotification": false}, "telemetry": map[string]bool{"enabled": false},
	}, true
}

func acpEnvironment(inherited []string, overrides map[string]string) []string {
	env := make([]string, 0, len(inherited)+len(overrides))
	for _, entry := range inherited {
		key, _, _ := strings.Cut(entry, "=")
		if _, replace := overrides[key]; !replace {
			env = append(env, entry)
		}
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func acpEnv(env []string, key string) string {
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			return strings.TrimPrefix(entry, key+"=")
		}
	}
	return ""
}

func acpAbsent(path string) bool { _, err := os.Lstat(path); return os.IsNotExist(err) }

// A conservative metadata preflight, not an OS sandbox. Existing management
// settings/policies are never replaced. Config and ancestors must stay trusted
// and stable through the run; this check cannot provide hostile-writer isolation.
func acpGeminiPreflight(cwd string, env []string) bool {
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if key == "GEMINI_SANDBOX" {
			// Gemini 0.68.0 normalizes this override before ACP reads stdin.
			// An empty value falls back to our verified tools.sandbox=false.
			switch strings.ToLower(acpGeminiTrim(value)) {
			case "", "0", "false":
			default:
				return false
			}
		}
		if strings.HasPrefix(key, "GEMINI_CLI_") && key != "GEMINI_CLI_HOME" && !(key == "GEMINI_CLI_NO_RELAUNCH" && value == "1") {
			return false
		}
	}
	for _, key := range []string{"GEMINI_CLI_SYSTEM_SETTINGS_PATH", "GEMINI_CLI_SYSTEM_DEFAULTS_PATH", "SANDBOX", "NODE_OPTIONS", "GEMINI_SYSTEM_MD", "GEMINI_WRITE_SYSTEM_MD"} {
		if acpEnv(env, key) != "" {
			return false
		}
	}
	home := acpGeminiHome(env)
	if !filepath.IsAbs(home) || !filepath.IsAbs(cwd) || !acpTrustedPath(home, true) || !acpAbsent(filepath.Join(home, ".gemini")) && !acpTrustedPath(filepath.Join(home, ".gemini"), true) {
		return false
	}
	system := "/etc/gemini-cli"
	if runtime.GOOS == "darwin" {
		system = "/Library/Application Support/GeminiCli"
	} else if runtime.GOOS != "linux" {
		return false
	}
	for _, path := range []string{filepath.Join(system, "settings.json"), filepath.Join(system, "system-defaults.json"), filepath.Join(system, "policies"), filepath.Join(home, ".gemini", "policies"), filepath.Join(home, ".gemini", ".env"), filepath.Join(home, ".env")} {
		if !acpAbsent(path) {
			return false
		}
	}
	// dotenv and workspace settings can otherwise change auth/config before ACP
	// session creation. Only the original user auth remains in its original HOME.
	for path := filepath.Clean(cwd); ; path = filepath.Dir(path) {
		for _, name := range []string{".env", ".gemini"} {
			if name == ".gemini" && path == home {
				continue
			}
			if !acpAbsent(filepath.Join(path, name)) {
				return false
			}
		}
		if path == filepath.Dir(path) {
			break
		}
	}
	settingsPath := filepath.Join(home, ".gemini", "settings.json")
	if acpAbsent(settingsPath) {
		return true
	}
	raw, _, stable := acpReadStable(settingsPath, acpFrameLimit)
	if !stable || !acpStrictJSON(raw) {
		return false
	}
	settings, ok := acpObject(raw)
	if !ok {
		return false
	}
	// Unknown management/startup sources are not replaced by private settings.
	for key := range settings {
		if !slices.Contains([]string{"context", "general", "ide", "output", "security", "tools", "ui", "privacy", "telemetry", "model"}, key) {
			return false
		}
	}
	if general, ok := acpObject(settings["general"]); ok {
		if checkpoint, ok := acpObject(general["checkpointing"]); ok && string(checkpoint["enabled"]) != "false" {
			return false
		}
	}
	for _, key := range []string{"policyPaths", "adminPolicyPaths"} {
		if value := settings[key]; value != nil && string(value) != "[]" {
			return false
		}
	}
	// Exact system core would override these, but reject policy-affecting user
	// tool settings until their merge/priority contract is explicitly supported.
	if raw := settings["tools"]; raw != nil {
		tools, ok := acpObject(raw)
		if !ok {
			return false
		}
		for _, key := range []string{"core", "allowed", "exclude", "confirmationRequired", "discoveryCommand", "callCommand"} {
			if tools[key] != nil {
				return false
			}
		}
	}
	return true
}

// Only call after preflight and dependency no-op verification. Empty mcp/plugin
// maps cannot erase inherited configuration; guards establish its absence.
func acpOpenCodeLaunch(c ACPConfig, cwd, server, cacheHome string, inherited []string) (acpLaunch, bool) {
	if server == "" || len(c.ToolNames) == 0 {
		return acpLaunch{}, false
	}
	permission := map[string]string{"*": "deny"}
	for _, tool := range c.ToolNames {
		key := server + "_" + tool
		for _, r := range key {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				return acpLaunch{}, false
			}
		}
		if permission[key] != "" {
			return acpLaunch{}, false
		}
		permission[key] = "allow"
	}
	provider, model, ok := strings.Cut(c.Model, "/")
	if !ok || provider != "opencode" || model == "" {
		return acpLaunch{}, false
	}
	// In the pinned v1.19.6 image, provider.ts checks disabled_providers before
	// invoking custom(dep) loaders; enabled_providers filters after invocation.
	// Exclude every other entry of that table without changing the inherited
	// environment. Re-audit the list before admitting a different image.
	disabledProviders := []string{
		"amazon-bedrock", "anthropic", "azure", "azure-cognitive-services", "cerebras",
		"cloudflare-ai-gateway", "cloudflare-workers-ai", "github-copilot", "gitlab",
		"google-vertex", "google-vertex-anthropic", "kilo", "llmgateway", "meta", "nvidia",
		"openai", "openrouter", "sap-ai-core", "snowflake-cortex", "vercel", "xai", "zenmux",
	}
	content := map[string]any{"model": c.Model, "small_model": c.Model, "default_agent": server,
		"enabled_providers": []string{provider}, "disabled_providers": disabledProviders,
		"provider": map[string]any{provider: map[string]any{"whitelist": []string{model}}},
		"agent":    map[string]any{server: map[string]any{"mode": "primary", "permission": permission}}, "mcp": map[string]any{}, "plugin": []string{},
		"share": "disabled", "autoupdate": false, "snapshot": false, "formatter": false, "lsp": false, "skills": map[string]any{"paths": []string{}, "urls": []string{}}}
	overrides := map[string]string{
		"XDG_CACHE_HOME": cacheHome, "OPENCODE_MODELS_PATH": acpOpenCodeModelsPath(cacheHome),
		"OPENCODE_PURE": "true", "OPENCODE_DISABLE_PROJECT_CONFIG": "true", "OPENCODE_DISABLE_AUTOUPDATE": "true",
		"OPENCODE_DISABLE_MODELS_FETCH": "true", "OPENCODE_DISABLE_LSP_DOWNLOAD": "true",
		"OPENCODE_DISABLE_CLAUDE_CODE": "true", "OPENCODE_DISABLE_EXTERNAL_SKILLS": "true",
		"OPENCODE_EXPERIMENTAL": "false", "OPENCODE_EXPERIMENTAL_CODE_MODE": "false",
		"OPENCODE_EXPERIMENTAL_BACKGROUND_SUBAGENTS": "false", "OPENCODE_EXPERIMENTAL_NATIVE_LLM": "false",
		"OPENCODE_CONFIG_CONTENT": string(acpJSON(content)),
	}
	if c.OpenCode != nil {
		overrides["XDG_CONFIG_HOME"] = filepath.Dir(c.OpenCode.ConfigDir)
	}
	env := acpEnvironment(inherited, overrides)
	return acpLaunch{server: server, agent: server, args: []string{"--pure", "acp", "--cwd", cwd, "--hostname", "127.0.0.1", "--port", "0", "--mdns=false"}, env: env}, true
}
