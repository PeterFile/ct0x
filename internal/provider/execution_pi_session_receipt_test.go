package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPiSessionReceiptRequiresExactFields(t *testing.T) {
	const start = `{"type":"message_start","message":{"role":"assistant","timestamp":2,"content":[],"stopReason":"pending"}}`
	const end = `{"type":"message_end","message":{"role":"assistant","timestamp":2,"content":[{"type":"text","text":"current answer"}],"stopReason":"stop"}}`
	const settled = `{"type":"agent_settled","aborted":false}`
	for _, tc := range []struct {
		name, final, settlement string
		terminal                Outcome
		answers                 []string
	}{
		{"canonical", end, settled, OutcomeSucceeded, []string{"current answer"}},
		{"settled-alias", end, `{"type":"agent_settled","aborted":true,"Aborted":false}`, "", []string{"current answer"}},
		{"settled-alias-only", end, `{"type":"agent_settled","Aborted":false}`, "", []string{"current answer"}},
		{"settled-escaped-duplicate", end, `{"type":"agent_settled","aborted":true,"\u0061borted":false}`, "", []string{"current answer"}},
		{"genuine-abort-after-invalid-settlement", end, `{"type":"agent_settled","aborted":null}` + "\n" + `{"type":"agent_settled","aborted":true}`, OutcomeAborted, []string{"current answer"}},
		{"message-alias", strings.Replace(end, `"message":`, `"Message":`, 1), settled, "", nil},
		{"role-alias", strings.Replace(end, `"role":"assistant"`, `"role":"user","Role":"assistant"`, 1), settled, "", nil},
		{"stop-reason-alias", strings.Replace(end, `"stopReason":"stop"`, `"stopReason":"error","ſtopReason":"stop"`, 1), settled, "", nil},
		{"content-alias", strings.Replace(end, `"content":`, `"content":[],"Content":`, 1), settled, "", nil},
		{"block-type-alias", strings.Replace(end, `"type":"text"`, `"type":"thinking","Type":"text"`, 1), settled, "", nil},
		{"block-text-alias", strings.Replace(end, `"text":"current answer"`, `"text":"current answer","Text":"other answer"`, 1), settled, "", nil},
		{"nested-duplicate", strings.Replace(end, `"text":"current answer"`, `"text":"current answer","text":"other answer"`, 1), settled, "", nil},
		{"after-genuine-terminal", end, settled + "\n" + `{"type":"agent_settled","Aborted":true}`, OutcomeSucceeded, []string{"current answer"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary := executable(t, filepath.Join(piSessionPrivateTemp(t), "synthetic-child"), "/bin/cat >/dev/null\n/bin/cat events.jsonl\n")
			selected, extension, request := piSessionFixtureSetup(t, binary)
			// Pi trims BOM, but does not trim U+0085 from the prompt.
			request.Prompt = "\ufeff \r\n\u0085hello\t "
			lines := piSessionTestLines()
			lines[0]["cwd"] = request.CWD
			user := lines[4]["message"].(map[string]any)
			user["content"] = "\u0085hello"
			data := piSessionTestBytes([]map[string]any{lines[0], {"type": "agent_start"}, {"type": "turn_start"},
				{"type": "message_start", "message": user}, {"type": "message_end", "message": user}})
			data = append(data, (start + "\n" + tc.final + "\n" + tc.settlement + "\n")...)
			if err := os.WriteFile(filepath.Join(request.CWD, "events.jsonl"), data, 0600); err != nil {
				t.Fatal(err)
			}
			e, _ := piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{})
			// Even canonical native frames do not prove success without the file.
			piSessionCheckReceiptEvents(t, e, request, OutcomeUnknown, tc.terminal, true, tc.name == "canonical", tc.answers, "")
		})
	}
}

func TestPiSessionReceiptRequiresCurrentAnswer(t *testing.T) {
	binary := piSessionBuildFixture(t)
	t.Run("tool-turn-then-final-assistant", func(t *testing.T) {
		selected, extension, request := piSessionFixtureSetup(t, binary)
		request.Prompt = " \tfixture:tool-ok\n"
		e, _ := piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{})
		var answers []string
		r := e.Execute(context.Background(), request, func(ev Event) error {
			if ev.Kind == ResponseEvent {
				answers = append(answers, ev.Text)
			}
			return nil
		})
		piSessionAssertSucceeded(t, r, true)
		if !slices.Equal(answers, []string{"", "offline complete"}) {
			t.Errorf("expected the final assistant end to replace the tool-turn snapshot, got %q", answers)
		}
	})
	for _, mode := range []string{"missing-user", "wrong-user", "missing-user-end", "missing-assistant-start",
		"stale-assistant", "invalid-assistant-timestamp", "prior-response", "prior-response-after-user"} {
		t.Run(mode, func(t *testing.T) {
			selected, extension, request := piSessionFixtureSetup(t, binary)
			options := PiSessionOptions{Binding: json.RawMessage(`{"turn":2}`)}
			if strings.HasPrefix(mode, "prior-response") {
				e, _ := piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{})
				first := e.Execute(context.Background(), request, nil)
				piSessionAssertSucceeded(t, first, true)
				options.Resume = first.NativeRef
				request.RunID = "run2"
			}
			request.Prompt = "fixture:" + mode
			e, _ := piSessionFixtureExecutor(t, selected, extension, piSessionTokenTwo, options)
			accepted := !slices.Contains([]string{"missing-user", "wrong-user", "missing-user-end", "prior-response"}, mode)
			piSessionCheckReceiptEvents(t, e, request, OutcomeUnknown, OutcomeSucceeded, accepted, true, nil, string(options.Binding))
		})
	}
}

func TestPiSessionReceiptRequiresPersistedTurn(t *testing.T) {
	binary := piSessionBuildFixture(t)
	for _, tc := range []struct {
		suffix   string
		terminal Outcome
	}{
		{"", OutcomeSucceeded},
		{"-aborted", OutcomeAborted},
		{"-failed", OutcomeFailed},
		{"-truncated", OutcomeTruncated},
	} {
		t.Run(string(tc.terminal), func(t *testing.T) {
			selected, extension, request := piSessionFixtureSetup(t, binary)
			e, _ := piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{})
			first := e.Execute(context.Background(), request, nil)
			piSessionAssertSucceeded(t, first, true)

			const binding = `{"turn":2}`
			e, _ = piSessionFixtureExecutor(t, selected, extension, piSessionTokenTwo, PiSessionOptions{Resume: first.NativeRef, Binding: json.RawMessage(binding)})
			request.RunID, request.Prompt = "run2", "fixture:prior-response-reformatted"+tc.suffix
			// The old text looks current on stdout. Neither its candidate callback
			// nor a clean terminal and Wait can settle the turn without persistence.
			piSessionCheckReceiptEvents(t, e, request, OutcomeUnknown, tc.terminal, true, true, []string{"offline complete"}, binding)
			data, err := os.ReadFile(first.NativeRef.Pi.SessionFile)
			if err != nil {
				t.Fatal(err)
			}
			if piSessionBytesDigest(data) != first.NativeRef.Pi.FileSHA256 {
				t.Fatal("reformatted prior response must not append to the native file")
			}
		})
	}
}

func piSessionCheckReceiptEvents(t *testing.T, e Executor, request ExecuteRequest, want, terminal Outcome, accepted, outputComplete bool, answers []string, binding string) {
	t.Helper()
	var events []Event
	r := e.Execute(context.Background(), request, func(ev Event) error { events = append(events, ev); return nil })
	if r.Outcome != want || r.Terminal != terminal || !r.Started || r.Accepted != accepted || !r.DirectChildReaped || r.OutputComplete != outputComplete || r.DeliveryFailed || r.ExitCode != 0 || !piSessionUUID.MatchString(r.NativeSessionID) {
		t.Errorf("receipt or independently observed lifecycle differs: %+v", r)
	}
	if binding == "" && r.NativeRef != nil || binding != "" && (r.NativeRef == nil || r.NativeRef.Pi != nil || string(r.NativeRef.Binding) != binding) {
		t.Error("an unverified stream must not qualify a reference or lose its binding-only barrier")
	}
	wantKinds := []EventKind{SessionEvent}
	if accepted {
		wantKinds = append(wantKinds, AcceptedEvent)
	}
	var gotKinds []EventKind
	var gotAnswers []string
	for _, ev := range events {
		gotKinds = append(gotKinds, ev.Kind)
		if ev.NativeSessionID != r.NativeSessionID {
			t.Error("callback session identity differs from observed header")
		}
		if ev.Kind == ResponseEvent {
			gotAnswers = append(gotAnswers, ev.Text)
		}
		if ev.Kind == TerminalEvent && ev.Terminal != terminal {
			t.Errorf("terminal callback differs from observed settlement: %s", ev.Terminal)
		}
	}
	for range answers {
		wantKinds = append(wantKinds, ResponseEvent)
	}
	if accepted && terminal != "" {
		wantKinds = append(wantKinds, TerminalEvent)
	}
	if !slices.Equal(gotKinds, wantKinds) || !slices.Equal(gotAnswers, answers) {
		t.Errorf("callbacks=%v answers=%q, want %v %q", gotKinds, gotAnswers, wantKinds, answers)
	}
}
