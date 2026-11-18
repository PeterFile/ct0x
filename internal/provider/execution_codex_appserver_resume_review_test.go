package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCodexSessionReviewCaptureContradictionsStayUnknown(t *testing.T) {
	for _, mode := range []string{
		"capture-active", "capture-external-item", "capture-external-unsupported", "capture-external-oversized-anchor",
		"capture-unobserved-mcp", "capture-unobserved-mcp-invalid-tool", "capture-wrong-thread", "capture-wrong-turn",
		"capture-wrong-turn-unsupported", "capture-answer-drift-unsupported", "capture-in-progress", "capture-extra-turn",
		"capture-budget-users", "capture-questions-text-drift", "capture-plan-missing-observed", "capture-plan-kind-drift",
		"capture-partial-multi-user", "capture-partial-text-drift", "capture-late-external-safe-prefix",
		"capture-deep-active", "capture-deep-idle", "capture-deep-external",
	} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("AW_SESSION_MODE", mode)
			selected, config, request := appServerSessionFixture(t)
			binding := json.RawMessage(`{"purpose":"generic","version":1}`)
			executor, err := NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, CodexAppServerSessionOptions{Binding: binding}, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			release, done := make(chan struct{}), make(chan struct{})
			releaseDelivery := sync.OnceFunc(func() { close(release) })
			var receipt ExecutionReceipt
			var events []Event
			go func() {
				receipt = executor.Execute(context.Background(), request, func(event Event) error {
					<-release
					events = append(events, event)
					return nil
				})
				close(done)
			}()
			defer func() { releaseDelivery(); <-done }()
			deadline := time.After(4 * time.Second)
			for {
				files, _ := filepath.Glob(filepath.Join(request.CWD, "eof-*"))
				if len(files) != 0 {
					break
				}
				select {
				case <-done:
					t.Fatal("returned before the child observed stdin shutdown")
				case <-deadline:
					t.Fatal("capture did not close and drain independently of delivery")
				case <-time.After(time.Millisecond):
				}
			}
			select {
			case <-done:
				t.Fatal("returned before the in-flight callback joined")
			default:
			}
			releaseDelivery()
			<-done
			assertCodexCaptureFixture(t, request.CWD, mode)
			if receipt.Outcome != OutcomeUnknown || !receipt.Accepted || receipt.Terminal != OutcomeSucceeded || !receipt.DirectChildReaped || !receipt.OutputComplete || receipt.ExitCode != 0 || receipt.DeliveryFailed || receipt.NativeRef == nil || receipt.NativeRef.Codex != nil || string(receipt.NativeRef.Binding) != string(binding) {
				t.Fatalf("contradictory capture must retain terminal/drain/wait but not settle: outcome=%s terminal=%s accepted=%v reaped=%v outputComplete=%v exit=%d deliveryFailed=%v", receipt.Outcome, receipt.Terminal, receipt.Accepted, receipt.DirectChildReaped, receipt.OutputComplete, receipt.ExitCode, receipt.DeliveryFailed)
			}
			terminals := 0
			for _, event := range events {
				if event.Kind == TerminalEvent {
					terminals++
					if event.Terminal != OutcomeSucceeded {
						t.Fatal("capture rewrote the actual terminal callback")
					}
				}
			}
			if terminals != 1 {
				t.Fatal("the real terminal callback was dropped or duplicated")
			}
		})
	}
}

func TestCodexSessionReviewOptionalCaptureUnavailableStillSucceeds(t *testing.T) {
	for _, mode := range []string{"capture-error", "capture-unsupported-anchor", "capture-questions-no-drift", "capture-plan-complete", "capture-partial-missing-observed"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("AW_SESSION_MODE", mode)
			selected, config, request := appServerSessionFixture(t)
			binding := json.RawMessage(`{"purpose":"generic"}`)
			executor, err := NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, CodexAppServerSessionOptions{Binding: binding}, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			r := executor.Execute(context.Background(), request, nil)
			if mode != "capture-error" {
				assertCodexCaptureFixture(t, request.CWD, mode)
			}
			if r.Outcome != OutcomeSucceeded || r.Terminal != OutcomeSucceeded || !r.OutputComplete || !r.DirectChildReaped || r.ExitCode != 0 || r.NativeRef == nil || r.NativeRef.Codex != nil || string(r.NativeRef.Binding) != string(binding) {
				t.Fatal("optional anchor unavailability erased real success or invented a resumable reference")
			}
		})
	}
}

func assertCodexCaptureFixture(t *testing.T, cwd, mode string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(cwd, "capture-meta-*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatal("missing bounded child capture metadata")
	}
	data, err := os.ReadFile(paths[0])
	var meta struct {
		Mode           string         `json:"mode"`
		FrameBytes     int            `json:"frame_bytes"`
		Items          int            `json:"items"`
		Counts         map[string]int `json:"counts"`
		ObservedMCP    bool           `json:"observed_mcp_present"`
		ObservedAnswer bool           `json:"observed_answer_present"`
		AnswerMatches  bool           `json:"answer_text_matches"`
		QuestionsEmpty bool           `json:"questions_empty"`
		ArgumentsDepth int            `json:"arguments_nesting"`
		CommandIndex   int            `json:"command_index"`
	}
	if err != nil || json.Unmarshal(data, &meta) != nil || meta.Mode != mode || meta.FrameBytes <= 0 || meta.FrameBytes >= codexRPCFrameLimit {
		t.Fatal("fixture did not send a valid sub-limit capture frame")
	}
	t.Logf("synthetic capture metadata: %s", data)
	switch mode {
	case "capture-budget-users":
		if meta.Counts["userMessage"] != codexSessionHistoryItemLimit+1 || meta.Counts["reasoning"] != 0 || !meta.ObservedMCP || !meta.ObservedAnswer || !meta.AnswerMatches {
			t.Fatal("budget fixture must contain distinct matching users and observed items, without earlier reasoning/text conflicts")
		}
	case "capture-late-external-safe-prefix":
		if meta.Counts["plan"] != 16 || meta.Counts["reasoning"] != 0 || meta.CommandIndex != meta.Items-1 || !meta.ObservedMCP || !meta.ObservedAnswer || !meta.AnswerMatches {
			t.Fatal("late external fixture must have only nonconflicting unsupported plans before the command")
		}
	case "capture-questions-text-drift", "capture-partial-text-drift":
		if !meta.QuestionsEmpty || meta.AnswerMatches {
			t.Fatal("questions must be schema-valid and must not conceal text drift")
		}
	case "capture-questions-no-drift":
		if !meta.QuestionsEmpty || !meta.AnswerMatches {
			t.Fatal("unsupported questions control must preserve observed text")
		}
	case "capture-plan-missing-observed", "capture-partial-missing-observed":
		if meta.ObservedMCP || !meta.ObservedAnswer || !meta.AnswerMatches {
			t.Fatal("coverage fixture must omit the observed MCP item without changing the observed answer")
		}
	case "capture-deep-active", "capture-deep-idle", "capture-deep-external":
		if meta.ArgumentsDepth <= 64 || !meta.ObservedMCP || !meta.ObservedAnswer || !meta.AnswerMatches {
			t.Fatal("deep fixture must exceed strict parsing depth without losing observed items")
		}
	}
}

func TestCodexSessionReviewEmptyAnswerRetainsActualTerminal(t *testing.T) {
	t.Setenv("AW_SESSION_MODE", "empty-answer")
	selected, config, request := appServerSessionFixture(t)
	executor, err := NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, CodexAppServerSessionOptions{}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r := executor.Execute(context.Background(), request, nil)
	if r.Outcome != OutcomeSucceeded || r.Terminal != OutcomeSucceeded || !r.OutputComplete || !r.DirectChildReaped || r.NativeRef == nil || r.NativeRef.Codex == nil {
		t.Fatal("empty response text cannot replace or invalidate a real terminal")
	}
}

func TestCodexSessionReviewColdResumePassiveReplay(t *testing.T) {
	phases := []string{"before-response", "after-response", "before-history", "after-history", "before-turn-response", "after-turn-response", "after-turn-started"}
	for _, kind := range []string{"goal-cleared", "usage", "goal-and-usage"} {
		for _, phase := range phases {
			t.Run(kind+"/"+phase, func(t *testing.T) {
				selected, config, request := appServerSessionFixture(t)
				executor, err := NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, CodexAppServerSessionOptions{}, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				first := executor.Execute(context.Background(), request, nil)
				if first.Outcome != OutcomeSucceeded || first.NativeRef == nil || first.NativeRef.Codex == nil {
					t.Fatal("fresh fixture baseline was not captured")
				}
				t.Setenv("AW_SESSION_REPLAY_KIND", kind)
				t.Setenv("AW_SESSION_REPLAY_PHASE", phase)
				executor, err = NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, CodexAppServerSessionOptions{Resume: first.NativeRef}, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				request.RunID, request.Prompt = "resume-passive", "next independently authorized input"
				var events []Event
				second := executor.Execute(context.Background(), request, func(event Event) error { events = append(events, event); return nil })
				if second.Outcome != OutcomeSucceeded || !second.Accepted || second.Terminal != OutcomeSucceeded || !second.OutputComplete || !second.DirectChildReaped || second.ExitCode != 0 || second.NativeRef == nil || second.NativeRef.Codex == nil || second.NativeRef.Codex.NativeThreadID != first.NativeRef.Codex.NativeThreadID || second.NativeRef.Codex.NativeTurnID != "session-turn-2" || second.NativeRef.Codex.HistoryTurns != 2 {
					t.Fatalf("valid cold resume metadata rejected: outcome=%s accepted=%v terminal=%s reaped=%v reason=%s", second.Outcome, second.Accepted, second.Terminal, second.DirectChildReaped, second.Reason)
				}
				accepted, terminal := 0, 0
				for _, event := range events {
					if event.Kind == AcceptedEvent {
						accepted++
					}
					if event.Kind == TerminalEvent {
						terminal++
					}
				}
				if accepted != 1 || terminal != 1 {
					t.Fatal("passive replay invented current-turn lifecycle evidence")
				}
				assertSessionReviewClientStarts(t, request.CWD, request.Prompt, 1)
			})
		}
	}
}

func TestCodexSessionReviewReplayOlderVerifiedTurn(t *testing.T) {
	selected, config, request := appServerSessionFixture(t)
	var ref *NativeSessionReference
	for i, run := range []string{"older-first", "older-second", "older-third"} {
		if i == 2 {
			t.Setenv("AW_SESSION_REPLAY_KIND", "usage")
			t.Setenv("AW_SESSION_REPLAY_PHASE", "after-turn-started")
		}
		executor, err := NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, CodexAppServerSessionOptions{Resume: ref}, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		request.RunID = run
		r := executor.Execute(context.Background(), request, nil)
		if r.Outcome != OutcomeSucceeded || r.NativeRef == nil || r.NativeRef.Codex == nil || r.NativeRef.Codex.HistoryTurns != i+1 {
			t.Fatal("replay did not bind to the full verified old-turn set")
		}
		ref = r.NativeRef
	}
}

func TestCodexSessionReviewGoalsDisabledOnlyInSessionThreadConfig(t *testing.T) {
	t.Setenv("AW_SESSION_ENFORCE_GOALS_DISABLED", "1")
	selected, config, request := appServerSessionFixture(t)
	var ref *NativeSessionReference
	for i, run := range []string{"goals-fresh", "goals-resume"} {
		if i == 1 {
			t.Setenv("AW_SESSION_REPLAY_KIND", "goal-and-usage")
			t.Setenv("AW_SESSION_REPLAY_PHASE", "after-response")
		}
		executor, err := NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, CodexAppServerSessionOptions{Resume: ref}, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		request.RunID = run
		r := executor.Execute(context.Background(), request, nil)
		if r.Outcome != OutcomeSucceeded || r.NativeRef == nil || r.NativeRef.Codex == nil {
			t.Fatalf("native thread config must disable autonomous goals before start/resume, while empty snapshots remain passive: outcome=%s accepted=%v terminal=%s", r.Outcome, r.Accepted, r.Terminal)
		}
		ref = r.NativeRef
	}
	t.Setenv("AW_SESSION_ENFORCE_GOALS_DISABLED", "")
	legacy, err := NewCodexAppServerMemberToolExecutor(selected, config, []string{"read_channel"}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	request.RunID = "goals-legacy"
	r := legacy.Execute(context.Background(), request, nil)
	if r.Outcome != OutcomeSucceeded || r.NativeRef != nil {
		t.Fatal("session-only goal policy changed the legacy constructor")
	}
	fresh, resumed, unchanged := 0, 0, 0
	for _, frames := range sessionClientFrames(t, request.CWD) {
		for _, frame := range frames {
			if *frame.Method != "thread/start" && *frame.Method != "thread/resume" {
				continue
			}
			var params struct {
				Config struct {
					Features map[string]json.RawMessage `json:"features"`
				} `json:"config"`
			}
			if json.Unmarshal(frame.Params, &params) != nil {
				t.Fatal("invalid fixture thread config")
			}
			if enabled, present := params.Config.Features["goals"]; present {
				if string(enabled) != "false" {
					t.Fatal("autonomous goals not explicitly disabled")
				}
				if *frame.Method == "thread/resume" {
					resumed++
				} else {
					fresh++
				}
			} else {
				unchanged++
			}
		}
	}
	args, _ := filepath.Glob(filepath.Join(request.CWD, "argv-*.json"))
	if fresh != 1 || resumed != 1 || unchanged != 1 || len(args) != 3 {
		t.Fatal("goal override was missing, widened to legacy, or bypassed cold resume")
	}
	for _, path := range args {
		data, err := os.ReadFile(path)
		if err != nil || strings.Contains(string(data), "features.goals") {
			t.Fatal("session-only goal policy leaked into shared native argv")
		}
	}
}

func TestCodexSessionReviewRejectsUnqualifiedReplay(t *testing.T) {
	for _, test := range []struct {
		kind, phase, historyMode string
		foreign                  bool
	}{
		{kind: "goal-cleared", phase: "before-response", foreign: true},
		{kind: "goal-cleared", phase: "after-response", foreign: true},
		{kind: "usage", phase: "before-response", foreign: true},
		{kind: "usage", phase: "after-turn-started", foreign: true},
		{kind: "usage-unverified", phase: "before-response"},
		{kind: "usage-unverified", phase: "after-response"},
		{kind: "usage-unverified", phase: "before-history"},
		{kind: "usage-unverified", phase: "before-turn-response"},
		{kind: "usage-unverified", phase: "after-turn-response"},
		{kind: "usage-unverified", phase: "after-turn-started"},
		{kind: "usage-future-turn", phase: "after-response"},
		{kind: "usage-without-turn", phase: "after-response"},
		{kind: "usage-overflow", phase: "after-response"},
		{kind: "usage-byte-overflow", phase: "after-response"},
		{kind: "usage", phase: "after-response", historyMode: "unfinished-history"},
		{kind: "goal-nonempty", phase: "before-response"},
		{kind: "goal-nonempty", phase: "after-response"},
		{kind: "goal-nonempty", phase: "after-turn-started"},
		{kind: "goal-cleared-with-goal", phase: "after-response"},
		{kind: "goal-cleared-with-turn", phase: "after-turn-started"},
	} {
		name := test.kind + "/" + test.phase + "/" + test.historyMode
		if test.foreign {
			name += "/foreign-thread"
		}
		t.Run(name, func(t *testing.T) {
			selected, config, request := appServerSessionFixture(t)
			binding := json.RawMessage(`{"purpose":"generic"}`)
			executor, err := NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, CodexAppServerSessionOptions{Binding: binding}, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			first := executor.Execute(context.Background(), request, nil)
			if first.Outcome != OutcomeSucceeded || first.NativeRef == nil || first.NativeRef.Codex == nil {
				t.Fatal("fresh fixture baseline was not captured")
			}
			t.Setenv("AW_SESSION_REPLAY_KIND", test.kind)
			t.Setenv("AW_SESSION_REPLAY_PHASE", test.phase)
			t.Setenv("AW_SESSION_MODE", test.historyMode)
			if test.foreign {
				t.Setenv("AW_SESSION_REPLAY_FOREIGN", "1")
			}
			executor, err = NewCodexAppServerSessionExecutor(selected, config, []string{"read_channel"}, CodexAppServerSessionOptions{Binding: binding, Resume: first.NativeRef}, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			request.RunID, request.Prompt = "resume-rejected", "must not be used as replay authority"
			second := executor.Execute(context.Background(), request, nil)
			if second.Outcome != OutcomeUnknown || !second.DirectChildReaped || second.NativeRef == nil || second.NativeRef.Codex != nil || string(second.NativeRef.Binding) != string(binding) {
				t.Fatal("foreign/unverified/nonempty replay was adopted as a qualified execution")
			}
			starts := 0
			if strings.Contains(test.phase, "turn-") {
				starts = 1 // The race occurs only after the independently gated input.
			} else if second.Accepted || second.Terminal != "" {
				t.Fatal("replay granted current-turn identity or acceptance")
			}
			assertSessionReviewClientStarts(t, request.CWD, request.Prompt, starts)
		})
	}
}

func assertSessionReviewClientStarts(t *testing.T, cwd, prompt string, want int) {
	t.Helper()
	fresh, resumed, submitted := 0, 0, 0
	frames := sessionClientFrames(t, cwd)
	for _, messages := range frames {
		for _, message := range messages {
			switch *message.Method {
			case "thread/start":
				fresh++
			case "thread/resume":
				resumed++
			case "turn/start":
				if strings.Contains(string(message.Params), prompt) {
					submitted++
				}
			}
		}
	}
	if len(frames) != 2 || fresh != 1 || resumed != 1 || submitted != want {
		t.Fatalf("unexpected cold resume submissions/fallback: children=%d fresh=%d resumed=%d submitted=%d want=%d", len(frames), fresh, resumed, submitted, want)
	}
}
