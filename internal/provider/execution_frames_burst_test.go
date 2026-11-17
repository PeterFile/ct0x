package provider

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// Match the smallest observed raw delta frame without retaining native content.
// Use the captured span's 114-frame count; 131 shortest frames fit one 32 KiB read.
func executionFramesDeltaBurst(count int) []byte {
	var burst bytes.Buffer
	for i := 0; i < count; i++ {
		prefix := fmt.Sprintf(`{"type":"message_update","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"assistantMessageEvent":{"type":"toolcall_delta","contentIndex":%d,"delta":"`, i%10)
		const suffix = `"}}`
		burst.WriteString(prefix)
		burst.WriteString(strings.Repeat(string(rune('a'+i%26)), 249-len(prefix)-len(suffix)))
		burst.WriteString(suffix + "\n")
	}
	return burst.Bytes()
}

func TestExecutionFramesSingleReadDeltaBurst(t *testing.T) {
	for _, count := range []int{114, 131} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			burst := executionFramesDeltaBurst(count)
			if len(burst) != count*250 || len(burst) > 32768 {
				t.Fatal("generated burst does not fit one copier read")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			log := executionFramesLog(t, cancel)
			r := ExecutionReceipt{Outcome: OutcomeUnknown}
			out := &executionOutput{receipt: &r, runID: "run1"}
			preludeDone, entered, gate := make(chan struct{}), make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(gate) })
			seen, damages := 0, 0
			var parsed []string
			f := newExecutionFrames(log, cancel, func(frame executionFrame) {
				if frame.damaged {
					damages++
				} else {
					if seen == 2 {
						close(entered)
						<-gate
					}
					if seen >= 2 {
						parsed = append(parsed, string(frame.data))
					}
					seen++
				}
				out.recordFrame(frame)
				if seen == 2 {
					close(preludeDone)
				}
			})
			finish := sync.OnceFunc(f.finish)
			defer func() { release(); finish() }()
			f.Write([]byte(`{"type":"session","version":3,"id":"run1","cwd":"/fixture"}` + "\n" + executionStart + "\n"))
			select {
			case <-preludeDone:
			case <-time.After(2 * time.Second):
				t.Fatal("prelude did not drain")
			}
			written := make(chan struct{})
			go func() { f.Write(burst); close(written) }()
			select {
			case <-written:
			case <-time.After(2 * time.Second):
				t.Fatal("single-read producer blocked on the parser")
			}
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("delta parser did not enter gate")
			}
			f.mu.Lock()
			pending, size, overflow := f.count, f.bytes, f.overflowed
			f.mu.Unlock()
			if overflow || ctx.Err() != nil || pending != count || size != count*249 {
				t.Fatalf("legal single-read delta burst canceled: pending=%d bytes=%d overflow=%t canceled=%t", pending, size, overflow, ctx.Err() != nil)
			}
			joined := make(chan struct{})
			go func() { finish(); close(joined) }()
			select {
			case <-joined:
				t.Fatal("finish abandoned the in-flight parser")
			default:
			}
			release()
			select {
			case <-joined:
			case <-time.After(2 * time.Second):
				t.Fatal("accepted delta prefix did not drain")
			}
			want := strings.Split(strings.TrimSuffix(string(burst), "\n"), "\n")
			if !reflect.DeepEqual(parsed, want) || damages != 0 || out.damaged || !r.Accepted || r.Terminal != "" || r.Outcome != OutcomeUnknown || f.count != 0 || f.bytes != 0 {
				t.Fatalf("delta drain reordered/lost frames, leaked budget, or fabricated terminal: parsed=%d damage=%d pending=%d bytes=%d terminal=%t", len(parsed), damages, f.count, f.bytes, r.Terminal != "")
			}
		})
	}
}

func TestExecutionFramesOverflowReason(t *testing.T) {
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)
	body := "set -eu\n" + executionHeader + executionEmit(executionStart) +
		"while [ ! -f accepted ]; do :; done\n/bin/cat burst.jsonl\nexec /bin/sleep 10\n"
	e, request, _, _ := memberToolFixture(t, body)
	// Small valid frames exceed the fixed count budget in one available pipe read.
	data := strings.Repeat(`{"type":"turn_start"}`+"\n", 1024)
	if err := os.WriteFile(filepath.Join(request.CWD, "burst.jsonl"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := e.Execute(ctx, request, func(event Event) error {
		if event.Kind == AcceptedEvent {
			return os.WriteFile(filepath.Join(request.CWD, "accepted"), nil, 0600)
		}
		return nil
	})
	const reason = "输出帧队列超限，执行结果未知，需对账"
	if got.Reason != reason || len(got.Reason) > 72 || got.Outcome != OutcomeUnknown || !got.Started || !got.Accepted || !got.DirectChildReaped || got.ExitCode != -1 || got.OutputComplete || got.Terminal != "" || got.DeliveryFailed || got.LogsTruncated || ctx.Err() != nil {
		t.Fatalf("raw overflow lacks bounded static diagnosis or changed evidence: reason=%q bytes=%d outcome=%s started=%t accepted=%t reaped=%t exit=%d complete=%t terminal=%t delivery_failed=%t truncated=%t caller_canceled=%t", got.Reason, len(got.Reason), got.Outcome, got.Started, got.Accepted, got.DirectChildReaped, got.ExitCode, got.OutputComplete, got.Terminal != "", got.DeliveryFailed, got.LogsTruncated, ctx.Err() != nil)
	}
}

func BenchmarkExecutionFramesDeltaBurst(b *testing.B) {
	for _, count := range []int{32, 114, 131} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			file, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			if err != nil {
				b.Fatal(err)
			}
			defer file.Close()
			burst := executionFramesDeltaBurst(count)
			prelude := []byte(`{"type":"session","version":3,"id":"run1","cwd":"/fixture"}` + "\n" + executionStart + "\n")
			cancels, parsed := 0, 0
			run := func() {
				log := &executionLog{file: file, cancel: func() { cancels++ }}
				r := ExecutionReceipt{}
				out := &executionOutput{receipt: &r, runID: "run1"}
				ready, gate := make(chan struct{}), make(chan struct{})
				seen := 0
				f := newExecutionFrames(log, log.cancel, func(frame executionFrame) {
					if !frame.damaged {
						if seen == 2 {
							<-gate
						}
						seen++
					}
					out.recordFrame(frame)
					if seen == 2 {
						close(ready)
					}
				})
				f.Write(prelude)
				<-ready
				f.Write(burst)
				close(gate)
				f.finish()
				parsed += seen - 2
			}
			run()
			cancels, parsed = 0, 0
			b.ReportAllocs()
			b.SetBytes(int64(len(prelude) + len(burst)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				run()
			}
			b.StopTimer()
			b.ReportMetric(float64(cancels)/float64(b.N), "cancels/op")
			b.ReportMetric(float64(parsed)/float64(b.N), "parsed_frames/op")
			b.ReportMetric(float64(executionFramesCountLimit)*float64(unsafe.Sizeof(executionFrame{})), "channel_slot_bytes")
		})
	}
}
