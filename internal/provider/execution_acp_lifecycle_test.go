package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func acpPreparedFixture(t *testing.T, mode string, extraEnv ...string) (acpExecutor, ExecuteRequest, acpLaunch) {
	t.Helper()
	e, request, base := acpFixture(t, mode)
	request.CWD = t.TempDir()
	if err := os.Chmod(request.CWD, 0700); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	env := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "AW_ACP_FIXTURE_MODE=" + mode}
	if strings.HasPrefix(mode, "oc-") {
		e.config.VendorProfile, e.config.Version, e.config.Model = ACPOpenCode119, "1.19.6", "opencode/big-pickle"
		profile := acpDependencyFixture(t)
		e.config.OpenCode = &profile
		digest, err := acpOpenCodeDependencies(profile)
		if err != nil {
			t.Fatal(err)
		}
		e.openCodeDigest = digest
		// Pin synthetic fixture bytes only inside the package. NewACPExecutor
		// admits only the audited vendor image for a prepared OpenCode profile.
		e.agentFile, err = os.Stat(e.config.AgentPath)
		if err != nil {
			t.Fatal(err)
		}
		e.agentReal, err = filepath.EvalSymlinks(e.config.AgentPath)
		if err != nil {
			t.Fatal(err)
		}
		e.openCodeAgentSHA = acpOpenCodeExecutableSHA256(e.agentReal, e.agentFile)
		if e.openCodeAgentSHA == "" {
			t.Fatal("could not pin synthetic fixture source")
		}
	}
	launch, reason := e.prepareWithEnvironment(context.Background(), request, t.TempDir(), base.member, append(env, extraEnv...))
	if reason != "" {
		t.Fatal(reason)
	}
	if launch.seal == nil || !launch.seal.valid(e.config, request.CWD, launch) {
		t.Fatal("preparation did not bind a sealed launch")
	}
	return e, request, launch
}

func TestACPChildPreparedSealedProfilesSettleMetadataPoorOrdinaryCalls(t *testing.T) {
	for _, vendor := range []string{"gemini", "oc"} {
		t.Run(vendor, func(t *testing.T) {
			e, request, launch := acpPreparedFixture(t, vendor+"-prepared")
			r := e.execute(context.Background(), request, nil, launch)
			if r.Outcome != OutcomeSucceeded || !r.OutputComplete || !r.DirectChildReaped || r.Terminal != OutcomeSucceeded {
				t.Fatalf("actual prepared MCP-only lifecycle did not settle: %+v", r)
			}
			raw, err := os.ReadFile(r.StdoutPath)
			if err != nil || strings.Contains(string(raw), "must not infer identity") || strings.Contains(string(raw), `"identity":"read_channel"`) {
				t.Fatal("title/identity fabricated or logged")
			}
		})
	}
}

func TestACPChildPreparedSealDoesNotAuthorizeOrHideContradictions(t *testing.T) {
	for _, mode := range []string{"gemini-prepared-native", "gemini-prepared-foreign", "oc-prepared-native", "oc-prepared-foreign", "gemini-prepared-permission", "oc-prepared-permission"} {
		t.Run(mode, func(t *testing.T) {
			e, request, launch := acpPreparedFixture(t, mode)
			r := e.execute(context.Background(), request, nil, launch)
			if r.Outcome != OutcomeUnknown || !r.DirectChildReaped {
				t.Fatalf("seal widened authority or hid contradiction: %+v", r)
			}
			if strings.HasSuffix(mode, "-permission") {
				raw, err := os.ReadFile(filepath.Join(request.CWD, "permission-reply.json"))
				if err != nil || !strings.Contains(string(raw), "deny-83") || strings.Contains(string(raw), "this-once-47") {
					t.Fatal("seal authorized a metadata-poor permission")
				}
			}
		})
	}
	for _, vendor := range []string{"gemini", "oc"} {
		t.Run("unsealed-"+vendor, func(t *testing.T) {
			e, request, launch := acpFixture(t, vendor+"-prepared")
			if vendor == "oc" {
				e.config.VendorProfile, e.config.Version, e.config.Model = ACPOpenCode119, "1.19.6", "opencode/big-pickle"
				launch.env = append(launch.env, `OPENCODE_CONFIG_CONTENT={"default_agent":"aw_fixture_unique"}`)
			}
			r := e.execute(context.Background(), request, nil, launch)
			if r.Outcome != OutcomeUnknown {
				t.Fatalf("unattested call settled: %+v", r)
			}
		})
	}
}

func TestACPChildSealedLifecycleFencesRemainUnknown(t *testing.T) {
	for _, vendor := range []string{"gemini", "oc"} {
		for _, fault := range []string{"pending", "orphan", "duplicate", "cross-session", "diff", "location", "background", "config", "mode", "nonzero", "duplicate-terminal", "late-text", "fs-request", "terminal-request"} {
			t.Run(vendor+"-"+fault, func(t *testing.T) {
				e, request, launch := acpPreparedFixture(t, vendor+"-prepared-"+fault)
				r := e.execute(context.Background(), request, nil, launch)
				if r.Outcome != OutcomeUnknown || !r.DirectChildReaped {
					t.Fatalf("sealed profile hid incomplete/foreign work: %+v", r)
				}
			})
		}
		t.Run(vendor+"-delivery", func(t *testing.T) {
			e, request, launch := acpPreparedFixture(t, vendor+"-prepared")
			r := e.execute(context.Background(), request, func(event Event) error {
				if event.Kind == TerminalEvent {
					return errors.New("fixture persistence failure")
				}
				return nil
			}, launch)
			if r.Outcome != OutcomeUnknown || !r.DeliveryFailed || !r.DirectChildReaped {
				t.Fatalf("seal hid delivery failure: %+v", r)
			}
		})
	}
	for _, fault := range []string{"no-model", "model-null"} {
		t.Run("gemini-"+fault, func(t *testing.T) {
			e, request, launch := acpPreparedFixture(t, "gemini-prepared-"+fault)
			r := e.execute(context.Background(), request, nil, launch)
			if r.Outcome != OutcomeUnknown || r.Accepted || !r.DirectChildReaped {
				t.Fatalf("unobserved model reached prompt: %+v", r)
			}
		})
	}
	for _, suffix := range []string{"effort", "effort-change"} {
		t.Run("oc-"+suffix, func(t *testing.T) {
			e, request, launch := acpPreparedFixture(t, "oc-prepared-"+suffix)
			r := e.execute(context.Background(), request, nil, launch)
			want := OutcomeSucceeded
			if suffix == "effort-change" {
				want = OutcomeUnknown
			}
			if r.Outcome != want {
				t.Fatalf("installed optional effort state drift: %+v", r)
			}
		})
	}
}

func TestACPGeminiAmbientContextRequiresManifestBeforeChild(t *testing.T) {
	e, request, base := acpFixture(t, "normal")
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".gemini"), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, ".gemini", "GEMINI.md")
	if err := os.WriteFile(file, []byte("Preserved user instruction.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home}
	if _, reason := e.prepareWithEnvironment(context.Background(), request, t.TempDir(), base.member, env); reason == "" {
		t.Fatal("ambient user instructions were accepted without provenance")
	}
	body := []byte("Preserved user instruction.\n")
	hash := sha256.Sum256(body)
	e.config.GeminiContext = &ACPGeminiContextManifest{Version: "0.68.0", CWD: request.CWD, Files: []ACPGeminiContextFile{{Path: file, SHA256: hex.EncodeToString(hash[:])}}}
	if _, reason := e.prepareWithEnvironment(context.Background(), request, t.TempDir(), base.member, env); reason != "" {
		t.Fatal(reason)
	}
	if err := os.WriteFile(file, []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, reason := e.prepareWithEnvironment(context.Background(), request, t.TempDir(), base.member, env); reason == "" {
		t.Fatal("changed manifest body admitted")
	}
}

func TestACPChildOpenCodePreparedSourcesCannotDrift(t *testing.T) {
	for _, before := range []bool{false, true} {
		e, request, launch := acpPreparedFixture(t, "oc-prepared")
		change := func() {
			if err := os.WriteFile(filepath.Join(e.config.OpenCode.ConfigDir, ".gitignore"), []byte("drift"), 0600); err != nil {
				t.Error(err)
			}
		}
		if before {
			change()
		}
		r := e.execute(context.Background(), request, func(event Event) error {
			if !before && event.Kind == TerminalEvent {
				change()
			}
			return nil
		}, launch)
		if before {
			if r.Started || r.Outcome != OutcomeNotStarted {
				t.Fatalf("changed preparation reached child: %+v", r)
			}
		} else if r.Outcome != OutcomeUnknown || r.OutputComplete || !r.DirectChildReaped {
			t.Fatalf("post-wait preparation drift settled: %+v", r)
		}
	}
}

func TestACPChildOpenCodeCloudflareGatewayLoaderFence(t *testing.T) {
	for _, name := range []string{"private-config", "enabled-only-control"} {
		t.Run(name, func(t *testing.T) {
			e, request, launch := acpPreparedFixture(t, "oc-prepared-cloudflare-gateway",
				"CLOUDFLARE_ACCOUNT_ID=fixture-account", "CLOUDFLARE_GATEWAY_ID=fixture-gateway")
			control := name == "enabled-only-control"
			if control {
				// Model the old private config, with the allowlist but no early disable fence.
				const key = "OPENCODE_CONFIG_CONTENT"
				var config map[string]json.RawMessage
				if err := json.Unmarshal([]byte(acpEnv(launch.env, key)), &config); err != nil {
					t.Fatal(err)
				}
				delete(config, "disabled_providers")
				launch.env = acpEnvironment(launch.env, map[string]string{key: string(acpJSON(config))})
				launch.seal = acpSeal(e.config, request.CWD, launch) // This synthetic config is deliberate, not launch drift.
				if launch.seal == nil {
					t.Fatal("could not seal fixture control")
				}
			}
			r := e.execute(context.Background(), request, nil, launch)
			if !r.Started || !r.DirectChildReaped || r.ExitCode != 0 {
				t.Fatalf("Cloudflare gateway fixture did not start and exit cleanly: %+v", r)
			}
			if !control {
				if r.Outcome != OutcomeSucceeded || !r.Accepted || !r.OutputComplete || r.Terminal != OutcomeSucceeded {
					t.Fatalf("unrelated Cloudflare gateway state blocked the prepared session: %+v", r)
				}
				return
			}
			if r.Outcome != OutcomeUnknown || r.Accepted || r.Terminal != "" || r.OutputComplete || r.NativeSessionID != "" {
				t.Fatalf("session/new loader error was not classified as started but unknown: %+v", r)
			}
			raw, err := os.ReadFile(filepath.Join(request.CWD, "client.jsonl"))
			if err != nil || !strings.Contains(string(raw), `"method":"session/new"`) || strings.Contains(string(raw), `"method":"session/prompt"`) {
				t.Fatal("control did not reach session/new without sending a prompt")
			}
		})
	}
}

func TestACPChildOpenCodeCacheDoesNotSelectOriginalCatalog(t *testing.T) {
	for _, tc := range []struct{ name, catalog string }{
		{"same-selector-different-api-sdk", `{"opencode":{"models":{"big-pickle":{"id":"big-pickle","provider":{"api":"https://different.invalid/v1","npm":"fixture-nonbundled-sdk"}}}}}`},
		{"invalid-json", `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalCache := t.TempDir()
			originalCatalog := filepath.Join(originalCache, "opencode", "models.json")
			if err := os.Mkdir(filepath.Dir(originalCatalog), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(originalCatalog, []byte(tc.catalog), 0600); err != nil {
				t.Fatal(err)
			}
			originalInfo, err := os.Lstat(originalCatalog)
			if err != nil {
				t.Fatal(err)
			}
			// A Gemini sandbox setting must not block a valid OpenCode preparation.
			e, request, launch := acpPreparedFixture(t, "oc-prepared-cache", "XDG_CACHE_HOME="+originalCache, "GEMINI_SANDBOX=true")
			r := e.execute(context.Background(), request, nil, launch)
			if r.Outcome != OutcomeSucceeded || !r.Accepted || !r.OutputComplete || !r.DirectChildReaped || r.ExitCode != 0 {
				t.Errorf("child could not select the bundled catalog without using the original cache: %+v", r)
			}
			if output, err := os.ReadFile(filepath.Join(request.CWD, "catalog-source")); err != nil || string(output) != "snapshot" {
				t.Error("fixture did not observe the catalog loader's snapshot prerequisites")
			}
			raw, err := os.ReadFile(originalCatalog)
			after, statErr := os.Lstat(originalCatalog)
			if err != nil || statErr != nil || string(raw) != tc.catalog || !acpStableInfo(originalInfo, after) || !acpAbsent(filepath.Join(originalCache, "opencode", "bin")) {
				t.Fatal("original cache was changed")
			}
		})
	}
}

func TestACPChildOpenCodeCatalogDriftFencesSettlement(t *testing.T) {
	for _, phase := range []string{"before-start", "cache-replaced", "before-prompt", "after-terminal"} {
		t.Run(phase, func(t *testing.T) {
			mode := "oc-prepared"
			if phase == "before-prompt" {
				mode = "oc-prepared-cache-drift"
			}
			e, request, launch := acpPreparedFixture(t, mode)
			path := acpEnv(launch.env, "OPENCODE_MODELS_PATH")
			if !filepath.IsAbs(path) {
				t.Fatal("no private catalog selector")
			}
			change := func() {
				if err := os.WriteFile(path, []byte(`{"opencode":{"models":{"big-pickle":{"id":"big-pickle"}}}}`), 0600); err != nil {
					t.Error(err)
				}
			}
			if phase == "before-start" {
				change()
			} else if phase == "cache-replaced" {
				cache := filepath.Dir(path)
				if err := os.Rename(cache, cache+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(cache, 0700); err != nil {
					t.Fatal(err)
				}
			}
			r := e.execute(context.Background(), request, func(event Event) error {
				if phase == "after-terminal" && event.Kind == TerminalEvent {
					change()
				}
				return nil
			}, launch)
			switch phase {
			case "before-start", "cache-replaced":
				if r.Started || r.Outcome != OutcomeNotStarted || !acpAbsent(filepath.Join(request.CWD, "pid")) {
					t.Fatalf("changed catalog source reached the child: %+v", r)
				}
			case "before-prompt":
				if !r.Started || r.NativeSessionID == "" || r.Accepted || !r.DirectChildReaped || r.Outcome != OutcomeUnknown || r.OutputComplete {
					t.Fatalf("bootstrap catalog drift did not fence the prompt: %+v", r)
				}
				raw, err := os.ReadFile(filepath.Join(request.CWD, "client.jsonl"))
				if err != nil || strings.Contains(string(raw), `"method":"session/prompt"`) {
					t.Fatal("fixture with drift received a prompt")
				}
			case "after-terminal":
				if r.Terminal != OutcomeSucceeded || !r.Accepted || !r.DirectChildReaped || r.Outcome != OutcomeUnknown || r.OutputComplete {
					t.Fatalf("catalog drift falsely settled after real Wait: %+v", r)
				}
			}
		})
	}
}

func TestACPOpenCodePreparedGitignoreMustAlreadyBeNoOp(t *testing.T) {
	p := acpDependencyFixture(t)
	if err := os.Remove(filepath.Join(p.ConfigDir, ".gitignore")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := acpOpenCodeDependencies(p); err == nil {
		t.Fatal("ensureGitignore would mutate a supposedly frozen preparation")
	}
}
