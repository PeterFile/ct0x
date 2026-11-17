package provider

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func workspaceWorkerProfile() []string {
	return []string{"read_channel", "search_history", "read_history", "submit_result", "workspace_list", "workspace_read", "workspace_write", "publish_workspace_artifact"}
}

func TestPiWorkspaceWorkerCompleteProfileOnly(t *testing.T) {
	selected, extension, config := memberToolFiles(t, `: > "$0.invoked"; exit 99`)
	names := workspaceWorkerProfile()
	slices.Reverse(names)
	executor, err := NewPiMemberToolExecutorWithTools(selected, extension, config, names, time.Second)
	if err != nil || executor == nil {
		t.Fatalf("complete workspace profile rejected: %T %v", executor, err)
	}
	if !slices.Equal(executor.(piExecutor).memberTools.toolNames, workspaceWorkerProfile()) {
		t.Fatal("workspace profile was not canonicalized")
	}
	for _, invalid := range [][]string{
		{"workspace_list"}, {"workspace_read"}, {"workspace_write"}, {"publish_workspace_artifact"},
		append(workspaceWorkerProfile(), "send_message"), append(workspaceWorkerProfile(), "create_member"),
		append(workspaceWorkerProfile(), "decide_dispatch"), append(workspaceWorkerProfile(), "workspace_read"),
		append(workspaceWorkerProfile()[:7], "read_artifact"), workspaceWorkerProfile()[:7],
	} {
		if got, err := NewPiMemberToolExecutorWithTools(selected, extension, config, invalid, 0); got != nil || err != errPiMemberToolConfiguration {
			t.Fatalf("incomplete/mixed workspace profile accepted: %v, %T %v", invalid, got, err)
		}
	}
	if _, err := os.Stat(selected.Path + ".invoked"); !os.IsNotExist(err) {
		t.Fatal("profile selection invoked a CLI")
	}
}

func TestPiWorkspaceWorkerLiteralFrozenArgv(t *testing.T) {
	body := "set -eu\nprintf '%s\\n' \"$@\" > argv\n/bin/cat > stdin\n" + executionHeader + executionEmit(executionStart, executionAssistant("stop"), executionEnd, executionSettled)
	selected, extension, config := memberToolFiles(t, body)
	names := workspaceWorkerProfile()
	executor, err := NewPiMemberToolExecutorWithTools(selected, extension, config, names, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for i := range names {
		names[i] = "bash"
	}
	request := ExecuteRequest{RunID: "run1", CWD: t.TempDir(), RuntimeDir: t.TempDir(), Prompt: "synthetic workspace prompt", Model: "fixture/model"}
	if err := os.Chmod(request.RuntimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	receipt := executor.Execute(context.Background(), request, nil)
	if receipt.Outcome != OutcomeSucceeded || !receipt.OutputComplete || !receipt.DirectChildReaped {
		t.Fatalf("synthetic native receipt: %+v", receipt)
	}
	argv, err := os.ReadFile(filepath.Join(request.CWD, "argv"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n")
	index := slices.Index(args, "--tools")
	if index < 0 || args[index+1] != strings.Join(workspaceWorkerProfile(), ",") || slices.Contains(args, "bash") {
		t.Fatalf("workspace allowlist widened or changed: %q", args)
	}
	if strings.Contains(string(argv), memberToolTestToken) || strings.Contains(string(argv), config) {
		t.Fatal("workspace argv leaked a credential or config path")
	}
}

func TestPiWorkspaceAdapterSamplesExactToolsWithoutLaunch(t *testing.T) {
	selected, extension, config := memberToolFiles(t, `: > "$0.invoked"; exit 99`)
	adapter := NewPiWorkspaceMemberToolAdapter(extension, "")
	caps, err := adapter.Capabilities(selected)
	if err != nil || !caps.Supports(CapabilityPlatformTools) || !caps.Supports(CapabilityTerminalEvidence) || caps.Evidence.Protocol != PiExtensionProtocol {
		t.Fatalf("verified file adapter support missing: %+v %v", caps, err)
	}
	if executor, err := adapter.NewExecutor(selected, time.Second); executor != nil || err == nil {
		t.Fatal("sampling-only adapter constructed without a config")
	}
	executor, err := NewPiWorkspaceMemberToolAdapter(extension, config).NewExecutor(selected, time.Second)
	if err != nil || !slices.Equal(executor.(piExecutor).memberTools.toolNames, workspaceWorkerProfile()) {
		t.Fatal("file adapter constructed a text-only or widened profile")
	}
	selected.Name, selected.Version = Codex, "0.166.0"
	if _, err := adapter.Capabilities(selected); err == nil {
		t.Fatal("Pi file adapter accepted another provider")
	}
	if _, err := os.Stat(selected.Path + ".invoked"); !os.IsNotExist(err) {
		t.Fatal("capability sampling launched a CLI")
	}
}

func TestCodexRejectsWorkspaceWorkerProfile(t *testing.T) {
	selected, _, config := memberToolFiles(t, `: > "$0.invoked"; exit 99`)
	selected.Name, selected.Version = Codex, "0.166.0"
	if executor, err := NewCodexAppServerMemberToolExecutor(selected, config, workspaceWorkerProfile(), time.Second); err == nil || executor != nil {
		t.Fatal("Codex text/readOnly adapter acquired file tools")
	}
	caps, err := NewCodexAppServerMemberToolAdapter("", workspaceWorkerProfile()).Capabilities(selected)
	if err == nil || caps.Supports(CapabilityPlatformTools) {
		t.Fatal("Codex reported file profile support")
	}
	if _, err := os.Stat(selected.Path + ".invoked"); !os.IsNotExist(err) {
		t.Fatal("unsupported Codex profile invoked a CLI")
	}
}
