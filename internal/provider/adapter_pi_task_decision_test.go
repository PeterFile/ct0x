package provider

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiFreshTaskDecisionNativeWriteUncertainty(t *testing.T) {
	const op = ProposeTaskDecisionTool
	const run = "run1"
	marker := memberToolJSON(t, typedWriteTestDetails("followup", run, op))
	unknown := typedWriteTestEnd(op, `{"`+typedWriteTestField+`":`+marker+`}`)
	checked := `{"type":"tool_execution_end","toolCallId":"` + typedWriteTestCall + `","toolName":"` + op + `","result":{"content":[]},"isError":false}`
	start := typedWriteTestStart(op, typedWriteTestCall, `{}`)
	for _, tc := range []struct {
		name, frames string
		want         Outcome
	}{
		{"checked", start + "\n" + checked, OutcomeSucceeded},
		{"unknown despite success", start + "\n" + unknown, OutcomeUnknown},
		{"missing end", start, OutcomeUnknown},
		{"missing start", unknown, OutcomeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := strings.Join([]string{`{"type":"session","id":"run1","version":3,"cwd":"/fixture"}`,
				executionStart, tc.frames, executionAssistant("stop"), executionSettled, ""}, "\n")
			selected, req, dir := typedWriteTestChild(t, stream)
			config := filepath.Join(req.CWD, "issued.json")
			wire, err := PiFreshTaskDecisionConfig("http://127.0.0.1:1/api/v1/member-tools", "test-bearer", strings.Repeat("1", 64))
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
			e, err := a.NewTaskDecisionExecutor(ctx, selected, binding)
			if err != nil {
				t.Fatal(err)
			}
			got := e.Execute(ctx, req, func(Event) error { return nil })
			if got.Outcome != tc.want || got.Terminal != OutcomeSucceeded || !got.Accepted || !got.DirectChildReaped || got.NativeRef != nil {
				t.Fatalf("unexpected native proposal evidence: %+v", got)
			}
			if tc.frames == start+"\n"+unknown && (!got.OutputComplete || !strings.Contains(got.Reason, "写入")) {
				t.Fatal("matched proposal marker did not survive later success")
			}
		})
	}
}
