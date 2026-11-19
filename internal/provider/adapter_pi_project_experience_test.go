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
	writeProfile := func(names []string) NativeMemberToolConfig {
		t.Helper()
		wire, err := json.Marshal(map[string]any{"endpoint": "http://127.0.0.1:7310/api/v1/member-tools", "token": "fixture-only-bearer", "project_experience_tools": names})
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
		if exe, err := NewPiProjectExperienceExecutor(selected, entry, writeProfile(names), 0); exe != nil || err == nil {
			t.Fatalf("invalid fresh tool profile was accepted: %v", names)
		}
	}
	approved := []string{"search_history", "read_history", lookup, artifact}
	issued := writeProfile(approved)
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
	if err := os.WriteFile(filepath.Join(dir, "bridge.mjs"), []byte("// not the qualified parser"), 0600); err != nil {
		t.Fatal(err)
	}
	request.RunID = "changed"
	if got := exe.Execute(t.Context(), request, nil); got.Started {
		t.Fatal("a changed parser crossed the invocation fence")
	}
}
