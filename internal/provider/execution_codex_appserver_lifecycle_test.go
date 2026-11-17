package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestCodexAppServerFailsClosedOnUntrustworthyLifecycle(t *testing.T) {
	for _, mode := range []string{
		"wrong-initialize-id", "wrong-turn-response-id", "wrong-turn-id", "terminal-before-start", "duplicate-start", "duplicate-item",
		"cross-thread", "cross-turn", "contradiction", "late-item", "partial", "rpc-error", "unknown-origin", "config-names", "nonzero-exit", "terminal-only",
	} {
		t.Run(mode, func(t *testing.T) {
			e, request := appServerFixture(t, mode)
			var events []Event
			receipt := e.Execute(context.Background(), request, func(event Event) error { events = append(events, event); return nil })
			if receipt.Outcome != OutcomeUnknown || !receipt.Started || !receipt.DirectChildReaped || strings.Contains(receipt.Reason, appServerFixtureSecret) {
				t.Fatalf("untrustworthy execution was settled: %+v", receipt)
			}
			if (mode == "wrong-initialize-id" || mode == "wrong-turn-response-id" || mode == "wrong-turn-id" || mode == "terminal-before-start" || mode == "unknown-origin" || mode == "config-names") && receipt.Accepted {
				t.Fatal("unbound or out-of-order turn was accepted")
			}
			if mode == "contradiction" && receipt.Terminal != OutcomeSucceeded {
				t.Fatal("first native terminal evidence was discarded")
			}
			if mode == "nonzero-exit" && (receipt.OutputComplete || receipt.ExitCode != 7) {
				t.Fatal("native terminal hid a real failed process wait")
			}
			for _, path := range []string{receipt.StdoutPath, receipt.StderrPath} {
				data, _ := os.ReadFile(path)
				if strings.Contains(string(data), appServerFixtureSecret) {
					t.Fatal("config or RPC response leaked into logs")
				}
			}
		})
	}
}

func TestCodexAppServerNativeFailureAndInterruptedTerminals(t *testing.T) {
	for _, mode := range []string{"failed", "interrupted"} {
		t.Run(mode, func(t *testing.T) {
			e, request := appServerFixture(t, mode)
			receipt := e.Execute(context.Background(), request, nil)
			want := OutcomeFailed
			if mode == "interrupted" {
				want = OutcomeAborted
			}
			if receipt.Outcome != want || receipt.Terminal != want || !receipt.OutputComplete || !receipt.DirectChildReaped || strings.Contains(receipt.Reason, appServerFixtureSecret) {
				t.Fatalf("native failure was guessed or leaked: %+v", receipt)
			}
			data, err := os.ReadFile(receipt.StdoutPath)
			if err != nil || strings.Contains(string(data), appServerFixtureSecret) || strings.Contains(string(data), "additionalDetails") {
				t.Fatal("native terminal error/details were persisted in stdout")
			}
		})
	}
}

func TestCodexAppServerReviewRegressions(t *testing.T) {
	for _, mode := range []string{"managed", "hidden-managed", "missing-layers", "orphan-delta", "delta-kind-mismatch", "terminal-item-mismatch", "terminal-item-duplicate", "cross-thread-name"} {
		t.Run(mode, func(t *testing.T) {
			e, request := appServerFixture(t, mode)
			receipt := e.Execute(context.Background(), request, nil)
			if receipt.Outcome != OutcomeUnknown || !receipt.DirectChildReaped {
				t.Fatalf("review regression was settled: %+v", receipt)
			}
			if strings.Contains(mode, "managed") || mode == "missing-layers" {
				data, _ := os.ReadFile(filepath.Join(request.CWD, "after-rejected-config"))
				if receipt.NativeSessionID != "" || receipt.Accepted || len(data) != 0 {
					t.Fatal("thread/start was sent without safe, complete config layer metadata")
				}
			}
		})
	}
	for _, mode := range []string{"account-update", "account-rate-limits", "global-warning", "thread-name", "active-delta"} {
		t.Run(mode, func(t *testing.T) {
			e, request := appServerFixture(t, mode)
			receipt := e.Execute(context.Background(), request, nil)
			if receipt.Outcome != OutcomeSucceeded || !receipt.OutputComplete || !receipt.DirectChildReaped {
				t.Fatalf("valid passive metadata or bound item lifecycle was rejected: %+v", receipt)
			}
		})
	}
}

func TestCodexAppServerConfigRequiresAllLayerMetadata(t *testing.T) {
	flags := map[string]any{"name": map[string]string{"type": "sessionFlags"}, "version": "v1", "config": map[string]any{}}
	for _, kind := range []string{"legacyManagedConfigTomlFromFile", "legacyManagedConfigTomlFromMdm", "mdm", "enterpriseManaged", "system", "futureLayer"} {
		config := map[string]any{"config": map[string]any{}, "origins": map[string]any{}, "layers": []any{flags,
			map[string]any{"name": map[string]string{"type": kind, "file": "/synthetic/managed.toml"}, "version": "managed-v1", "config": map[string]string{"private": appServerFixtureSecret}}}}
		if _, err := readCodexAppServerConfig(mustCodexAppServerJSON(config)); err == nil {
			t.Fatal("a managed/unknown layer absent from winning origins was accepted")
		}
	}
	for _, layers := range []any{nil, []any{}, []any{map[string]any{"name": map[string]string{"type": "user", "file": "/synthetic/config.toml"}, "version": "v1"}}} {
		config := map[string]any{"config": map[string]any{}, "origins": map[string]any{}, "layers": layers}
		if _, err := readCodexAppServerConfig(mustCodexAppServerJSON(config)); err == nil {
			t.Fatal("missing layers/session override evidence was accepted")
		}
	}
}

func TestCodexAppServerLayerVersionDriftDoesNotStartTurn(t *testing.T) {
	p, receive, _ := appServerCompatThreadPending(t)
	receive(appServerCompatThreadResponse)
	receive(`{"id":4,"result":{"config":{"mcp_servers":{}},"origins":{},"layers":[{"name":{"type":"sessionFlags"},"version":"flags-v2","config":{}}]}}`)
	if !p.damaged || p.ctx.Err() == nil || p.receipt.Accepted || p.turnID != "" {
		t.Fatal("layer-only version drift was missed because winning origins stayed unchanged")
	}
}

func TestCodexAppServerCancellationInterruptIsNotTerminal(t *testing.T) {
	for _, mode := range []string{"cancel", "interrupt-ack-only"} {
		t.Run(mode, func(t *testing.T) {
			e, request := appServerFixture(t, mode)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			callbacks := 0
			start := time.Now()
			receipt := e.Execute(ctx, request, func(event Event) error {
				callbacks++
				if event.Kind == AcceptedEvent {
					cancel()
				}
				return nil
			})
			wantTerminal := OutcomeAborted
			if mode == "interrupt-ack-only" {
				wantTerminal = ""
			}
			if receipt.Outcome != OutcomeUnknown || receipt.Terminal != wantTerminal || !receipt.Accepted || !receipt.DirectChildReaped || receipt.DeliveryFailed || callbacks != 2 || time.Since(start) > 4*time.Second {
				t.Fatalf("unsafe or unbounded cancellation: %+v callbacks=%d", receipt, callbacks)
			}
			data, err := os.ReadFile(filepath.Join(request.CWD, "client.jsonl"))
			if err != nil || strings.Count(string(data), `"method":"turn/start"`) != 1 || strings.Count(string(data), `"method":"turn/interrupt"`) != 1 || !strings.Contains(string(data), `"turnId":"`+appServerFixtureTurn+`"`) {
				t.Fatal("interrupt was missing, repeated, unbound, or started another turn")
			}
		})
	}
}

func TestCodexAppServerKillCannotMasqueradeAsNormalWait(t *testing.T) {
	for _, mode := range []string{"refuse-exit", "blocked-stdin"} {
		t.Run(mode, func(t *testing.T) {
			e, request := appServerFixture(t, mode)
			e, _ = NewCodexAppServerExecutor(e.(codexAppServerExecutor).path, 200*time.Millisecond)
			if mode == "blocked-stdin" {
				request.Prompt = strings.Repeat("\x00", 1<<20)
			}
			start := time.Now()
			receipt := e.Execute(context.Background(), request, nil)
			if receipt.Outcome != OutcomeUnknown || !receipt.Started || !receipt.DirectChildReaped || receipt.OutputComplete || receipt.ExitCode == 0 || time.Since(start) > 4*time.Second {
				t.Fatalf("kill/writer block was treated as complete or shutdown exceeded its bound: %+v elapsed=%s", receipt, time.Since(start))
			}
		})
	}
}

func TestCodexAppServerInheritedPipesAndExternalWorkStayUnknown(t *testing.T) {
	for _, mode := range []string{"inherited-pipe", "pending-tool", "background-tool", "subagent", "approval"} {
		t.Run(mode, func(t *testing.T) {
			e, request := appServerFixture(t, mode)
			start := time.Now()
			receipt := e.Execute(context.Background(), request, nil)
			if receipt.Outcome != OutcomeUnknown || !receipt.Started || !receipt.Accepted || !receipt.DirectChildReaped || time.Since(start) > 4*time.Second {
				t.Fatalf("unproven external work was settled: %+v", receipt)
			}
			if mode == "inherited-pipe" {
				data, err := os.ReadFile(filepath.Join(request.CWD, "descendant"))
				pid, parseErr := strconv.Atoi(string(data))
				if err != nil || parseErr != nil || pid <= 1 {
					t.Fatal("missing synthetic descendant")
				}
				t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
				if receipt.OutputComplete || receipt.Terminal != OutcomeSucceeded || syscall.Kill(pid, 0) != nil {
					t.Fatal("direct-child wait was confused with inherited pipe or descendant completion")
				}
			}
			data, _ := os.ReadFile(filepath.Join(request.CWD, "pid"))
			pid, _ := strconv.Atoi(string(data))
			if _, err := syscall.Wait4(pid, nil, syscall.WNOHANG, nil); err != syscall.ECHILD {
				t.Fatal("Execute returned without reaping the direct child")
			}
		})
	}
}

func TestCodexAppServerOutputIsBoundedAndDamagedStreamsStillDrain(t *testing.T) {
	for _, mode := range []string{"large-logs", "malformed-drain"} {
		t.Run(mode, func(t *testing.T) {
			e, request := appServerFixture(t, mode)
			receipt := e.Execute(context.Background(), request, nil)
			if !receipt.Started || !receipt.DirectChildReaped || !receipt.LogsTruncated || (receipt.Outcome == OutcomeSucceeded) != (mode == "large-logs") {
				t.Fatalf("output did not drain within bounds: %+v", receipt)
			}
			if mode == "malformed-drain" {
				if _, err := os.Stat(filepath.Join(request.CWD, "drained")); err != nil || receipt.OutputComplete {
					t.Fatal("damaged stdout stopped draining before later stderr and the child exit")
				}
			}
			for _, path := range []string{receipt.StdoutPath, receipt.StderrPath} {
				info, err := os.Stat(path)
				if err != nil || info.Size() > executionLogLimit {
					t.Fatal("unbounded raw log")
				}
			}
		})
	}
}

func TestCodexAppServerDeliveryFailureNeverRetries(t *testing.T) {
	for _, at := range []EventKind{SessionEvent, AcceptedEvent, ResponseEvent, TerminalEvent} {
		t.Run(string(at), func(t *testing.T) {
			e, request := appServerFixture(t, "normal")
			failed := false
			receipt := e.Execute(context.Background(), request, func(event Event) error {
				if failed {
					t.Error("callback after delivery failure")
				}
				if event.Kind == at {
					failed = true
					return errors.New("SYNTHETIC-CALLBACK-SECRET")
				}
				return nil
			})
			if receipt.Outcome != OutcomeUnknown || !receipt.DeliveryFailed || !failed || strings.Contains(receipt.Reason, "SYNTHETIC-CALLBACK-SECRET") || !receipt.DirectChildReaped || at == TerminalEvent && receipt.Terminal != OutcomeSucceeded {
				t.Fatalf("failed delivery was trusted or evidence lost: %+v", receipt)
			}
			data, _ := os.ReadFile(filepath.Join(request.CWD, "client.jsonl"))
			if strings.Count(string(data), `"method":"initialize"`) != 1 || strings.Count(string(data), `"method":"turn/start"`) > 1 {
				t.Fatal("failed delivery retried a process or turn")
			}
		})
	}
}

func TestCodexAppServerSlowDeliveryAndOverflowJoinCallbacks(t *testing.T) {
	for _, mode := range []string{"slow-delivery", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			e, request := appServerFixture(t, mode)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			gate, entered, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(gate) })
			defer release()
			var receipt ExecutionReceipt
			var kinds []EventKind
			go func() {
				receipt = e.Execute(ctx, request, func(event Event) error {
					kinds = append(kinds, event.Kind)
					if event.Kind == AcceptedEvent {
						close(entered)
						_ = os.WriteFile(filepath.Join(request.CWD, "emit-tail"), nil, 0600)
						<-gate
					}
					return nil
				})
				close(done)
			}()
			defer func() {
				cancel()
				release()
				select {
				case <-done:
				case <-time.After(4 * time.Second):
					t.Error("Execute did not join the released callback")
				}
			}()
			select {
			case <-entered:
			case <-done:
				t.Fatal("execution finished before acceptance")
			case <-time.After(3 * time.Second):
				t.Fatal("acceptance did not enter")
			}
			data, err := os.ReadFile(filepath.Join(request.CWD, "pid"))
			pid, parseErr := strconv.Atoi(string(data))
			if err != nil || parseErr != nil || pid <= 1 {
				t.Fatal("missing synthetic direct child")
			}
			bound := time.After(4 * time.Second)
			for syscall.Kill(pid, 0) == nil {
				select {
				case <-done:
					t.Fatal("Execute abandoned the gated callback")
				case <-bound:
					t.Fatal("slow callback prevented output draining or child wait")
				case <-time.After(time.Millisecond):
				}
			}
			select {
			case <-done:
				t.Fatal("Execute returned without joining callback work")
			case <-time.After(250 * time.Millisecond):
			}
			release()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("Execute did not finish after callback release")
			}
			want, outcome := []EventKind{SessionEvent, AcceptedEvent, ResponseEvent, TerminalEvent}, OutcomeSucceeded
			if mode == "overflow" {
				want, outcome = want[:2], OutcomeUnknown
			}
			if receipt.Outcome != outcome || receipt.DeliveryFailed != (mode == "overflow") || receipt.Terminal != OutcomeSucceeded || !receipt.DirectChildReaped || !reflect.DeepEqual(kinds, want) {
				t.Fatalf("delivery backpressure changed native evidence or callbacks escaped: %+v kinds=%v", receipt, kinds)
			}
		})
	}
}

func TestCodexAppServerInputValidationDoesNotCreateRunState(t *testing.T) {
	if _, err := NewCodexAppServerExecutor("codex", 0); err == nil {
		t.Fatal("relative executable accepted")
	}
	for _, field := range []string{"cwd", "runtime", "public-runtime", "symlink-runtime", "run-id", "prompt", "large-prompt", "model", "thinking", "cancel"} {
		t.Run(field, func(t *testing.T) {
			e, request := appServerFixture(t, "normal")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			originalRuntime := request.RuntimeDir
			switch field {
			case "cwd":
				request.CWD = "relative"
			case "runtime":
				request.RuntimeDir = "relative"
			case "public-runtime":
				_ = os.Chmod(request.RuntimeDir, 0755)
			case "symlink-runtime":
				link := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(request.RuntimeDir, link); err != nil {
					t.Fatal(err)
				}
				request.RuntimeDir = link + "/"
			case "run-id":
				request.RunID = "../escape"
			case "prompt":
				request.Prompt = " \n\t"
			case "large-prompt":
				request.Prompt = strings.Repeat("x", (1<<20)+1)
			case "model":
				request.Model = "bad\nmodel"
			case "thinking":
				request.Thinking = "max"
			case "cancel":
				cancel()
			}
			callbacks := 0
			receipt := e.Execute(ctx, request, func(Event) error { callbacks++; return nil })
			if receipt.Started || receipt.Accepted || receipt.Outcome != OutcomeNotStarted || receipt.Reason == "" || callbacks != 0 {
				t.Fatalf("invalid input started an execution: %+v", receipt)
			}
			if entries, _ := os.ReadDir(originalRuntime); len(entries) != 0 {
				t.Fatal("invalid input created run state")
			}
		})
	}
}

func TestCodexAppServerConfigUsesUniqueStaticProvenance(t *testing.T) {
	metadata := map[string]any{"name": map[string]any{"type": "user", "file": "/synthetic/config.toml", "profile": nil}, "version": "v1"}
	layers := []any{
		map[string]any{"name": map[string]string{"type": "sessionFlags"}, "version": "flags-v1", "config": map[string]any{}},
		map[string]any{"name": metadata["name"], "version": metadata["version"], "config": map[string]string{"private": appServerFixtureSecret}},
	}
	config := func(origins any) json.RawMessage {
		return mustCodexAppServerJSON(map[string]any{"config": map[string]any{"mcp_servers": map[string]any{"literal.name": map[string]any{"env": map[string]string{"TOKEN": appServerFixtureSecret}}}}, "origins": origins, "layers": layers})
	}
	a, err := readCodexAppServerConfig(config(map[string]any{"first": metadata}))
	if err != nil {
		t.Fatal(err)
	}
	b, err := readCodexAppServerConfig(config(map[string]any{"other": metadata, "repeated": metadata}))
	if err != nil || a.origins != b.origins || !reflect.DeepEqual(a.names, b.names) {
		t.Fatal("origin keys were mistaken for authority instead of unique provenance")
	}
	for _, raw := range []string{
		`{"config":{"mcp_servers":{"dup":{},"dup":{}}},"origins":{}}`,
		`{"config":{"mcp_servers":[]},"origins":{}}`,
		`{"config":{},"origins":{"a":{"name":{"type":"futureLayer"},"version":"v1"}}}`,
		`{"config":{},"origins":{"a":{"name":{"type":"user","file":"relative"},"version":"v1"}}}`,
		`{"config":{},"origins":{"a":{"name":{"type":"sessionFlags"},"version":null}}}`,
		`{"config":{},"origins":{},"layers":[{"config":"` + appServerFixtureSecret + `"}]}`,
	} {
		var fields map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &fields) != nil {
			t.Fatal("invalid synthetic config fixture")
		}
		if fields["layers"] == nil {
			fields["layers"] = mustCodexAppServerJSON(layers)
		}
		if _, err := readCodexAppServerConfig(mustCodexAppServerJSON(fields)); err == nil {
			t.Fatal("unknown or damaged configuration was trusted")
		}
	}
}
