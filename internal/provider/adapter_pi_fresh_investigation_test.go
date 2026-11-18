package provider

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func TestPiFreshInvestigationExactBinding(t *testing.T) {
	root := t.TempDir()
	write := func(path, text string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), mode); err != nil {
			t.Fatal(err)
		}
	}
	entry := filepath.Join(root, "fresh-investigation-work.ts")
	dependency := filepath.Join(root, "fresh-task-work.mjs")
	for _, file := range []string{entry, dependency} {
		data, err := os.ReadFile(filepath.Join("../../extensions/pi-bridge", filepath.Base(file)))
		if err != nil {
			t.Fatal(err)
		}
		write(file, string(data), 0600)
	}
	cli, path := filepath.Join(root, "fixture-pi"), filepath.Join(root, "issued.json")
	write(cli, `#!/bin/sh
printf '%s\n' "$@" >&2
printf '%s\n' '{"type":"session","id":"turn-owner","version":3,"cwd":"/fixture","timestamp":"2026-11-18T00:00:00.000Z"}'
printf '{"type":"entry_appended","entry":{"type":"custom","customType":"agent_workspace_investigation_ready_v1","data":{"run_id":"turn-owner","source_kind":"report","config_sha256":"%s"},"id":"abcdef01","parentId":null,"timestamp":"2026-11-18T00:00:00.000Z"}}\n' "$AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256"
printf '%s\n' '{"type":"agent_start"}' '{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[]}}' '{"type":"agent_settled","aborted":false}'
`, 0700)
	selected := Provider{ID: "fixture", Name: Pi, Version: "1.4.0", Installation: Installed, Path: cli}
	profile := PiFreshInvestigationProfile{Action: domain.ChatActionReadOnlyInvestigation, SourceKind: domain.TaskWorkSourceReport,
		Operations: []string{"read_investigation_channel", "read_investigation_artifact", "read_work_report", "read_work_inputs", "ack_work_input", "hand_back_work_input", "submit_result"}}
	adapter := NewPiFreshInvestigationAdapter(entry, 0)
	if !adapter.Supports(selected, profile) {
		t.Fatal("exact Pi and report owner profile not supported")
	}
	wrong := selected
	wrong.Version = "1.4.1"
	if adapter.Supports(wrong, profile) || NewPiFreshInvestigationAdapter(dependency, 0).Supports(selected, profile) {
		t.Fatal("unsupported Pi version or entry qualified")
	}
	ctx := context.Background()
	const body = `{"endpoint":"http://127.0.0.1:7310/api/v1/member-tools","token":"fixture-bearer","run_id":"turn-owner","action":"read_only_investigation","source_kind":"report","operations":["read_investigation_channel","read_investigation_artifact","read_work_report","read_work_inputs","ack_work_input","hand_back_work_input","submit_result"]}` + "\n"
	raw, digest, err := PiFreshInvestigationConfig("http://127.0.0.1:7310/api/v1/member-tools", "fixture-bearer", "turn-owner", profile)
	if err != nil || string(raw) != body || digest != sha256.Sum256([]byte(body)) {
		t.Fatal("config does not match the issued cross-language wire format", err)
	}
	write(path, body, 0600)
	binding := PiFreshInvestigationBinding{ConfigPath: path, ConfigSHA256: digest, RunID: "turn-owner", Profile: profile}
	for label, edit := range map[string]func(*PiFreshInvestigationBinding){
		"absent digest":  func(b *PiFreshInvestigationBinding) { b.ConfigSHA256 = [32]byte{} },
		"wrong digest":   func(b *PiFreshInvestigationBinding) { b.ConfigSHA256[0] ^= 1 },
		"invalid run ID": func(b *PiFreshInvestigationBinding) { b.RunID = "../turn-owner" },
		"wrong run ID":   func(b *PiFreshInvestigationBinding) { b.RunID = "turn-other" },
		"wrong action":   func(b *PiFreshInvestigationBinding) { b.Profile.Action = domain.ChatActionTextSubmission },
		"wrong source":   func(b *PiFreshInvestigationBinding) { b.Profile.SourceKind = domain.TaskWorkSourceFollowup },
		"wrong scope":    func(b *PiFreshInvestigationBinding) { b.Profile.Operations = b.Profile.Operations[1:] },
		"extra operation": func(b *PiFreshInvestigationBinding) {
			b.Profile.Operations = append(slices.Clone(b.Profile.Operations), "report_work")
		},
		"unordered prefix": func(b *PiFreshInvestigationBinding) {
			b.Profile.Operations[0], b.Profile.Operations[1] = b.Profile.Operations[1], b.Profile.Operations[0]
		},
	} {
		t.Run(label, func(t *testing.T) {
			b := binding
			b.Profile.Operations = slices.Clone(profile.Operations)
			edit(&b)
			if e, err := adapter.NewExecutor(ctx, selected, b); e != nil || err == nil {
				t.Fatal("mismatched launch binding constructed an executor")
			}
		})
	}
	extra := strings.Replace(body, `"run_id":`, `"task_id":"unbound","run_id":`, 1)
	write(path, extra, 0600)
	bad := binding
	bad.ConfigSHA256 = sha256.Sum256([]byte(extra))
	if e, err := adapter.NewExecutor(ctx, selected, bad); e != nil || err == nil {
		t.Fatal("digest did not fence the exact private config schema")
	}
	write(path, body, 0600)
	construct := func() Executor {
		t.Helper()
		e, err := adapter.NewExecutor(ctx, selected, binding)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	runtimeDir := t.TempDir()
	if err := os.Chmod(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	request := ExecuteRequest{RunID: binding.RunID, RuntimeDir: runtimeDir, CWD: root, Prompt: "synthetic"}
	assertDenied := func(e Executor, r ExecuteRequest) {
		t.Helper()
		if got := e.Execute(ctx, r, nil); got.Started || got.Outcome != OutcomeUnknown {
			t.Fatal("mismatched or repeated invocation did not fail closed", got.Outcome)
		}
	}
	e := construct()
	wrongRun := request
	wrongRun.RunID = "turn-other"
	assertDenied(e, wrongRun)
	assertDenied(e, request)
	if _, err := os.Stat(filepath.Join(request.RuntimeDir, wrongRun.RunID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("run mismatch was checked after process preparation")
	}
	e = construct()
	replacement := path + ".next"
	write(replacement, body, 0600)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	assertDenied(e, request)

	e = construct()
	binding.Profile.Operations[0] = "read_channel" // The executor owns a frozen snapshot.
	got := e.Execute(ctx, request, nil)
	binding.Profile.Operations[0] = "read_investigation_channel"
	if !got.Started || got.Outcome != OutcomeSucceeded || got.NativeRef != nil {
		t.Fatal("synthetic command did not execute as a fresh-only turn", got.Outcome)
	}
	argv, err := os.ReadFile(got.StderrPath)
	want := []string{"--offline", "--mode", "json", "-p", "--no-extensions", "--no-mcp", "--no-builtin-tools", "--no-skills",
		"--no-prompt-templates", "--no-context-files", "--no-approve", "--session-dir", filepath.Join(request.RuntimeDir, request.RunID),
		"--session-id", "turn-owner", "--no-session", "--tools", "read_investigation_channel,read_investigation_artifact,read_work_report,read_work_inputs,ack_work_input,hand_back_work_input,submit_result", "--extension", adapter.qualifiedBridge(),
		"--no-themes", "--exclude-tools", "read,bash,powershell,edit,write,grep,find,ls,codemode,tool_search,mcp__*",
		"--system-prompt", "你只可使用本轮注册的调查工具，在绑定范围内读取材料或执行工具定义的平台操作。", "--append-system-prompt", "不得向宿主机请求文件、命令或网络工具。"}
	if err != nil || string(argv) != strings.Join(want, "\n")+"\n" {
		t.Fatalf("incorrect fresh-only argv: %q (%v)", argv, err)
	}
	assertDenied(e, request)
	e = construct()
	write(dependency, "different dependency bytes", 0600)
	if adapter.Supports(selected, profile) {
		t.Fatal("bridge with changed dependency qualified")
	}
	assertDenied(e, request)
}

// The new startup flags and ready requirement must not leak into the two
// other qualified member-tool invocations, including C's text fallback.
func TestPiFreshInvestigationNeighborArgv(t *testing.T) {
	for _, name := range []string{"strict initial", "fresh text"} {
		t.Run(name, func(t *testing.T) {
			const head = `{"type":"session","id":"run1","version":3,"cwd":"/fixture"}`
			stream := strings.Join([]string{head, executionStart, executionAssistant("stop"), executionSettled, ""}, "\n")
			selected, req, extensions := typedWriteTestChild(t, stream)
			// Only the synthetic executable records argv; it neither starts Pi nor reads ambient resources.
			if err := os.WriteFile(selected.Path, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >&2\nexec /bin/cat frames.jsonl\n"), 0700); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(req.CWD, "issued.json")
			wire := `{"endpoint":"http://127.0.0.1:1/api/v1/member-tools","token":"synthetic-capability"}` + "\n"
			if name == "strict initial" {
				wire = strings.TrimSuffix(wire, "}\n") + `,"investigation_tools":["read_investigation_channel","decide_dispatch"]}` + "\n"
			}
			if err := os.WriteFile(config, []byte(wire), 0600); err != nil {
				t.Fatal(err)
			}
			issued := sha256.Sum256([]byte(wire))
			want := []string{"--offline", "--mode", "json", "-p", "--no-extensions", "--no-mcp", "--no-builtin-tools", "--no-skills",
				"--no-prompt-templates", "--no-context-files", "--no-approve", "--session-dir", filepath.Join(req.RuntimeDir, req.RunID), "--session-id", req.RunID}
			var e Executor
			var err error
			if name == "strict initial" {
				entry := filepath.Join(extensions, "index.ts")
				names := []string{ReadInvestigationChannelTool, "decide_dispatch"}
				e, err = NewPiInvestigationAdapter(entry, NativeMemberToolConfig{Path: config, SHA256: issued}, names).NewExecutor(selected, 0)
				want = append(want, "--tools", "read_investigation_channel,decide_dispatch", "--extension", entry,
					"--no-themes", "--exclude-tools", "read,bash,powershell,edit,write,grep,find,ls,codemode,tool_search,mcp__*",
					"--system-prompt", "你只可使用本轮获准的只读调查工具。", "--append-system-prompt", "不得向宿主机请求文件、命令或网络工具。")
			} else {
				entry := filepath.Join(extensions, "fresh-task-work.ts")
				e, err = NewPiFreshTaskWorkAdapter(entry, 0).NewExecutor(t.Context(), selected, FreshTaskWorkBinding{
					ConfigPath: config, ConfigSHA256: issued, RunID: req.RunID, Operations: PiFreshTaskWorkToolNames(),
				})
				want = append(want, "--tools", "read_work_inputs,ack_work_input,hand_back_work_input,submit_result", "--extension", entry)
			}
			if err != nil {
				t.Fatal(err)
			}
			got := e.Execute(t.Context(), req, nil)
			if got.Outcome != OutcomeSucceeded || !got.Accepted {
				t.Fatalf("baseline required the typed investigation proof: %+v", got)
			}
			if argv, err := os.ReadFile(got.StderrPath); err != nil || string(argv) != strings.Join(want, "\n")+"\n" {
				t.Fatalf("baseline argv changed: %q (%v)", argv, err)
			}
		})
	}
}

const investigationTestToken = "fixture-capability-NOT-A-REAL-CREDENTIAL-937519" // Same as the SDK fixture.

func investigationTestConfig(t *testing.T, runID string, profile PiFreshInvestigationProfile) ([]byte, [sha256.Size]byte) {
	t.Helper()
	config, digest, err := PiFreshInvestigationConfig("http://127.0.0.1:7310/api/v1/member-tools", investigationTestToken, runID, profile)
	if err != nil {
		t.Fatal(err)
	}
	return config, digest
}

func investigationTestHeader(runID string) string {
	return fmt.Sprintf(`{"type":"session","id":%q,"version":3,"cwd":"/fixture","timestamp":"2026-11-18T00:00:00.000Z"}`, runID)
}

func investigationTestReady(runID string, kind domain.TaskWorkSourceKind, digest [sha256.Size]byte) string {
	return fmt.Sprintf(`{"type":"entry_appended","entry":{"type":"custom","customType":"agent_workspace_investigation_ready_v1","data":{"run_id":%q,"source_kind":%q,"config_sha256":"%x"},"id":"abcdef01","parentId":null,"timestamp":"2026-11-18T00:00:00.000Z"}}`, runID, kind, digest)
}

func TestPiFreshInvestigationStartupProof(t *testing.T) {
	const runID = "run1"
	profile := PiFreshInvestigationProfile{Action: domain.ChatActionReadOnlyInvestigation,
		SourceKind: domain.TaskWorkSourceDelegation, Operations: []string{"report_work"}}
	_, digest := investigationTestConfig(t, runID, profile)
	wider := profile
	wider.Operations = []string{ReadInvestigationChannelTool, "report_work"}
	_, otherDigest := investigationTestConfig(t, runID, wider)
	header, ack := investigationTestHeader(runID), investigationTestReady(runID, profile.SourceKind, digest)
	// These bytes would be the real frame outside native quoting/nesting.
	forgedText := fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":%s}]}}`, memberToolJSON(t, ack+"\n"))
	forgedArgs := typedWriteTestStart("report_work", typedWriteTestCall, `{"evidence":`+ack+`}`)
	stop := strings.Join([]string{executionStart, executionAssistant("stop"), executionSettled}, "\n")
	join := func(lines ...string) string { return strings.Join(append(lines, ""), "\n") }
	for _, c := range []struct {
		name, stream string
		accepted     bool // A late invalid frame can't retract a proved live acceptance.
	}{
		{"no output", "", false},
		{"only session", join(header), false},
		{"no agent start", join(header, ack), false},
		{"missing readiness", join(header, stop), false},
		{"wrong native run", join(investigationTestHeader("other-run"), ack, stop), false},
		{"wrong ack run", join(header, investigationTestReady("other-run", profile.SourceKind, digest), stop), false},
		{"wrong source kind", join(header, investigationTestReady(runID, domain.TaskWorkSourceReport, digest), stop), false},
		{"different profile digest", join(header, investigationTestReady(runID, profile.SourceKind, otherDigest), stop), false},
		{"malformed JSON", join(header, ack[:len(ack)-1], ack, stop), false},
		{"forbidden extra payload", join(header, strings.Replace(ack, `"data":{`, `"data":{"diagnostic":"extra",`, 1), stop), false},
		{"duplicate decoded key", join(header, strings.Replace(ack, `"run_id":`, `"\u0072un_id":"run1","run_id":`, 1), stop), false},
		{"aliased data key", join(header, strings.Replace(ack, `"source_kind"`, `"Source_kind"`, 1), stop), false},
		{"before header", join(ack, header, stop), false},
		{"after agent start", join(header, executionStart, ack, stop), false},
		{"duplicate before agent start", join(header, ack, ack, stop), false},
		{"duplicate after terminal", join(header, ack, stop, ack), true},
		{"partial duplicate at EOF", join(header, ack, stop) + ack, true},
		{"ack in model text", join(header, executionStart, forgedText, executionSettled), false},
		{"ack in tool arguments", join(header, forgedArgs, stop), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, req := investigationTestExecutor(t, profile, runID, c.stream)
			var events []Event
			got := e.Execute(t.Context(), req, func(event Event) error { events = append(events, event); return nil })
			var wantEvents []Event
			var wantID string
			if c.accepted {
				wantID = runID
				wantEvents = []Event{{Kind: SessionEvent, NativeSessionID: runID}, {Kind: AcceptedEvent, NativeSessionID: runID}}
			}
			if got.Outcome != OutcomeUnknown || !got.Started || !got.DirectChildReaped || got.ExitCode != 0 || got.Accepted != c.accepted ||
				got.Terminal != "" || got.NativeSessionID != wantID || got.OutputComplete || got.DeliveryFailed || !slices.Equal(events, wantEvents) {
				t.Fatalf("unproved startup promoted native output: %+v, %v", got, events)
			}
			if retry := e.Execute(t.Context(), req, nil); retry.Started || retry.Outcome != OutcomeUnknown {
				t.Fatalf("unproved startup replayed: %+v", retry)
			}
		})
	}
	t.Run("start failure is not startup ambiguity", func(t *testing.T) {
		e, req := investigationTestExecutor(t, profile, runID, "")
		if err := os.Remove(filepath.Join(req.CWD, "fixture-pi")); err != nil {
			t.Fatal(err)
		}
		got := e.Execute(t.Context(), req, func(event Event) error { t.Errorf("unexpected event: %v", event); return nil })
		if got.Outcome != OutcomeNotStarted || got.Started || got.Accepted || got.DirectChildReaped || got.OutputComplete || got.ExitCode != -1 {
			t.Fatalf("definitive Start failure became ambiguous: %+v", got)
		}
	})
}

func TestPiFreshInvestigationWriteUncertainty(t *testing.T) {
	for _, tc := range []struct {
		kind  domain.TaskWorkSourceKind
		tools []string
		op    string
	}{
		{domain.TaskWorkSourceFollowup, PiFreshTaskWorkToolNames(), "ack_work_input"},
		{domain.TaskWorkSourceDelegation, []string{"report_work"}, "report_work"},
		{domain.TaskWorkSourceReport, append([]string{"read_work_report"}, PiFreshTaskWorkToolNames()...), "submit_result"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			const runID = "run1"
			profile := PiFreshInvestigationProfile{Action: domain.ChatActionReadOnlyInvestigation, SourceKind: tc.kind, Operations: tc.tools}
			_, digest := investigationTestConfig(t, runID, profile)
			start := typedWriteTestStart(tc.op, typedWriteTestCall, `{}`)
			marker := func(role string) string {
				identity := memberToolJSON(t, typedWriteTestDetails(role, runID, tc.op))
				return typedWriteTestEnd(tc.op, `{"`+typedWriteTestField+`":`+identity+`}`)
			}
			pair := func(end string) string { return start + "\n" + end }
			checked := fmt.Sprintf(`{"type":"tool_execution_end","toolCallId":%q,"toolName":%q,"result":{"content":[],"isError":false},"isError":false}`, typedWriteTestCall, tc.op)
			for _, c := range []struct {
				name, frames     string
				want             Outcome
				complete, marked bool
			}{
				{"checked", pair(checked), OutcomeSucceeded, true, false},
				{"unknown", pair(marker(string(tc.kind))), OutcomeUnknown, true, true},
				{"foreign source", pair(marker("foreign")), OutcomeUnknown, false, false},
				{"no start", checked, OutcomeUnknown, false, false},
				{"no end", start, OutcomeUnknown, false, false},
				{"aliased envelope", pair(strings.Replace(checked, `"toolCallId"`, `"ToolCallId"`, 1)), OutcomeUnknown, false, false},
				{"contradictory flags", pair(strings.TrimSuffix(checked, `false}`) + `true}`), OutcomeUnknown, false, false},
			} {
				t.Run(c.name, func(t *testing.T) {
					stream := strings.Join([]string{investigationTestHeader(runID), investigationTestReady(runID, tc.kind, digest),
						executionStart, c.frames, executionAssistant("stop"), executionSettled, ""}, "\n")
					checkInvestigationWriteReceipt(t, profile, runID, stream, c.want, c.complete, c.marked)
				})
			}
		})
	}
}

func investigationTestExecutor(t *testing.T, profile PiFreshInvestigationProfile, runID, stream string) (Executor, ExecuteRequest) {
	t.Helper()
	selected, req, bridges := typedWriteTestChild(t, stream)
	req.RunID = runID
	config, digest := investigationTestConfig(t, runID, profile)
	path := filepath.Join(req.CWD, "issued.json")
	if err := os.WriteFile(path, config, 0600); err != nil {
		t.Fatal(err)
	}
	a := NewPiFreshInvestigationAdapter(filepath.Join(bridges, "fresh-investigation-work.ts"), 0)
	e, err := a.NewExecutor(t.Context(), selected, PiFreshInvestigationBinding{ConfigPath: path, ConfigSHA256: digest, RunID: runID, Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	return e, req
}

func checkInvestigationWriteReceipt(t *testing.T, profile PiFreshInvestigationProfile, runID, stream string, want Outcome, complete, marked bool) {
	t.Helper()
	e, req := investigationTestExecutor(t, profile, runID, stream)
	var events []Event
	got := e.Execute(t.Context(), req, func(event Event) error { events = append(events, event); return nil })
	if got.Outcome != want || got.Terminal != OutcomeSucceeded || got.OutputComplete != complete ||
		!got.Accepted || !got.Started || !got.DirectChildReaped || got.ExitCode != 0 || got.NativeRef != nil ||
		got.NativeSessionID != runID || got.LogsTruncated || got.DeliveryFailed || strings.Contains(got.Reason, "写入") != marked {
		t.Fatalf("typed role lost write uncertainty or native terminal: %+v", got)
	}
	if len(events) < 4 || events[0].Kind != SessionEvent || events[1].Kind != AcceptedEvent ||
		events[len(events)-2].Kind != ResponseEvent || events[len(events)-1].Kind != TerminalEvent {
		t.Fatalf("lost validated lifecycle: %v", events)
	}
	if log, err := os.ReadFile(got.StdoutPath); err != nil || string(log) != stream {
		t.Fatal("private stdout did not retain the exact native stream", err)
	}
}

// Replay real SDK print output through the exact delegation adapter; edit only
// the native lifecycle frame, leaving the tool result and terminal intact.
func TestPiFreshInvestigationSDKPrintLifecycle(t *testing.T) {
	dir := os.Getenv("TEST_PI_TYPED_PRINT_FIXTURES")
	if dir == "" {
		t.Skip("set TEST_PI_TYPED_PRINT_FIXTURES to the captured Pi JSON print fixtures")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "delegation-unknown.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	const op, runID = "report_work", "turn-investigation"
	profile := PiFreshInvestigationProfile{Action: domain.ChatActionReadOnlyInvestigation,
		SourceKind: domain.TaskWorkSourceDelegation, Operations: []string{op}}
	stream := string(raw)
	checkInvestigationWriteReceipt(t, profile, runID, stream, OutcomeUnknown, true, true)
	for _, kind := range []string{"tool_execution_start", "tool_execution_end"} {
		t.Run("missing "+kind, func(t *testing.T) {
			checkInvestigationWriteReceipt(t, profile, runID, dropTypedPrintFrame(t, stream, kind, op), OutcomeUnknown, false, false)
		})
	}
}

func TestPiFreshInvestigationProfiles(t *testing.T) {
	owner := []string{"read_work_inputs", "ack_work_input", "hand_back_work_input", "submit_result"}
	for _, c := range []struct {
		kind  domain.TaskWorkSourceKind
		names []string
		allow bool
	}{
		{domain.TaskWorkSourceFollowup, append([]string{"read_investigation_channel"}, owner...), true},
		{domain.TaskWorkSourceDelegation, []string{"read_investigation_artifact", "report_work"}, true},
		{domain.TaskWorkSourceDelegation, []string{"report_work"}, true},
		{domain.TaskWorkSourceReport, append([]string{"read_work_report"}, owner...), true},
		{domain.TaskWorkSourceFollowup, []string{"read_investigation_channel", "submit_result"}, false},
		{domain.TaskWorkSourceDelegation, []string{"read_investigation_artifact", "report_work", "submit_result"}, false},
		{domain.TaskWorkSourceReport, []string{"read_work_report"}, false},
		{domain.TaskWorkSourceReport, append([]string{"read_work_report", "read_work_report"}, owner...), false},
		{domain.TaskWorkSourceHandback, owner, false},
	} {
		p := PiFreshInvestigationProfile{Action: domain.ChatActionReadOnlyInvestigation, SourceKind: c.kind, Operations: c.names}
		_, _, err := PiFreshInvestigationConfig("http://[::1]:7310/api/v1/member-tools", "fixture", "turn-fixture", p)
		if (err == nil) != c.allow {
			t.Errorf("unexpected profile decision for %s %v: %v", c.kind, c.names, err)
		}
	}
}

func TestPiFreshInvestigationSDKStartupProof(t *testing.T) {
	dir := os.Getenv("TEST_PI_TYPED_PRINT_FIXTURES")
	if dir == "" {
		t.Skip("set TEST_PI_TYPED_PRINT_FIXTURES to the captured Pi JSON print fixtures")
	}
	read := func(name string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(dir, name+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	const runID = "turn-investigation"
	profile := PiFreshInvestigationProfile{Action: domain.ChatActionReadOnlyInvestigation,
		SourceKind: domain.TaskWorkSourceDelegation, Operations: []string{"report_work"}}
	checkInvestigationWriteReceipt(t, profile, runID, read("delegation-checked"), OutcomeSucceeded, true, false)
	t.Run("late duplicate keeps proved write uncertainty", func(t *testing.T) {
		stream := read("delegation-unknown")
		lines := strings.SplitN(stream, "\n", 3)
		if len(lines) < 3 || !strings.Contains(lines[1], `"customType":"agent_workspace_investigation_ready_v1"`) {
			t.Fatal("fixture is missing the startup proof")
		}
		want := stream + lines[1] + "\n"
		e, req := investigationTestExecutor(t, profile, runID, want)
		got := e.Execute(t.Context(), req, func(ev Event) error {
			if ev.Kind != SessionEvent && ev.Kind != AcceptedEvent {
				t.Errorf("late duplicate published %v", ev.Kind)
			}
			return nil
		})
		if got.Outcome != OutcomeUnknown || got.Terminal != "" || got.OutputComplete || !got.Accepted || !strings.Contains(got.Reason, "写入") {
			t.Fatalf("lost write evidence or promoted an invalid execution: %+v", got)
		}
		if raw, err := os.ReadFile(got.StdoutPath); err != nil || string(raw) != want {
			t.Fatal("lost native write identity", err)
		}
	})
}
