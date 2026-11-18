package provider

import (
	"context"
	"crypto/sha256"
	"errors"
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
printf '%s\n' '{"type":"session","id":"turn-owner","version":3,"cwd":"/fixture"}' '{"type":"agent_start"}' '{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[]}}' '{"type":"agent_settled","aborted":false}'
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
	request := ExecuteRequest{RunID: binding.RunID, RuntimeDir: t.TempDir(), CWD: root, Prompt: "synthetic"}
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
		"--session-id", "turn-owner", "--no-session", "--tools", "read_investigation_channel,read_investigation_artifact,read_work_report,read_work_inputs,ack_work_input,hand_back_work_input,submit_result", "--extension", adapter.qualifiedBridge()}
	if err != nil || string(argv) != strings.Join(want, "\n")+"\n" {
		t.Fatal("incorrect fresh-only argv", err)
	}
	assertDenied(e, request)
	e = construct()
	write(dependency, "different dependency bytes", 0600)
	if adapter.Supports(selected, profile) {
		t.Fatal("bridge with changed dependency qualified")
	}
	assertDenied(e, request)
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
