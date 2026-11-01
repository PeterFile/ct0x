package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestCodexExecutionSlowAcceptancePreservesNativeOutput(t *testing.T) {
	testExecutionSlowAcceptance(t, true, "")
}

func TestExecutionStoppedDeliveryPreservesNativeTerminal(t *testing.T) {
	for _, native := range []string{"pi", "codex"} {
		for _, stop := range []string{"failure", "cancel"} {
			t.Run(native+"/"+stop, func(t *testing.T) {
				testExecutionSlowAcceptance(t, native == "codex", stop)
			})
		}
	}
}

func TestExecutionDeliveryOverflowStopsPendingCallbacks(t *testing.T) {
	// Small responses exceed the event count while staying below the byte cap.
	// Large responses exceed 1 MiB in fewer than 64 events; each JSONL frame
	// remains below 64 KiB. Neither case may wait for acceptance persistence.
	for _, tc := range []struct {
		name          string
		responses     int
		responseBytes int
	}{
		{"count", 128, 1},
		{"bytes", 32, 48 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := strings.Replace(executionAssistant("stop"), "合成\\u2028回答", strings.Repeat("x", tc.responseBytes), 1)
			body := "set -eu\necho invocation >> invocations\nprintf '%s\\n' \"$$\" > pid\n" + executionHeader + executionEmit(executionStart) +
				"while [ ! -f emit-tail ]; do :; done\ni=0\n" +
				fmt.Sprintf("while [ \"$i\" -lt %d ]; do\n", tc.responses) + executionEmit(response) +
				"i=$((i + 1))\ndone\n" + executionEmit(executionSettled) + "exec /bin/sleep 10\n"
			executor, request := executionFixture(t, body)
			ctx, cancel := context.WithCancel(context.Background())
			entered, gate, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(gate) })
			var receipt ExecutionReceipt
			var events []Event
			go func() {
				receipt = executor.Execute(ctx, request, func(event Event) error {
					events = append(events, event)
					if event.Kind == AcceptedEvent {
						close(entered)
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
				case <-time.After(2 * time.Second):
					t.Error("executor did not join the released callback")
				}
			}()
			bound := time.NewTimer(5 * time.Second)
			defer bound.Stop()
			select {
			case <-entered:
			case <-done:
				t.Fatalf("execution returned before acceptance: %+v", receipt)
			case <-bound.C:
				t.Fatal("acceptance callback did not enter")
			}
			if err := os.WriteFile(filepath.Join(request.CWD, "emit-tail"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(request.CWD, "pid"))
			var pid int
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Sscan(string(data), &pid); err != nil || pid <= 1 {
				t.Fatalf("invalid direct-child PID %q: %v", data, err)
			}
			poll := time.NewTicker(time.Millisecond)
			defer poll.Stop()
			for {
				err := syscall.Kill(pid, 0)
				if errors.Is(err, syscall.ESRCH) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-poll.C:
				case <-done:
					t.Fatal("Execute abandoned an in-flight callback")
				case <-bound.C:
					t.Fatal("bounded delivery overflow did not cancel the child while the callback was gated")
				}
			}
			select {
			case <-done:
				t.Fatal("Execute returned without joining the callback")
			default:
			}
			release()
			select {
			case <-done:
			case <-bound.C:
				t.Fatal("overflow did not finish after callback release")
			}
			want := []Event{{Kind: SessionEvent, NativeSessionID: request.RunID}, {Kind: AcceptedEvent, NativeSessionID: request.RunID}}
			if !reflect.DeepEqual(events, want) {
				t.Errorf("pending callbacks executed after overflow: %+v", events)
			}
			if ctx.Err() != nil || receipt.Outcome != OutcomeUnknown || !receipt.DeliveryFailed ||
				!receipt.Started || !receipt.Accepted || !receipt.DirectChildReaped || receipt.OutputComplete {
				t.Errorf("overflow must cancel internally and remain unknown: %+v caller=%v", receipt, ctx.Err())
			}
			data, err = os.ReadFile(filepath.Join(request.CWD, "invocations"))
			if err != nil || string(data) != "invocation\n" {
				t.Errorf("overflow retried the full invocation: %q, %v", data, err)
			}
			for _, path := range []string{receipt.StdoutPath, receipt.StderrPath} {
				info, err := os.Stat(path)
				if err != nil || info.Size() > executionLogLimit {
					t.Errorf("unbounded log %s: %v", path, err)
				}
			}
		})
	}
}

func TestExecutionRetryDeliveryReplacesResponseSnapshots(t *testing.T) {
	for _, native := range []string{"pi", "codex"} {
		t.Run(native, func(t *testing.T) {
			fixture, body, nativeID := executionFixture, executionHeader+executionEmit(executionStart), "run1"
			if native == "codex" {
				fixture, body, nativeID = codexFixture, executionEmit(codexHeader, codexStart), "native-thread"
			}
			wantSnapshots := []string{"retry-1", "retry-2", "latest"}
			for i, snapshot := range wantSnapshots {
				if native == "codex" {
					body += executionEmit(fmt.Sprintf(`{"type":"item.completed","item":{"id":"answer-%d","type":"agent_message","text":%q}}`, i, snapshot))
				} else {
					reason := "error"
					if i == len(wantSnapshots)-1 {
						reason = "stop"
					}
					body += executionEmit(strings.Replace(executionAssistant(reason), "合成\\u2028回答", snapshot, 1), executionEnd)
					if i < len(wantSnapshots)-1 {
						body += executionEmit(executionStart)
					}
				}
			}
			if native == "codex" {
				body += executionEmit(codexCompleted)
			} else {
				body += executionEmit(executionSettled)
			}
			executor, request := fixture(t, body)
			var kinds []EventKind
			var snapshots []string
			latest := ""
			receipt := executor.Execute(context.Background(), request, func(event Event) error {
				kinds = append(kinds, event.Kind)
				if event.NativeSessionID != nativeID {
					t.Errorf("native identity reordered or rebound: %+v", event)
				}
				if event.Kind == ResponseEvent {
					latest = event.Text
					snapshots = append(snapshots, latest)
				}
				return nil
			})
			if receipt.Outcome != OutcomeSucceeded || !receipt.OutputComplete || receipt.DeliveryFailed || latest != "latest" ||
				!reflect.DeepEqual(snapshots, wantSnapshots) || !reflect.DeepEqual(kinds, []EventKind{SessionEvent, AcceptedEvent, ResponseEvent, ResponseEvent, ResponseEvent, TerminalEvent}) {
				t.Fatalf("retry snapshots were merged, reordered or lost: %+v kinds=%v snapshots=%v", receipt, kinds, snapshots)
			}
		})
	}
}
