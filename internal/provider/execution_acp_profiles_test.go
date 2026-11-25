package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestACPAdapterSelectionIsExplicitAndUnregistered(t *testing.T) {
	e, request, _ := acpFixture(t, "normal")
	config := e.config
	a := NewACPAdapter(config)
	config.ToolNames[0] = "shell"
	p := Provider{ID: "fixture-provider", Name: "gemini", Path: e.config.AgentPath, Version: "0.68.0", Installation: Installed}
	caps, err := a.Capabilities(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range []Capability{CapabilityPlatformTools, CapabilityTerminalEvidence, CapabilityPermissionInteraction, CapabilityCancel, CapabilityNativeResume, CapabilitySteer} {
		if caps.Supports(capability) {
			t.Fatal("fixture/static metadata advertised native acceptance")
		}
	}
	if caps.Evidence.Protocol != ACPV1Protocol || caps.Evidence.Source != BuiltinContractEvidence {
		t.Fatal("wrong capability provenance")
	}
	if _, err := DefaultAdapterRegistry().Capabilities(AdapterSelection{ID: "acp_v1", Protocol: ACPV1Protocol}, p); err == nil {
		t.Fatal("ACP became automatically registered")
	}
	if _, err := a.NewExecutor(p, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(request.CWD, "pid")); !os.IsNotExist(err) {
		t.Fatal("capability/factory performed native discovery")
	}
	p.Version = "0.69.0"
	if _, err := a.Capabilities(p); err == nil {
		t.Fatal("unsupported version was inferred compatible")
	}
}

func TestACPFactoryRejectsImplicitModelAndChangedMemberConfig(t *testing.T) {
	for _, field := range []string{"model", "model-flag", "model-space", "version", "path", "profile", "paid-opencode", "thinking", "request-model", "config-drift"} {
		t.Run(field, func(t *testing.T) {
			e, request, _ := acpFixture(t, "normal")
			config := e.config
			switch field {
			case "model":
				config.Model = ""
			case "model-flag":
				config.Model = "--yolo"
			case "model-space":
				config.Model = "model with whitespace"
			case "version":
				config.Version = "latest"
			case "path":
				config.AgentPath = "gemini"
			case "profile":
				config.VendorProfile = "generic-unverified"
			case "paid-opencode":
				config.VendorProfile = ACPOpenCode119
				config.Version = "1.19.6"
				config.Model = "paid/model"
			}
			factory, err := NewACPExecutor(config, time.Second)
			if slices.Contains([]string{"model", "model-flag", "model-space", "version", "path", "profile", "paid-opencode"}, field) {
				if err == nil {
					t.Fatal("unsafe/implicit factory selection accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch field {
			case "thinking":
				request.Thinking = "max"
			case "request-model":
				request.Model = "other-model"
			case "config-drift":
				if err := os.WriteFile(config.MemberToolConfigPath, []byte(`{"endpoint":"http://127.0.0.1:12345/api/v1/member-tools","token":"different-token"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			r := factory.Execute(context.Background(), request, nil)
			if r.Started || r.Outcome != OutcomeUnknown || !acpAbsent(filepath.Join(request.RuntimeDir, request.RunID)) {
				t.Fatalf("rejected request did not remain unreserved and unknown: %+v", r)
			}
		})
	}
}

func TestACPGeminiExactCoreNamesAndRegistrationPolicy(t *testing.T) {
	settings, ok := acpGeminiSettings("aw0123456789", []string{"read_channel", "submit_result"})
	if !ok {
		t.Fatal("valid exact tools rejected")
	}
	core := settings["tools"].(map[string]any)["core"].([]string)
	want := []string{"mcp_aw0123456789_read_channel", "mcp_aw0123456789_submit_result"}
	if !reflect.DeepEqual(core, want) {
		t.Fatalf("not installed generateValidName: %v", core)
	}
	// Installed 0.68 policy: exact core ALLOW=4.25 beats wildcard DENY=4.24;
	// server allowed/trusted 4.1/4.2 does not. Builtin registration checks core.
	for _, name := range []string{"read_file", "write_file", "run_shell_command", "invoke_agent", "web_fetch", "mcp_foreign_read_channel"} {
		if slices.Contains(core, name) {
			t.Fatal("nonissued registration admitted")
		}
	}
	if settings["hooksConfig"].(map[string]bool)["enabled"] || settings["experimental"].(map[string]bool)["enableAgents"] || settings["tools"].(map[string]any)["useRipgrep"] != false {
		t.Fatal("native helper inheritance remained enabled")
	}
	for _, server := range []string{strings.Repeat("a", 64), "aw*", "aw/other"} {
		if _, ok := acpGeminiSettings(server, []string{"read_channel"}); ok {
			t.Fatal("canonical name would truncate/sanitize/collide")
		}
	}
	if _, ok := acpGeminiSettings("aw", []string{"read_channel", "read_channel"}); ok {
		t.Fatal("duplicate canonical tools accepted")
	}
}

func TestACPGeminiProfileChildEnforcesExactCorePriorityAndRegistration(t *testing.T) {
	e, request, base := acpFixture(t, "gemini-profile")
	inherited := append(os.Environ(), "AW_ACP_FIXTURE_MODE=gemini-profile")
	launch, reason := acpGeminiLaunch(e.config, t.TempDir(), "aw0123456789", base.member, inherited)
	if reason != "" {
		t.Fatal(reason)
	}
	r := e.execute(context.Background(), request, nil, launch)
	if r.Outcome != OutcomeSucceeded || !r.DirectChildReaped {
		t.Fatalf("restricted profile not applied to child: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(request.CWD, "profile-checked")); err != nil {
		t.Fatal("child did not verify policy priority and native registration")
	}
	argv, err := os.ReadFile(filepath.Join(request.CWD, "argv.json"))
	if err != nil || strings.Contains(string(argv), acpFixtureToken) {
		t.Fatal("bearer appeared in child argv")
	}
	if acpEnv(launch.env, "HOME") != os.Getenv("HOME") || acpEnv(launch.env, "GEMINI_CLI_HOME") != os.Getenv("GEMINI_CLI_HOME") {
		t.Fatal("original Gemini credential location changed")
	}
	info, err := os.Stat(acpEnv(launch.env, "GEMINI_CLI_SYSTEM_SETTINGS_PATH"))
	if err != nil || info.Mode() != 0600 {
		t.Fatal("private Gemini restrictions not 0600")
	}
}

func TestACPGeminiPreflightRejectsPolicyAndWorkspaceInheritance(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	env := []string{"HOME=" + home}
	if !acpGeminiPreflight(cwd, env) {
		t.Fatal("clean synthetic home rejected")
	}
	for _, key := range []string{"GEMINI_CLI_SYSTEM_SETTINGS_PATH", "GEMINI_CLI_SYSTEM_DEFAULTS_PATH", "NODE_OPTIONS", "SANDBOX"} {
		if acpGeminiPreflight(cwd, append(env, key+"=untrusted")) {
			t.Fatal("inherited runtime/management override accepted")
		}
	}
	if err := os.Mkdir(filepath.Join(cwd, ".gemini"), 0700); err != nil {
		t.Fatal(err)
	}
	if acpGeminiPreflight(cwd, env) {
		t.Fatal("project config was treated as host isolation")
	}
	clean := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".gemini"), 0700); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(home, ".gemini", "settings.json")
	for _, raw := range []string{`{"policyPaths":["unknown.toml"]}`, `{"tools":{"core":[]}}`, `{"tools":{"allowed":["*"]}}`} {
		if err := os.WriteFile(settings, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if acpGeminiPreflight(clean, env) {
			t.Fatal("policy-affecting user inheritance accepted")
		}
	}
	if err := os.WriteFile(settings, []byte(`{"tools":{"shell":{}},"security":{"auth":{"selectedType":"fixture"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if !acpGeminiPreflight(clean, env) {
		t.Fatal("original auth selection/simple user settings not preserved")
	}
}

func TestACPGeminiInheritedSandboxGate(t *testing.T) {
	e, request, base := acpFixture(t, "normal")
	request.CWD = t.TempDir()
	home := t.TempDir()
	// An override that would block OpenCode must not disable Gemini's own profile.
	env := []string{"HOME=" + home, "GEMINI_CLI_HOME=" + home, "OPENCODE_CONFIG_DIR=/fixture/foreign"}
	for _, tc := range []struct {
		name    string
		entries []string
		allow   bool
	}{
		{"absent", nil, true},
		{"empty", []string{"GEMINI_SANDBOX="}, true},
		{"js-whitespace", []string{"GEMINI_SANDBOX= \t\u00a0\ufeff"}, true},
		{"zero", []string{"GEMINI_SANDBOX=0"}, true},
		{"false-case-trim", []string{"GEMINI_SANDBOX= \tFaLsE\r\n"}, true},
		{"true", []string{"GEMINI_SANDBOX=true"}, false},
		{"one-case-trim", []string{"GEMINI_SANDBOX= \t1 "}, false},
		{"command", []string{"GEMINI_SANDBOX= SANDBOX-EXEC "}, false},
		{"unknown", []string{"GEMINI_SANDBOX=off"}, false},
		{"non-js-whitespace", []string{"GEMINI_SANDBOX=\u0085false"}, false},
		{"non-js-fold", []string{"GEMINI_SANDBOX=fal\u017fe"}, false},
		{"duplicate", []string{"GEMINI_SANDBOX=false", "GEMINI_SANDBOX=true"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			inherited := append(slices.Clone(env), tc.entries...)
			ctx := context.Background()
			launch, reason := e.prepareWithEnvironment(ctx, request, dir, base.member, inherited)
			if tc.allow {
				if reason != "" || launch.seal == nil || !launch.verify(ctx) {
					t.Fatalf("provably disabled sandbox did not prepare: %s", reason)
				}
				for _, key := range []string{"GEMINI_SANDBOX", "HOME", "GEMINI_CLI_HOME"} {
					if acpEnv(launch.env, key) != acpEnv(inherited, key) {
						t.Fatal("Gemini inherited configuration was silently overridden")
					}
				}
			} else if reason == "" || launch.seal != nil || !acpAbsent(filepath.Join(dir, "gemini-restricted-settings.json")) {
				t.Fatal("unproven inherited sandbox reached a sealed launch")
			}
		})
	}
}

func acpDependencyFixture(t *testing.T) ACPOpenCodeProfile {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "opencode")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		".gitignore":        "node_modules\npackage.json\npackage-lock.json\nbun.lock\n.gitignore",
		"package.json":      `{"private":true,"dependencies":{"@opencode-ai/plugin":"1.19.6"}}`,
		"package-lock.json": `{"lockfileVersion":3,"packages":{"":{"dependencies":{"@opencode-ai/plugin":"1.19.6"}},"node_modules/@opencode-ai/plugin":{"version":"1.19.6"}}}`,
		"node_modules/@opencode-ai/plugin/package.json": `{"name":"@opencode-ai/plugin","version":"1.19.6"}`,
		"node_modules/@opencode-ai/plugin/index.js":     "// Synthetic fixture, not a native installation.\n",
	}
	for name, raw := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	hash := sha256.Sum256([]byte(files["package-lock.json"]))
	return ACPOpenCodeProfile{ConfigDir: dir, LockSHA256: hex.EncodeToString(hash[:])}
}

func TestACPOpenCodeExactPermissionsAndOriginalCredentialEnvironment(t *testing.T) {
	profile := acpDependencyFixture(t)
	c := ACPConfig{Model: "opencode/big-pickle", ToolNames: []string{"read_channel", "submit_result"}, OpenCode: &profile}
	inherited := []string{"HOME=/original/home", "XDG_DATA_HOME=/original/data", "XDG_CONFIG_HOME=/old/config", "XDG_CACHE_HOME=/old/cache",
		"HTTPS_PROXY=http://proxy.invalid:8080", "NO_PROXY=127.0.0.1,localhost", "SSL_CERT_FILE=/original/certs.pem"}
	cache := t.TempDir()
	launch, ok := acpOpenCodeLaunch(c, "/controlled/cwd", "awABC", cache, inherited)
	if !ok {
		t.Fatal("valid exact OpenCode config rejected")
	}
	if !reflect.DeepEqual(launch.args, []string{"--pure", "acp", "--cwd", "/controlled/cwd", "--hostname", "127.0.0.1", "--port", "0", "--mdns=false"}) {
		t.Fatal("OpenCode argv not restricted")
	}
	if acpEnv(launch.env, "HOME") != "/original/home" || acpEnv(launch.env, "XDG_DATA_HOME") != "/original/data" || acpEnv(launch.env, "XDG_CONFIG_HOME") != filepath.Dir(profile.ConfigDir) {
		t.Fatal("original auth location changed")
	}
	for _, key := range []string{"HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE"} {
		if acpEnv(launch.env, key) != acpEnv(inherited, key) {
			t.Fatalf("inherited transport setting %s changed", key)
		}
	}
	for _, key := range []string{"OPENCODE_PURE", "OPENCODE_DISABLE_PROJECT_CONFIG", "OPENCODE_DISABLE_AUTOUPDATE", "OPENCODE_DISABLE_MODELS_FETCH", "OPENCODE_DISABLE_LSP_DOWNLOAD", "OPENCODE_DISABLE_CLAUDE_CODE", "OPENCODE_DISABLE_EXTERNAL_SKILLS"} {
		if acpEnv(launch.env, key) != "true" {
			t.Fatal("startup helper still enabled")
		}
	}
	if acpEnv(launch.env, "OPENCODE_DISABLE_DEFAULT_PLUGINS") != "" {
		t.Fatal("original auth plugins disabled")
	}
	if acpEnv(launch.env, "XDG_CACHE_HOME") != cache || acpEnv(launch.env, "OPENCODE_MODELS_PATH") != filepath.Join(cache, "opencode", "models.json") || acpEnv(launch.env, "OPENCODE_EXPERIMENTAL_NATIVE_LLM") != "false" {
		t.Fatal("the child does not select the pinned catalog and bundled SDK path")
	}
	var content map[string]any
	if json.Unmarshal([]byte(acpEnv(launch.env, "OPENCODE_CONFIG_CONTENT")), &content) != nil {
		t.Fatal("invalid config content")
	}
	permissions := content["agent"].(map[string]any)["awABC"].(map[string]any)["permission"].(map[string]any)
	if !reflect.DeepEqual(permissions, map[string]any{"*": "deny", "awABC_read_channel": "allow", "awABC_submit_result": "allow"}) || content["model"] != "opencode/big-pickle" || content["small_model"] != content["model"] || content["share"] != "disabled" {
		t.Fatal("native/wildcard tools or model fallback enabled")
	}
	if !reflect.DeepEqual(content["provider"], map[string]any{"opencode": map[string]any{"whitelist": []any{"big-pickle"}}}) {
		t.Fatal("alternate model or route/SDK override in runtime config")
	}
	wantDisabled := []any{
		"amazon-bedrock", "anthropic", "azure", "azure-cognitive-services", "cerebras",
		"cloudflare-ai-gateway", "cloudflare-workers-ai", "github-copilot", "gitlab",
		"google-vertex", "google-vertex-anthropic", "kilo", "llmgateway", "meta", "nvidia",
		"openai", "openrouter", "sap-ai-core", "snowflake-cortex", "vercel", "xai", "zenmux",
	}
	if !reflect.DeepEqual(content["enabled_providers"], []any{"opencode"}) || !reflect.DeepEqual(content["disabled_providers"], wantDisabled) {
		t.Fatal("private config does not fence the pinned custom loaders and final provider selection")
	}
	if strings.Contains(strings.Join(launch.args, " "), acpFixtureToken) || strings.Contains(strings.Join(launch.env, " "), acpFixtureToken) {
		t.Fatal("MCP bearer moved out of private stdio")
	}
}

func TestACPOpenCodeDependencyNoOpAndDrift(t *testing.T) {
	profile := acpDependencyFixture(t)
	before, err := acpOpenCodeDependencies(profile)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(profile.ConfigDir, "node_modules", "@opencode-ai", "plugin", "index.js")
	if err := os.WriteFile(file, []byte("// changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	after, err := acpOpenCodeDependencies(profile)
	if err != nil || before == after {
		t.Fatal("materialized dependency drift not detected")
	}
	for _, name := range []string{"tool", "plugins", "skills", "opencode.jsonc", ".npmrc"} {
		t.Run(name, func(t *testing.T) {
			p := acpDependencyFixture(t)
			if err := os.WriteFile(filepath.Join(p.ConfigDir, name), []byte("unexpected"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := acpOpenCodeDependencies(p); err == nil {
				t.Fatal("custom config/modules could reach bootstrap")
			}
		})
	}
	p := acpDependencyFixture(t)
	p.LockSHA256 = strings.Repeat("0", 64)
	if _, err := acpOpenCodeDependencies(p); err == nil {
		t.Fatal("untrusted lock accepted")
	}
}

func TestACPOpenCodeUnknownExecutableRejectsPreparedProfile(t *testing.T) {
	e, _, _ := acpFixture(t, "normal")
	profile := acpDependencyFixture(t)
	e.config.VendorProfile, e.config.Version, e.config.Model = ACPOpenCode119, "1.19.6", "opencode/big-pickle"
	e.config.OpenCode = &profile
	if _, err := NewACPExecutor(e.config, time.Second); err != ErrAdapterConfiguration {
		t.Fatalf("OpenCode accepted an unverified executable on its declared version alone: %v", err)
	}
}

func TestACPOpenCodePreparedDependencyMetadataOnly(t *testing.T) {
	dir := os.Getenv("AW_ACP_PREPARED_CONFIG_DIR")
	if dir == "" {
		t.Skip("explicit source-trusted dependency metadata path not supplied")
	}
	profile := ACPOpenCodeProfile{ConfigDir: dir, LockSHA256: os.Getenv("AW_ACP_PREPARED_LOCK_SHA256")}
	digest, err := acpOpenCodeDependencies(profile)
	if err != nil {
		t.Fatal("prepared materialized dependencies fail no-op/drift validation")
	}
	again, err := acpOpenCodeDependencies(profile)
	if err != nil || again != digest {
		t.Fatal("prepared no-op sources did not remain stable")
	}
	t.Logf("dependency_metadata_only_sha256=%x; no CLI/auth/model invoked", digest)
	if path := os.Getenv("AW_ACP_PINNED_OPENCODE_BINARY"); path != "" {
		e, _, _ := acpFixture(t, "normal")
		e.config.AgentPath = path
		e.config.VendorProfile, e.config.Version, e.config.Model = ACPOpenCode119, "1.19.6", "opencode/big-pickle"
		e.config.OpenCode = &profile
		if _, err := NewACPExecutor(e.config, time.Second); err != nil {
			t.Fatal("installed image or dependencies failed source-only constructor check")
		}
		t.Log("installed image matched the audited pin without executing it")
	}
}

func TestACPOpenCodeFirstAnonymousPreflightAndNoPromptOnRestriction(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	env := []string{"HOME=" + home}
	if err := os.Chmod(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	if !acpOpenCodePreflight(cwd, env) {
		t.Fatal("clean synthetic first-use profile rejected")
	}
	for _, key := range []string{"OPENCODE_API_KEY", "OPENCODE_AUTH_CONTENT", "OPENCODE_CONSOLE_TOKEN", "OPENCODE_DB", "OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_PERMISSION", "OPENCODE_TEST_HOME", "OPENCODE_MODELS_PATH", "OPENCODE_MODELS_URL"} {
		if acpOpenCodePreflight(cwd, append(env, key+"=fixture")) {
			t.Fatal("unexpected account/config override admitted")
		}
	}
	data := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(data, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "opencode.db"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if acpOpenCodePreflight(cwd, env) {
		t.Fatal("existing account DB incorrectly treated as anonymous")
	}
	e, request, _ := acpFixture(t, "normal")
	e.config.VendorProfile = ACPOpenCode119
	e.config.Version = "1.19.6"
	e.config.Model = "opencode/big-pickle"
	factory, err := NewACPExecutor(e.config, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r := factory.Execute(context.Background(), request, nil)
	if r.Started || r.Outcome != OutcomeNotStarted || r.Reason != acpOpenCodeRestrictedReason {
		t.Fatalf("unproven bootstrap reached native process: %+v", r)
	}
}
