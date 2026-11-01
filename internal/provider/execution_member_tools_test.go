package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

const memberToolConfigEnv = "AGENT_WORKSPACE_MEMBER_TOOL_CONFIG"
const memberToolTestToken = "SYNTHETIC-MEMBER-TOKEN-MUST-NOT-LEAK"
const memberToolTestConfig = `{"endpoint":"http://127.0.0.1:1/api/v1/member-tools","token":"` + memberToolTestToken + `"}` + "\n"

func memberToolFiles(t *testing.T, body string) (Provider, string, string) {
	t.Helper()
	root := t.TempDir()
	path := executable(t, filepath.Join(root, "selected pi ; executable"), body)
	extension := filepath.Join(root, "trusted entry ; $(touch extension-marker).ts")
	config := filepath.Join(root, "private config ; $(touch config-marker).json")
	for path, data := range map[string]string{extension: "export default function () {}\n", config: memberToolTestConfig} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return Provider{ID: "selected", Name: Pi, Path: path, Version: "1.4.0", Installation: Installed,
		Account: AccountUnknown, Connectivity: Unchecked, Capabilities: Capabilities{Tools: true, Resume: true},
		Evidence: []Evidence{{Reason: "unchanged"}}}, extension, config
}

func memberToolFixture(t *testing.T, body string) (Executor, ExecuteRequest, string, string) {
	t.Helper()
	selected, extension, config := memberToolFiles(t, body)
	e, err := NewPiMemberToolExecutor(selected, extension, config, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	request := ExecuteRequest{RunID: "run1", CWD: t.TempDir(), RuntimeDir: t.TempDir(), Prompt: "synthetic prompt", Model: "synthetic/model"}
	if err := os.Chmod(request.RuntimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	return e, request, extension, config
}

func TestPiMemberToolsConstructionDoesNotProbeOrChangeMetadata(t *testing.T) {
	selected, extension, config := memberToolFiles(t, `: > "$0.invoked"; exit 99`)
	before := selected
	before.Evidence = append([]Evidence(nil), selected.Evidence...)
	for _, timeout := range []time.Duration{0, 3 * time.Second} {
		e, err := NewPiMemberToolExecutor(selected, extension, config, timeout)
		if err != nil || e == nil {
			t.Fatalf("verified Pi rejected: %T, %v", e, err)
		}
		command := e.(piExecutor).executionCommand
		wantTimeout := timeout
		if wantTimeout == 0 {
			wantTimeout = 10 * time.Minute
		}
		if command.path != selected.Path || command.timeout != wantTimeout || !reflect.DeepEqual(selected, before) {
			t.Fatalf("binding, default timeout or selected metadata changed: %+v, %+v", command, selected)
		}
	}
	// The bridge owns JSON validation and the sole content read, not the provider.
	if err := os.WriteFile(config, []byte("not JSON; "+memberToolTestToken), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPiMemberToolExecutor(selected, extension, config, 0); err != nil {
		t.Fatalf("provider interpreted config contents: %v", err)
	}
	if _, err := os.Stat(selected.Path + ".invoked"); !os.IsNotExist(err) {
		t.Fatalf("constructor probed executable: %v", err)
	}
}

func TestPiMemberToolsRejectsUnverifiedProvider(t *testing.T) {
	for _, name := range []string{"codex", "unknown", "blank-id", "not-installed", "version-missing", "version-prefix", "version-old", "version-new", "path-relative", "path-nul", "path-missing", "path-directory", "path-not-executable", "negative-timeout"} {
		t.Run(name, func(t *testing.T) {
			selected, extension, config := memberToolFiles(t, `: > "$0.invoked"; exit 99`)
			path, timeout := selected.Path, time.Duration(0)
			switch name {
			case "codex":
				selected.Name, selected.Version = Codex, "0.162.1"
			case "unknown":
				selected.Name = "other"
			case "blank-id":
				selected.ID = " "
			case "not-installed":
				selected.Installation = "absent"
			case "version-missing":
				selected.Version = ""
			case "version-prefix":
				selected.Version = "v1.4.0"
			case "version-old":
				selected.Version = "1.3.0"
			case "version-new":
				selected.Version = "1.4.1"
			case "path-relative":
				selected.Path = "pi"
			case "path-nul":
				selected.Path += "\x00"
			case "path-missing":
				selected.Path += memberToolTestToken
			case "path-directory":
				selected.Path = t.TempDir()
			case "path-not-executable":
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "negative-timeout":
				timeout = -time.Second
			}
			e, err := NewPiMemberToolExecutor(selected, extension, config, timeout)
			if e != nil || err == nil || err.Error() != "提供方执行配置无效或版本不受支持" {
				t.Fatalf("unsupported selection accepted or leaked: %T, %v", e, err)
			}
			if _, err := os.Stat(path + ".invoked"); !os.IsNotExist(err) {
				t.Fatal("rejected selection was probed")
			}
		})
	}
}

func TestPiMemberToolsRejectsUnsafeFiles(t *testing.T) {
	for _, field := range []string{"extension", "config"} {
		for _, invalid := range []string{"empty", "relative", "nul", "missing", "directory", "fifo", "unreadable", "symlink", "trailing-symlink", "public", "group", "executable", "special"} {
			if field == "extension" && (strings.Contains(invalid, "symlink") || invalid == "public" || invalid == "group" || invalid == "executable" || invalid == "special") {
				continue // Extension targets may be readable regular files; only config requires private non-symlink metadata.
			}
			t.Run(field+"/"+invalid, func(t *testing.T) {
				selected, extension, config := memberToolFiles(t, `: > "$0.invoked"; exit 99`)
				path := &extension
				if field == "config" {
					path = &config
				}
				switch invalid {
				case "empty":
					*path = ""
				case "relative":
					*path = "relative"
				case "nul":
					*path += "\x00"
				case "missing":
					*path += memberToolTestToken
				case "directory":
					*path = t.TempDir()
				case "fifo":
					*path += ".fifo"
					if err := syscall.Mkfifo(*path, 0600); err != nil {
						t.Fatal(err)
					}
				case "symlink", "trailing-symlink":
					link := *path + ".link"
					if err := os.Symlink(*path, link); err != nil {
						t.Fatal(err)
					}
					*path = link
					if invalid == "trailing-symlink" {
						*path += "/"
					}
				default:
					mode := map[string]os.FileMode{"unreadable": 0000, "public": 0644, "group": 0660, "executable": 0700, "special": 0600 | os.ModeSetuid}[invalid]
					if err := os.Chmod(*path, mode); err != nil {
						t.Fatal(err)
					}
				}
				e, err := NewPiMemberToolExecutor(selected, extension, config, 0)
				if e != nil || err == nil || err.Error() != "成员工具执行配置无效" {
					t.Fatalf("unsafe file accepted or leaked: %T, %v", e, err)
				}
				if _, err := os.Stat(selected.Path + ".invoked"); !os.IsNotExist(err) {
					t.Fatal("unsafe file validation invoked executable")
				}
			})
		}
	}
}

func TestPiMemberToolsLiteralArgumentsAndChildOnlyConfig(t *testing.T) {
	t.Setenv(memberToolConfigEnv, "inherited-untrusted-config")
	body := "set -eu\nprintf '%s\\n' \"$@\" > argv\nprintf '%s' \"$AGENT_WORKSPACE_MEMBER_TOOL_CONFIG\" > config-path\n/bin/cat > stdin\n" + executionHeader + executionEmit(executionStart, executionAssistant("stop"), executionEnd, executionSettled)
	e, request, extension, config := memberToolFixture(t, body)
	request.Thinking = "max"
	request.Prompt = "@secret-file\n$(touch prompt-marker) ; --help\n"
	request.Model = "synthetic/model;$(touch model-marker)"
	var events []Event
	got := e.Execute(context.Background(), request, func(event Event) error { events = append(events, event); return nil })
	if got.Outcome != OutcomeSucceeded || !got.OutputComplete || !got.DirectChildReaped {
		t.Fatalf("tools execution failed: %+v", got)
	}
	argv, err := os.ReadFile(filepath.Join(request.CWD, "argv"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--offline", "--mode", "json", "-p", "--no-extensions", "--no-mcp", "--no-builtin-tools", "--no-skills", "--no-prompt-templates", "--no-context-files", "--no-approve", "--session-dir", filepath.Join(request.RuntimeDir, request.RunID), "--session-id", request.RunID,
		"--tools", "list_members,read_channel,send_message,create_member", "--extension", extension, "--model", request.Model, "--thinking", "max"}
	if !reflect.DeepEqual(strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n"), want) {
		t.Fatalf("unexpected tools argv: %s", argv)
	}
	for path, want := range map[string]string{filepath.Join(request.CWD, "config-path"): config, filepath.Join(request.CWD, "stdin"): request.Prompt, config: memberToolTestConfig} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("literal input/config changed at %s: %v", path, err)
		}
	}
	for _, name := range []string{"extension-marker", "config-marker", "prompt-marker", "model-marker"} {
		if _, err := os.Stat(filepath.Join(request.CWD, name)); !os.IsNotExist(err) {
			t.Fatal("shell interpreted trusted path or model input")
		}
	}
	if os.Getenv(memberToolConfigEnv) != "inherited-untrusted-config" {
		t.Fatal("parent environment changed")
	}
	encoded, err := json.Marshal(struct {
		Receipt ExecutionReceipt
		Events  []Event
	}{got, events})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{got.StdoutPath, got.StderrPath, filepath.Join(request.CWD, "argv"), filepath.Join(request.CWD, "stdin")} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		encoded = append(encoded, data...)
	}
	if strings.Contains(string(encoded), memberToolTestToken) || strings.Contains(string(encoded), "http://127.0.0.1:1/api/v1/member-tools") {
		t.Fatal("config contents entered argv, prompt, logs, events or receipt")
	}
}

func TestPiMemberToolsEnvironmentReplacesDuplicatesWithoutMutation(t *testing.T) {
	inherited := []string{"KEEP=literal=value", memberToolConfigEnv + "=first", "AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_EXTRA=keep", memberToolConfigEnv + "=second"}
	before := append([]string(nil), inherited...)
	got := piMemberToolEnvironment(inherited, "/trusted/private.json")
	want := []string{"KEEP=literal=value", "AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_EXTRA=keep", memberToolConfigEnv + "=/trusted/private.json"}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(inherited, before) {
		t.Fatalf("environment did not replace only the exact key in a fresh child snapshot: %v", got)
	}
}
