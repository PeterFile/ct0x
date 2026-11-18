package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"
)

// Adapted from the independent review repro. These children are synthetic,
// offline protocol fixtures, not Pi or evidence of a real external write.
func TestPiSessionRepairReceiptSafety(t *testing.T) {
	binary := piSessionBuildFixture(t)
	for _, mode := range []string{"unknown-write", "missing-tool-end", "pending-tool", "foreign-tool", "mismatched-tool-id", "mismatched-result", "write-error-file-clean", "bad-lf", "replace-file"} {
		t.Run(mode, func(t *testing.T) {
			selected, extension, request := piSessionFixtureSetup(t, binary)
			request.Prompt = "fixture:" + mode
			e, _ := piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{})
			var events []Event
			r := e.Execute(context.Background(), request, func(ev Event) error { events = append(events, ev); return nil })
			want := OutcomeUnknown
			if mode == "bad-lf" || mode == "replace-file" {
				want = OutcomeSucceeded
			}
			t.Logf("synthetic mode=%s outcome=%s terminal=%s started=%t accepted=%t reaped=%t output_complete=%t ref=%t callbacks=%d", mode, r.Outcome, r.Terminal, r.Started, r.Accepted, r.DirectChildReaped, r.OutputComplete, r.NativeRef != nil, len(events))
			piSessionRepairAssertEvidence(t, r, events, want)
		})
	}
	t.Run("mismatched-native-id-evidence", func(t *testing.T) {
		selected, extension, request := piSessionFixtureSetup(t, binary)
		e, _ := piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{})
		first := e.Execute(context.Background(), request, nil)
		piSessionAssertSucceeded(t, first, true)
		e, _ = piSessionFixtureExecutor(t, selected, extension, piSessionTokenTwo, PiSessionOptions{Resume: first.NativeRef, Binding: json.RawMessage(`{"turn":2}`)})
		request.RunID, request.Prompt = "run2", "fixture:wrong-native-id"
		var events []Event
		r := e.Execute(context.Background(), request, func(ev Event) error { events = append(events, ev); return nil })
		raw, err := os.ReadFile(r.StdoutPath)
		if err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(bytes.TrimSpace(raw), []byte{'\n'})
		var header struct{ Type, ID string }
		if err := json.Unmarshal(lines[0], &header); err != nil {
			t.Fatal(err)
		}
		t.Logf("synthetic stdout_header_id=%s outcome=%s terminal=%s native_id=%q accepted=%t callbacks=%d", header.ID, r.Outcome, r.Terminal, r.NativeSessionID, r.Accepted, len(events))
		if header.Type != "session" || !piSessionUUID.MatchString(header.ID) || header.ID == first.NativeSessionID || r.NativeSessionID != header.ID {
			t.Error("retain the actual valid UUID header as evidence, not the expected reference identity")
		}
		piSessionRepairAssertEvidence(t, r, events, OutcomeUnknown)
		if r.NativeRef == nil || r.NativeRef.Pi != nil || string(r.NativeRef.Binding) != `{"turn":2}` {
			t.Fatal("mismatched observed identity must not become a qualified reference or lose the current binding barrier")
		}
	})
	t.Run("new-process-does-not-erase-write-error", func(t *testing.T) {
		selected, extension, request := piSessionFixtureSetup(t, binary)
		e, _ := piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{})
		first := e.Execute(context.Background(), request, nil)
		piSessionAssertSucceeded(t, first, true)
		original := cloneNativeSessionReference(first.NativeRef)
		e, _ = piSessionFixtureExecutor(t, selected, extension, piSessionTokenTwo, PiSessionOptions{Resume: first.NativeRef})
		request.RunID, request.Prompt = "run2", "fixture:write-error-file-clean"
		var events []Event
		r := e.Execute(context.Background(), request, func(ev Event) error { events = append(events, ev); return nil })
		piSessionRepairAssertEvidence(t, r, events, OutcomeUnknown)
		if first.NativeRef.Pi.FileSHA256 != original.Pi.FileSHA256 || first.NativeRef.Pi.Size != original.Pi.Size {
			t.Fatal("unknown invocation mutated the prior reference")
		}
		e, _ = piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{Resume: original})
		request.RunID, request.Prompt = "run3", "must-not-replay"
		r = e.Execute(context.Background(), request, nil)
		if r.Started || r.Outcome != OutcomeNotStarted || r.NativeRef != nil {
			t.Fatal("a new process must not reuse a stale reference or adopt the clean-looking file after an unknown write")
		}
	})
}

func piSessionRepairAssertEvidence(t *testing.T, r ExecutionReceipt, events []Event, want Outcome) {
	t.Helper()
	if r.Outcome != want || r.Terminal != OutcomeSucceeded || !r.Started || !r.Accepted || !r.DirectChildReaped || !r.OutputComplete || r.DeliveryFailed || r.ExitCode != 0 || r.NativeRef != nil && r.NativeRef.Pi != nil {
		t.Fatalf("execution classification or actual native/output/wait evidence changed: %+v", r)
	}
	var kinds []EventKind
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
		if ev.NativeSessionID != r.NativeSessionID {
			t.Fatal("callback identity differs from actual observed header")
		}
	}
	if len(kinds) < 4 || kinds[0] != SessionEvent || kinds[1] != AcceptedEvent || kinds[len(kinds)-1] != TerminalEvent || !slices.Contains(kinds, ResponseEvent) || events[len(events)-1].Terminal != OutcomeSucceeded {
		t.Fatalf("independent lifecycle callbacks were swallowed: %v", kinds)
	}
}

// No session content is persisted by this child, so capture always fails. Tool
// observation must remain active before and after other anchor-only failures.
func piSessionRepairEvents(tb testing.TB, mode string) []map[string]any {
	tb.Helper()
	lines, _ := piSessionToolHistory("send_message")
	events := []map[string]any{lines[0], {"type": "agent_start"}}
	message := func(body any) {
		events = append(events, map[string]any{"type": "message_start", "message": body}, map[string]any{"type": "message_end", "message": body})
	}
	message(lines[3]["message"])
	message(lines[4]["message"])
	if mode != "valid" {
		events = append(events, map[string]any{"type": "entry_appended", "entry": map[string]any{"type": "custom"}})
	}
	call := lines[5]["message"].(map[string]any)
	result := lines[6]["message"].(map[string]any)
	start := map[string]any{"type": "tool_execution_start", "toolCallId": "call1", "toolName": "send_message", "args": map[string]any{"text": "offline"}}
	end := map[string]any{"type": "tool_execution_end", "toolCallId": "call1", "toolName": "send_message", "isError": false, "result": map[string]any{"content": result["content"]}}
	switch mode {
	case "unknown-write", "late-success-after-error":
		end["isError"] = true
	case "foreign-tool":
		call["content"].([]any)[0].(map[string]any)["name"] = "bash"
		start["toolName"], end["toolName"], result["toolName"] = "bash", "bash", "bash"
	case "wrong-id":
		end["toolCallId"] = "other"
	case "wrong-name":
		end["toolName"] = "create_member"
	case "wrong-args":
		start["args"] = map[string]any{"text": "other"}
	case "wrong-result":
		end["result"] = map[string]any{"content": "different"}
	case "error-result":
		result["isError"] = true
	case "missing-is-error":
		delete(end, "isError")
	case "contradictory-error-flag":
		end["result"] = map[string]any{"content": result["content"], "isError": true}
	case "result-wrong-name":
		result["toolName"] = "create_member"
	case "model-anchor-only":
		call["model"] = "other"
	}
	message(call)
	if mode != "missing-start" {
		events = append(events, start)
	}
	if mode == "duplicate-start" {
		events = append(events, start)
	}
	if mode == "wrong-update-args" {
		events = append(events, map[string]any{"type": "tool_execution_update", "toolCallId": "call1", "toolName": "send_message", "args": map[string]any{"text": "different"}})
	}
	if mode != "missing-end" && mode != "pending-tool" {
		events = append(events, end)
	}
	if mode == "duplicate-end" {
		events = append(events, end)
	}
	if mode == "late-success-after-error" {
		later := map[string]any{}
		for key, value := range end {
			later[key] = value
		}
		later["isError"] = false
		events = append(events, later)
	}
	if mode != "missing-result" && mode != "pending-tool" {
		message(result)
	}
	message(lines[7]["message"])
	return append(events, map[string]any{"type": "agent_end", "messages": []any{}, "willRetry": false}, map[string]any{"type": "agent_settled", "aborted": false})
}

func TestPiSessionRepairObservesToolsAfterAnchorLoss(t *testing.T) {
	for _, mode := range []string{"valid", "anchor-only", "model-anchor-only", "unknown-write", "late-success-after-error", "missing-end", "pending-tool", "foreign-tool", "wrong-id", "wrong-name", "wrong-args", "wrong-result", "error-result", "missing-is-error", "contradictory-error-flag", "result-wrong-name", "missing-start", "duplicate-start", "duplicate-end", "missing-result", "wrong-update-args"} {
		t.Run(mode, func(t *testing.T) {
			binary := executable(t, filepath.Join(piSessionPrivateTemp(t), "synthetic-child"), "/bin/cat >/dev/null\n/bin/cat events.jsonl\nprintf done > child-finished\n")
			selected, extension, request := piSessionFixtureSetup(t, binary)
			events := piSessionRepairEvents(t, mode)
			events[0]["cwd"] = request.CWD
			if err := os.WriteFile(filepath.Join(request.CWD, "events.jsonl"), piSessionTestBytes(events), 0600); err != nil {
				t.Fatal(err)
			}
			e, _ := piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{})
			var callbacks []Event
			r := e.Execute(context.Background(), request, func(ev Event) error { callbacks = append(callbacks, ev); return nil })
			want := OutcomeUnknown
			if mode == "valid" || mode == "anchor-only" || mode == "model-anchor-only" {
				want = OutcomeSucceeded
			}
			piSessionRepairAssertEvidence(t, r, callbacks, want)
			if _, err := os.Stat(filepath.Join(request.CWD, "child-finished")); err != nil {
				t.Fatal("execution returned before the synthetic child finished")
			}
		})
	}
}

func TestPiSessionRepairMalformedHeaderDoesNotInventEvidence(t *testing.T) {
	for _, mode := range []string{"invalid-uuid", "duplicate-key", "wrong-cwd", "invalid-timestamp"} {
		t.Run(mode, func(t *testing.T) {
			binary := executable(t, filepath.Join(piSessionPrivateTemp(t), "synthetic-child"), "/bin/cat >/dev/null\n/bin/cat events.jsonl\n")
			selected, extension, request := piSessionFixtureSetup(t, binary)
			events := piSessionRepairEvents(t, "valid")
			events[0]["cwd"] = request.CWD
			switch mode {
			case "invalid-uuid":
				events[0]["id"] = request.RunID
			case "wrong-cwd":
				events[0]["cwd"] = "/wrong"
			case "invalid-timestamp":
				events[0]["timestamp"] = "invalid"
			}
			data := piSessionTestBytes(events)
			if mode == "duplicate-key" {
				data = bytes.Replace(data, []byte(`"version":3`), []byte(`"version":3,"version":3`), 1)
			}
			if err := os.WriteFile(filepath.Join(request.CWD, "events.jsonl"), data, 0600); err != nil {
				t.Fatal(err)
			}
			e, _ := piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{})
			callbacks := 0
			r := e.Execute(context.Background(), request, func(Event) error { callbacks++; return nil })
			if r.Outcome != OutcomeUnknown || r.NativeSessionID != "" || r.Accepted || r.Terminal != "" || callbacks != 0 || !r.Started || !r.DirectChildReaped || r.OutputComplete || r.NativeRef != nil {
				t.Fatalf("malformed header manufactured associated lifecycle evidence: %+v callbacks=%d", r, callbacks)
			}
		})
	}
}

func TestPiSessionRepairUnknownJoinsCallback(t *testing.T) {
	binary := piSessionBuildFixture(t)
	selected, extension, request := piSessionFixtureSetup(t, binary)
	request.Prompt = "fixture:unknown-write"
	e, _ := piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{})
	ready, release, done := make(chan struct{}), make(chan struct{}), make(chan ExecutionReceipt, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var events []Event
	joined := make(chan struct{})
	releaseCallback := sync.OnceFunc(func() { close(release) })
	go func() {
		defer close(joined)
		done <- e.Execute(ctx, request, func(ev Event) error {
			events = append(events, ev)
			if ev.Kind == TerminalEvent {
				close(ready)
				<-release
			}
			return nil
		})
	}()
	defer func() { releaseCallback(); <-joined }()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("terminal callback not reached")
	}
	piSessionWaitFile(t, filepath.Join(request.CWD, "fixture-finished"))
	select {
	case <-done:
		t.Fatal("unknown outcome returned with callback still running")
	default:
	}
	releaseCallback()
	select {
	case r := <-done:
		piSessionRepairAssertEvidence(t, r, events, OutcomeUnknown)
	case <-ctx.Done():
		t.Fatal("unknown execution did not join its callback")
	}
}

func TestPiSessionRepairEntryBoundMemory(t *testing.T) {
	profile := piSessionTestProfile()
	// Stop on the small reviewer RED to avoid intentionally allocating hundreds
	// of MiB on the baseline. GREEN must also cover both near-limit inputs.
	for _, size := range []int{2 << 20, piSessionFileLimit - 1, piSessionFileLimit} {
		data := bytes.Repeat([]byte{'\n'}, size)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := parsePiSession(data, profile)
		runtime.ReadMemStats(&after)
		extra := after.TotalAlloc - before.TotalAlloc
		t.Logf("input_bytes=%d allocated_bytes=%d entry_limit=%d rejected=%t", len(data), extra, piSessionEntryLimit, err != nil)
		if err == nil || extra > 1<<20 {
			t.Fatalf("over-entry-limit rejection allocated per newline: error=%v allocated=%d", err, extra)
		}
	}
	for _, data := range [][]byte{
		bytes.Repeat([]byte("{}\n"), piSessionFileLimit/3),
		append(bytes.Repeat([]byte{'x'}, piSessionFileLimit-1), '\n'),
	} {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := parsePiSession(data, profile)
		runtime.ReadMemStats(&after)
		extra := after.TotalAlloc - before.TotalAlloc
		t.Logf("near_bound_short_or_long_lines input_bytes=%d allocated_bytes=%d rejected=%t", len(data), extra, err != nil)
		if err == nil || extra > 1<<20 {
			t.Fatalf("short-line/long-line bounds were not checked before allocation: error=%v allocated=%d", err, extra)
		}
	}
}

func TestPiSessionRepairExactFileBoundsRemainValid(t *testing.T) {
	t.Run("entry-count", func(t *testing.T) {
		lines := piSessionTestLines()
		parent := "usage1"
		for len(lines) < piSessionEntryLimit {
			id := fmt.Sprintf("usage-%d", len(lines))
			lines = append(lines, map[string]any{"type": "usage", "id": id, "parentId": parent, "timestamp": piSessionTestTime, "kind": "fixture", "provider": "synthetic", "model": "model"})
			parent = id
		}
		if _, err := parsePiSession(piSessionTestBytes(lines), piSessionTestProfile()); err != nil {
			t.Fatal("exact entry limit rejected", err)
		}
		lines = append(lines, map[string]any{"type": "usage", "id": "extra", "parentId": parent, "timestamp": piSessionTestTime, "kind": "fixture", "provider": "synthetic", "model": "model"})
		if _, err := parsePiSession(piSessionTestBytes(lines), piSessionTestProfile()); err == nil {
			t.Fatal("one extra entry accepted")
		}
	})
	t.Run("line-length", func(t *testing.T) {
		lines := piSessionTestLines()
		encoded, err := json.Marshal(lines[5])
		if err != nil {
			t.Fatal(err)
		}
		block := lines[5]["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
		block["text"] = string(bytes.Repeat([]byte{'x'}, piSessionLineLimit-len(encoded)+len("done")))
		if _, err := parsePiSession(piSessionTestBytes(lines), piSessionTestProfile()); err != nil {
			t.Fatal("exact line limit rejected", err)
		}
		block["text"] = block["text"].(string) + "x"
		if _, err := parsePiSession(piSessionTestBytes(lines), piSessionTestProfile()); err == nil {
			t.Fatal("one extra byte in a line accepted")
		}
	})
}

func BenchmarkPiSessionRepairObserver(b *testing.B) {
	for _, mode := range []string{"valid", "anchor-only", "unknown-write"} {
		b.Run(mode, func(b *testing.B) {
			profile := piSessionTestProfile()
			profile.tools = []string{"send_message"}
			var lines [][]byte
			for _, event := range piSessionRepairEvents(b, mode) {
				data, err := json.Marshal(event)
				if err != nil {
					b.Fatal(err)
				}
				lines = append(lines, data)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r := ExecutionReceipt{}
				out := executionOutput{receipt: &r, piSession: newPiSessionTracker(profile, "")}
				for _, line := range lines {
					out.recordPi(line)
				}
				if out.damaged || r.Terminal != OutcomeSucceeded {
					b.Fatal("invalid synthetic observer benchmark stream")
				}
			}
		})
	}
}
