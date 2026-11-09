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

func TestPiMemberToolsHistoryNamesValidationWithoutLaunch(t *testing.T) {
	selected, extension, config := memberToolFiles(t, `: > "$0.invoked"; exit 99`)
	// Construction must still leave config parsing exclusively to the bridge.
	if err := os.WriteFile(config, []byte("not JSON; "+memberToolTestToken), 0600); err != nil {
		t.Fatal(err)
	}
	for _, names := range [][]string{nil, {}, {""}, {"search_history", "search_history"},
		{"read_history", "read_history"}, {"list_members", "create_task"}, {"Search_history"},
		{"READ_HISTORY"}, {"search_history "}, {" read_history"}, {"read_history\n"}, {"read_history\x00"}} {
		executor, err := NewPiMemberToolExecutorWithTools(selected, extension, config, names, time.Second)
		if executor != nil || err != errPiMemberToolConfiguration {
			t.Errorf("invalid trusted names accepted or leaked: executor=%T, error=%v", executor, err)
		}
	}
	for _, name := range strings.Split(piMemberToolNames+",search_history,read_history", ",") {
		if _, err := NewPiMemberToolExecutorWithTools(selected, extension, config, []string{name}, 0); err != nil {
			t.Errorf("valid singleton %s rejected: %v", name, err)
		}
	}
	if _, err := os.Stat(selected.Path + ".invoked"); !os.IsNotExist(err) {
		t.Fatal("trusted profile constructor launched a CLI")
	}
}

func TestPiMemberToolsHistoryCanonicalArgvAndFrozenSelection(t *testing.T) {
	for _, profile := range []struct {
		name   string
		input  []string
		want   string
		legacy bool
	}{
		{"legacy", nil, piMemberToolNames, true},
		{"unordered-four", []string{"create_member", "read_channel", "list_members", "send_message"}, piMemberToolNames, false},
		{"unordered-six", []string{"read_history", "create_member", "search_history", "send_message", "read_channel", "list_members"}, piMemberToolNames + ",search_history,read_history", false},
		{"history-subset", []string{"read_history", "search_history"}, "search_history,read_history", false},
	} {
		t.Run(profile.name, func(t *testing.T) {
			body := "set -eu\nprintf '%s\\n' \"$@\" > argv\nprintf '%s' \"$AGENT_WORKSPACE_MEMBER_TOOL_CONFIG\" > config-path\n/bin/cat > stdin\n" + executionHeader + executionEmit(executionStart, executionAssistant("stop"), executionEnd, executionSettled)
			selected, extension, config := memberToolFiles(t, body)
			var executor Executor
			var err error
			if profile.legacy {
				executor, err = NewPiMemberToolExecutor(selected, extension, config, 10*time.Second)
			} else {
				executor, err = NewPiMemberToolExecutorWithTools(selected, extension, config, profile.input, 10*time.Second)
			}
			if err != nil {
				t.Fatal(err)
			}
			for i := range profile.input {
				profile.input[i] = "create_task"
			}
			request := ExecuteRequest{RunID: "run1", CWD: t.TempDir(), RuntimeDir: t.TempDir(), Prompt: "synthetic history prompt", Model: "synthetic/model", Thinking: "max"}
			if err := os.Chmod(request.RuntimeDir, 0700); err != nil {
				t.Fatal(err)
			}
			receipt := executor.Execute(context.Background(), request, nil)
			if receipt.Outcome != OutcomeSucceeded || !receipt.OutputComplete || !receipt.DirectChildReaped {
				t.Fatalf("synthetic profile execution failed: %+v", receipt)
			}
			argv, err := os.ReadFile(filepath.Join(request.CWD, "argv"))
			want := []string{"--offline", "--mode", "json", "-p", "--no-extensions", "--no-mcp", "--no-builtin-tools", "--no-skills", "--no-prompt-templates", "--no-context-files", "--no-approve", "--session-dir", filepath.Join(request.RuntimeDir, request.RunID), "--session-id", request.RunID,
				"--tools", profile.want, "--extension", extension, "--model", request.Model, "--thinking", "max"}
			if err != nil || !slices.Equal(strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n"), want) {
				t.Fatalf("argv not canonical or caller mutation widened tools: %s, %v", argv, err)
			}
			path, err := os.ReadFile(filepath.Join(request.CWD, "config-path"))
			if err != nil || string(path) != config {
				t.Fatal("profile changed the config environment handoff")
			}
			for _, path := range []string{filepath.Join(request.CWD, "argv"), filepath.Join(request.CWD, "stdin"), receipt.StdoutPath, receipt.StderrPath} {
				data, err := os.ReadFile(path)
				if err != nil || strings.Contains(string(data), memberToolTestToken) || strings.Contains(string(data), config) {
					t.Fatalf("private profile input leaked or missing output: %v", err)
				}
			}
		})
	}
}
