package provider

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

const appServerSessionNullProvider = `,"model_provider":null`

func appServerSessionProfileConfig(providerFields string) string {
	return strings.Replace(appServerCompatEmptySystemConfig, `"mcp_servers":{}`, `"mcp_servers":{},"model_reasoning_effort":"max"`+providerFields, 1)
}

func executeSessionProfile(t *testing.T, selected Provider, config string, request ExecuteRequest, resume *NativeSessionReference) ExecutionReceipt {
	t.Helper()
	e, err := NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, CodexAppServerSessionOptions{Resume: resume}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return e.Execute(context.Background(), request, nil)
}

func TestCodexSessionImplicitProviderFreshAndColdResume(t *testing.T) {
	selected, config, request := appServerSessionFixture(t)
	t.Setenv("AW_SESSION_NATIVE_DEFAULT", "1")
	t.Setenv("AW_SESSION_CONFIG_AFTER", appServerSessionProfileConfig(appServerSessionNullProvider))
	var ref *NativeSessionReference
	for i, run := range []struct{ id, providerFields string }{
		{"null-provider", appServerSessionNullProvider}, {"omitted-provider", ""},
	} {
		t.Setenv("AW_SESSION_CONFIG_BEFORE", appServerSessionProfileConfig(run.providerFields))
		request.RunID = run.id
		r := executeSessionProfile(t, selected, config, request, ref)
		if r.Outcome != OutcomeSucceeded || !r.Accepted || r.NativeRef == nil || r.NativeRef.Codex == nil {
			t.Fatalf("%s: native default not accepted: outcome=%s accepted=%v thread=%q methods=%v", run.id, r.Outcome, r.Accepted, r.NativeSessionID, sessionProfileMethods(t, request.CWD))
		}
		if c := r.NativeRef.Codex; c.Provider != "openai" || c.ReasoningEffort != "max" || c.HistoryTurns != i+1 || (ref != nil && c.NativeThreadID != ref.Codex.NativeThreadID) {
			t.Fatal("reference did not bind the concrete native profile and thread")
		}
		ref = r.NativeRef
	}

	// The reference for the implicit provider must not pin the next run to it
	// when the current config instead selects a custom provider.
	t.Setenv("AW_SESSION_NATIVE_DEFAULT", "")
	t.Setenv("AW_SESSION_CONFIG_BEFORE", appServerSessionProfileConfig(`,"model_provider":"synthetic"`))
	request.RunID, request.Prompt = "changed-provider", "must not send across a provider change"
	r := executeSessionProfile(t, selected, config, request, ref)
	if r.Outcome != OutcomeUnknown || !r.Started || r.Accepted || r.Terminal != "" || !r.DirectChildReaped || r.NativeRef != nil {
		t.Fatal("changed effective provider reused a native reference")
	}

	counts := map[string]int{}
	for _, frames := range sessionClientFrames(t, request.CWD) {
		for _, frame := range frames {
			method := *frame.Method
			counts[method]++
			switch method {
			case "thread/start", "thread/resume":
				params, err := codexRPCObject(frame.Params, "modelProvider", "config", "approvalsReviewer")
				cfg, cfgErr := codexRPCObject(params["config"], "model_provider", "model_reasoning_effort", "approvals_reviewer")
				if err != nil || cfgErr != nil || params["modelProvider"] != nil || cfg["model_provider"] != nil || string(cfg["model_reasoning_effort"]) != `"max"` ||
					string(params["approvalsReviewer"]) != `"user"` || string(cfg["approvals_reviewer"]) != `"user"` {
					t.Fatal("implicit provider was overridden or current effort/reviewer was lost")
				}
			case "turn/start":
				params, err := codexRPCObject(frame.Params, "effort")
				if err != nil || string(params["effort"]) != `"max"` {
					t.Fatal("turn did not retain the verified effort")
				}
			}
		}
	}
	if counts["initialize"] != 3 || counts["thread/start"] != 1 || counts["thread/resume"] != 1 || counts["turn/start"] != 2 {
		t.Fatalf("expected only one fresh and one cold-resume turn: %v", counts)
	}
}

func TestCodexSessionProviderProfileFailsClosed(t *testing.T) {
	implicit := appServerSessionProfileConfig(appServerSessionNullProvider)
	explicit := appServerSessionProfileConfig(`,"model_provider":"synthetic"`)
	explicitHigh := strings.Replace(explicit, `"max"`, `"high"`, 1)
	implicitHigh := strings.Replace(implicit, `"max"`, `"high"`, 1)
	managed := strings.Replace(implicit, `"type":"system"`, `"type":"legacyManagedConfigTomlFromFile"`, 1)
	for _, tc := range []struct {
		name, before, after, mode string
		wantFrames                int
	}{
		{"null-with-managed-layer", managed, implicit, "", 3},
		{"managed-layer-appears", implicit, managed, "", 5},
		{"empty-provider", appServerSessionProfileConfig(`,"model_provider":""`), implicit, "", 3},
		{"invalid-provider", appServerSessionProfileConfig(`,"model_provider":" invalid "`), implicit, "", 3},
		{"wrong-provider-type", appServerSessionProfileConfig(`,"model_provider":42`), implicit, "", 3},
		{"duplicate-provider", appServerSessionProfileConfig(appServerSessionNullProvider + `,"model_provider":"synthetic"`), implicit, "", 3},
		{"explicit-provider-appears", implicit, explicit, "", 5},
		{"explicit-provider-disappears", explicitHigh, implicitHigh, "", 5},
		{"effort-changes", implicit, implicitHigh, "", 5},
		{"effective-provider-differs", implicit, implicit, "wrong-provider", 4},
		{"thread-provider-differs", implicit, implicit, "wrong-thread-provider", 4},
		{"reasoning-effort-differs", implicit, implicit, "wrong-effort", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected, config, request := appServerSessionFixture(t)
			t.Setenv("AW_SESSION_CONFIG_BEFORE", tc.before)
			t.Setenv("AW_SESSION_CONFIG_AFTER", tc.after)
			t.Setenv("AW_SESSION_MODE", tc.mode)
			if tc.before == explicitHigh {
				t.Setenv("AW_SESSION_NATIVE_DEFAULT", "") // Keep the original synthetic/high response.
			} else {
				t.Setenv("AW_SESSION_NATIVE_DEFAULT", "1")
			}
			r := executeSessionProfile(t, selected, config, request, nil)
			if r.Outcome != OutcomeUnknown || !r.Started || r.Accepted || r.Terminal != "" || !r.DirectChildReaped || r.NativeRef != nil {
				t.Fatal("ambiguous or changed profile was adopted")
			}
			want := []string{"initialize", "initialized", "config/read", "thread/start", "config/read"}[:tc.wantFrames]
			if methods := sessionProfileMethods(t, request.CWD); !slices.Equal(methods, want) {
				t.Fatalf("wrong fencing point: sent %v; want %v", methods, want)
			}
		})
	}
}

func sessionProfileMethods(t *testing.T, cwd string) []string {
	t.Helper()
	var methods []string
	for _, frames := range sessionClientFrames(t, cwd) {
		for _, frame := range frames {
			methods = append(methods, *frame.Method)
		}
	}
	return methods
}
