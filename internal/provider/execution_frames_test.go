package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The content is parsed twice by recordPi: first as RawMessage, then as blocks.
// Sixteen real tool calls reach 1 MiB without any individual HTTP args exceeding
// 65536 bytes. The last args object, not an arbitrary top-level field, fills it.
func executionFramesAssistant(t *testing.T, size int) string {
	t.Helper()
	full := memberToolSizedPayload(t, memberToolHTTPBudget, 4, func(payload string) string {
		return memberToolJSON(t, map[string]any{"content": payload})
	})
	var args map[string]any
	if err := json.Unmarshal([]byte(full), &args); err != nil {
		t.Fatal(err)
	}
	return memberToolSizedPayload(t, size, 4, func(payload string) string {
		last := map[string]any{"content": payload}
		if len(memberToolJSON(t, last)) > memberToolHTTPBudget {
			t.Fatal("assistant fixture exceeds per-tool HTTP args budget")
		}
		content := make([]any, 16)
		for i := range content {
			callArgs := args
			if i == len(content)-1 {
				callArgs = last
			}
			content[i] = map[string]any{"type": "toolCall", "id": fmt.Sprintf("call-%d", i), "name": "send_message", "arguments": callArgs}
		}
		return memberToolJSON(t, map[string]any{"type": "message_end", "message": memberToolAssistantMessage(t, content, "toolUse")})
	})
}

// This test-only writer controls the time spent on the existing synchronous
// framing/decoding path. It blocks just before a complete maximum frame is decoded,
// not in a callback, and does not alter child output, deadlines or WaitDelay.
type executionFramesStalledWriter struct {
	out   *executionOutput
	stall func()
}

func (w executionFramesStalledWriter) Write(p []byte) (int, error) {
	if i := bytes.IndexByte(p, '\n'); i >= 0 && len(w.out.line)+i == memberToolFrameBudget {
		w.stall()
	}
	return w.out.Write(p)
}

func TestExecutionFramesParserStallDrainsEOF(t *testing.T) {
	for _, tc := range []struct {
		shape  string
		queued bool
	}{{"agent_end-CRLF", false}, {"assistant-tool-calls", false}, {"agent_end-CRLF", true}, {"assistant-tool-calls", true}} {
		t.Run(fmt.Sprintf("%s/queued=%t", tc.shape, tc.queued), func(t *testing.T) {
			shape := tc.shape
			frame := executionFramesAssistant(t, memberToolFrameBudget)
			if shape == "agent_end-CRLF" {
				run := memberToolAggregateBoundary(t, memberToolFrameBudget-1)
				frame = run[len(run)-1] + "\r"
			}
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "frame.jsonl"), []byte(frame+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			body := "set -eu\n" + executionHeader + executionEmit(executionStart) + "/bin/cat frame.jsonl\nwhile [ ! -f emit-tail ]; do :; done\n" + executionEmit(executionAssistant("stop"), executionSettled) + ": > tail-emitted\n"
			path := executable(t, filepath.Join(root, "fake-pi"), body)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			file, err := os.CreateTemp(root, "stdout-log")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			log := &executionLog{file: file, cancel: cancel}
			r := ExecutionReceipt{Outcome: OutcomeUnknown, ExitCode: -1}
			out := &executionOutput{log: log, receipt: &r, runID: "run1", frameLimit: memberToolFrameBudget}
			entered, gate, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(gate) })
			stall := func() { close(entered); <-gate }
			var frames *executionFrames
			var writer io.Writer = executionFramesStalledWriter{out: out, stall: stall}
			if tc.queued {
				frames = newExecutionFrames(log, cancel, func(frame executionFrame) {
					if len(frame.data) == memberToolFrameBudget {
						stall()
					}
					out.recordFrame(frame)
				})
				writer = frames
			}
			finish := sync.OnceFunc(func() { frames.finish() })
			defer func() { release(); finish() }()
			drain := &executionDrain{Writer: writer}
			cmd := exec.CommandContext(ctx, path)
			cmd.Dir, cmd.Stdout = root, drain
			cmd.WaitDelay = 100 * time.Millisecond
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			var waitErr error
			waited := make(chan struct{})
			go func() {
				waitErr = cmd.Wait()
				close(waited)
				finish()
				close(done)
			}()
			defer func() {
				cancel()
				release()
				<-done
			}()
			bound := time.NewTimer(5 * time.Second)
			defer bound.Stop()
			select {
			case <-entered:
			case <-bound.C:
				t.Fatal("parser did not reach the maximum native frame")
			}
			if err := os.WriteFile(filepath.Join(root, "emit-tail"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			poll := time.NewTicker(time.Millisecond)
			defer poll.Stop()
			for !errors.Is(syscall.Kill(cmd.Process.Pid, 0), syscall.ESRCH) {
				select {
				case <-poll.C:
				case <-bound.C:
					t.Fatal("direct child did not exit while parser was gated")
				}
			}
			if _, err := os.Stat(filepath.Join(root, "tail-emitted")); err != nil {
				t.Fatalf("child did not write its complete tail before exiting: %v", err)
			}
			if tc.queued {
				select {
				case <-waited:
				case <-bound.C:
					t.Fatal("copier could not finish while the raw-frame consumer was gated")
				}
				if waitErr != nil || !drain.complete {
					t.Fatalf("queue must isolate pipe EOF from parser work: Wait=%v EOF=%t", waitErr, drain.complete)
				}
			}
			// A controlled parser stall beyond the unchanged 100ms WaitDelay.
			hold := time.NewTimer(250 * time.Millisecond)
			defer hold.Stop()
			select {
			case <-hold.C:
			case <-done:
				t.Fatal("execution abandoned an in-flight parser")
			}
			release()
			select {
			case <-done:
			case <-bound.C:
				t.Fatal("Wait did not join parsing after release")
			}
			r.Started, r.DirectChildReaped, r.ExitCode = true, cmd.ProcessState != nil, cmd.ProcessState.ExitCode()
			r.OutputComplete = waitErr == nil && drain.complete && !out.damaged && len(out.line) == 0 && !out.dropping
			if r.OutputComplete && ctx.Err() == nil && r.Terminal != "" {
				r.Outcome = r.Terminal
			}
			r.LogsTruncated = log.truncated
			t.Logf("native=%s queued=%t frame=%d child wrote tail and disappeared before 250ms parser hold; Wait=%v EOF=%t receipt=%+v", shape, tc.queued, len(frame), waitErr, drain.complete, r)
			wantOutcome, wantTerminal := OutcomeUnknown, Outcome("")
			if tc.queued {
				wantOutcome, wantTerminal = OutcomeSucceeded, OutcomeSucceeded
			} else if !errors.Is(waitErr, exec.ErrWaitDelay) || drain.complete {
				t.Fatal("synchronous control did not reproduce WaitDelay closing the unread pipe")
			}
			if !r.Started || !r.Accepted || !r.DirectChildReaped || r.ExitCode != 0 || r.OutputComplete != tc.queued || r.Outcome != wantOutcome || r.Terminal != wantTerminal || ctx.Err() != nil {
				t.Fatal("complete child output must survive bounded parser work independently of pipe EOF")
			}
		})
	}
}

func executionFramesLog(t *testing.T, cancel context.CancelFunc) *executionLog {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return &executionLog{file: file, cancel: cancel}
}

func TestExecutionFramesLimitsIncludeInFlight(t *testing.T) {
	for _, budget := range []string{"count", "bytes"} {
		t.Run(budget, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			log := executionFramesLog(t, cancel)
			entered, gate := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(gate) })
			var parsed []string
			damages := 0
			frames := newExecutionFrames(log, cancel, func(frame executionFrame) {
				if len(parsed) == 0 && !frame.damaged {
					close(entered)
					<-gate
				}
				if frame.damaged {
					damages++
				} else {
					parsed = append(parsed, string(frame.data))
				}
			})
			finish := sync.OnceFunc(frames.finish)
			defer func() { release(); finish() }()
			limit := 64
			data := []byte(`{"type":"turn_start","index":0}`)
			if budget == "bytes" {
				limit = 8
				data = []byte(executionFramesAssistant(t, memberToolFrameBudget))
			}
			frames.Write(append(append([]byte(nil), data...), '\n'))
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("raw-frame consumer did not enter gate")
			}
			want := []string{string(data)}
			written := make(chan struct{})
			go func() {
				for i := 1; i < limit; i++ {
					frames.Write(append(append([]byte(nil), data...), '\n'))
				}
				close(written)
			}()
			select {
			case <-written:
			case <-time.After(2 * time.Second):
				t.Fatal("enqueue blocked on the parser before the declared boundary")
			}
			frames.mu.Lock()
			count, size, overflowed := frames.count, frames.bytes, frames.overflowed
			frames.mu.Unlock()
			if count != limit || size != limit*len(data) || overflowed || ctx.Err() != nil || cap(frames.line) > memberToolFrameBudget {
				t.Fatalf("in-flight frame was excluded or exact boundary rejected: count=%d bytes=%d overflow=%t", count, size, overflowed)
			}
			for i := 1; i < limit; i++ {
				want = append(want, want[0])
			}
			// Mutate the producer's input after enqueue; the consumer must own its copy.
			for i := range data {
				data[i] = 'x'
			}
			overflowDone := make(chan struct{})
			go func() {
				frames.Write([]byte("0\n")) // One additional raw byte/frame exceeds the active budget.
				frames.Write([]byte("AFTER-OVERFLOW-DRAIN\n"))
				close(overflowDone)
			}()
			select {
			case <-overflowDone:
			case <-time.After(2 * time.Second):
				t.Fatal("overflow blocked raw stdout draining")
			}
			frames.mu.Lock()
			count, size, overflowed = frames.count, frames.bytes, frames.overflowed
			frames.mu.Unlock()
			if count != limit || size != limit*len(data) || !overflowed || ctx.Err() == nil {
				t.Fatalf("overflow did not preserve bounded in-flight accounting and cancel: count=%d bytes=%d overflow=%t", count, size, overflowed)
			}
			joined := make(chan struct{})
			go func() { finish(); close(joined) }()
			select {
			case <-joined:
				t.Fatal("finish abandoned a gated parser")
			default:
			}
			release()
			select {
			case <-joined:
			case <-time.After(2 * time.Second):
				t.Fatal("parser did not finish its bounded prefix after release")
			}
			if !reflect.DeepEqual(parsed, want) || damages != 1 || frames.count != 0 || frames.bytes != 0 {
				t.Fatalf("raw buffers were reused, prefix reordered, or accounting leaked: parsed=%d want=%d damage=%d count=%d bytes=%d", len(parsed), len(want), damages, frames.count, frames.bytes)
			}
			if budget == "count" {
				data, err := os.ReadFile(log.file.Name())
				if err != nil || !bytes.HasSuffix(data, []byte("AFTER-OVERFLOW-DRAIN\n")) {
					t.Fatal("overflow stopped bounded raw logging/draining")
				}
			} else if log.size != 1<<20 || !log.truncated {
				t.Fatal("raw log cap was confused with parser pending bytes")
			}
			t.Logf("budget=%s accepted including in-flight=%d frames/%d raw bytes; overflow nonblocking, prefix joined, damage=%d", budget, limit, size, damages)
		})
	}
}

func TestExecutionFramesOverflowDrainsAndReapsChild(t *testing.T) {
	root := t.TempDir()
	body := "set -eu\n" + executionHeader + executionEmit(executionStart) + "while [ ! -f emit-tail ]; do :; done\n" +
		strings.Repeat(executionEmit(`{"type":"turn_start"}`), 80) + executionEmit(executionAssistant("stop"), executionSettled) + "printf '%s\\n' 'AFTER-OVERFLOW-STDERR-DRAIN' >&2\n: > tail-emitted\n"
	path := executable(t, filepath.Join(root, "fake-pi"), body)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log, errLog := executionFramesLog(t, cancel), executionFramesLog(t, cancel)
	r := ExecutionReceipt{Outcome: OutcomeUnknown, ExitCode: -1}
	delivery := newExecutionDelivery(ctx, cancel, func(Event) error { return nil })
	out := &executionOutput{receipt: &r, delivery: delivery, runID: "run1"}
	entered, gate := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	frames := newExecutionFrames(log, cancel, func(frame executionFrame) {
		out.recordFrame(frame)
		if string(frame.data) == executionStart {
			close(entered)
			<-gate
		}
	})
	finish := sync.OnceFunc(func() { frames.finish(); r.DeliveryFailed = delivery.finish() })
	defer func() { release(); finish() }()
	drain, errDrain := &executionDrain{Writer: frames}, &executionDrain{Writer: errLog}
	// This bounded child deliberately does not obey the queue's cancellation
	// request. Draining must not depend on cancellation terminating a writer.
	cmd := exec.Command(path)
	cmd.Dir, cmd.Stdout, cmd.Stderr, cmd.WaitDelay = root, drain, errDrain, 100*time.Millisecond
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var waitErr error
	waited := make(chan struct{})
	go func() { waitErr = cmd.Wait(); close(waited) }()
	defer func() { release(); cmd.Process.Kill(); <-waited }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("parser did not reach acceptance gate")
	}
	if err := os.WriteFile(filepath.Join(root, "emit-tail"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("overflow blocked the producer, pipe EOF or direct-child reaping")
	}
	if waitErr != nil || !drain.complete || !errDrain.complete || ctx.Err() == nil {
		t.Fatalf("overflow must request cancellation without abandoning complete child output: Wait=%v stdoutEOF=%t stderrEOF=%t ctx=%v", waitErr, drain.complete, errDrain.complete, ctx.Err())
	}
	if _, err := os.Stat(filepath.Join(root, "tail-emitted")); err != nil {
		t.Fatal("child did not write its entire native tail")
	}
	release()
	finish()
	r.Started, r.DirectChildReaped, r.ExitCode = true, cmd.ProcessState != nil, cmd.ProcessState.ExitCode()
	r.OutputComplete = waitErr == nil && drain.complete && errDrain.complete && !out.damaged
	if !r.Started || !r.Accepted || !r.DirectChildReaped || r.ExitCode != 0 || r.OutputComplete || r.Outcome != OutcomeUnknown || r.Terminal != "" || r.DeliveryFailed || !out.damaged {
		t.Fatalf("parser overflow became delivery failure, terminal inference or success: %+v", r)
	}
	data, err := os.ReadFile(log.file.Name())
	if err != nil || !bytes.HasSuffix(data, []byte(executionSettled+"\n")) {
		t.Fatal("overflow stopped continuous raw logging/draining")
	}
	data, err = os.ReadFile(errLog.file.Name())
	if err != nil || string(data) != "AFTER-OVERFLOW-STDERR-DRAIN\n" {
		t.Fatal("overflow stopped stderr draining")
	}
	t.Logf("child wrote all output despite cancellation request; Wait=%v stdoutEOF=%t stderrEOF=%t receipt=%+v", waitErr, drain.complete, errDrain.complete, r)
}

func TestExecutionFramesDamageIsOrdered(t *testing.T) {
	large := executionFramesAssistant(t, memberToolFrameBudget+1)
	for name, bad := range map[string]string{"malformed": `{"type":"turn_end"`, "oversize": large, "partial-EOF": `{"type":"agent_end"`} {
		for _, later := range []bool{false, true} {
			if name == "partial-EOF" && later {
				continue
			}
			t.Run(fmt.Sprintf("%s/later=%t", name, later), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				log := executionFramesLog(t, cancel)
				r := ExecutionReceipt{Outcome: OutcomeUnknown}
				var snapshots []string
				delivery := newExecutionDelivery(ctx, cancel, func(event Event) error {
					if event.Kind == ResponseEvent {
						snapshots = append(snapshots, event.Text)
					}
					return nil
				})
				out := &executionOutput{receipt: &r, delivery: delivery, runID: "run1"}
				frames := newExecutionFrames(log, cancel, out.recordFrame)
				stream := `{"type":"session","version":3,"id":"run1","cwd":"/fixture"}` + "\n" + executionStart + "\n" + executionAssistant("stop") + "\n" + bad
				if name != "partial-EOF" {
					stream += "\n"
					if later {
						stream += strings.Replace(executionAssistant("stop"), "合成\\u2028回答", "later authoritative response", 1) + "\n"
					}
					stream += executionSettled + "\n"
				}
				data := []byte(stream)
				for len(data) > 0 {
					n := min(len(data), 32749)
					frames.Write(data[:n])
					data = data[n:]
				}
				frames.finish()
				r.DeliveryFailed = delivery.finish()
				wantTerminal := Outcome("")
				wantSnapshots := []string{"合成\u2028回答"}
				if later {
					wantTerminal = OutcomeSucceeded
					wantSnapshots = append(wantSnapshots, "later authoritative response")
				}
				if !out.damaged || out.last != wantTerminal || r.Terminal != wantTerminal || !r.Accepted || r.Outcome != OutcomeUnknown || r.DeliveryFailed || ctx.Err() != nil || !reflect.DeepEqual(snapshots, wantSnapshots) {
					t.Fatalf("damage was reordered, retained stale terminal inference, or lost later valid frames: receipt=%+v last=%s damaged=%t snapshots=%q", r, out.last, out.damaged, snapshots)
				}
			})
		}
	}
}

func TestExecutionFramesAssistantBoundaries(t *testing.T) {
	for _, tc := range []struct {
		size   int
		ending string
		valid  bool
	}{{1 << 20, "\n", true}, {(1 << 20) + 1, "\n", false}, {(1 << 20) - 1, "\r\n", true}, {1 << 20, "\r\n", false}} {
		t.Run(fmt.Sprintf("JSON=%d/CR=%t", tc.size, tc.ending == "\r\n"), func(t *testing.T) {
			body := "set -eu\n" + executionHeader + executionEmit(executionStart) + "/bin/cat frame.jsonl\n" + executionEmit(executionAssistant("stop"), executionSettled) + ": > drained\n"
			e, request, _, _ := memberToolFixture(t, body)
			frame := executionFramesAssistant(t, tc.size)
			if err := os.WriteFile(filepath.Join(request.CWD, "frame.jsonl"), []byte(frame+tc.ending), 0600); err != nil {
				t.Fatal(err)
			}
			var snapshots []string
			got := e.Execute(context.Background(), request, func(event Event) error {
				if event.Kind == ResponseEvent {
					snapshots = append(snapshots, event.Text)
				}
				return nil
			})
			wantOutcome := OutcomeUnknown
			wantSnapshots := []string{"合成\u2028回答"}
			if tc.valid {
				wantOutcome = OutcomeSucceeded
				wantSnapshots = append([]string{""}, wantSnapshots...)
			}
			if got.Outcome != wantOutcome || got.Terminal != OutcomeSucceeded || !got.Started || !got.Accepted || !got.DirectChildReaped || got.ExitCode != 0 || got.OutputComplete != tc.valid || !got.LogsTruncated || got.DeliveryFailed || !reflect.DeepEqual(snapshots, wantSnapshots) {
				t.Fatalf("assistant content frame path broke bounded framing, empty snapshots or later parsing: %+v snapshots=%q", got, snapshots)
			}
			if _, err := os.Stat(filepath.Join(request.CWD, "drained")); err != nil {
				t.Fatal("child did not finish writing all native frames")
			}
		})
	}
}

func TestExecutionFramesToolCancellationAndInheritedPipes(t *testing.T) {
	for _, mode := range []string{"caller-cancel", "inherited"} {
		t.Run(mode, func(t *testing.T) {
			body := "/bin/sleep 10 &\nprintf '%s\\n' \"$!\" > descendant\n" + executionHeader + executionEmit(executionStart)
			if mode == "inherited" {
				body += executionEmit(executionAssistant("stop"), executionSettled) + "exit 0\n"
			} else {
				body += "wait\n"
			}
			e, request, _, _ := memberToolFixture(t, body)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			started := time.Now()
			got := e.Execute(ctx, request, func(event Event) error {
				if mode == "caller-cancel" && event.Kind == AcceptedEvent {
					cancel()
				}
				return nil
			})
			data, err := os.ReadFile(filepath.Join(request.CWD, "descendant"))
			var pid int
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Sscan(string(data), &pid); err != nil || pid <= 1 {
				t.Fatal("missing fixture descendant")
			}
			t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
			if syscall.Kill(pid, 0) != nil || got.Outcome != OutcomeUnknown || !got.Started || !got.Accepted || !got.DirectChildReaped || got.OutputComplete || got.DeliveryFailed || time.Since(started) > 2*time.Second {
				t.Fatalf("cancellation or inherited pipes falsely proved clean external completion: %+v", got)
			}
			if mode == "inherited" && (got.ExitCode != 0 || got.Terminal != OutcomeSucceeded) {
				t.Fatalf("genuine native terminal evidence was lost with incomplete pipes: %+v", got)
			}
		})
	}
}

func TestExecutionFramesStartFailure(t *testing.T) {
	e, request, _, _ := memberToolFixture(t, "exit 0\n")
	if err := os.Remove(e.(piExecutor).path); err != nil {
		t.Fatal(err)
	}
	calls := 0
	got := e.Execute(context.Background(), request, func(Event) error { calls++; return nil })
	if got.Started || got.Accepted || got.Outcome != OutcomeNotStarted || got.DirectChildReaped || got.OutputComplete || got.DeliveryFailed || calls != 0 {
		t.Fatalf("failed Start created native evidence or delivery work: %+v calls=%d", got, calls)
	}
}

func BenchmarkExecutionFramesPlainParser(b *testing.B) {
	for _, codex := range []bool{false, true} {
		b.Run(fmt.Sprintf("codex=%t", codex), func(b *testing.B) {
			file, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			if err != nil {
				b.Fatal(err)
			}
			defer file.Close()
			stream := `{"type":"session","version":3,"id":"run1","cwd":"/fixture"}` + "\n" + executionStart + "\n" + executionAssistant("stop") + "\n" + executionSettled + "\n"
			if codex {
				stream = codexHeader + "\n" + codexStart + "\n" + codexResponse + "\n" + codexCompleted + "\n"
			}
			data := []byte(stream)
			log := &executionLog{file: file, cancel: func() {}}
			run := func() {
				log.size = 0
				r := ExecutionReceipt{}
				out := executionOutput{log: log, receipt: &r, runID: "run1", codex: codex, frameLimit: executionFrameLimit}
				out.Write(data)
				if r.Terminal != OutcomeSucceeded || out.damaged {
					b.Fatal("invalid plain parser benchmark fixture")
				}
			}
			run() // Warm encoding/json's type caches outside the allocation measurement.
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				run()
			}
		})
	}
}
