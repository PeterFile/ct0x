package provider

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiProjectExperienceABI(t *testing.T) {
	selected, _, configPath := memberToolFiles(t, `
[ "$AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256" = "$FIXTURE_SHA" ] || exit 51
prev=
for arg do
  if [ "$prev" = --tools ]; then [ "$arg" = "$FIXTURE_TOOLS" ] || exit 52; fi
  if [ "$prev" = --extension ]; then [ "$arg" = "$FIXTURE_ENTRY" ] || exit 53; fi
  [ "$arg" != "$AGENT_WORKSPACE_MEMBER_TOOL_CONFIG" ] || exit 54
  prev=$arg
done
exit 0`)
	writeProfile := func(names []string, hints map[string]any) NativeMemberToolConfig {
		t.Helper()
		config := map[string]any{"endpoint": "http://127.0.0.1:7310/api/v1/member-tools", "token": "fixture-only-bearer", "project_experience_tools": names}
		for name, value := range hints {
			config[name] = value
		}
		wire, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, append(wire, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		return testIssuedMemberConfig(t, configPath)
	}
	// Private copy: drift tests must not touch the actual trusted bridge.
	dir, entry := t.TempDir(), testPiReleaseBridge(t)
	for _, name := range []string{"index.ts", "bridge.mjs"} {
		data, err := os.ReadFile(filepath.Join(filepath.Dir(entry), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	entry = filepath.Join(dir, "index.ts")
	physical, err := filepath.EvalSymlinks(entry)
	if err != nil || !PiProjectExperienceBridgeSupported(entry) || !PiCombinedIntakeBridgeSupported(entry) || !PiNativeSessionBridgeSupported(entry) {
		t.Fatal("new pair is not exact or lost an existing profile")
	}
	const lookup, artifact = LookupProjectExperiencesTool, ReadArtifactTool
	for _, names := range [][]string{
		nil, {"read_history", lookup}, {"search_history", "read_history", artifact},
		{"search_history", "read_history", lookup, artifact, artifact}, {"search_history", "read_history", artifact, lookup},
		{"search_history", "read_history", lookup, ReadInvestigationArtifactTool},
		{"read_channel", "search_history", "read_history", "submit_result", lookup},
	} {
		if exe, err := NewPiProjectExperienceExecutor(selected, entry, writeProfile(names, nil), 0); exe != nil || err == nil {
			t.Fatalf("invalid fresh tool profile was accepted: %v", names)
		}
	}
	intake := []string{"list_members", "read_channel", "send_message", "search_history", "read_history", lookup}
	for name, value := range map[string]any{"continue_work": true, "intake_assessment_action": "read_only_investigation"} {
		if exe, err := NewPiProjectExperienceExecutor(selected, entry, writeProfile(intake, map[string]any{name: value}), 0); exe != nil || err == nil {
			t.Fatalf("digest-matched experience profile accepted %s", name)
		}
	}
	if exe, err := NewPiProjectExperienceExecutor(selected, entry, writeProfile(intake, map[string]any{"discussion_only_excerpt": true}), 0); exe == nil || err != nil {
		t.Fatalf("experience profile rejected the discussion hint: %v", err)
	}
	approved := []string{"search_history", "read_history", lookup, artifact}
	issued := writeProfile(approved, nil)
	badDigest := issued
	badDigest.SHA256[0] ^= 1
	if exe, err := NewPiProjectExperienceExecutor(selected, entry, badDigest, 0); exe != nil || err == nil {
		t.Fatal("changed config digest was accepted")
	}
	if _, err := NewPiMemberToolAdapter(entry, configPath, approved[:2]).NewExecutor(selected, 0); err == nil {
		t.Fatal("legacy launcher accepted the fresh manifest on its path-only bootstrap")
	}
	exe, err := NewPiProjectExperienceExecutor(selected, entry, issued, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIXTURE_SHA", hex.EncodeToString(issued.SHA256[:]))
	t.Setenv("FIXTURE_TOOLS", strings.Join(approved, ","))
	t.Setenv("FIXTURE_ENTRY", physical)
	request := ExecuteRequest{RunID: "fresh", CWD: piSessionPrivateTemp(t), RuntimeDir: piSessionPrivateTemp(t), Model: "fixture/model", Prompt: "offline"}
	if got := exe.Execute(t.Context(), request, nil); !got.Started || !got.DirectChildReaped || got.ExitCode != 0 || got.NativeRef != nil {
		t.Fatal("the offline child did not receive the exact fresh profile")
	}
	impl := filepath.Join(dir, "bridge.mjs")
	source, err := os.ReadFile(impl)
	if err != nil {
		t.Fatal(err)
	}
	// Even a drift that preserves the new guard must not qualify.
	if err := os.WriteFile(impl, append(source, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	request.RunID = "changed"
	if got := exe.Execute(t.Context(), request, nil); got.Started {
		t.Fatal("a changed parser crossed the invocation fence")
	}
}
