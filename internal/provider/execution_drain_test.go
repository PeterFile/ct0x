package provider

import (
	"context"
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

func TestExecutionSlowAcceptancePreservesNativeOutput(t *testing.T) {
	testExecutionSlowAcceptance(t, false, "")
}

func testExecutionSlowAcceptance(t *testing.T, codex bool, stop string) {
	t.Helper()
	// The shell has no descendants. Its tail cannot enter the stdout read that
	// invoked AcceptedEvent: the test permits that write only inside the callback.
	fixture, header, tail, nativeID := executionFixture, executionHeader+executionEmit(executionStart), executionEmit(executionAssistant("stop"), executionEnd, executionSettled), "run1"
	if codex {
		fixture, header, tail, nativeID = codexFixture, executionEmit(codexHeader, codexStart), executionEmit(codexResponse, codexCompleted), "native-thread"
	}
	body := "set -eu\nprintf '%s\\n' \"$$\" > pid\n" + header +
		"while [ ! -f emit-tail ]; do :; done\n" + tail +
		": > tail-emitted\nexit 0\n"
	executor, request := fixture(t, body)
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
				<-gate // Model bounded acceptance persistence, independently of pipe reads.
				if stop == "failure" {
					return errors.New("RAW-CALLBACK-SECRET")
				}
				if stop == "cancel" {
					cancel()
				}
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
			t.Error("executor did not stop after cancellation and gate release")
		}
	}()
	// This bounds test coordination; the execution context has no deadline.
	bound := time.NewTimer(5 * time.Second)
	defer bound.Stop()
	select {
	case <-entered:
	case <-done:
		t.Fatalf("execution returned before acceptance: receipt=%+v events=%+v", receipt, events)
	case <-bound.C:
		t.Fatal("acceptance callback did not enter the gate")
	}

	data, err := os.ReadFile(filepath.Join(request.CWD, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		t.Fatalf("invalid direct-child PID %q: %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(request.CWD, "emit-tail"), nil, 0600); err != nil {
		t.Fatal(err)
	}

	// Probe existence without reaping: cmd.Wait must remain the only waiter.
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if err != nil {
			t.Fatalf("cannot observe direct-child exit: %v", err)
		}
		select {
		case <-poll.C:
		case <-done:
			t.Fatal("execution returned while the acceptance callback was gated")
		case <-bound.C:
			t.Fatal("direct child did not exit while the acceptance callback was gated")
		}
	}
	if _, err := os.Stat(filepath.Join(request.CWD, "tail-emitted")); err != nil {
		t.Fatalf("child exited without successfully writing all native frames: %v", err)
	}

	// Start only after external exit proof, with ample margin over WaitDelay=100ms.
	exitedAt := time.Now()
	hold := time.NewTimer(500 * time.Millisecond)
	defer hold.Stop()
	select {
	case <-hold.C:
	case <-done:
		t.Fatal("execution returned while the acceptance callback was gated")
	case <-bound.C:
		t.Fatal("test bound expired before normal callback release")
	}
	t.Logf("direct child %d disappeared (ESRCH), all tail writes succeeded; callback held another %s", pid, time.Since(exitedAt))
	release()
	select {
	case <-done:
	case <-bound.C:
		t.Fatal("execution did not return after normal callback release")
	}
	if (ctx.Err() != nil) != (stop == "cancel") {
		t.Fatalf("unexpected caller cancellation: %v", ctx.Err())
	}
	stdout, err := os.ReadFile(receipt.StdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.ReadFile(receipt.StderrPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("receipt=%+v events=%+v stdout=%q stderr=%q", receipt, events, stdout, stderr)

	wantEvents := []Event{
		{Kind: SessionEvent, NativeSessionID: nativeID},
		{Kind: AcceptedEvent, NativeSessionID: nativeID},
		{Kind: ResponseEvent, NativeSessionID: nativeID, Text: "合成\u2028回答"},
		{Kind: TerminalEvent, NativeSessionID: nativeID, Terminal: OutcomeSucceeded},
	}
	wantOutcome := OutcomeSucceeded
	if stop != "" {
		wantEvents, wantOutcome = wantEvents[:2], OutcomeUnknown
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Errorf("slow callback discarded, reordered or delivered events after stopping: got=%+v want=%+v", events, wantEvents)
	}
	if receipt.Outcome != wantOutcome || receipt.Terminal != OutcomeSucceeded ||
		!receipt.Started || !receipt.Accepted || !receipt.DirectChildReaped || !receipt.OutputComplete ||
		receipt.DeliveryFailed != (stop == "failure") || receipt.LogsTruncated || receipt.ExitCode != 0 || receipt.NativeSessionID != nativeID {
		t.Errorf("complete native evidence must survive slow acceptance persistence: %+v", receipt)
	}
}
