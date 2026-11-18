package provider

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPiInvestigationAdapterExactProfileAndDigest(t *testing.T) {
	selected, _, path := memberToolFiles(t, `: > "$0.invoked"; exit 99`)
	bridge, err := filepath.Abs("../../extensions/pi-bridge/index.ts")
	if err != nil {
		t.Fatal(err)
	}
	encode := func(names []string) []byte {
		t.Helper()
		wire, err := json.Marshal(map[string]any{"endpoint": "http://127.0.0.1:1/api/v1/member-tools", "token": memberToolTestToken, "investigation_tools": names})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, wire, 0600); err != nil {
			t.Fatal(err)
		}
		return wire
	}
	const channel, artifact = ReadInvestigationChannelTool, ReadInvestigationArtifactTool
	for _, reads := range [][]string{{channel}, {artifact}, {channel, artifact}} {
		for _, command := range []string{"decide_dispatch", "submit_result"} {
			want := append(slices.Clone(reads), command)
			issued := NativeMemberToolConfig{Path: path, SHA256: sha256.Sum256(encode(want))}
			arg := slices.Clone(want)
			slices.Reverse(arg)
			adapter := NewPiInvestigationAdapter(bridge, issued, arg)
			arg[0] = "bash"
			e, err := adapter.NewExecutor(selected, 0)
			if err != nil || !slices.Equal(e.(piDigestBoundExecutor).memberTools.toolNames, want) {
				t.Fatalf("strict adapter did not freeze %v: %v", want, err)
			}
			if _, err := NewPiMemberToolAdapter(bridge, path, want).NewExecutor(selected, 0); err == nil {
				t.Fatal("ordinary Pi accepted a strict tool set")
			}
			codex := selected
			codex.Name, codex.Version = Codex, "0.166.0"
			if _, err := adapter.Capabilities(codex); err == nil {
				t.Fatal("strict Pi advertised a Codex profile")
			}
			if _, err := NewCodexAppServerMemberToolAdapter(path, want).Capabilities(codex); err == nil {
				t.Fatal("experimental Codex advertised an investigation profile")
			}
		}
	}
	for _, invalid := range [][]string{
		{channel}, {"submit_result"}, {channel, channel, "submit_result"}, {channel, "submit_result", "decide_dispatch"},
		{artifact, "submit_result", "read_channel"}, {channel, "submit_result", "read_work_inputs"}, {channel, "submit_result", "workspace_write"},
	} {
		if _, err := NewPiInvestigationAdapter(bridge, NativeMemberToolConfig{}, invalid).Capabilities(selected); err == nil {
			t.Fatalf("strict Pi accepted partial or mixed profile: %v", invalid)
		}
	}
	want := []string{channel, "submit_result"}
	wire := encode(want)
	if _, err := NewPiInvestigationAdapter(bridge, NativeMemberToolConfig{Path: path}, want).NewExecutor(selected, 0); err == nil {
		t.Fatal("strict config without a digest was accepted")
	}
	adapter := NewPiInvestigationAdapter(bridge, NativeMemberToolConfig{Path: path, SHA256: sha256.Sum256(wire)}, want)
	e, err := adapter.NewExecutor(selected, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(wire, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	request := ExecuteRequest{RunID: "strict-fixture", CWD: t.TempDir(), RuntimeDir: t.TempDir(), Prompt: "synthetic", Model: "fixture/model"}
	if err := os.Chmod(request.RuntimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	if receipt := e.Execute(t.Context(), request, nil); receipt.Started || receipt.Outcome != OutcomeNotStarted || receipt.Reason != "成员工具配置无效或已变化，执行未启动" {
		t.Fatal("strict Pi did not reject changed config bytes before launching")
	}
	if _, err := os.Stat(selected.Path + ".invoked"); !os.IsNotExist(err) {
		t.Fatal("constructor or rejected invocation called Pi")
	}
}
