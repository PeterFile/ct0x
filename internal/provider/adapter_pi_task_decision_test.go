package provider

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiFreshTaskDecisionNativeWriteUncertainty(t *testing.T) {
	const op = ProposeTaskDecisionTool
	const run = "run1"
	end := func(details map[string]string) string {
		return typedWriteTestEnd(op, `{"`+typedWriteTestField+`":`+memberToolJSON(t, details)+`}`)
	}
	details := typedWriteTestDetails("followup", run, op)
	unscoped := end(details)
	details["idempotency_key"] = fmt.Sprintf("%x", sha256.Sum256([]byte(op+"\x00run2\x00"+typedWriteTestCall)))
	foreignKey := end(details)
	details["run_id"] = "run2"
	foreignRun := end(details)
	details["run_id"] = run
	details["idempotency_key"] = fmt.Sprintf("%x", sha256.Sum256([]byte(op+"\x00"+run+"\x00"+typedWriteTestCall)))
	unknown := end(details)
	checked := `{"type":"tool_execution_end","toolCallId":"` + typedWriteTestCall + `","toolName":"` + op + `","result":{"content":[]},"isError":false}`
	start := typedWriteTestStart(op, typedWriteTestCall, `{}`)
	for _, tc := range []struct {
		name, frames     string
		want             Outcome
		complete, marked bool
	}{
		{"checked", start + "\n" + checked, OutcomeSucceeded, true, false},
		{"unknown despite success", start + "\n" + unknown, OutcomeUnknown, true, true},
		{"unscoped key", start + "\n" + unscoped, OutcomeUnknown, false, false},
		{"other run's key", start + "\n" + foreignKey, OutcomeUnknown, false, false},
		{"other run's marker", start + "\n" + foreignRun, OutcomeUnknown, false, false},
		{"duplicate call ID", strings.Join([]string{start, checked, start, checked}, "\n"), OutcomeUnknown, false, false},
		{"missing end", start, OutcomeUnknown, false, false},
		{"missing start", unknown, OutcomeUnknown, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := strings.Join([]string{`{"type":"session","id":"run1","version":3,"cwd":"/fixture"}`,
				executionStart, tc.frames, executionAssistant("stop"), executionSettled, ""}, "\n")
			got := runTaskDecisionStream(t, run, stream)
			if got.Outcome != tc.want || got.OutputComplete != tc.complete || strings.Contains(got.Reason, "写入") != tc.marked ||
				got.Terminal != OutcomeSucceeded || !got.Accepted || !got.DirectChildReaped || got.NativeRef != nil {
				t.Fatalf("unexpected native proposal evidence: %+v", got)
			}
		})
	}
}

func TestPiFreshTaskDecisionSDKPrintLifecycle(t *testing.T) {
	dir := os.Getenv("TEST_PI_TYPED_PRINT_FIXTURES")
	if dir == "" {
		t.Skip("set TEST_PI_TYPED_PRINT_FIXTURES to the captured Pi JSON print fixtures")
	}
	stream, err := os.ReadFile(filepath.Join(dir, "proposal-unknown.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	const run = "fixture-fresh-turn"
	got := runTaskDecisionStream(t, run, string(stream))
	if got.Outcome != OutcomeUnknown || !got.OutputComplete || !strings.Contains(got.Reason, "写入") ||
		got.Terminal != OutcomeSucceeded || !got.Accepted || got.NativeSessionID != run || !got.DirectChildReaped || got.NativeRef != nil {
		t.Fatalf("SDK's run-scoped marker did not survive native success: %+v", got)
	}
}

func runTaskDecisionStream(t *testing.T, run, stream string) ExecutionReceipt {
	t.Helper()
	selected, req, dir := typedWriteTestChild(t, stream)
	req.RunID = run
	config := filepath.Join(req.CWD, "issued.json")
	wire, err := PiFreshTaskDecisionConfig("http://127.0.0.1:1/api/v1/member-tools", "test-bearer", strings.Repeat("1", 64), run)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, wire, 0600); err != nil {
		t.Fatal(err)
	}
	a := NewPiFreshTaskWorkAdapter(filepath.Join(dir, "fresh-task-work.ts"), 0)
	binding := FreshTaskWorkBinding{ConfigPath: config, ConfigSHA256: sha256.Sum256(wire), RunID: run, Operations: PiFreshTaskDecisionToolNames()}
	ctx := t.Context()
	if e, err := a.NewExecutor(ctx, selected, binding); e != nil || err == nil {
		t.Fatal("four-tool adapter accepted the newer proposal profile")
	}
	foreign := binding
	foreign.RunID += "-wrong"
	if e, err := a.NewTaskDecisionExecutor(ctx, selected, foreign); e != nil || err == nil {
		t.Fatal("proposal adapter accepted digest-bound config for a different Run")
	}
	e, err := a.NewTaskDecisionExecutor(ctx, selected, binding)
	if err != nil {
		t.Fatal(err)
	}
	return e.Execute(ctx, req, func(Event) error { return nil })
}
