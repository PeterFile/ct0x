package provider

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func claudeControlledExecutor(t *testing.T, mode string) (*claudeSDKExecutor, ExecuteRequest) {
	t.Helper()
	selected, options := claudeFixtureSelection(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("controlled Node fixture requires Node")
	}
	options.NodePath, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	options.BridgePath, err = filepath.Abs(filepath.Join("..", "..", "extensions", "claude-bridge", "tests", "go-bridge-fixture.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	built, err := NewClaudeSDKExecutor(selected, options, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	runtime := t.TempDir()
	if err := os.Chmod(runtime, 0700); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	return built.(*claudeSDKExecutor), ExecuteRequest{RunID: "not-a-native-uuid", CWD: real, RuntimeDir: runtime, Prompt: mode, Model: "fixture-model"}
}

func claudeAssertPrivateLogs(t *testing.T, r ExecutionReceipt) {
	t.Helper()
	for _, path := range []string{r.StdoutPath, r.StderrPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("public evidence file: %s", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"fixture-secret-token", "sensitive-args", "受控 result", "Bearer "} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("secret/raw result in private summary: %s", path)
			}
		}
	}
}

func TestClaudeSDKControlledDualWaitDrainAndLateAcceptance(t *testing.T) {
	e, request := claudeControlledExecutor(t, "fixture-tool")
	start := time.Now()
	var events []Event
	r := e.execute(context.Background(), request, func(event Event) error { events = append(events, event); return nil })
	if r.Outcome != OutcomeSucceeded || !r.Accepted || !r.Started || !r.DirectChildReaped || !r.OutputComplete || r.ExitCode != 0 {
		t.Fatalf("controlled receipt: %+v", r)
	}
	if time.Since(start) < 110*time.Millisecond {
		t.Fatal("returned before controlled native and bridge completion")
	}
	if len(events) != 4 || events[0].Kind != SessionEvent || events[1].Kind != AcceptedEvent || events[2].Kind != ResponseEvent || events[2].Text != "受控 result" || events[3].Kind != TerminalEvent {
		t.Fatalf("bad ordered events: %+v", events)
	}
	if r.NativeSessionID == request.RunID || !claudeUUIDPattern.MatchString(r.NativeSessionID) {
		t.Fatal("RunID reused as session UUID")
	}
	for _, file := range []string{"native-wait.json", "bridge-finished"} {
		if _, err := os.Stat(filepath.Join(request.CWD, file)); err != nil {
			t.Fatalf("missing actual child wait marker: %v", err)
		}
	}
	var native struct{ Exited, Stdout, Stderr, Close bool }
	raw, err := os.ReadFile(filepath.Join(request.CWD, "native-wait.json"))
	if err != nil || json.Unmarshal(raw, &native) != nil || !native.Exited || !native.Stdout || !native.Stderr || !native.Close {
		t.Fatalf("missing real descendant evidence: %s %v", raw, err)
	}
	args, err := os.ReadFile(filepath.Join(request.CWD, "bridge-argv.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "fixture-secret-token") || strings.Contains(string(args), "fixture-model") || strings.Contains(string(args), "member.json") {
		t.Fatal("config/model/secret leaked to argv")
	}
	claudeAssertPrivateLogs(t, r)
}

func TestClaudeSDKControlledUnknownEvidenceMatrix(t *testing.T) {
	for _, mode := range []string{"fixture-submitted-only", "fixture-error-success", "fixture-wrong-input", "fixture-pending-tool", "fixture-unsupported-reason", "fixture-duplicate-key", "fixture-oversized", "fixture-unknown-kind", "fixture-bad-seq", "fixture-duplicate-terminal", "fixture-no-native-closed", "fixture-undrained", "fixture-native-deny", "fixture-subagent-deny", "fixture-node-exit-unclosed"} {
		t.Run(mode, func(t *testing.T) {
			e, request := claudeControlledExecutor(t, mode)
			r := e.execute(context.Background(), request, nil)
			if r.Outcome != OutcomeUnknown || !r.Started || !r.DirectChildReaped {
				t.Fatalf("unknown promoted: %+v", r)
			}
			if (mode == "fixture-submitted-only" || mode == "fixture-wrong-input") && r.Accepted {
				t.Fatal("init/submission/unrelated result promoted to acceptance")
			}
			if mode == "fixture-native-deny" || mode == "fixture-subagent-deny" {
				raw, err := os.ReadFile(filepath.Join(request.CWD, "permission-decision.json"))
				if err != nil || !strings.Contains(string(raw), `"decision":"deny"`) {
					t.Fatalf("native permission not denied: %s %v", raw, err)
				}
			}
			if mode == "fixture-node-exit-unclosed" {
				if r.OutputComplete {
					t.Fatal("Node exit substituted for CLI close")
				}
				// This intentionally orphaned, short-lived controlled descendant has
				// no trustworthy close receipt. Wait for its bounded completion marker
				// before deleting the fixture directory; do not call it native reaping.
				deadline := time.Now().Add(3 * time.Second)
				for {
					if _, err := os.Stat(filepath.Join(request.CWD, "descendant-completed")); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("controlled descendant did not finish")
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			claudeAssertPrivateLogs(t, r)
		})
	}
}

func TestClaudeSDKControlledSuccessSubtypeErrorIsFailed(t *testing.T) {
	e, request := claudeControlledExecutor(t, "fixture-error-result")
	var responses int
	r := e.execute(context.Background(), request, func(event Event) error {
		if event.Kind == ResponseEvent {
			responses++
		}
		return nil
	})
	if r.Outcome != OutcomeFailed || r.Terminal != OutcomeFailed || responses != 0 || !r.Accepted || !r.OutputComplete {
		t.Fatalf("is_error ignored: %+v responses=%d", r, responses)
	}
}

func TestClaudeSDKControlledCancellationDeniesPendingAndWaits(t *testing.T) {
	e, request := claudeControlledExecutor(t, "fixture-cancel-pending")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := e.execute(ctx, request, func(event Event) error {
		if event.Kind == SessionEvent {
			cancel()
		}
		return nil
	})
	if r.Outcome != OutcomeUnknown || !r.DirectChildReaped || r.Accepted {
		t.Fatalf("cancel released unknown: %+v", r)
	}
	raw, err := os.ReadFile(filepath.Join(request.CWD, "permission-decision.json"))
	if err != nil || !strings.Contains(string(raw), `"decision":"deny"`) || !strings.Contains(string(raw), `"reason_code":"cancelled"`) {
		t.Fatalf("pending permission not cancelled: %s %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(request.CWD, "native-wait.json")); err != nil {
		t.Fatal("cancel returned before actual fixture child wait")
	}
	claudeAssertPrivateLogs(t, r)
}

func TestClaudeSDKControlledDeliveryJoinsAndConfigurationDrift(t *testing.T) {
	t.Run("callback-join", func(t *testing.T) {
		e, request := claudeControlledExecutor(t, "fixture-success")
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan ExecutionReceipt, 1)
		var once sync.Once
		go func() {
			done <- e.execute(context.Background(), request, func(Event) error { once.Do(func() { close(entered); <-release }); return nil })
		}()
		<-entered
		time.Sleep(200 * time.Millisecond)
		select {
		case <-done:
			t.Fatal("returned before callback joined")
		default:
		}
		close(release)
		if r := <-done; r.Outcome != OutcomeSucceeded {
			t.Fatalf("callback receipt: %+v", r)
		}
	})
	t.Run("config-drift", func(t *testing.T) {
		e, request := claudeControlledExecutor(t, "fixture-success")
		if err := os.WriteFile(e.configPath, []byte(`{"endpoint":"http://127.0.0.1:7310/api/v1/member-tools","token":"changed"}`), 0600); err != nil {
			t.Fatal(err)
		}
		if r := e.execute(context.Background(), request, nil); r.Started || r.Outcome != OutcomeNotStarted {
			t.Fatalf("configuration drift ignored: %+v", r)
		}
	})
}
