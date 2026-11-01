package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

const memberToolHTTPBudget = 65536
const memberToolFrameBudget = 1 << 20
const memberToolFinalText = "最终助手回答\u2028不包含工具内容"

type memberToolNativeCall struct {
	name   string
	args   map[string]any
	result string
}

func memberToolJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func memberToolAssistantMessage(t *testing.T, content []any, reason string) map[string]any {
	t.Helper()
	var event struct{ Message map[string]any }
	if err := json.Unmarshal([]byte(executionAssistant(reason)), &event); err != nil {
		t.Fatal(err)
	}
	event.Message["content"] = content
	return event.Message
}

func memberToolHTTPMessage(content string) map[string]any {
	return map[string]any{"id": "message-1", "group_id": "group-1", "topic_id": "", "sender_id": "member-1", "content": content,
		"created_at": "2026-11-01T00:00:00Z", "deliveries": []any{}}
}

// Each quote/backslash pair takes four bytes in HTTP JSON and eight when that
// complete JSON document is embedded in Pi's text result. ASCII fills the residue.
func memberToolSizedPayload(t *testing.T, size, pairBytes int, encode func(string) string) string {
	t.Helper()
	remaining := size - len(encode(""))
	if remaining < 0 {
		t.Fatal("fixture metadata exceeds target size")
	}
	got := encode(strings.Repeat("\"\\", remaining/pairBytes) + strings.Repeat("x", remaining%pairBytes))
	if len(got) != size || !json.Valid([]byte(got)) {
		t.Fatalf("fixture is not valid JSON of exactly %d bytes: bytes=%d", size, len(got))
	}
	return got
}

// These are Pi 1.4.0 print-mode shapes, not a reduced adapter-only protocol.
// Args remain objects; results contain the complete HTTP JSON text (including LF).
// turn_end duplicates the whole batch; agent_end accumulates this run's messages.
func memberToolNativeRun(t *testing.T, turns [][]memberToolNativeCall, reason, text string, willRetry bool) []string {
	t.Helper()
	records := []string{executionStart, `{"type":"turn_start"}`}
	initial := map[string]any{"role": "user", "content": "synthetic prompt", "timestamp": 1}
	messages := []any{initial}
	for _, kind := range []string{"message_start", "message_end"} {
		records = append(records, memberToolJSON(t, map[string]any{"type": kind, "message": initial}))
	}
	pending := memberToolAssistantMessage(t, []any{}, "pending")
	for turn, calls := range turns {
		if turn != 0 {
			records = append(records, `{"type":"turn_start"}`)
		}
		content := make([]any, len(calls))
		for i, call := range calls {
			content[i] = map[string]any{"type": "toolCall", "id": fmt.Sprintf("call-%d-%d", turn, i), "name": call.name, "arguments": call.args}
		}
		assistant := memberToolAssistantMessage(t, content, "toolUse")
		records = append(records, memberToolJSON(t, map[string]any{"type": "message_start", "message": pending}), memberToolJSON(t, map[string]any{"type": "message_end", "message": assistant}))
		messages = append(messages, assistant)
		results := make([]any, 0, len(calls))
		for i, call := range calls {
			id := fmt.Sprintf("call-%d-%d", turn, i)
			blocks := []any{map[string]any{"type": "text", "text": call.result}}
			result := map[string]any{"role": "toolResult", "toolCallId": id, "toolName": call.name, "content": blocks, "isError": false, "timestamp": 1, "durationMs": 1}
			records = append(records,
				memberToolJSON(t, map[string]any{"type": "tool_execution_start", "toolCallId": id, "toolName": call.name, "args": call.args}),
				memberToolJSON(t, map[string]any{"type": "tool_execution_end", "toolCallId": id, "toolName": call.name, "result": map[string]any{"content": blocks}, "isError": false, "durationMs": 1}),
				memberToolJSON(t, map[string]any{"type": "message_start", "message": result}),
				memberToolJSON(t, map[string]any{"type": "message_end", "message": result}))
			results = append(results, result)
			messages = append(messages, result)
		}
		records = append(records, memberToolJSON(t, map[string]any{"type": "turn_end", "message": assistant, "toolResults": results}))
	}
	if len(turns) != 0 {
		records = append(records, `{"type":"turn_start"}`)
	}
	final := memberToolAssistantMessage(t, []any{map[string]any{"type": "text", "text": text}}, reason)
	records = append(records, memberToolJSON(t, map[string]any{"type": "message_start", "message": pending}), memberToolJSON(t, map[string]any{"type": "message_end", "message": final}),
		memberToolJSON(t, map[string]any{"type": "turn_end", "message": final, "toolResults": []any{}}))
	messages = append(messages, final)
	return append(records, memberToolJSON(t, map[string]any{"type": "agent_end", "messages": messages, "willRetry": willRetry}))
}

func memberToolExecuteNative(t *testing.T, stream string) (ExecutionReceipt, []Event) {
	t.Helper()
	body := "set -eu\nprintf '%s\\n' \"$$\" > pid\n" + executionHeader + "/bin/cat native.jsonl\n: > drained\n"
	e, request, _, _ := memberToolFixture(t, body)
	if err := os.WriteFile(filepath.Join(request.CWD, "native.jsonl"), []byte(stream), 0600); err != nil {
		t.Fatal(err)
	}
	var events []Event
	got := e.Execute(context.Background(), request, func(event Event) error { events = append(events, event); return nil })
	if !got.Started || !got.Accepted || !got.DirectChildReaped || got.ExitCode != 0 || got.DeliveryFailed {
		t.Fatalf("native fixture did not exit normally: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(request.CWD, "drained")); err != nil {
		t.Fatalf("child did not finish writing the entire stream: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(request.CWD, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Sscan(string(data), &pid); err != nil || pid <= 1 {
		t.Fatalf("invalid direct-child PID: %q, %v", data, err)
	}
	if _, err := syscall.Wait4(pid, nil, syscall.WNOHANG, nil); err != syscall.ECHILD {
		t.Fatalf("direct child was not reaped by Execute: %v", err)
	}
	return got, events
}

func TestPiMemberToolsEmptyAssistantSnapshotReplacesPreviousText(t *testing.T) {
	const earlier = "earlier assistant text must not be published"
	for _, tools := range []bool{false, true} {
		for _, tc := range []struct {
			name, reason string
			aborted      bool
			outcome      Outcome
		}{
			{"native-abort-after-toolUse", "toolUse", true, OutcomeAborted},
			{"terminal-tool-only", "stop", false, OutcomeSucceeded},
		} {
			t.Run(fmt.Sprintf("tools=%t/%s", tools, tc.name), func(t *testing.T) {
				call := func(id string) map[string]any {
					return map[string]any{"type": "toolCall", "id": id, "name": "send_message", "arguments": map[string]any{"content": "PRIVATE-TOOL-ARGS"}}
				}
				previous := memberToolAssistantMessage(t, []any{map[string]any{"type": "text", "text": earlier}, call("previous-call")}, "toolUse")
				toolOnly := memberToolAssistantMessage(t, []any{call("last-call")}, tc.reason)
				result := map[string]any{"role": "toolResult", "toolCallId": "previous-call", "toolName": "send_message",
					"content": []any{map[string]any{"type": "text", "text": "{\"content\":\"PRIVATE-TOOL-RESULT\"}\n"}}, "isError": false, "timestamp": 1, "durationMs": 1}
				records := []string{executionStart,
					memberToolJSON(t, map[string]any{"type": "message_end", "message": previous}),
					memberToolJSON(t, map[string]any{"type": "message_end", "message": result}),
					memberToolJSON(t, map[string]any{"type": "message_end", "message": toolOnly}),
					memberToolJSON(t, map[string]any{"type": "agent_settled", "aborted": tc.aborted})}
				body := "set -eu\n" + executionHeader + executionEmit(records...) + ": > drained\n"
				var executor Executor
				var request ExecuteRequest
				if tools {
					executor, request, _, _ = memberToolFixture(t, body)
				} else {
					executor, request = executionFixture(t, body)
				}
				// Model native settlement, not caller cancellation or a damaged pipe.
				ctx := context.Background()
				candidate, candidateAtTerminal, terminals := "", "", 0
				var snapshots []string
				got := executor.Execute(ctx, request, func(event Event) error {
					if event.NativeSessionID != request.RunID {
						t.Errorf("unbound snapshot: %+v", event)
					}
					switch event.Kind {
					case ResponseEvent:
						candidate = event.Text // Same replacement rule as store.RecordTurnEvent.
						snapshots = append(snapshots, candidate)
					case TerminalEvent:
						candidateAtTerminal, terminals = candidate, terminals+1
						if event.Terminal != tc.outcome {
							t.Errorf("unexpected native terminal: %+v", event)
						}
					}
					return nil
				})
				if got.Outcome != tc.outcome || got.Terminal != tc.outcome || !got.Started || !got.Accepted || !got.DirectChildReaped || !got.OutputComplete || got.ExitCode != 0 || got.DeliveryFailed || got.LogsTruncated || ctx.Err() != nil {
					t.Fatalf("clean native settlement must retain its authoritative outcome: %+v", got)
				}
				if _, err := os.Stat(filepath.Join(request.CWD, "drained")); err != nil {
					t.Fatalf("child did not finish writing through clean EOF: %v", err)
				}
				if !reflect.DeepEqual(snapshots, []string{earlier, ""}) || candidate != "" || terminals != 1 || candidateAtTerminal != "" {
					t.Fatalf("complete empty snapshot did not clear the earlier candidate before terminal publication: snapshots=%q candidate=%q terminal_candidate=%q terminals=%d", snapshots, candidate, candidateAtTerminal, terminals)
				}
			})
		}
	}
}

func TestPiMemberToolsMaximumNativeJSONAndIndependentLogCap(t *testing.T) {
	// The bridge serializes request objects without LF. HTTP responses include LF;
	// the latter's 65536-byte budget includes that byte before native re-escaping.
	input := memberToolSizedPayload(t, memberToolHTTPBudget, 4, func(payload string) string {
		return memberToolJSON(t, map[string]any{"content": "private-args-" + payload, "recipient_ids": []any{}})
	})
	result := memberToolSizedPayload(t, memberToolHTTPBudget, 4, func(payload string) string {
		return memberToolJSON(t, memberToolHTTPMessage("private-result-"+payload)) + "\n"
	})
	var args map[string]any
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		t.Fatal(err)
	}
	turns := [][]memberToolNativeCall{
		{{"list_members", map[string]any{"limit": 1}, "{\"items\":[],\"next_cursor\":\"\"}\n"}},
		{{"read_channel", map[string]any{"limit": 200}, memberToolSizedPayload(t, memberToolHTTPBudget, 4, func(payload string) string {
			return memberToolJSON(t, map[string]any{"items": []any{memberToolHTTPMessage("private-history-" + payload)}, "next_cursor": ""}) + "\n"
		})}},
		{{"send_message", args, result}},
		{{"create_member", map[string]any{"name": "researcher", "role": "verify sources", "reason": "review evidence"}, "{\"id\":\"member-2\",\"name\":\"researcher\"}\n"}},
	}
	records := memberToolNativeRun(t, turns, "stop", memberToolFinalText, false)
	largest := 0
	for _, record := range records {
		largest = max(largest, len(record))
		var shape map[string]any
		if err := json.Unmarshal([]byte(record), &shape); err != nil {
			t.Fatal(err)
		}
		if shape["type"] == "tool_execution_start" {
			if _, ok := shape["args"].(map[string]any); !ok {
				t.Fatal("native tool args were double-stringified")
			}
		}
		if shape["type"] == "tool_execution_end" && shape["toolName"] == "send_message" {
			nativeResult := shape["result"].(map[string]any)
			if _, ok := nativeResult["details"]; ok || nativeResult["content"].([]any)[0].(map[string]any)["text"] != result || len(record) <= memberToolHTTPBudget {
				t.Fatal("HTTP text was not re-escaped intact with details omitted")
			}
		}
	}
	stream := strings.Join(append(records, executionSettled), "\n") + "\n"
	if len(stream) <= 1<<20 || largest <= 64<<10 || largest > memberToolFrameBudget {
		t.Fatalf("fixture does not exercise independent frame/log/HTTP budgets: stream=%d largest=%d", len(stream), largest)
	}
	got, events := memberToolExecuteNative(t, stream)
	var snapshots []string
	var lifecycle []Event
	for _, event := range events {
		if event.Kind == ResponseEvent {
			if event.NativeSessionID != "run1" {
				t.Fatalf("unbound response snapshot: %+v", event)
			}
			snapshots = append(snapshots, event.Text)
		} else {
			lifecycle = append(lifecycle, event)
		}
	}
	// Each tool-only assistant clears the candidate before the final text replaces it.
	wantSnapshots := make([]string, len(turns)+1)
	wantSnapshots[len(turns)] = memberToolFinalText
	wantLifecycle := []Event{{Kind: SessionEvent, NativeSessionID: "run1"}, {Kind: AcceptedEvent, NativeSessionID: "run1"},
		{Kind: TerminalEvent, NativeSessionID: "run1", Terminal: OutcomeSucceeded}}
	if got.Outcome != OutcomeSucceeded || got.Terminal != OutcomeSucceeded || !got.OutputComplete || !got.LogsTruncated || !reflect.DeepEqual(snapshots, wantSnapshots) || !reflect.DeepEqual(lifecycle, wantLifecycle) {
		t.Fatalf("tool args/results became candidates, or log truncation damaged valid protocol: %+v, events=%+v", got, events)
	}
	info, err := os.Stat(got.StdoutPath)
	if err != nil || info.Size() != 1<<20 {
		t.Fatalf("stdout log cap changed: %v, %v", info, err)
	}
	t.Logf("HTTP input=%d result=%d (including LF); native bytes=%d largest frame=%d; complete protocol with stdout log capped at %d", len(input), len(result), len(stream), largest, info.Size())
}

// Eight legal HTTP results are enough to reach the cumulative 1 MiB boundary.
// Only the eighth result varies; no arbitrary giant field or precomputed blob.
func memberToolAggregateBoundary(t *testing.T, size int) []string {
	t.Helper()
	full := memberToolSizedPayload(t, memberToolHTTPBudget, 4, func(payload string) string {
		return memberToolJSON(t, map[string]any{"items": []any{memberToolHTTPMessage(payload)}, "next_cursor": ""}) + "\n"
	})
	var run []string
	memberToolSizedPayload(t, size, 8, func(payload string) string {
		last := memberToolJSON(t, map[string]any{"items": []any{memberToolHTTPMessage(payload)}, "next_cursor": ""}) + "\n"
		if len(last) > memberToolHTTPBudget {
			t.Fatalf("boundary result exceeds HTTP cap: %d", len(last))
		}
		calls := make([]memberToolNativeCall, 8)
		for i := range calls {
			calls[i] = memberToolNativeCall{"read_channel", map[string]any{"limit": 200}, full}
		}
		calls[7].result = last
		run = memberToolNativeRun(t, [][]memberToolNativeCall{calls}, "error", "retry snapshot", true)
		return run[len(run)-1]
	})
	var aggregate struct{ Messages []map[string]any }
	if err := json.Unmarshal([]byte(run[len(run)-1]), &aggregate); err != nil || len(aggregate.Messages) != 11 {
		t.Fatalf("agent_end must include initial, assistant, eight results and final messages: %v", err)
	}
	return run
}

func TestPiMemberToolsAggregateFrameBoundaryAndDrain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		size   int
		ending string
		valid  bool
	}{
		{"exact-LF", memberToolFrameBudget, "\n", true},
		{"oversize-LF", memberToolFrameBudget + 1, "\n", false},
		{"exact-CRLF", memberToolFrameBudget - 1, "\r\n", true},
		{"oversize-CRLF", memberToolFrameBudget, "\r\n", false},
		{"oversize-EOF", memberToolFrameBudget + 1, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := memberToolAggregateBoundary(t, tc.size)
			stream := strings.Join(run[:len(run)-1], "\n") + "\n" + run[len(run)-1] + tc.ending
			if tc.ending != "" {
				// A real retry after agent_end proves parsing/delivery continues past
				// the oversized aggregate, without repairing the damaged outcome.
				tail := memberToolNativeRun(t, nil, "stop", memberToolFinalText, false)
				stream += strings.Join(append(tail, executionSettled), "\n") + "\n"
			}
			got, events := memberToolExecuteNative(t, stream)
			wantOutcome := OutcomeUnknown
			if tc.valid {
				wantOutcome = OutcomeSucceeded
			}
			if got.Outcome != wantOutcome || got.OutputComplete != tc.valid || !got.LogsTruncated {
				t.Fatalf("raw line budget excludes LF but includes CR; damage cannot be repaired: %+v", got)
			}
			if tc.ending != "" {
				if got.Terminal != OutcomeSucceeded || len(events) < 2 || events[len(events)-2].Text != memberToolFinalText || events[len(events)-1].Kind != TerminalEvent {
					t.Fatalf("later native response/terminal was not drained: %+v, events=%+v", got, events)
				}
			} else if got.Terminal != "" {
				t.Fatalf("unterminated oversized EOF acquired a terminal: %+v", got)
			}
			t.Logf("agent_end JSON=%d raw line=%d (excluding LF), stream=%d; outcome=%s complete=%t reaped=%t", tc.size, tc.size+len(strings.TrimSuffix(tc.ending, "\n")), len(stream), got.Outcome, got.OutputComplete, got.DirectChildReaped)
		})
	}
}

func TestPiMemberToolsMalformedAndOversizeIgnoredEventsStayUnknown(t *testing.T) {
	calls := [][]memberToolNativeCall{{{"read_channel", map[string]any{"limit": 1}, "{\"items\":[],\"next_cursor\":\"\"}\n"}}}
	run := memberToolNativeRun(t, calls, "stop", memberToolFinalText, false)
	bad := map[string]string{}
	for _, record := range run {
		var shape struct{ Type string }
		if err := json.Unmarshal([]byte(record), &shape); err != nil {
			t.Fatal(err)
		}
		if shape.Type == "tool_execution_end" || shape.Type == "turn_end" || shape.Type == "agent_end" {
			bad["malformed-"+shape.Type] = strings.TrimSuffix(record, "}")
		}
	}
	// A turn_end batch may itself exceed the cap, even before agent_end repeats it.
	large := memberToolAggregateBoundary(t, memberToolFrameBudget+1024)
	for _, record := range large {
		var shape struct{ Type string }
		if err := json.Unmarshal([]byte(record), &shape); err != nil {
			t.Fatal(err)
		}
		if shape.Type == "turn_end" && len(record) > memberToolFrameBudget {
			bad["oversize-turn_end"] = record
		}
	}
	if bad["oversize-turn_end"] == "" {
		t.Fatal("fixture lacks an oversized accumulated turn_end")
	}
	for name, damaged := range bad {
		t.Run(name, func(t *testing.T) {
			// Include the native session's first agent_start before the damaged frame.
			stream := executionStart + "\n" + damaged + "\n" + strings.Join(append(run, executionSettled), "\n") + "\n"
			got, events := memberToolExecuteNative(t, stream)
			if got.Outcome != OutcomeUnknown || got.OutputComplete || got.Terminal != OutcomeSucceeded || events[len(events)-1].Kind != TerminalEvent {
				t.Fatalf("ignored event damage was skipped or stopped draining: %+v, events=%+v", got, events)
			}
		})
	}
}

func TestPiMemberToolsFinalAssistantStopReasonIsAuthoritative(t *testing.T) {
	for reason, want := range map[string]Outcome{"stop": OutcomeSucceeded, "error": OutcomeFailed, "aborted": OutcomeAborted, "length": OutcomeTruncated} {
		t.Run(reason, func(t *testing.T) {
			run := memberToolNativeRun(t, [][]memberToolNativeCall{{{"list_members", map[string]any{}, "{\"items\":[],\"next_cursor\":\"\"}\n"}}}, reason, memberToolFinalText, false)
			got, events := memberToolExecuteNative(t, strings.Join(append(run, executionSettled), "\n")+"\n")
			var snapshots []string
			for _, event := range events {
				if event.Kind == ResponseEvent {
					snapshots = append(snapshots, event.Text)
				}
			}
			if got.Outcome != want || got.Terminal != want || !got.OutputComplete || !reflect.DeepEqual(snapshots, []string{"", memberToolFinalText}) {
				t.Fatalf("tool completion replaced the final assistant's native stop reason: %+v, events=%+v", got, events)
			}
		})
	}
}

func TestPiMemberToolsBaselineFrameBudgetUnchanged(t *testing.T) {
	for _, codex := range []bool{false, true} {
		for _, tc := range []struct {
			size   int
			ending string
			valid  bool
		}{{64 << 10, "\n", true}, {(64 << 10) + 1, "\n", false}, {(64 << 10) - 1, "\r\n", true}, {64 << 10, "\r\n", false}} {
			t.Run(fmt.Sprintf("codex=%t/size=%d/CR=%t", codex, tc.size, tc.ending == "\r\n"), func(t *testing.T) {
				fixture, header, response, tail := executionFixture, executionHeader+executionEmit(executionStart), executionAssistant("stop"), executionEmit(executionAssistant("stop"), executionSettled)
				if codex {
					fixture, header, response, tail = codexFixture, executionEmit(codexHeader, codexStart), codexResponse, executionEmit(codexResponse, codexCompleted)
				}
				frame := strings.Replace(response, "合成\\u2028回答", strings.Repeat("x", tc.size-len(response)+len("合成\\u2028回答")), 1)
				if len(frame) != tc.size || !json.Valid([]byte(frame)) {
					t.Fatal("invalid baseline boundary fixture")
				}
				e, request := fixture(t, "set -eu\n"+header+"/bin/cat frame.jsonl\n"+tail+": > drained\n")
				if err := os.WriteFile(filepath.Join(request.CWD, "frame.jsonl"), []byte(frame+tc.ending), 0600); err != nil {
					t.Fatal(err)
				}
				got := e.Execute(context.Background(), request, nil)
				want := OutcomeUnknown
				if tc.valid {
					want = OutcomeSucceeded
				}
				if got.Outcome != want || got.OutputComplete != tc.valid || !got.DirectChildReaped || got.LogsTruncated || got.Terminal != OutcomeSucceeded {
					t.Fatalf("baseline 64 KiB raw line semantics changed: %+v", got)
				}
				if _, err := os.Stat(filepath.Join(request.CWD, "drained")); err != nil {
					t.Fatal("baseline did not drain later output")
				}
			})
		}
	}
}
