package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

const codexHeader = `{"type":"thread.started","thread_id":"native-thread"}`
const codexStart = `{"type":"turn.started"}`
const codexResponse = `{"type":"item.completed","item":{"id":"answer","type":"agent_message","text":"合成\u2028回答"}}`
const codexCompleted = `{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":2}}`
const codexFailed = `{"type":"turn.failed","error":{"message":"RAW-NATIVE-SECRET"}}`

func codexFixture(t *testing.T, body string) (Executor, ExecuteRequest) {
	t.Helper()
	base, r := executionFixture(t, body)
	e, err := NewCodexExecutor(base.(piExecutor).path, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return e, r
}

func TestCodexExecutionLiteralInputsAndImmediatePrivateDelivery(t *testing.T) {
	wait := func(file string) string { return "while [ ! -f " + file + " ]; do /bin/sleep 0.01; done\n" }
	body := "echo invocation >> invocations\nprintf '%s\\n' \"$@\" > argv\npwd > cwd\n/bin/cat > stdin\nprintf '%s' \"$CODEX_HOME\" > native-home\nprintf 'RAW-STDERR-SECRET' >&2\n"
	body += executionEmit(codexHeader) + wait("session") + executionEmit(codexStart) + wait("accepted")
	body += executionEmit(`{"type":"item.completed","item":{"id":"first","type":"agent_message","text":"earlier snapshot"}}`)
	for _, kind := range []string{"reasoning", "command_execution", "file_change", "mcp_tool_call"} {
		body += executionEmit(fmt.Sprintf(`{"type":"item.completed","item":{"id":"private","type":%q,"text":"RAW-ITEM-SECRET"}}`, kind))
	}
	body += executionEmit(codexResponse, codexCompleted) + "echo exited > exited\n"
	e, r := codexFixture(t, body)
	r.CWD = filepath.Join(r.CWD, "workspace ; $(literal)")
	if err := os.Mkdir(r.CWD, 0700); err != nil {
		t.Fatal(err)
	}
	nativeHome := t.TempDir()
	t.Setenv("CODEX_HOME", nativeHome)
	r.Model, r.Prompt = "synthetic/model;$(touch model-marker)", "@secret-file\n$(touch prompt-marker) ; --help\n"
	var kinds []EventKind
	var snapshots []string
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := e.Execute(ctx, r, func(event Event) error {
		kinds = append(kinds, event.Kind)
		if event.NativeSessionID != "native-thread" {
			t.Errorf("unbound native identity: %+v", event)
		}
		if event.Kind == SessionEvent || event.Kind == AcceptedEvent {
			if _, err := os.Stat(filepath.Join(r.CWD, "exited")); !os.IsNotExist(err) {
				t.Error("callback buffered until exit")
			}
			return os.WriteFile(filepath.Join(r.CWD, string(event.Kind)), nil, 0600)
		}
		if event.Kind == ResponseEvent {
			snapshots = append(snapshots, event.Text)
		}
		return nil
	})
	if got.Outcome != OutcomeSucceeded || got.Terminal != OutcomeSucceeded || !got.Started || !got.Accepted || !got.DirectChildReaped || !got.OutputComplete || got.NativeSessionID == r.RunID || !reflect.DeepEqual(kinds, []EventKind{SessionEvent, AcceptedEvent, ResponseEvent, ResponseEvent, TerminalEvent}) || !reflect.DeepEqual(snapshots, []string{"earlier snapshot", "合成\u2028回答"}) {
		t.Fatalf("incorrect delivery: %+v, %v, %v", got, kinds, snapshots)
	}
	args, _ := os.ReadFile(filepath.Join(r.CWD, "argv"))
	want := []string{"--ask-for-approval", "never", "exec", "--json", "--sandbox", "read-only", "--ignore-user-config", "--ephemeral", "--cd", r.CWD, "--skip-git-repo-check", "--color", "never", "--model", r.Model, "-"}
	if !reflect.DeepEqual(strings.Split(strings.TrimSuffix(string(args), "\n"), "\n"), want) {
		t.Fatalf("unsafe/nonliteral argv: %s", args)
	}
	for name, want := range map[string]string{"stdin": r.Prompt, "cwd": r.CWD + "\n", "native-home": nativeHome} {
		data, err := os.ReadFile(filepath.Join(r.CWD, name))
		if err != nil || string(data) != want {
			t.Fatalf("nonliteral %s: %q, %v", name, data, err)
		}
	}
	before := map[string]string{}
	for _, path := range []string{got.StdoutPath, got.StderrPath, filepath.Dir(got.StdoutPath)} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0077 != 0 || filepath.Dir(got.StdoutPath) != filepath.Join(r.RuntimeDir, r.RunID) {
			t.Fatalf("nonprivate/noncontained logs: %s, %v", path, err)
		}
		if !info.IsDir() {
			data, _ := os.ReadFile(path)
			before[path] = string(data)
		}
	}
	again := e.Execute(context.Background(), r, nil)
	if again.Outcome != OutcomeUnknown || again.Started || again.Accepted || again.Terminal != "" || again.NativeSessionID != "" {
		t.Fatalf("historical run was retried or guessed: %+v", again)
	}
	before[filepath.Join(r.CWD, "invocations")] = "invocation\n"
	for path, want := range before {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("duplicate run overwrote state: %s, %v", path, err)
		}
	}
	for _, name := range []string{"model-marker", "prompt-marker"} {
		if _, err := os.Stat(filepath.Join(r.CWD, name)); !os.IsNotExist(err) {
			t.Fatal("shell interpreted model or prompt")
		}
	}
}

func TestCodexExecutionRequiresTrustworthyBoundTerminal(t *testing.T) {
	for _, tc := range []struct {
		name, records string
		exit          int
		want          Outcome
	}{
		{"completed", executionEmit(codexHeader, codexStart, codexResponse, codexCompleted), 0, OutcomeSucceeded},
		{"failed-nonzero", executionEmit(codexHeader, codexStart, codexFailed), 7, OutcomeFailed},
		{"failed-zero", executionEmit(codexHeader, codexStart, codexFailed), 0, OutcomeFailed},
		{"success-nonzero", executionEmit(codexHeader, codexStart, codexCompleted), 7, OutcomeUnknown},
		{"exit-only", "", 0, OutcomeUnknown},
		{"header-only", executionEmit(codexHeader), 0, OutcomeUnknown},
		{"missing-terminal", executionEmit(codexHeader, codexStart, codexResponse), 0, OutcomeUnknown},
		{"standalone-error", executionEmit(codexHeader, codexStart, `{"type":"error","message":"RAW-NATIVE-SECRET"}`), 7, OutcomeUnknown},
		{"retry-diagnostic", executionEmit(codexHeader, codexStart, `{"type":"error","message":"retrying"}`, codexResponse, codexCompleted), 0, OutcomeSucceeded},
		{"missing-id", executionEmit(`{"type":"thread.started"}`, codexStart, codexCompleted), 0, OutcomeUnknown},
		{"invalid-id", executionEmit(`{"type":"thread.started","thread_id":42}`, codexStart, codexCompleted), 0, OutcomeUnknown},
		{"blank-id", executionEmit(`{"type":"thread.started","thread_id":" "}`, codexStart, codexCompleted), 0, OutcomeUnknown},
		{"rebind", executionEmit(codexHeader, `{"type":"thread.started","thread_id":"other"}`, codexStart, codexCompleted), 0, OutcomeUnknown},
		{"start-before-header", executionEmit(codexStart, codexHeader, codexStart, codexCompleted), 0, OutcomeUnknown},
		{"response-before-header", executionEmit(codexResponse, codexHeader, codexStart, codexCompleted), 0, OutcomeUnknown},
		{"response-before-start", executionEmit(codexHeader, codexResponse, codexStart, codexCompleted), 0, OutcomeUnknown},
		{"terminal-before-start", executionEmit(codexHeader, codexCompleted), 0, OutcomeUnknown},
		{"duplicate-start", executionEmit(codexHeader, codexStart, codexStart, codexCompleted), 0, OutcomeUnknown},
		{"missing-text", executionEmit(codexHeader, codexStart, `{"type":"item.completed","item":{"id":"bad","type":"agent_message"}}`, codexCompleted), 0, OutcomeUnknown},
		{"missing-usage", executionEmit(codexHeader, codexStart, `{"type":"turn.completed"}`), 0, OutcomeUnknown},
		{"malformed-usage", executionEmit(codexHeader, codexStart, `{"type":"turn.completed","usage":{"input_tokens":-1}}`), 0, OutcomeUnknown},
		{"malformed-failure", executionEmit(codexHeader, codexStart, `{"type":"turn.failed","error":{}}`), 7, OutcomeUnknown},
		{"contradiction", executionEmit(codexHeader, codexStart, codexFailed, codexCompleted), 7, OutcomeUnknown},
		{"late-response", executionEmit(codexHeader, codexStart, codexCompleted, codexResponse), 0, OutcomeUnknown},
		{"late-start", executionEmit(codexHeader, codexStart, codexCompleted, codexStart), 0, OutcomeUnknown},
		{"invalid-utf8", executionEmit(codexHeader, codexStart, strings.Replace(codexResponse, "合成", "\xff", 1), codexCompleted), 0, OutcomeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, r := codexFixture(t, tc.records+fmt.Sprintf("exit %d\n", tc.exit))
			bound, accepted, terminal := false, false, false
			got := e.Execute(context.Background(), r, func(event Event) error {
				if terminal || event.NativeSessionID != "native-thread" || (event.Kind != SessionEvent && !bound) || (event.Kind == ResponseEvent && !accepted) {
					t.Errorf("late/unbound event: %+v", event)
				}
				bound = bound || event.Kind == SessionEvent
				accepted = accepted || event.Kind == AcceptedEvent
				terminal = terminal || event.Kind == TerminalEvent
				return nil
			})
			if got.Outcome != tc.want || !got.Started || !got.DirectChildReaped || got.ExitCode != tc.exit || got.Accepted != accepted || (tc.want != OutcomeUnknown && (!got.OutputComplete || got.Terminal != tc.want)) || (tc.name == "contradiction" && got.Terminal != OutcomeFailed) || strings.Contains(got.Reason, "RAW-") {
				t.Fatalf("untrustworthy lifecycle: %+v", got)
			}
		})
	}
}

func TestCodexExecutionBoundsAndDrainsOutput(t *testing.T) {
	for _, mode := range []string{"stderr", "noise", "partial"} {
		t.Run(mode, func(t *testing.T) {
			body := executionEmit(codexHeader, codexStart) + "printf 'RAW-STDERR-SECRET' >&2\n/usr/bin/head -c 2097152 /dev/zero >&2\n"
			if mode == "noise" {
				body += "/usr/bin/head -c 2097152 /dev/zero | /usr/bin/tr '\\000' x\nprintf '\\nnoise\\n'\n"
			}
			body += executionEmit(codexResponse)
			if mode == "partial" {
				body += "printf '%s' '" + codexCompleted + "'"
			} else {
				body += executionEmit(codexCompleted)
			}
			e, r := codexFixture(t, body)
			responses := 0
			got := e.Execute(context.Background(), r, func(event Event) error {
				if event.Kind == ResponseEvent {
					responses++
					if event.Text != "合成\u2028回答" {
						t.Errorf("private/unbounded response: %+v", event)
					}
				}
				return nil
			})
			if responses != 1 || !got.LogsTruncated || !got.DirectChildReaped || (got.Outcome == OutcomeSucceeded) != (mode == "stderr") || got.Outcome == OutcomeNotStarted {
				t.Fatalf("output not bounded/drained: %+v", got)
			}
			data, _ := json.Marshal(got)
			if strings.Contains(string(data), "RAW-") {
				t.Fatal("diagnostics leaked into receipt")
			}
			for _, path := range []string{got.StdoutPath, got.StderrPath} {
				info, err := os.Stat(path)
				if err != nil || info.Size() > executionLogLimit {
					t.Fatalf("unbounded log: %s, %v", path, err)
				}
			}
		})
	}
}

func TestCodexExecutionNonzeroExitDoesNotProvePipeCompletion(t *testing.T) {
	for _, mode := range []string{"stdout", "stderr", "late-terminal"} {
		t.Run(mode, func(t *testing.T) {
			child := "/bin/sleep 10 &\n"
			if mode == "stdout" {
				child = "/bin/sleep 10 2>/dev/null &\n"
			} else if mode == "stderr" {
				child = "/bin/sleep 10 >/dev/null &\n"
			} else {
				child = "(/bin/sleep 0.3\n" + executionEmit(codexFailed) + "exec /bin/sleep 10) &\n"
			}
			body := executionEmit(codexHeader, codexStart) + child + "echo $! > pid\n"
			if mode != "late-terminal" {
				body += executionEmit(codexFailed)
			}
			e, r := codexFixture(t, body+"exit 7\n")
			calls, start := 0, time.Now()
			got := e.Execute(context.Background(), r, func(Event) error { calls++; return nil })
			data, _ := os.ReadFile(filepath.Join(r.CWD, "pid"))
			var pid int
			if _, err := fmt.Sscan(string(data), &pid); err != nil || pid <= 0 {
				t.Fatalf("missing fixture descendant: %q, %v", data, err)
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			if got.Outcome != OutcomeUnknown || got.OutputComplete || !got.DirectChildReaped || got.ExitCode != 7 || time.Since(start) > time.Second || (mode != "late-terminal" && got.Terminal != OutcomeFailed) {
				t.Fatalf("ExitError masked incomplete transport: %+v", got)
			}
			if err := syscall.Kill(pid, 0); err != nil {
				t.Fatalf("descendant must remain unproven: %v", err)
			}
			before := calls
			time.Sleep(350 * time.Millisecond)
			if calls != before {
				t.Fatal("callback after Execute returned")
			}
		})
	}
}

func TestCodexExecutionCallbackFailureAndCancellation(t *testing.T) {
	for _, at := range []EventKind{SessionEvent, AcceptedEvent, ResponseEvent, TerminalEvent, "cancel-accepted", "cancel-terminal"} {
		t.Run(string(at), func(t *testing.T) {
			e, r := codexFixture(t, "echo invocation >> invocations\n"+executionEmit(codexHeader, codexStart, codexResponse, codexCompleted)+"exec /bin/sleep 10\n")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			stopped, start := false, time.Now()
			got := e.Execute(ctx, r, func(event Event) error {
				if stopped {
					t.Error("callback after failure/cancellation")
				}
				if event.Kind == at || string(at) == "cancel-"+string(event.Kind) {
					stopped = true
					if strings.HasPrefix(string(at), "cancel-") {
						cancel()
						return nil
					}
					return errors.New("RAW-CALLBACK-SECRET")
				}
				return nil
			})
			calls, _ := os.ReadFile(filepath.Join(r.CWD, "invocations"))
			if !stopped || got.Outcome != OutcomeUnknown || got.DeliveryFailed != !strings.HasPrefix(string(at), "cancel-") || string(calls) != "invocation\n" || time.Since(start) > time.Second || strings.Contains(got.Reason, "RAW-") || ((at == TerminalEvent || at == "cancel-terminal") && got.Terminal != OutcomeSucceeded) {
				t.Fatalf("unsafe callback/cancellation handling: %+v", got)
			}
		})
	}
}

func TestCodexExecutionConfigurationAndTimeout(t *testing.T) {
	if _, err := NewCodexExecutor("codex", 0); err == nil {
		t.Fatal("relative executable accepted")
	}
	e, r := codexFixture(t, "printf '%s\\n' \"$@\" > argv\n"+executionEmit(codexHeader, codexStart, codexCompleted))
	r.Model = ""
	if got := e.Execute(context.Background(), r, nil); got.Outcome != OutcomeSucceeded {
		t.Fatalf("optional model failed: %+v", got)
	}
	args, _ := os.ReadFile(filepath.Join(r.CWD, "argv"))
	if strings.Contains(string(args), "--model") {
		t.Fatal("empty model must not override native selection")
	}
	e, r = codexFixture(t, executionEmit(codexHeader, codexStart)+"exec /bin/sleep 10\n")
	argsPath := e.(codexExecutor).path
	e, _ = NewCodexExecutor(argsPath, 100*time.Millisecond)
	start := time.Now()
	if got := e.Execute(context.Background(), r, nil); got.Outcome != OutcomeUnknown || !got.Accepted || time.Since(start) > time.Second {
		t.Fatalf("configured timeout ignored: %+v", got)
	}
}
