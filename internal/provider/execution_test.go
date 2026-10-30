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

const executionHeader = `printf '{"type":"session","version":3,"id":"run1","cwd":"%s"}\n' "$PWD"` + "\n"
const executionStart = `{"type":"agent_start"}`
const executionEnd = `{"type":"agent_end","messages":[],"willRetry":false}`
const executionSettled = `{"type":"agent_settled","aborted":false}`

func executionEmit(records ...string) string {
	return "printf '%s\\n' '" + strings.Join(records, "' '") + "'\n"
}

func executionAssistant(reason string) string {
	return fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"thinking","thinking":"synthetic-private"},{"type":"text","text":"合成\u2028回答"}],"api":"synthetic","provider":"synthetic","model":"fixture","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":%q,"timestamp":1}}`, reason)
}

func executionFixture(t *testing.T, body string) (Executor, ExecuteRequest) {
	t.Helper()
	path := executable(t, filepath.Join(t.TempDir(), "fake pi ; executable"), body)
	e, err := NewPiExecutor(path, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r := ExecuteRequest{RunID: "run1", CWD: t.TempDir(), RuntimeDir: t.TempDir(), Model: "synthetic/model", Prompt: "synthetic prompt"}
	if err := os.Chmod(r.RuntimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	return e, r
}

func TestExecutionLiteralInputsAndPrivateExclusiveLogs(t *testing.T) {
	header := `while [ "$1" != "--session-id" ]; do shift; done
printf '{"type":"session","version":3,"id":"%s","cwd":"%s"}\n' "$2" "$PWD"
`
	e, r := executionFixture(t, "echo invocation >> invocations\nprintf 'synthetic stderr\\n' >&2\nprintf '%s\\n' \"$@\" > argv\npwd > cwd\n/bin/cat > stdin\n"+header+executionEmit(executionStart, executionAssistant("stop"), executionSettled))
	r.Model = "synthetic/model;$(touch model-marker)"
	r.Prompt = "@secret-file\n$(touch prompt-marker) ; --help\n"
	got := e.Execute(context.Background(), r, nil)
	if got.Outcome != OutcomeSucceeded || !got.Started || !got.Accepted || !got.DirectChildReaped || !got.OutputComplete {
		t.Fatalf("literal execution failed: %+v", got)
	}
	args, _ := os.ReadFile(filepath.Join(r.CWD, "argv"))
	want := []string{"--offline", "--mode", "json", "-p", "--no-extensions", "--no-mcp", "--no-tools", "--no-skills", "--no-prompt-templates", "--no-context-files", "--no-approve", "--session-dir", filepath.Join(r.RuntimeDir, r.RunID), "--session-id", r.RunID, "--model", r.Model}
	if !reflect.DeepEqual(strings.Split(strings.TrimSuffix(string(args), "\n"), "\n"), want) {
		t.Fatalf("unexpected argv: %s", args)
	}
	for file, want := range map[string]string{"stdin": r.Prompt, "cwd": r.CWD + "\n"} {
		data, err := os.ReadFile(filepath.Join(r.CWD, file))
		if err != nil || string(data) != want {
			t.Fatalf("%s was not literal: %q, %v", file, data, err)
		}
	}
	for _, name := range []string{"model-marker", "prompt-marker"} {
		if _, err := os.Stat(filepath.Join(r.CWD, name)); !os.IsNotExist(err) {
			t.Fatal("shell interpreted input")
		}
	}
	before := make(map[string][]byte)
	for _, path := range []string{got.StdoutPath, got.StderrPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = data
	}
	for _, path := range []string{filepath.Dir(got.StdoutPath), got.StdoutPath, got.StderrPath} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0077 != 0 || filepath.Dir(got.StdoutPath) != filepath.Join(r.RuntimeDir, r.RunID) {
			t.Fatalf("logs must be private and contained: %s, %v", path, err)
		}
	}
	again := e.Execute(context.Background(), r, nil)
	if again.Started || again.Accepted || again.Outcome != OutcomeUnknown || again.Terminal != "" || again.NativeSessionID != "" {
		t.Fatalf("existing run identity was treated as safe to retry: %+v", again)
	}
	for path, want := range before {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != string(want) {
			t.Fatalf("existing log changed: %s, %v", path, err)
		}
	}
	calls, err := os.ReadFile(filepath.Join(r.CWD, "invocations"))
	if err != nil || string(calls) != "invocation\n" {
		t.Fatalf("existing run invoked the executable again: %q, %v", calls, err)
	}
	r.RunID = "run2"
	other := e.Execute(context.Background(), r, nil)
	if !other.Started || !other.Accepted || other.Outcome != OutcomeSucceeded || other.StdoutPath == got.StdoutPath {
		t.Fatalf("distinct runs must execute with distinct logs: %+v", other)
	}
}

func TestExecutionThinkingLiteralArguments(t *testing.T) {
	for _, thinking := range []string{"", "off", "minimal", "low", "medium", "high", "xhigh", "max"} {
		t.Run("thinking="+thinking, func(t *testing.T) {
			e, r := executionFixture(t, "printf '%s\\n' \"$@\" > argv\n"+executionHeader+executionEmit(executionStart, executionAssistant("stop"), executionSettled))
			r.Thinking = thinking
			if got := e.Execute(context.Background(), r, nil); got.Outcome != OutcomeSucceeded {
				t.Fatalf("thinking execution failed: %+v", got)
			}
			args, err := os.ReadFile(filepath.Join(r.CWD, "argv"))
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"--offline", "--mode", "json", "-p", "--no-extensions", "--no-mcp", "--no-tools", "--no-skills", "--no-prompt-templates", "--no-context-files", "--no-approve", "--session-dir", filepath.Join(r.RuntimeDir, r.RunID), "--session-id", r.RunID, "--model", r.Model}
			if thinking != "" {
				want = append(want, "--thinking", thinking)
			}
			if !reflect.DeepEqual(strings.Split(strings.TrimSuffix(string(args), "\n"), "\n"), want) {
				t.Fatalf("thinking was not passed literally or default argv changed: %s", args)
			}
		})
	}
}

func TestExecutionRejectsInvalidThinkingBeforeStart(t *testing.T) {
	for _, thinking := range []string{"invalid", "MAX", " max", "max ", "max\n--model other", "max\x00", "max;$(touch thinking-marker)"} {
		t.Run(thinking, func(t *testing.T) {
			e, r := executionFixture(t, "echo invoked > invoked\n"+executionHeader+executionEmit(executionStart, executionAssistant("stop"), executionSettled))
			r.Thinking = thinking
			callbacks := 0
			got := e.Execute(context.Background(), r, func(Event) error { callbacks++; return nil })
			if got.Outcome != OutcomeNotStarted || got.Started || got.Accepted || got.NativeSessionID != "" || got.Reason == "" || callbacks != 0 {
				t.Errorf("invalid thinking was not rejected before start: %+v", got)
			}
			for _, path := range []string{filepath.Join(r.CWD, "invoked"), filepath.Join(r.CWD, "thinking-marker"), filepath.Join(r.RuntimeDir, r.RunID)} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Error("invalid thinking created state or invoked the executable")
				}
			}
		})
	}
}

func TestCodexExecutionRejectsUnsupportedThinkingBeforeStart(t *testing.T) {
	for _, thinking := range []string{"max", "off", "invalid"} {
		t.Run(thinking, func(t *testing.T) {
			e, r := codexFixture(t, "echo invoked > invoked\n"+executionEmit(codexHeader, codexStart, codexResponse, codexCompleted))
			r.Thinking = thinking
			callbacks := 0
			got := e.Execute(context.Background(), r, func(Event) error { callbacks++; return nil })
			if got.Outcome != OutcomeNotStarted || got.Started || got.Accepted || got.NativeSessionID != "" || got.Reason != "Codex 暂不支持 thinking 参数" || callbacks != 0 {
				t.Errorf("unsupported Codex thinking was not explicitly rejected: %+v", got)
			}
			for _, path := range []string{filepath.Join(r.CWD, "invoked"), filepath.Join(r.RuntimeDir, r.RunID)} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Error("unsupported Codex thinking created state or invoked the executable")
				}
			}
		})
	}
}

func TestExecutionExistingRunDirectoryRequiresReconciliation(t *testing.T) {
	for _, state := range []string{"empty", "unreconciled-logs"} {
		t.Run(state, func(t *testing.T) {
			e, r := executionFixture(t, "echo invoked > invoked\n")
			dir := filepath.Join(r.RuntimeDir, r.RunID)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			logs := make(map[string]string)
			if state == "unreconciled-logs" {
				for _, name := range []string{"stdout.jsonl", "stderr.log"} {
					logs[name] = "synthetic unreconciled " + name + "\n"
					if err := os.WriteFile(filepath.Join(dir, name), []byte(logs[name]), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			got := e.Execute(context.Background(), r, nil)
			if got.Outcome != OutcomeUnknown || got.Started || got.Accepted || got.Terminal != "" || got.NativeSessionID != "" || !strings.Contains(got.Reason, "已存在") || !strings.Contains(got.Reason, "对账") || strings.Contains(got.Reason, r.RuntimeDir) {
				t.Fatalf("existing run history was guessed or treated as safe to retry: %+v", got)
			}
			if _, err := os.Stat(filepath.Join(r.CWD, "invoked")); !os.IsNotExist(err) {
				t.Fatal("existing run invoked the executable")
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != len(logs) {
				t.Fatalf("existing run directory changed: %v, %v", entries, err)
			}
			for name, want := range logs {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || string(data) != want {
					t.Fatalf("existing log changed: %s, %v", name, err)
				}
			}
		})
	}
}

func TestExecutionRejectsUnsafeInputsBeforeStart(t *testing.T) {
	if _, err := NewPiExecutor("pi", 0); err == nil {
		t.Fatal("relative executable accepted")
	}
	for _, field := range []string{"cwd", "runtime", "prompt", "identity", "model", "public-runtime", "symlink-runtime", "trailing-symlink", "absent", "cancel"} {
		t.Run(field, func(t *testing.T) {
			e, r := executionFixture(t, "echo invoked > invoked\n"+executionEmit(executionStart))
			originalCWD := r.CWD
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch field {
			case "cwd":
				r.CWD = "relative"
			case "runtime":
				r.RuntimeDir = "relative"
			case "prompt":
				r.Prompt = " \n\t"
			case "identity":
				r.RunID = "../escape"
			case "model":
				r.Model = "fixture\x00invalid"
			case "public-runtime":
				if err := os.Chmod(r.RuntimeDir, 0755); err != nil {
					t.Fatal(err)
				}
			case "symlink-runtime", "trailing-symlink":
				link := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(r.RuntimeDir, link); err != nil {
					t.Fatal(err)
				}
				r.RuntimeDir = link
				if field == "trailing-symlink" {
					r.RuntimeDir += "/"
				}
			case "absent":
				e, _ = NewPiExecutor(filepath.Join(t.TempDir(), "absent"), 0)
			case "cancel":
				cancel()
			}
			got := e.Execute(ctx, r, nil)
			if got.Started || got.Accepted || got.Outcome != OutcomeNotStarted || got.Reason == "" {
				t.Fatalf("unsafe/unstarted request was accepted: %+v", got)
			}
			if _, err := os.Stat(filepath.Join(originalCWD, "invoked")); !os.IsNotExist(err) {
				t.Fatal("invalid request invoked the executable")
			}
		})
	}
}

func TestExecutionDeliversSessionAndAcceptanceBeforeExit(t *testing.T) {
	wait := func(file string) string { return "while [ ! -f " + file + " ]; do /bin/sleep 0.01; done\n" }
	e, r := executionFixture(t, executionHeader+wait("header-seen")+executionEmit(executionStart)+wait("accepted-seen")+executionEmit(executionAssistant("stop"), executionSettled)+"echo done > exited")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var events []EventKind
	got := e.Execute(ctx, r, func(event Event) error {
		events = append(events, event.Kind)
		if event.Kind == SessionEvent || event.Kind == AcceptedEvent {
			if event.NativeSessionID != r.RunID {
				t.Errorf("missing native ID: %+v", event)
			}
			if _, err := os.Stat(filepath.Join(r.CWD, "exited")); !os.IsNotExist(err) {
				t.Error("event arrived after exit")
			}
			file := "header-seen"
			if event.Kind == AcceptedEvent {
				file = "accepted-seen"
			}
			return os.WriteFile(filepath.Join(r.CWD, file), nil, 0600)
		}
		if event.Kind == ResponseEvent && event.Text != "合成\u2028回答" {
			t.Errorf("unexpected response: %+v", event)
		}
		return nil
	})
	if got.Outcome != OutcomeSucceeded || !reflect.DeepEqual(events, []EventKind{SessionEvent, AcceptedEvent, ResponseEvent, TerminalEvent}) {
		t.Fatalf("events were buffered, reordered or incorrectly framed: %+v, %v", got, events)
	}
}

func TestExecutionRequiresBoundLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		events     []EventKind
		terminal   Outcome
	}{
		{"start-without-header", executionEmit(executionStart, executionAssistant("stop"), executionSettled), nil, ""},
		{"start-before-header", executionEmit(executionStart) + executionHeader + executionEmit(executionAssistant("stop"), executionSettled), []EventKind{SessionEvent}, ""},
		{"response-without-header", executionEmit(executionAssistant("stop"), executionSettled), nil, ""},
		{"response-before-header", executionEmit(executionAssistant("stop")) + executionHeader + executionEmit(executionStart, executionSettled), []EventKind{SessionEvent, AcceptedEvent}, ""},
		{"response-before-start", executionHeader + executionEmit(executionAssistant("stop"), executionStart, executionSettled), []EventKind{SessionEvent, AcceptedEvent}, ""},
		{"invalid-header", executionEmit(`{"type":"session","version":2,"id":"run1","cwd":"/fixture"}`, executionStart, executionAssistant("stop"), executionSettled), nil, ""},
		{"start-before-header-with-bound-retry", executionEmit(executionStart) + executionHeader + executionEmit(executionStart, executionAssistant("stop"), executionSettled), []EventKind{SessionEvent, AcceptedEvent, ResponseEvent, TerminalEvent}, OutcomeSucceeded},
		{"response-before-start-with-bound-response", executionHeader + executionEmit(executionAssistant("stop"), executionStart, executionAssistant("stop"), executionSettled), []EventKind{SessionEvent, AcceptedEvent, ResponseEvent, TerminalEvent}, OutcomeSucceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, r := executionFixture(t, tc.body)
			var events []EventKind
			session, accepted := false, false
			got := e.Execute(context.Background(), r, func(event Event) error {
				events = append(events, event.Kind)
				if event.NativeSessionID != r.RunID {
					t.Errorf("unbound event: %+v", event)
				}
				switch event.Kind {
				case SessionEvent:
					session = true
				case AcceptedEvent:
					if !session {
						t.Error("acceptance delivered before session identity")
					}
					accepted = true
				case ResponseEvent:
					if !accepted {
						t.Error("response delivered before acceptance")
					}
				}
				return nil
			})
			identity := ""
			if session {
				identity = r.RunID
			}
			if got.Outcome != OutcomeUnknown || !got.Started || !got.DirectChildReaped || got.OutputComplete || got.DeliveryFailed || got.Accepted != accepted || got.NativeSessionID != identity || got.Terminal != tc.terminal || !reflect.DeepEqual(events, tc.events) {
				t.Fatalf("unbound lifecycle was trusted or bound evidence was lost: %+v, events=%v, want=%v", got, events, tc.events)
			}
		})
	}
}

func TestExecutionRequiresSettledAndLastAssistantOutcome(t *testing.T) {
	for _, tc := range []struct {
		name, records string
		outcome       Outcome
		accepted      bool
	}{
		{"header", "", OutcomeUnknown, false},
		{"start", executionEmit(executionStart), OutcomeUnknown, true},
		{"agent-end", executionEmit(executionStart, executionAssistant("stop"), executionEnd), OutcomeUnknown, true},
		{"stop", executionEmit(executionStart, executionAssistant("stop"), executionEnd, executionSettled), OutcomeSucceeded, true},
		{"retry", executionEmit(executionStart, executionAssistant("error"), executionEnd, `{"type":"auto_retry_start","attempt":1,"maxAttempts":3,"delayMs":1,"errorMessage":"synthetic"}`, executionStart, executionAssistant("stop"), executionEnd, executionSettled), OutcomeSucceeded, true},
		{"error", executionEmit(executionStart, executionAssistant("error"), executionSettled), OutcomeFailed, true},
		{"aborted", executionEmit(executionStart, executionAssistant("aborted"), executionSettled), OutcomeAborted, true},
		{"length", executionEmit(executionStart, executionAssistant("length"), executionSettled), OutcomeTruncated, true},
		{"settled-aborted", executionEmit(executionStart, `{"type":"agent_settled","aborted":true}`), OutcomeAborted, true},
		{"missing-aborted", executionEmit(executionStart, executionAssistant("stop"), `{"type":"agent_settled"}`), OutcomeUnknown, true},
		{"malformed-final", executionEmit(executionStart, executionAssistant("stop"), `{"type":"message_end","message":{"role":"assistant"}}`, executionSettled), OutcomeUnknown, true},
		{"missing-message", executionEmit(executionStart, executionAssistant("stop"), `{"type":"message_end"}`, executionSettled), OutcomeUnknown, true},
		{"malformed-text", executionEmit(executionStart, `{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text"}]}}`, executionSettled), OutcomeUnknown, true},
		{"invalid-utf8", executionEmit(executionStart, strings.Replace(executionAssistant("stop"), "合成\\u2028回答", "\xff", 1), executionSettled), OutcomeUnknown, true},
		{"unfinished-assistant", executionEmit(executionStart, executionAssistant("stop"), `{"type":"message_start","message":{"role":"assistant"}}`, executionSettled), OutcomeUnknown, true},
		{"oversized-final", executionEmit(executionStart, executionAssistant("stop")) + "printf '{\"type\":\"message_end\",\"message\":\"'\n/usr/bin/head -c 131072 /dev/zero | /usr/bin/tr '\\000' x\nprintf '\"}\\n'\n" + executionEmit(executionSettled), OutcomeUnknown, true},
		{"unexpected-exit", executionEmit(executionStart, executionAssistant("stop"), executionSettled) + "exit 7", OutcomeUnknown, true},
		{"partial-record", executionEmit(executionStart, executionAssistant("stop")) + "printf '%s' '" + executionSettled + "'", OutcomeUnknown, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, r := executionFixture(t, executionHeader+tc.records)
			got := e.Execute(context.Background(), r, nil)
			if got.Outcome != tc.outcome || got.Accepted != tc.accepted || !got.Started {
				t.Fatalf("incorrect lifecycle: %+v", got)
			}
		})
	}
}

func TestExecutionDrainsBeyondPayloadAndLogLimits(t *testing.T) {
	for _, noise := range []bool{false, true} {
		t.Run(fmt.Sprint(noise), func(t *testing.T) {
			body := executionHeader + executionEmit(executionStart) + "printf 'RAW-STDERR-SECRET' >&2\n/usr/bin/head -c 2097152 /dev/zero >&2\n"
			if noise {
				body += "/usr/bin/head -c 2097152 /dev/zero | /usr/bin/tr '\\000' x\nprintf '\\nnoise\\n'\n"
			}
			e, r := executionFixture(t, body+executionEmit(executionAssistant("stop"), executionSettled))
			responses := 0
			got := e.Execute(context.Background(), r, func(event Event) error {
				if len(event.Text) > 64<<10 {
					t.Error("unbounded event payload")
				}
				if event.Kind == ResponseEvent {
					responses++
				}
				return nil
			})
			if responses != 1 {
				t.Error("later authoritative events were not drained")
			}
			want := OutcomeSucceeded
			if noise {
				want = OutcomeUnknown
			}
			if got.Outcome != want || !got.LogsTruncated || !got.DirectChildReaped || got.ExitCode != 0 {
				t.Fatalf("draining failed: %+v", got)
			}
			data, _ := json.Marshal(got)
			if strings.Contains(string(data), "RAW-STDERR-SECRET") {
				t.Fatal("stderr leaked into receipt")
			}
			for _, path := range []string{got.StdoutPath, got.StderrPath} {
				info, err := os.Stat(path)
				if err != nil || info.Size() > 1<<20 {
					t.Fatalf("unbounded log: %s, %v", path, err)
				}
			}
		})
	}
}

func TestExecutionCancellationAndInheritedPipesStayUnknown(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel", "inherited", "late-terminal"} {
		t.Run(mode, func(t *testing.T) {
			body := ""
			if mode == "late-terminal" {
				body += "(/bin/sleep 0.4\n" + executionEmit(executionAssistant("stop"), executionSettled) + "exec /bin/sleep 10) &\n"
			} else {
				body += "/bin/sleep 10 &\n"
			}
			body += "echo \"$$ $!\" > pids\n" + executionHeader + executionEmit(executionStart)
			if mode == "inherited" {
				body += executionEmit(executionAssistant("stop"), executionSettled) + "exit 0"
			} else {
				body += "wait"
			}
			e, r := executionFixture(t, body)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var count int
			start := time.Now()
			got := e.Execute(ctx, r, func(event Event) error {
				count++
				if (mode == "cancel" || mode == "late-terminal") && event.Kind == AcceptedEvent {
					cancel()
				}
				return nil
			})
			pids, _ := os.ReadFile(filepath.Join(r.CWD, "pids"))
			var parent, child int
			if n, _ := fmt.Sscanf(string(pids), "%d %d", &parent, &child); n != 2 {
				t.Fatalf("missing descendant: %s", pids)
			}
			t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
			if got.Outcome != OutcomeUnknown || !got.Started || !got.DirectChildReaped || time.Since(start) > 3*time.Second {
				t.Fatalf("unbounded or unsafe cancellation: %+v", got)
			}
			if err := syscall.Kill(child, 0); err != nil {
				t.Fatalf("fixture must retain descendant, not claim it stopped: %v", err)
			}
			if _, err := syscall.Wait4(parent, nil, syscall.WNOHANG, nil); err != syscall.ECHILD {
				t.Fatalf("direct child not reaped: %v", err)
			}
			before := count
			time.Sleep(450 * time.Millisecond)
			if count != before {
				t.Fatal("callback mutated state after return")
			}
		})
	}
}

func TestExecutionModelTurnTimeoutIsNotAProbeTimeout(t *testing.T) {
	e, r := executionFixture(t, executionHeader+executionEmit(executionStart)+"/bin/sleep 5.1\n"+executionEmit(executionAssistant("stop"), executionSettled))
	path := e.(piExecutor).path
	e, _ = NewPiExecutor(path, 0)
	if got := e.Execute(context.Background(), r, nil); got.Outcome != OutcomeSucceeded {
		t.Fatalf("model turn inherited a probe timeout: %+v", got)
	}
	e, r = executionFixture(t, executionHeader+executionEmit(executionStart)+"exec /bin/sleep 10")
	e, _ = NewPiExecutor(e.(piExecutor).path, 500*time.Millisecond)
	start := time.Now()
	if got := e.Execute(context.Background(), r, nil); !got.Started || got.Outcome != OutcomeUnknown || time.Since(start) > 2*time.Second {
		t.Fatalf("configured timeout ignored: %+v", got)
	}
}

func TestExecutionCallbackFailureCancelsWithoutRetry(t *testing.T) {
	for _, failAt := range []EventKind{SessionEvent, AcceptedEvent, ResponseEvent, TerminalEvent} {
		t.Run(string(failAt), func(t *testing.T) {
			e, r := executionFixture(t, "echo invocation >> invocations\n"+executionHeader+executionEmit(executionStart, executionAssistant("stop"), executionSettled)+"exec /bin/sleep 10")
			calls, failed := 0, false
			start := time.Now()
			got := e.Execute(context.Background(), r, func(event Event) error {
				calls++
				if failed {
					t.Error("callback invoked again after failure")
				}
				if event.Kind == failAt {
					failed = true
					return errors.New("RAW-CALLBACK-SECRET")
				}
				return nil
			})
			data, _ := os.ReadFile(filepath.Join(r.CWD, "invocations"))
			if got.Outcome != OutcomeUnknown || !got.DeliveryFailed || !failed || calls == 0 || string(data) != "invocation\n" || time.Since(start) > time.Second || strings.Contains(got.Reason, "RAW-CALLBACK-SECRET") {
				t.Fatalf("callback failure was ignored/retried: %+v", got)
			}
			if failAt == TerminalEvent && got.Terminal != OutcomeSucceeded {
				t.Fatalf("terminal evidence lost: %+v", got)
			}
		})
	}
}
