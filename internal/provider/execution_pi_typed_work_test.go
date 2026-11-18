package provider

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const typedWriteTestField = "agent_workspace_write_unknown_v1"
const typedWriteTestCall = "call_one|fc_two"

func typedWriteTestDetails(role, runID, op string) map[string]string {
	return map[string]string{
		"role": role, "run_id": runID, "operation": op, "tool_call_id": typedWriteTestCall,
		"idempotency_key": fmt.Sprintf("%x", sha256.Sum256([]byte(op+typedWriteTestCall))),
		"request_sha256":  fmt.Sprintf("%x", sha256.Sum256([]byte(`{"input_id":"fixture"}`))),
	}
}

func typedWriteTestStart(op, id, args string) string {
	return fmt.Sprintf(`{"type":"tool_execution_start","toolCallId":%q,"toolName":%q,"args":%s}`, id, op, args)
}

func typedWriteTestEnd(op, details string) string {
	return fmt.Sprintf(`{"type":"tool_execution_end","toolCallId":%q,"toolName":%q,"result":{"content":[{"type":"text","text":"合成工具错误"}],"details":%s,"isError":true},"isError":true,"durationMs":1}`, typedWriteTestCall, op, details)
}

func typedWriteTestChild(t *testing.T, stream string) (Provider, ExecuteRequest, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "frames.jsonl"), []byte(stream), 0600); err != nil {
		t.Fatal(err)
	}
	cli := executable(t, filepath.Join(root, "fixture-pi"), "exec /bin/cat frames.jsonl")
	extensions, err := filepath.Abs("../../extensions/pi-bridge")
	if err != nil {
		t.Fatal(err)
	}
	runtimeDir := t.TempDir()
	if err := os.Chmod(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	return Provider{ID: "fixture-pi", Name: Pi, Version: "1.4.0", Installation: Installed, Path: cli},
		ExecuteRequest{RunID: "run1", RuntimeDir: runtimeDir, CWD: root, Prompt: "synthetic"}, extensions
}

func runTypedWriteTest(t *testing.T, runID, stream string, ordinary bool) (ExecutionReceipt, []Event) {
	t.Helper()
	selected, r, bridges := typedWriteTestChild(t, stream)
	r.RunID = runID
	const wire = `{"endpoint":"http://127.0.0.1:1/api/v1/member-tools","token":"synthetic-capability"}` + "\n"
	config := filepath.Join(r.CWD, "issued.json")
	if err := os.WriteFile(config, []byte(wire), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var e Executor
	var err error
	if ordinary {
		e, err = NewPiMemberToolExecutor(selected, filepath.Join(bridges, "index.ts"), config, 0)
	} else {
		a := NewPiFreshTaskWorkAdapter(filepath.Join(bridges, "fresh-task-work.ts"), 0)
		e, err = a.NewExecutor(ctx, selected, FreshTaskWorkBinding{ConfigPath: config, ConfigSHA256: sha256.Sum256([]byte(wire)), RunID: r.RunID, Operations: PiFreshTaskWorkToolNames()})
	}
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	got := e.Execute(ctx, r, func(ev Event) error {
		events = append(events, ev)
		return nil
	})
	return got, events
}

// The optional fixture is captured byte-for-byte by the offline Pi 1.4.0
// tests in extensions/pi-bridge/tests/fresh-task-work.test.mjs. Only the
// selected native frame changes; in particular toolResult and agent_end stay.
func TestPiFreshTaskWorkSDKPrintLifecycle(t *testing.T) {
	dir := os.Getenv("TEST_PI_TYPED_PRINT_FIXTURES")
	if dir == "" {
		t.Skip("set TEST_PI_TYPED_PRINT_FIXTURES to the captured Pi JSON print fixtures")
	}
	readFixture := func(name string) string {
		t.Helper()
		file, err := os.ReadFile(filepath.Join(dir, name+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		return string(file)
	}
	unknown, paired := readFixture("unknown"), readFixture("paired")
	for _, c := range []struct {
		name, stream     string
		want             Outcome
		complete, marked bool
	}{
		{"intact unknown marker", unknown, OutcomeUnknown, true, true},
		{"missing ack end", dropTypedPrintFrame(t, unknown, "tool_execution_end"), OutcomeUnknown, false, false},
		{"marker without ack start", dropTypedPrintFrame(t, unknown, "tool_execution_start"), OutcomeUnknown, false, false},
		{"intact parallel checked and denied", paired, OutcomeSucceeded, true, false},
		{"denial without ack end", dropTypedPrintFrame(t, paired, "tool_execution_end"), OutcomeUnknown, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			const runID = "fixture-fresh-turn"
			got, events := runTypedWriteTest(t, runID, c.stream, false)
			t.Logf("outcome=%s terminal=%s complete=%t accepted=%t reaped=%t exit=%d",
				got.Outcome, got.Terminal, got.OutputComplete, got.Accepted, got.DirectChildReaped, got.ExitCode)
			if got.Outcome != c.want || got.OutputComplete != c.complete || !got.Started || !got.Accepted ||
				got.NativeSessionID != runID || !got.DirectChildReaped || got.ExitCode != 0 || got.Terminal != OutcomeSucceeded ||
				got.LogsTruncated || got.DeliveryFailed || got.NativeRef != nil || strings.Contains(got.Reason, "写入") != c.marked {
				t.Fatalf("unexpected settlement or lost independent evidence: %+v", got)
			}
			if len(events) < 4 || events[0].Kind != SessionEvent || events[1].Kind != AcceptedEvent ||
				events[len(events)-2].Kind != ResponseEvent || events[len(events)-2].Text != "native stop" ||
				events[len(events)-1].Kind != TerminalEvent || events[len(events)-1].Terminal != OutcomeSucceeded {
				t.Fatalf("lost observed native ticks: %+v", events)
			}
			if logged, err := os.ReadFile(got.StdoutPath); err != nil || string(logged) != c.stream {
				t.Fatal("controlled child did not deliver the expected raw stream", err)
			}
		})
	}
}

func dropTypedPrintFrame(t *testing.T, stream, kind string) string {
	t.Helper()
	var rest strings.Builder
	dropped := 0
	for line := range strings.SplitAfterSeq(stream, "\n") {
		if line == "" {
			continue
		}
		var header struct{ Type, ToolName string }
		if err := json.Unmarshal([]byte(line), &header); err != nil {
			t.Fatal(err)
		}
		if header.Type == kind && header.ToolName == "ack_work_input" {
			dropped++
		} else {
			rest.WriteString(line)
		}
	}
	if dropped != 1 {
		t.Fatalf("expected exactly one ack_work_input %s, got %d", kind, dropped)
	}
	return rest.String()
}

// Feed the parser directly so this exercises the invocation's call budget,
// not a burst overflowing the independent asynchronous frame queue.
func TestPiFreshTaskWorkWriteCallBudget(t *testing.T) {
	const op = "ack_work_input"
	var r ExecutionReceipt
	w := &piTypedWorkOutput{role: "followup", tools: PiFreshTaskWorkToolNames()}
	o := executionOutput{receipt: &r, runID: "run1", piTypedWork: w}
	o.recordPi([]byte(`{"type":"session","id":"run1","version":3,"cwd":"/fixture"}`))
	o.recordPi([]byte(executionStart))
	pair := func(id string) {
		o.recordPi([]byte(typedWriteTestStart(op, id, `{}`)))
		o.recordPi([]byte(fmt.Sprintf(`{"type":"tool_execution_end","toolName":%q,"toolCallId":%q,"isError":false,"result":{"content":[]}}`, op, id)))
	}
	for i := range executionFramesCountLimit {
		pair(fmt.Sprintf("call-%d", i))
	}
	if o.damaged || w.hasOpenWrite() || len(w.writes) != executionFramesCountLimit {
		t.Fatal("write tracker did not accept the exact bound")
	}
	pair("extra-call")
	o.recordPi([]byte(executionAssistant("stop")))
	o.recordPi([]byte(executionSettled))
	if !o.damaged || len(w.writes) != executionFramesCountLimit || r.Terminal != OutcomeSucceeded || !r.Accepted {
		t.Fatal("write tracker overflow must deny settlement without losing terminal evidence or growing")
	}
}

func TestPiFreshTaskWorkWriteUncertainty(t *testing.T) {
	const op = "ack_work_input"
	m := typedWriteTestDetails("followup", "run1", op)
	evidence := memberToolJSON(t, m)
	details := `{"` + typedWriteTestField + `":` + evidence + `}`
	frame := typedWriteTestEnd(op, details)
	start := typedWriteTestStart(op, typedWriteTestCall, `{"input_id":"fixture"}`)
	pair := func(end string) string { return start + "\n" + end }
	checked := `{"type":"tool_execution_end","toolCallId":"call_one|fc_two","toolName":"ack_work_input","result":{"content":[{"type":"text","text":"checked"}]},"isError":false,"durationMs":1}`
	fakeTop := strings.Replace(evidence, `{`, `{"type":"`+typedWriteTestField+`",`, 1)
	fakeText := memberToolJSON(t, fakeTop+"\n"+frame)
	// Pi converts a thrown business denial into an outer isError with empty
	// details, not a bridge-authored isError result.
	denial := strings.Replace(typedWriteTestEnd(op, "{}"), `"details":{},"isError":true`, `"details":{}`, 1)
	fakeStart := typedWriteTestStart(op, typedWriteTestCall, `{"details":`+details+`,"text":`+fakeText+`}`)
	fakeEnd := strings.Replace(denial, `"合成工具错误"`, fakeText, 1)
	readStart := typedWriteTestStart("read_work_inputs", typedWriteTestCall, `{}`)
	readEnd := strings.Replace(checked, `"ack_work_input"`, `"read_work_inputs"`, 1)
	toolResult := `{"type":"message_end","message":{"role":"toolResult","toolCallId":"call_one|fc_two","toolName":"ack_work_input","isError":true,"content":[{"type":"text","text":"合成工具错误"}],"details":{}}}`
	largeFrame := `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"` + strings.Repeat("x", executionLogLimit/2) + `"}}`
	for _, c := range []struct {
		name, extra                            string
		want                                   Outcome
		complete, trusted, truncated, ordinary bool
	}{
		{name: "checked write", extra: pair(checked), want: OutcomeSucceeded, complete: true},
		{name: "definite denial", extra: pair(denial), want: OutcomeSucceeded, complete: true},
		{name: "missing write end with toolResult", extra: pair(toolResult), want: OutcomeUnknown},
		{name: "missing write start", extra: checked, want: OutcomeUnknown},
		{name: "end before start cannot recover", extra: checked + "\n" + pair(checked), want: OutcomeUnknown},
		{name: "read end cannot close write", extra: pair(readEnd), want: OutcomeUnknown},
		{name: "read start cannot open write", extra: readStart + "\n" + checked, want: OutcomeUnknown},
		{name: "different write name cannot close write", extra: pair(strings.Replace(checked, op, "submit_result", 1)), want: OutcomeUnknown},
		{name: "different call ID cannot close write", extra: pair(strings.Replace(checked, typedWriteTestCall, "another-id", 1)), want: OutcomeUnknown},
		{name: "duplicate write start", extra: start + "\n" + pair(checked), want: OutcomeUnknown},
		{name: "reuse closed write ID", extra: pair(checked) + "\n" + pair(checked), want: OutcomeUnknown},
		{name: "duplicate decoded start field", extra: strings.Replace(start, `"toolName":`, `"\u0074oolName":"read_work_inputs","toolName":`, 1) + "\n" + checked, want: OutcomeUnknown},
		{name: "missing end error flag", extra: pair(strings.Replace(checked, `,"isError":false`, "", 1)), want: OutcomeUnknown},
		{name: "empty end result is not denial", extra: pair(strings.Replace(denial, `"content":[{"type":"text","text":"合成工具错误"}],`, "", 1)), want: OutcomeUnknown},
		{name: "ambiguous then stop", extra: pair(frame), want: OutcomeUnknown, complete: true, trusted: true},
		{name: "returned error lost its control details", extra: pair(typedWriteTestEnd(op, "{}")), want: OutcomeUnknown},
		{name: "malformed key", extra: pair(strings.Replace(frame, m["idempotency_key"], strings.Repeat("0", 64), 1)), want: OutcomeUnknown},
		{name: "missing request digest", extra: pair(strings.Replace(frame, `"request_sha256":"`+m["request_sha256"]+`",`, "", 1)), want: OutcomeUnknown},
		{name: "foreign run replay", extra: pair(strings.Replace(frame, `"run1"`, `"other-run"`, 1)), want: OutcomeUnknown},
		{name: "foreign role", extra: pair(strings.Replace(frame, `"followup"`, `"delegation"`, 1)), want: OutcomeUnknown},
		{name: "duplicate decoded field", extra: pair(strings.Replace(frame, `"role":`, `"\u0072ole":"delegation","role":`, 1)), want: OutcomeUnknown},
		{name: "inconsistent marker call ID", extra: pair(strings.Replace(frame, `"tool_call_id":"`+typedWriteTestCall+`"`, `"tool_call_id":"another-id"`, 1)), want: OutcomeUnknown},
		{name: "inconsistent isError", extra: pair(strings.Replace(frame, `"isError":true,"durationMs"`, `"isError":false,"durationMs"`, 1)), want: OutcomeUnknown},
		{name: "unissued write", extra: typedWriteTestEnd("report_work", `{"`+typedWriteTestField+`":`+memberToolJSON(t, typedWriteTestDetails("followup", "run1", "report_work"))+`}`), want: OutcomeUnknown},
		{name: "read cannot mark a write", extra: typedWriteTestEnd("read_work_inputs", `{"`+typedWriteTestField+`":`+memberToolJSON(t, typedWriteTestDetails("followup", "run1", "read_work_inputs"))+`}`), want: OutcomeUnknown},
		{name: "duplicate control frame", extra: pair(frame) + "\n" + frame, want: OutcomeUnknown, trusted: true},
		{name: "printed-like data is not control", extra: fakeTop + "\n" + fakeStart + "\n" + fakeEnd + `
{"type":"message_end","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"text","text":` + fakeText + `}]}}`, want: OutcomeSucceeded, complete: true},
		{name: "outside retained stdout prefix", extra: largeFrame + "\n" + largeFrame + "\n" + pair(frame), want: OutcomeUnknown, complete: true, truncated: true, trusted: true},
		{name: "truncated log without retained confirmation", extra: largeFrame + "\n" + largeFrame, want: OutcomeUnknown, complete: true, truncated: true},
		{name: "ordinary mode ignores typed control", extra: frame, want: OutcomeSucceeded, complete: true, ordinary: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			stream := `{"type":"session","id":"run1","version":3,"cwd":"/fixture"}` + "\n" + executionStart + "\n" + c.extra + "\n" + executionAssistant("stop") + "\n" + executionSettled + "\n"
			got, events := runTypedWriteTest(t, "run1", stream, c.ordinary)
			var terminals []Outcome
			for _, ev := range events {
				if ev.Kind == TerminalEvent {
					terminals = append(terminals, ev.Terminal)
				}
			}
			if got.Outcome != c.want || got.OutputComplete != c.complete || got.LogsTruncated != c.truncated ||
				got.Terminal != OutcomeSucceeded || len(terminals) != 1 || terminals[0] != got.Terminal ||
				!got.Started || !got.Accepted || !got.DirectChildReaped || got.ExitCode != 0 || got.NativeRef != nil || got.DeliveryFailed {
				t.Fatalf("receipt and observed terminal diverged: %+v, terminal callbacks %v", got, terminals)
			}
			if strings.Contains(got.Reason, "写入") != c.trusted || strings.Contains(got.Reason, m["idempotency_key"]) {
				t.Fatal("write uncertainty was not kept separate from raw identity")
			}
			log, err := os.ReadFile(got.StdoutPath)
			if err != nil || len(log) > executionLogLimit || !c.truncated && string(log) != stream {
				t.Fatal("private stdout log bounds or original bytes changed", err)
			}
			if st, err := os.Stat(got.StdoutPath); err != nil || st.Mode().Perm() != 0600 {
				t.Fatal("control metadata lost private file permissions", err)
			}
			if c.trusted && c.truncated {
				trim := strings.TrimSuffix(string(log), "\n")
				var last map[string]string
				if err := json.Unmarshal([]byte(trim[strings.LastIndexByte(trim, '\n')+1:]), &last); err != nil ||
					last["type"] != typedWriteTestField || len(last) != len(m)+1 {
					t.Fatal("bounded log tail did not retain the control record", err)
				}
				for k, v := range m {
					if last[k] != v {
						t.Fatalf("retained identity changed field %s", k)
					}
				}
			}
		})
	}
}
