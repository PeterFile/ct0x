package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const piSessionTokenOne = "SYNTHETIC-PI-SESSION-TOKEN-ONE-NOT-A-CREDENTIAL"
const piSessionTokenTwo = "SYNTHETIC-PI-SESSION-TOKEN-TWO-NOT-A-CREDENTIAL"

type piSessionFixtureProof struct {
	PID         int      `json:"pid"`
	Argv        []string `json:"argv"`
	TokenSHA    string   `json:"token_sha"`
	ConfigSHA   string   `json:"config_sha"`
	InputSHA    string   `json:"input_sha"`
	SessionFile string   `json:"session_file"`
	Fresh       bool     `json:"fresh"`
}

func piSessionFixtureSetup(t *testing.T, binary string) (Provider, string, ExecuteRequest) {
	t.Helper()
	extension := filepath.Join(piSessionPrivateTemp(t), "trusted-extension.ts")
	if err := os.WriteFile(extension, []byte("export default function () {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	selected := Provider{ID: "selected", Name: Pi, Version: "1.4.0", Path: binary, Installation: Installed}
	request := ExecuteRequest{RunID: "run1", CWD: piSessionPrivateTemp(t), RuntimeDir: piSessionPrivateTemp(t), Prompt: "fixture:fresh", Model: "synthetic/model", Thinking: "max"}
	return selected, extension, request
}

func piSessionFixtureExecutor(t *testing.T, selected Provider, extension, token string, options PiSessionOptions) (Executor, string) {
	t.Helper()
	config := filepath.Join(piSessionPrivateTemp(t), "current-config.json")
	encoded, _ := json.Marshal(map[string]string{"endpoint": "http://127.0.0.1:1/api/v1/member-tools", "token": token})
	if err := os.WriteFile(config, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	e, err := NewPiSessionExecutor(selected, extension, config, []string{"send_message", "create_member", "read_channel"}, options, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return e, config
}

func piSessionReadProof(t *testing.T, request ExecuteRequest) piSessionFixtureProof {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(request.CWD, "fixture-proof.json"))
	if err != nil {
		t.Fatal(err)
	}
	var proof piSessionFixtureProof
	if err := json.Unmarshal(data, &proof); err != nil {
		t.Fatal(err)
	}
	return proof
}

func piSessionAssertSucceeded(t *testing.T, receipt ExecutionReceipt, resumable bool) {
	t.Helper()
	if receipt.Outcome != OutcomeSucceeded || receipt.Terminal != OutcomeSucceeded || !receipt.Started || !receipt.Accepted || !receipt.DirectChildReaped || !receipt.OutputComplete || receipt.DeliveryFailed || receipt.ExitCode != 0 {
		t.Fatalf("native settlement changed: %+v", receipt)
	}
	if got := receipt.NativeRef != nil && receipt.NativeRef.Pi != nil; got != resumable {
		t.Fatalf("resumable=%t, wanted %t: %+v", got, resumable, receipt)
	}
}

func piSessionAssertNoSecret(t *testing.T, receipt ExecutionReceipt, events []Event, tokens ...string) {
	t.Helper()
	encoded, err := json.Marshal(struct {
		Receipt ExecutionReceipt
		Events  []Event
		Ref     *NativeSessionReference
	}{receipt, events, receipt.NativeRef})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{receipt.StdoutPath, receipt.StderrPath} {
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		encoded = append(encoded, data...)
	}
	sessionFile := ""
	if receipt.NativeRef != nil && receipt.NativeRef.Pi != nil {
		sessionFile = receipt.NativeRef.Pi.SessionFile
	} else if receipt.StdoutPath != "" {
		sessionFile = filepath.Join(filepath.Dir(receipt.StdoutPath), "native", "session.jsonl")
	}
	if sessionFile != "" {
		data, err := os.ReadFile(sessionFile)
		if err != nil {
			t.Fatal(err)
		}
		encoded = append(encoded, data...)
	}
	for _, token := range tokens {
		if bytes.Contains(encoded, []byte(token)) {
			t.Fatal("current known synthetic token leaked")
		}
	}
	if bytes.Contains(encoded, []byte("http://127.0.0.1:1/api/v1/member-tools")) || bytes.Contains(encoded, []byte("current-config.json")) || bytes.Contains(encoded, []byte("AGENT_WORKSPACE_MEMBER_TOOL_CONFIG")) {
		t.Fatal("private config entered ref, session, logs or public events")
	}
}

func TestPiSessionExecutorOfflineChild(t *testing.T) {
	binary := piSessionBuildFixture(t)
	t.Run("fresh-resume-new-process-and-config", func(t *testing.T) {
		selected, extension, request := piSessionFixtureSetup(t, binary)
		binding := json.RawMessage(`{"purpose":"fixture","turn":1}`)
		e, configOne := piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{Binding: binding})
		binding[len(binding)-2] = '9' // The constructor must not retain caller-owned bytes.
		var events []Event
		first := e.Execute(context.Background(), request, func(event Event) error { events = append(events, event); return nil })
		piSessionAssertSucceeded(t, first, true)
		if first.NativeSessionID == request.RunID || !piSessionUUID.MatchString(first.NativeSessionID) || !validPiNativeReference(first.NativeRef) || string(first.NativeRef.Binding) != `{"purpose":"fixture","turn":1}` {
			t.Fatal("fresh reference lacks native UUID or frozen binding")
		}
		p := first.NativeRef.Pi
		if p.SessionFile != filepath.Join(request.RuntimeDir, request.RunID, "native", "session.jsonl") || p.LeafID == p.LastCompletedAssistantID {
			t.Fatal("exact file/current leaf evidence missing")
		}
		proofOne := piSessionReadProof(t, request)
		if !proofOne.Fresh || proofOne.TokenSHA != piSessionBytesDigest([]byte(piSessionTokenOne)) || proofOne.InputSHA != piSessionBytesDigest([]byte(request.Prompt)) {
			t.Fatal("fresh child input/config not observed")
		}
		for _, forbidden := range []string{"--session-id", "--no-session", "--continue", "--resume", "--fork"} {
			if slices.Contains(proofOne.Argv, forbidden) {
				t.Fatal("legacy/fallback flag in opt-in invocation")
			}
		}
		for flag, want := range map[string]string{"--session": p.SessionFile, "--session-dir": filepath.Dir(p.SessionFile), "--tools": "read_channel,send_message,create_member", "--extension": extension, "--model": request.Model, "--thinking": "max"} {
			i := slices.Index(proofOne.Argv, flag)
			if i < 0 || i+1 >= len(proofOne.Argv) || proofOne.Argv[i+1] != want {
				t.Fatalf("wrong literal %s argument", flag)
			}
		}
		if p.ToolsProfileSHA256 != piSessionDigest([]string{"create_member", "read_channel", "send_message"}) {
			t.Fatal("tool digest differs from canonical sorted unique names")
		}
		before, err := os.ReadFile(p.SessionFile)
		if err != nil {
			t.Fatal(err)
		}
		ref := cloneNativeSessionReference(first.NativeRef)
		resume, configTwo := piSessionFixtureExecutor(t, selected, extension, piSessionTokenTwo, PiSessionOptions{Resume: ref, Binding: json.RawMessage(`{"purpose":"fixture","turn":2}`)})
		ref.Pi.SessionFile = "/must-not-be-used"
		ref.Binding[1] = 'X'
		request.RunID, request.Prompt = "run2", "fixture:tool-ok"
		events = nil
		second := resume.Execute(context.Background(), request, func(event Event) error { events = append(events, event); return nil })
		piSessionAssertSucceeded(t, second, true)
		proofTwo := piSessionReadProof(t, request)
		if proofTwo.Fresh || proofTwo.PID == proofOne.PID || proofTwo.TokenSHA != piSessionBytesDigest([]byte(piSessionTokenTwo)) || proofTwo.ConfigSHA == proofOne.ConfigSHA || configOne == configTwo || second.NativeSessionID != first.NativeSessionID || second.NativeRef.Pi.SessionFile != p.SessionFile || second.NativeRef.Pi.Inode != p.Inode || second.NativeRef.Pi.LastCompletedAssistantID == p.LastCompletedAssistantID {
			t.Fatal("resume did not use exact history/new child/new config")
		}
		after, err := os.ReadFile(p.SessionFile)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(after, before) || len(after) <= len(before) {
			t.Fatal("resume rewrote historical prefix")
		}
		if second.NativeRef == first.NativeRef || second.NativeRef.Pi == first.NativeRef.Pi || string(first.NativeRef.Binding) != `{"purpose":"fixture","turn":1}` {
			t.Fatal("receipt sidechannel aliases previous receipt")
		}
		public, err := json.Marshal(second)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(public, []byte("session_file")) || bytes.Contains(public, []byte("native_ref")) || bytes.Contains(public, []byte("binding")) {
			t.Fatal("server-only reference changed canonical public receipt JSON")
		}
		piSessionAssertNoSecret(t, first, nil, piSessionTokenOne, piSessionTokenTwo)
		piSessionAssertNoSecret(t, second, events, piSessionTokenOne, piSessionTokenTwo)
	})
	t.Run("capture-after-actual-wait", func(t *testing.T) {
		selected, extension, request := piSessionFixtureSetup(t, binary)
		request.Prompt = "fixture:after-settled"
		e, _ := piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{})
		start := time.Now()
		got := e.Execute(context.Background(), request, nil)
		piSessionAssertSucceeded(t, got, true)
		if time.Since(start) < 150*time.Millisecond {
			t.Fatal("capture returned before child persisted after settled")
		}
	})
	for _, mode := range []string{"tool-ok", "unknown-write", "missing-tool-end", "mismatched-tool-id", "pending-tool", "write-error-file-clean", "mismatched-result", "foreign-tool", "wrong-model", "wrong-thinking", "bad-lf", "replace-file"} {
		t.Run(mode, func(t *testing.T) {
			selected, extension, request := piSessionFixtureSetup(t, binary)
			request.Prompt = "fixture:" + mode
			e, _ := piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{Binding: json.RawMessage(`{"purpose":"fixture"}`)})
			got := e.Execute(context.Background(), request, nil)
			if slices.Contains([]string{"unknown-write", "missing-tool-end", "mismatched-tool-id", "pending-tool", "write-error-file-clean", "mismatched-result", "foreign-tool"}, mode) {
				if got.Outcome != OutcomeUnknown || got.Terminal != OutcomeSucceeded || !got.Started || !got.Accepted || !got.DirectChildReaped || !got.OutputComplete || got.DeliveryFailed || got.ExitCode != 0 || got.NativeRef == nil || got.NativeRef.Pi != nil {
					t.Fatalf("unknown external execution must preserve native evidence without qualifying success: %+v", got)
				}
			} else {
				piSessionAssertSucceeded(t, got, mode == "tool-ok")
			}
			if got.NativeRef == nil || string(got.NativeRef.Binding) != `{"purpose":"fixture"}` {
				t.Fatal("capture failure lost current binding-only barrier")
			}
			piSessionAssertNoSecret(t, got, nil, piSessionTokenOne)
		})
	}
	for _, mode := range []string{"wrong-header", "eof", "output-tail", "queue", "inherited-pipe", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			selected, extension, request := piSessionFixtureSetup(t, binary)
			request.Prompt = "fixture:" + mode
			e, _ := piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			got := e.Execute(ctx, request, func(event Event) error {
				if mode == "cancel" && event.Kind == AcceptedEvent {
					cancel()
				}
				return nil
			})
			if got.Outcome != OutcomeUnknown || !got.Started || !got.DirectChildReaped || got.NativeRef != nil {
				t.Fatalf("unsafe native lifecycle qualified: %+v", got)
			}
			if mode == "inherited-pipe" {
				piSessionWaitFile(t, filepath.Join(request.CWD, "pipe-descendant-exited"))
			}
		})
	}
	t.Run("callback-join-before-capture", func(t *testing.T) {
		selected, extension, request := piSessionFixtureSetup(t, binary)
		e, _ := piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{})
		ctx, cancel := context.WithCancel(context.Background())
		ready, release, done := make(chan struct{}), make(chan struct{}), make(chan ExecutionReceipt, 1)
		joined := make(chan struct{})
		defer func() {
			cancel()
			select {
			case <-release:
			default:
				close(release)
			}
			<-joined
		}()
		go func() {
			defer close(joined)
			done <- e.Execute(ctx, request, func(event Event) error {
				if event.Kind == TerminalEvent {
					close(ready)
					<-release
				}
				return nil
			})
		}()
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("terminal callback not reached")
		}
		piSessionWaitFile(t, filepath.Join(request.CWD, "fixture-finished"))
		select {
		case <-done:
			t.Fatal("returned with callback still running")
		default:
		}
		file := filepath.Join(request.RuntimeDir, request.RunID, "native", "session.jsonl")
		info, err := os.Stat(file)
		if err != nil {
			close(release)
			t.Fatal(err)
		}
		if err := os.Truncate(file, info.Size()-1); err != nil {
			close(release)
			t.Fatal(err)
		}
		close(release)
		select {
		case got := <-done:
			piSessionAssertSucceeded(t, got, false)
		case <-time.After(5 * time.Second):
			t.Fatal("callback join did not finish")
		}
	})
	t.Run("callback-failure-no-ref", func(t *testing.T) {
		selected, extension, request := piSessionFixtureSetup(t, binary)
		e, _ := piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{})
		got := e.Execute(context.Background(), request, func(event Event) error {
			if event.Kind == TerminalEvent {
				return errors.New("fixture delivery failure")
			}
			return nil
		})
		if !got.DeliveryFailed || got.Outcome != OutcomeUnknown || !got.DirectChildReaped || got.NativeRef != nil {
			t.Fatalf("delivery unknown qualified: %+v", got)
		}
	})
	t.Run("resume-prefix-drift-after-start", func(t *testing.T) {
		selected, extension, request := piSessionFixtureSetup(t, binary)
		e, _ := piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{})
		first := e.Execute(context.Background(), request, nil)
		piSessionAssertSucceeded(t, first, true)
		e, _ = piSessionFixtureExecutor(t, selected, extension, piSessionTokenTwo, PiSessionOptions{Resume: first.NativeRef})
		request.RunID, request.Prompt = "run2", "fixture:prefix-drift"
		got := e.Execute(context.Background(), request, nil)
		piSessionAssertSucceeded(t, got, false)
	})
	t.Run("resume-id-not-platform-run-id", func(t *testing.T) {
		selected, extension, request := piSessionFixtureSetup(t, binary)
		e, _ := piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{})
		first := e.Execute(context.Background(), request, nil)
		piSessionAssertSucceeded(t, first, true)
		e, _ = piSessionFixtureExecutor(t, selected, extension, piSessionTokenTwo, PiSessionOptions{Resume: first.NativeRef})
		request.RunID, request.Prompt = "run2", "fixture:wrong-native-id"
		got := e.Execute(context.Background(), request, nil)
		if got.Outcome != OutcomeUnknown || !got.DirectChildReaped || got.NativeRef != nil {
			t.Fatalf("changed native ID accepted: %+v", got)
		}
	})
}

func TestPiSessionExecutorResumeRejectsBeforePrompt(t *testing.T) {
	binary := piSessionBuildFixture(t)
	for _, name := range []string{"model", "thinking", "cwd", "runtime", "implicit-model", "implicit-thinking", "cli", "tools", "missing", "empty", "bad-line", "no-lf", "header", "orphan", "identity", "same-stat-hash", "public", "symlink"} {
		t.Run(name, func(t *testing.T) {
			selected, extension, request := piSessionFixtureSetup(t, binary)
			e, _ := piSessionFixtureExecutor(t, selected, extension, piSessionTokenOne, PiSessionOptions{})
			first := e.Execute(context.Background(), request, nil)
			piSessionAssertSucceeded(t, first, true)
			ref := cloneNativeSessionReference(first.NativeRef)
			path := ref.Pi.SessionFile
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			invocations, err := os.ReadFile(filepath.Join(request.CWD, "fixture-invocations"))
			if err != nil {
				t.Fatal(err)
			}
			originalCWD := request.CWD
			switch name {
			case "model":
				request.Model = "synthetic/other"
			case "thinking":
				request.Thinking = "off"
			case "cwd":
				request.CWD = piSessionPrivateTemp(t)
			case "runtime":
				request.RuntimeDir = piSessionPrivateTemp(t)
			case "implicit-model":
				request.Model = ""
			case "implicit-thinking":
				request.Thinking = ""
			case "cli":
				ref.Pi.CLIPath += ".other"
			case "tools":
				ref.Pi.ToolsProfileSHA256 = strings.Repeat("0", 64)
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "empty":
				if err := os.Truncate(path, 0); err != nil {
					t.Fatal(err)
				}
			case "bad-line":
				if err := os.WriteFile(path, append(bytes.Clone(original), []byte("{broken}\n")...), 0600); err != nil {
					t.Fatal(err)
				}
			case "no-lf":
				if err := os.Truncate(path, info.Size()-1); err != nil {
					t.Fatal(err)
				}
			case "header":
				data := bytes.Replace(original, []byte(`"version":3`), []byte(`"version":2`), 1)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "orphan":
				lines := bytes.Split(original, []byte{'\n'})
				var entry map[string]any
				if err := json.Unmarshal(lines[2], &entry); err != nil {
					t.Fatal(err)
				}
				entry["parentId"] = "missing"
				lines[2], _ = json.Marshal(entry)
				if err := os.WriteFile(path, bytes.Join(lines, []byte{'\n'}), 0600); err != nil {
					t.Fatal(err)
				}
			case "identity":
				if err := os.WriteFile(path+".new", original, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".new", path); err != nil {
					t.Fatal(err)
				}
			case "same-stat-hash":
				data := bytes.Replace(original, []byte("offline complete"), []byte("OFFLINE COMPLETE"), 1)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, time.Now(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".target", path); err != nil {
					t.Fatal(err)
				}
			}
			before, beforeErr := os.ReadFile(path)
			e, _ = piSessionFixtureExecutor(t, selected, extension, piSessionTokenTwo, PiSessionOptions{Resume: ref, Binding: json.RawMessage(`{"purpose":"fixture"}`)})
			request.RunID, request.Prompt = "run2", "must-not-be-sent"
			callbacks := 0
			got := e.Execute(context.Background(), request, func(Event) error { callbacks++; return nil })
			if got.Started || got.Accepted || got.Outcome != OutcomeNotStarted || got.NativeSessionID != "" || got.DirectChildReaped || callbacks != 0 || got.NativeRef == nil || got.NativeRef.Pi != nil || got.Reason != errPiSession.Error() {
				t.Fatalf("resume failure launched/fell back/changed evidence: %+v", got)
			}
			after, afterErr := os.ReadFile(path)
			if !bytes.Equal(before, after) || (beforeErr == nil) != (afterErr == nil) {
				t.Fatal("failed preflight repaired or initialized native file")
			}
			current, err := os.ReadFile(filepath.Join(originalCWD, "fixture-invocations"))
			if err != nil || !bytes.Equal(current, invocations) {
				t.Fatal("rejected resume invoked a child")
			}
			if _, err := os.Stat(filepath.Join(request.RuntimeDir, request.RunID, "native")); !os.IsNotExist(err) {
				t.Fatal("rejected resume created a fresh native fallback")
			}
		})
	}
}

func piSessionWaitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("offline child marker not observed")
}

func TestPiSessionConstructorRejectsUnqualifiedReference(t *testing.T) {
	selected, extension, config := memberToolFiles(t, `: > "$0.invoked"; exit 99`)
	before := selected
	for _, options := range []PiSessionOptions{
		{Resume: &NativeSessionReference{Version: NativeSessionReferenceVersion}},
		{Resume: &NativeSessionReference{Version: NativeSessionReferenceVersion, Pi: &PiNativeSessionReference{}}},
		{Resume: &NativeSessionReference{Version: NativeSessionReferenceVersion, Codex: &CodexNativeSessionReference{}, Pi: &PiNativeSessionReference{}}},
		{Binding: json.RawMessage(`{"a":1,"a":2}`)}, {Binding: json.RawMessage(`{"a":1.0}`)}, {Binding: json.RawMessage(` {"a":1}`)}, {Binding: json.RawMessage(`[]`)},
	} {
		got, err := NewPiSessionExecutor(selected, extension, config, []string{"read_channel"}, options, 0)
		if got != nil || err == nil || err.Error() != errPiSession.Error() {
			t.Fatalf("unqualified options accepted: %T %v", got, err)
		}
	}
	if !reflect.DeepEqual(selected, before) {
		t.Fatal("constructor changed selected metadata")
	}
	if _, err := os.Stat(selected.Path + ".invoked"); !os.IsNotExist(err) {
		t.Fatal("constructor probed child")
	}
	if _, err := NewPiSessionExecutor(selected, extension, config, []string{"read_channel"}, PiSessionOptions{}, 0); err != nil {
		t.Fatal(err)
	}
	selected.Path = filepath.Dir(selected.Path) + "/./" + filepath.Base(selected.Path)
	if _, err := NewPiMemberToolExecutorWithTools(selected, extension, config, []string{"read_channel"}, 0); err != nil {
		t.Fatal("legacy path gate changed")
	}
	if e, err := NewPiSessionExecutor(selected, extension, config, []string{"read_channel"}, PiSessionOptions{}, 0); e != nil || err != errPiSession {
		t.Fatal("opt-in accepted a CLI path that cannot produce a canonical reference")
	}
}

func TestPiSessionLegacyConstructorStillUsesRunID(t *testing.T) {
	body := "set -eu\nprintf '%s\\n' \"$@\" > argv\n/bin/cat >/dev/null\n" + executionHeader + executionEmit(executionStart, executionAssistant("stop"), executionSettled)
	e, request, _, _ := memberToolFixture(t, body)
	got := e.Execute(context.Background(), request, nil)
	piSessionAssertSucceeded(t, got, false)
	if got.NativeSessionID != request.RunID || got.NativeRef != nil {
		t.Fatal("default constructor gained native capture or UUID requirement")
	}
	args, err := os.ReadFile(filepath.Join(request.CWD, "argv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--session-dir\n"+filepath.Join(request.RuntimeDir, request.RunID)+"\n--session-id\n"+request.RunID+"\n") || strings.Contains(string(args), "--session\n") {
		t.Fatal("legacy session flags changed")
	}
}
