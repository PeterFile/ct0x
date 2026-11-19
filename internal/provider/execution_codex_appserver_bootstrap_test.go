package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	codexBootstrapPrivatePayload = "synthetic private bootstrap payload"
	codexBootstrapPrivateID      = "synthetic-private-bootstrap-id"
	codexBootstrapPrivateMethod  = "synthetic/private-method"
	codexFixtureWarningText      = codexBootstrapPrivatePayload + " " + memberToolTestToken
	codexFixtureScopedWarning    = `{"method":"warning","params":{"message":"` + codexFixtureWarningText + `","threadId":"session-native-thread"}}`
	codexFixtureUnscopedWarnings = `{"method":"warning","params":{"message":"` + codexFixtureWarningText + `"}}` + "\n" +
		`{"method":"warning","params":{"message":"` + codexFixtureWarningText + `","threadId":null}}`
	codexFixtureThreadAnnouncement = `{"method":"thread/started","params":{"thread":{"id":"session-native-thread","preview":"` + codexBootstrapPrivatePayload + `"}}}`
)

func readCodexFixtureProjection(t *testing.T, path string, private ...string) map[string][]map[string]any {
	t.Helper()
	if stat, err := os.Stat(path); err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("missing private execution log")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range private {
		if strings.Contains(string(raw), value) {
			t.Fatal("private fixture payload entered the adapter projection")
		}
	}
	records := make(map[string][]map[string]any)
	for line := range strings.Lines(string(raw)) {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil {
			t.Fatal("invalid projected JSONL record")
		}
		kind, _ := record["type"].(string)
		switch kind {
		case "bootstrap_failure", "config_summary", "lifecycle_summary", "stderr_summary":
			records[kind] = append(records[kind], record)
		default:
			t.Fatal("unexpected log projection type")
		}
	}
	return records
}

func TestCodexSessionBootstrapFailureSummary(t *testing.T) {
	selected, config, request := appServerSessionFixture(t)
	const timeout = 5 * time.Second
	const binding = `{"turn":2}`
	tools := []string{"read_channel"}
	fresh, err := NewCodexAppServerSessionExecutor(selected, config, tools, CodexAppServerSessionOptions{}, timeout)
	if err != nil {
		t.Fatal(err)
	}
	first := fresh.Execute(t.Context(), request, nil)
	if first.Outcome != OutcomeSucceeded || first.NativeRef == nil || first.NativeRef.Codex == nil {
		t.Fatal("fixture baseline not captured")
	}
	resumed, err := NewCodexAppServerSessionExecutor(selected, config, tools, CodexAppServerSessionOptions{Resume: first.NativeRef, Binding: []byte(binding)}, timeout)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := NewCodexAppServerMemberToolExecutor(selected, config, tools, timeout)
	if err != nil {
		t.Fatal(err)
	}
	warning := codexFixtureScopedWarning
	afterWarning := warning + "\n"
	rpcError := func(id, code string) string {
		return `{"id":` + id + `,"error":{"code":` + code + `,"message":"` + codexBootstrapPrivatePayload + `","data":{"private":"` + memberToolTestToken + `"}}}`
	}
	starts, resumes := 1, 0 // One fresh baseline; no failure can issue a new turn.
	for _, tc := range []struct {
		name, mode, wire, phase, reason string
		code                            int
		bound                           bool
	}{
		{"warning-before-eof", "bootstrap-close", warning, "thread_resume", "connection_ended", 0, false},
		{"rpc-error", "bootstrap-close", afterWarning + rpcError("3", "-32000"), "thread_resume", "rpc_error", -32000, false},
		{"oversized-code", "bootstrap-close", rpcError("3", "1099511627776"), "thread_resume", "rpc_error", 0, false},
		{"uncorrelated-error", "bootstrap-close", afterWarning + rpcError(`"`+codexBootstrapPrivateID+`"`, "-32000"), "thread_resume", "response_id_mismatch", 0, false},
		{"malformed-frame", "", afterWarning + codexBootstrapPrivatePayload, "thread_resume", "rpc_frame_rejected", 0, false},
		{"oversize-frame", "bootstrap-oversize-frame", warning, "thread_resume", "rpc_frame_too_large", 0, false},
		{"incomplete-frame", "bootstrap-incomplete-frame", warning, "thread_resume", "rpc_frame_incomplete", 0, false},
		{"foreign-warning", "", strings.Replace(warning, "session-native-thread", codexBootstrapPrivateID, 1), "thread_resume", "warning_before_thread_bind", 0, false},
		{"warning-id", "", `{"id":99,` + warning[1:], "thread_resume", "server_request_before_bind", 0, false},
		{"warning-null-id", "", `{"id":null,` + warning[1:], "thread_resume", "rpc_frame_rejected", 0, false},
		{"warning-thread-type", "", strings.Replace(warning, `"session-native-thread"`, `17`, 1), "thread_resume", "notification_params_rejected", 0, false},
		{"warning-message-type", "", strings.Replace(warning, `"`+codexFixtureWarningText+`"`, `null`, 1), "thread_resume", "notification_params_rejected", 0, false},
		{"warning-thread-alias", "", strings.Replace(warning, `"threadId"`, `"thread_id"`, 1), "thread_resume", "notification_params_rejected", 0, false},
		{"warning-null-with-alias", "", strings.Replace(warning, `"threadId":`, `"threadId":null,"ThreadId":`, 1), "thread_resume", "notification_params_rejected", 0, false},
		{"warning-duplicate-key", "", strings.Replace(warning, `"threadId":`, `"threadId":null,"thread\u0049d":`, 1), "thread_resume", "notification_params_rejected", 0, false},
		{"warning-turn-scope", "", strings.Replace(warning, `"threadId":`, `"turnId":"session-turn-1","threadId":`, 1), "thread_resume", "notification_params_rejected", 0, false},
		{"warning-grant-fields", "", strings.Replace(warning, `"threadId":`, `"caller":"`+codexBootstrapPrivateID+`","threadId":`, 1), "thread_resume", "notification_params_rejected", 0, false},
		{"warning-count-limit", "bootstrap-warning-count", "", "thread_resume", "buffer_limit", 0, false},
		{"warning-byte-limit", "bootstrap-warning-bytes", "", "thread_resume", "buffer_limit", 0, false},
		{"unknown-then-error", "", afterWarning + `{"method":"` + codexBootstrapPrivateMethod + `","params":{"threadId":"` + codexBootstrapPrivateID + `"}}` + "\n" + rpcError("3", "-32000"), "thread_resume", "notification_not_allowed", 0, false},
		{"read-state", "", afterWarning + `{"method":"thread/readState/changed","params":{"threadId":"session-native-thread","readState":{"revision":"synthetic","firstUnread":null}}}`, "thread_resume", "notification_not_allowed", 0, false},
		{"invalid-connection-metadata", "", afterWarning + `{"method":"account/updated","params":{"authMode":"future-mode"}}`, "thread_resume", "connection_metadata_rejected", 0, false},
		{"explicit-logout", "", afterWarning + `{"id":99,"method":"account/logout","params":{}}`, "thread_resume", "server_request_before_bind", 0, false},
		{"thread-closed", "", afterWarning + `{"method":"thread/closed","params":{"threadId":"session-native-thread"}}`, "thread_resume", "thread_closed_before_thread_bind", 0, false},
		{"request-resolved", "", afterWarning + `{"method":"serverRequest/resolved","params":{"threadId":"session-native-thread","requestId":"` + codexBootstrapPrivateID + `"}}`, "thread_resume", "server_request_resolved_before_thread_bind", 0, false},
		{"old-turn-started", "", afterWarning + `{"method":"turn/started","params":{"threadId":"session-native-thread","turn":{"id":"session-turn-1","status":"inProgress","error":null,"items":[]}}}`, "thread_resume", "turn_started_before_thread_bind", 0, false},
		{"old-turn-completed", "", afterWarning + `{"method":"turn/completed","params":{"threadId":"session-native-thread","turn":{"id":"session-turn-1","status":"completed","error":null,"items":[]}}}`, "thread_resume", "turn_completed_before_thread_bind", 0, false},
		{"invalid-announcement", "", afterWarning + `{"method":"thread/started","params":{"thread":{"id":null}}}`, "thread_resume", "thread_announcement_invalid", 0, false},
		{"duplicate-announcement", "", afterWarning + codexFixtureThreadAnnouncement + "\n" + codexFixtureThreadAnnouncement, "thread_resume", "thread_announcement_duplicate", 0, false},
		{"conflicting-announcement", "", afterWarning + strings.Replace(codexFixtureThreadAnnouncement, "session-native-thread", codexBootstrapPrivateID, 1), "thread_resume_bind", "thread_announcement_mismatch", 0, false},
		{"wrong-native-home", "wrong-home", warning, "initialize", "initialize_rejected", 0, false},
		{"pre-config-profile", "", warning, "config_before", "config_profile_rejected", 0, false},
		{"response-model", "wrong-model", warning, "thread_resume_bind", "thread_response_rejected", 0, false},
		{"response-thread", "wrong-thread", warning, "thread_resume_bind", "thread_profile_rejected", 0, false},
		{"replayed-wrong-thread-status", "", afterWarning + `{"method":"thread/status/changed","params":{"threadId":"` + codexBootstrapPrivateID + `","status":{"type":"idle"}}}`, "thread_resume_replay", "notification_thread_rejected", 0, true},
		{"post-config-profile", "", warning, "", "", 0, true},
		{"wrong-mcp-origin", "bootstrap-wrong-mcp-origin", warning, "", "", 0, true},
		{"wrong-mcp-auth", "bootstrap-wrong-mcp-auth", warning, "", "", 0, true},
		{"changed-history", "changed-content", warning, "", "", 0, true},
		{"bound-thread-closed", "bootstrap-bound-thread-closed", warning, "", "", 0, true},
		{"fresh-scoped-warning", "", warning, "thread_start", "protocol_rejected", 0, false},
		{"legacy-scoped-warning", "", warning, "", "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AW_SESSION_MODE", tc.mode)
			t.Setenv("AW_SESSION_PREBIND_WIRE", tc.wire)
			executor, wantBinding := resumed, binding
			switch tc.name {
			case "pre-config-profile", "post-config-profile":
				which := "AW_SESSION_CONFIG_BEFORE"
				if tc.name == "post-config-profile" {
					which = "AW_SESSION_CONFIG_AFTER"
				}
				t.Setenv(which, `{"config":{},"origins":{},"layers":[{"name":{"type":"sessionFlags"},"version":"flags-v1","config":{}}]}`)
			case "fresh-scoped-warning":
				executor, wantBinding = fresh, ""
			case "legacy-scoped-warning":
				executor, wantBinding = legacy, ""
			}
			if wantBinding == "" {
				starts++
			} else if tc.phase != "initialize" && tc.phase != "config_before" {
				resumes++
			}
			request.RunID, request.Prompt = tc.name, "synthetic private follow-up"
			var events []Event
			r := executor.Execute(t.Context(), request, func(event Event) error {
				events = append(events, event)
				return nil
			})
			if r.Outcome != OutcomeUnknown || !r.Started || r.Accepted || r.Terminal != "" || r.OutputComplete || !r.DirectChildReaped || r.ExitCode != 0 || r.DeliveryFailed || r.LogsTruncated {
				t.Fatalf("bootstrap failure did not keep an unknown receipt: %s, complete=%t, exit=%d", r.Outcome, r.OutputComplete, r.ExitCode)
			}
			wantThread := ""
			if tc.bound {
				wantThread = first.NativeSessionID // Binding is independent of readiness to launch input.
			}
			if r.NativeSessionID != wantThread || len(events) != 0 && (wantThread == "" || !reflect.DeepEqual(events, []Event{{Kind: SessionEvent, NativeSessionID: wantThread}})) {
				t.Fatal("bootstrap failure changed verified lifecycle evidence")
			}
			if wantBinding == "" && r.NativeRef != nil || wantBinding != "" && (r.NativeRef == nil || r.NativeRef.Codex != nil || string(r.NativeRef.Binding) != wantBinding) {
				t.Fatal("bootstrap failure exposed resumable evidence or lost its current barrier")
			}
			private := []string{request.Prompt, memberToolTestToken, codexBootstrapPrivateMethod, codexBootstrapPrivateID, codexBootstrapPrivatePayload, "session-turn-1"}
			if !tc.bound {
				private = append(private, "session-native-thread")
			}
			for _, value := range private {
				if strings.Contains(r.Reason, value) {
					t.Fatal("native payload entered the receipt reason")
				}
			}
			out := readCodexFixtureProjection(t, r.StdoutPath, private...)
			err := readCodexFixtureProjection(t, r.StderrPath, private...)
			life := out["lifecycle_summary"]
			if !tc.bound && len(life) != 0 || tc.bound && (len(life) != 1 || life[0]["thread_id"] != wantThread || life[0]["turn_id"] != "") {
				t.Fatal("lifecycle projection did not preserve just the bound thread")
			}
			if len(err["bootstrap_failure"]) != 0 {
				t.Fatal("duplicate bootstrap projection in stderr")
			}
			got := out["bootstrap_failure"]
			if tc.phase == "" {
				if len(got) != 0 {
					t.Fatal("legacy or already bound executor gained bootstrap diagnostics")
				}
				return
			}
			want := map[string]any{"type": "bootstrap_failure", "phase": tc.phase, "reason": tc.reason}
			if tc.code != 0 {
				want["rpc_code"] = float64(tc.code)
			}
			if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
				t.Fatalf("want one fixed diagnostic: %s/%s", tc.phase, tc.reason)
			}
		})
	}
	counts := map[string]int{}
	for _, frames := range sessionClientFrames(t, request.CWD) {
		for _, frame := range frames {
			if frame.Method == nil {
				t.Fatal("pre-bind warning or control frame elicited a reply")
			}
			counts[*frame.Method]++
		}
	}
	if counts["thread/start"] != starts || counts["thread/resume"] != resumes || counts["turn/start"] != 1 || counts["account/login/start"] != 0 || counts["turn/interrupt"] != 0 {
		t.Fatal("bootstrap failure sent follow-up input, account work, interrupt or a fallback")
	}
}

func TestCodexSessionBootstrapWarningWaitsForHistory(t *testing.T) {
	const config = `{"config":{"model_reasoning_effort":"max"},"origins":{},"layers":[{"name":{"type":"sessionFlags"},"version":"flags-v1","config":{}}]}`
	const history = `{"thread":{"id":"synthetic-thread","source":"vscode","cliVersion":"0.166.0","ephemeral":false,
		"cwd":"/synthetic/workspace","modelProvider":"openai","status":{"type":"idle"},"turns":[{"id":"previous-turn","status":"completed",
		"items":[{"id":"answer","type":"agentMessage","text":"synthetic old answer"}]}]}}`
	anchor := newCodex166SourceProtocol(t)
	receiveCodex166SourceFrame(t, anchor, codex166StdioThreadStartFrame)
	prefix, ok := anchor.sessionHistory(json.RawMessage(history))
	if !ok {
		t.Fatal("invalid synthetic history fixture")
	}
	for _, name := range []string{"verified", "drift", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			p := newCodex166SourceProtocol(t)
			p.before, _ = readCodexAppServerConfig(json.RawMessage(config))
			p.summary = func(codexAppServerConfigSnapshot) {}
			p.session.resume = &CodexNativeSessionReference{NativeThreadID: "synthetic-thread", NativeTurnID: "previous-turn", HistorySHA256: prefix.digest(), HistoryTurns: 1, HistoryItems: 1, Provider: "openai", ReasoningEffort: "max"}
			p.member.server, p.member.origin, p.member.names = "issued-server", "http://127.0.0.1:1", []string{"read_channel"}
			receive := func(raw string) { receiveCodex166SourceFrame(t, p, raw) }
			receive(strings.Replace(codexFixtureScopedWarning, "session-native-thread", "synthetic-thread", 1))
			if p.damaged || p.threadID != "" || len(p.buffer) != 1 || p.awaiting != 3 {
				t.Fatal("pending warning did not wait for a correlated response")
			}
			receive(`{"method":"mcpServer/startupStatus/updated","params":{"threadId":"synthetic-thread","name":"issued-server","status":"starting"}}`)
			receive(codex166StdioThreadStartFrame) // The metadata-only response has the same validated fields.
			if p.damaged || p.awaiting != 4 || p.threadID != "synthetic-thread" || p.member.startup != "starting" || len(p.buffer) != 1 {
				t.Fatal("warning replayed at bind or changed existing MCP startup replay order")
			}
			receive(`{"id":4,"result":` + config + `}`)
			receive(`{"id":7,"result":{"data":[{"name":"issued-server","httpOrigin":"http://127.0.0.1:1","runtimeStatus":"connected","authStatus":"bearerToken","tools":{"read_channel":{"name":"read_channel"}}}]}}`)
			if p.damaged || !p.member.ready || p.awaiting != 8 || len(p.buffer) != 1 || p.receipt.Accepted {
				t.Fatal("config/MCP rechecks replayed the warning before the anchored read")
			}
			result := history
			if name == "drift" {
				result = strings.Replace(history, "synthetic old answer", "drifted answer", 1)
			}
			if name == "cancelled" {
				p.cancel()
			}
			receive(`{"id":8,"result":` + result + `}`)
			if name == "verified" {
				if p.damaged || p.awaiting != 5 || len(p.buffer) != 0 || p.receipt.Accepted {
					t.Fatal("verified history did not replay before the new turn request")
				}
			} else if p.awaiting != 0 || len(p.buffer) != 1 || p.receipt.Accepted {
				t.Fatal("drifted or cancelled read replayed a pending warning or started a turn")
			}
		})
	}
}

func TestCodexSessionBootstrapPendingContext(t *testing.T) {
	selected, config, request := appServerSessionFixture(t)
	tools := []string{"read_channel"}
	fresh, err := NewCodexAppServerSessionExecutor(selected, config, tools, CodexAppServerSessionOptions{}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	first := fresh.Execute(t.Context(), request, nil)
	if first.Outcome != OutcomeSucceeded || first.NativeRef == nil || first.NativeRef.Codex == nil {
		t.Fatal("fixture baseline not captured")
	}
	for _, name := range []string{"cancel", "deadline"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AW_SESSION_MODE", "bootstrap-pending")
			t.Setenv("AW_SESSION_PREBIND_WIRE", codexFixtureScopedWarning)
			resumed, err := NewCodexAppServerSessionExecutor(selected, config, tools, CodexAppServerSessionOptions{Resume: first.NativeRef}, 15*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			var ctx context.Context
			var cancel context.CancelFunc
			wantErr := context.Canceled
			if name == "deadline" {
				ctx, cancel = context.WithTimeout(t.Context(), 3*time.Second)
				wantErr = context.DeadlineExceeded
			} else {
				ctx, cancel = context.WithCancel(t.Context())
			}
			request.RunID = name
			done := make(chan struct{})
			var r ExecutionReceipt
			var events []Event
			go func() {
				r = resumed.Execute(ctx, request, func(event Event) error { events = append(events, event); return nil })
				close(done)
			}()
			defer func() { cancel(); <-done }()
			var markers []string
			deadline := time.After(8 * time.Second) // Independent of the fallback executor deadline.
			poll := time.NewTicker(5 * time.Millisecond)
			defer poll.Stop()
			for len(markers) != 1 {
				select {
				case <-done:
					t.Fatal("executor returned before context completed")
				case <-ctx.Done():
					t.Fatal("caller context finished before resume was pending")
				case <-deadline:
					t.Fatal("child did not reach thread/resume")
				case <-poll.C:
					markers, err = filepath.Glob(filepath.Join(request.CWD, "resume-pending-*"))
					if err != nil || len(markers) > 1 {
						t.Fatal("unexpected pending-resume markers")
					}
				}
			}
			pid, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(markers[0]), "resume-pending-"))
			if err != nil || pid <= 1 || ctx.Err() != nil {
				t.Fatal("no live pending-resume checkpoint")
			}
			if name == "cancel" {
				cancel() // Only after the child has read thread/resume and withheld a reply.
			}
			select {
			case <-done:
			case <-deadline:
				t.Fatal("executor did not stop on the caller's context")
			}
			if ctx.Err() != wantErr || r.Outcome != OutcomeUnknown || !r.Started || r.Accepted || r.Terminal != "" || r.NativeSessionID != "" || r.NativeRef != nil || len(events) != 0 ||
				!r.DirectChildReaped || r.ExitCode != 0 || r.OutputComplete || r.DeliveryFailed || r.LogsTruncated {
				t.Fatal("context completion masked an uncertain pending resume or invented evidence")
			}
			if _, err := syscall.Wait4(pid, nil, syscall.WNOHANG, nil); err != syscall.ECHILD {
				t.Fatal("Execute returned without reaping its child")
			}
			private := []string{codexBootstrapPrivatePayload, memberToolTestToken, first.NativeSessionID}
			out := readCodexFixtureProjection(t, r.StdoutPath, private...)
			errOut := readCodexFixtureProjection(t, r.StderrPath, private...)
			want := []map[string]any{{"type": "bootstrap_failure", "phase": "thread_resume", "reason": "context_done"}}
			if !reflect.DeepEqual(out["bootstrap_failure"], want) || len(errOut["bootstrap_failure"]) != 0 {
				t.Fatal("context completion was not reported by a single bounded diagnostic")
			}
			if err := os.Remove(markers[0]); err != nil {
				t.Fatal(err)
			}
		})
	}
}
