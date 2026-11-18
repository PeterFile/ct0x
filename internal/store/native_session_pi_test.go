package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

const nativePiTestID = "019f0000-0000-7000-8000-000000000001"

func nativePiBinding(turn domain.Turn) NativeRunBinding {
	b := nativeBindingFixture(turn)
	b.Profile.Model, b.Profile.Thinking = "synthetic/model", "max"
	b.Profile.Adapter, b.Profile.Protocol = "pi-json-text-member-session/v1", "pi-json-session/v3@1.4.0"
	b.Profile.CLIPath, b.Profile.CLIVersion = "/test/bin/pi", "1.4.0"
	b.Profile.NativePolicy = NativeTextPolicy{Sandbox: "tools-only", ApprovalPolicy: "never"}
	return b
}

func nativePiReference(t testing.TB, b NativeRunBinding) *provider.NativeSessionReference {
	t.Helper()
	binding, err := CanonicalNativeRunBinding(b)
	if err != nil {
		t.Fatalf("Pi binding rejected: %v", err)
	}
	tools, err := NativeToolsProfileSHA256(b.Profile.Tools)
	if err != nil {
		t.Fatal(err)
	}
	p := b.Profile
	return &provider.NativeSessionReference{Version: 1, Binding: binding, Pi: &provider.PiNativeSessionReference{
		Adapter: p.Adapter, Protocol: p.Protocol, CLIPath: p.CLIPath, CLIVersion: p.CLIVersion,
		Model: p.Model, Thinking: p.Thinking, CWD: p.CWD, ToolsProfileSHA256: tools,
		NativeSessionID: nativePiTestID, SessionFile: "/test/runs/source/native/session.jsonl",
		Device: 1, Inode: 123, Size: 2048, MTimeNS: 1000, FileSHA256: strings.Repeat("a", 64),
		LeafID: "metadata-1", LastCompletedAssistantID: "assistant-1",
	}}
}

func nativePiReceipt(t testing.TB, b NativeRunBinding) provider.ExecutionReceipt {
	t.Helper()
	r := queueReceipt(provider.OutcomeSucceeded)
	r.NativeSessionID, r.NativeRef = nativePiTestID, nativePiReference(t, b)
	return r
}

func nativePiNextReference(t testing.TB, b NativeRunBinding) *provider.NativeSessionReference {
	t.Helper()
	r := nativePiReference(t, b)
	r.Pi.Size, r.Pi.MTimeNS, r.Pi.FileSHA256 = 3072, 1001, strings.Repeat("b", 64)
	r.Pi.LeafID, r.Pi.LastCompletedAssistantID = "metadata-2", "assistant-2"
	return r
}

func nativePiMember(t *testing.T, f messageFixture) {
	t.Helper()
	if _, err := f.s.db.Exec("UPDATE members SET model='synthetic/model' WHERE id=?", f.a); err != nil {
		t.Fatal(err)
	}
}

func TestNativeSessionPiRoundTripAndExactProgression(t *testing.T) {
	f := nativeFixture(t)
	nativePiMember(t, f)
	firstTurn, first, _ := nativeClaim(t, f, f.groupID, f.a, "pi-topic")
	firstBinding := nativePiBinding(firstTurn)
	firstBinding.TopicID = first.TopicID
	r := nativePiReceipt(t, firstBinding)
	input, _ := json.Marshal(r.NativeRef)
	canonical, _ := json.Marshal(normalizeTurnReceipt(firstTurn, r))
	if got, err := f.s.ApplyTurnReceipt(ctx, first.Claim, r); err != nil || got.State != domain.TurnStateSucceeded {
		t.Fatalf("Pi Apply: %+v, %v", got, err)
	}
	_, purpose, savedBinding, savedRef := nativeStored(t, f.s, firstTurn.ID)
	if purpose != "generic" || !savedBinding.Valid || !savedRef.Valid || strings.Contains(savedRef.String, firstTurn.ClaimToken) {
		t.Fatal("successful Pi receipt did not persist a non-authorizing reference")
	}
	if queueReceiptJSON(t, f.s, first.Claim) != string(canonical) {
		t.Fatal("Pi side channel changed legacy canonical receipt bytes")
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.s = queueHandle(t, f.path, false)
	_, _, reopenedBinding, reopenedRef := nativeStored(t, f.s, firstTurn.ID)
	if reopenedBinding != savedBinding || reopenedRef != savedRef {
		t.Fatal("Pi metadata did not survive actual close/reopen")
	}
	secondTurn, second, _ := nativeClaim(t, f, f.groupID, f.a, "pi-topic")
	b := nativePiBinding(secondTurn)
	b.TopicID = second.TopicID
	selected, err := f.s.SelectNativeSession(ctx, second, b)
	if err != nil || selected.Reference == nil || selected.Reference.Pi == nil || selected.SourceTurnID != firstTurn.ID || selected.Reason != "" {
		t.Fatalf("Pi source: %+v, %v", selected, err)
	}
	selected.Reference.Pi.SessionFile = "/caller/mutation"
	selected.Reference.Binding[0] = '!'
	again, err := f.s.SelectNativeSession(ctx, second, b)
	if err != nil || again.Reference == nil || again.Reference.Pi.SessionFile != r.NativeRef.Pi.SessionFile || again.Reference.Binding[0] != '{' {
		t.Fatal("Pi selection aliases mutable caller metadata")
	}
	b.ResumedFromTurnID = firstTurn.ID
	next := nativePiReceipt(t, b)
	next.NativeRef = nativePiNextReference(t, b)
	if _, err := f.s.ApplyTurnReceipt(ctx, second.Claim, next); err != nil {
		t.Fatal(err)
	}
	var resumed string
	if err := f.s.db.QueryRow("SELECT resumed_from_turn_id FROM native_run_bindings WHERE turn_id=?", secondTurn.ID).Scan(&resumed); err != nil || resumed != firstTurn.ID {
		t.Fatalf("Pi actual resume link: %q, %v", resumed, err)
	}
	if _, _, _, ref := nativeStored(t, f.s, secondTurn.ID); !ref.Valid {
		t.Fatal("same-file Pi append lost resume eligibility")
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, second.Claim, next); err != nil {
		t.Fatalf("identical Pi replay: %v", err)
	}
	late := next
	late.NativeRef = nativePiNextReference(t, b)
	late.NativeRef.Pi.FileSHA256 = strings.Repeat("c", 64)
	if _, err := f.s.ApplyTurnReceipt(ctx, second.Claim, late); !errors.Is(err, ErrConflict) {
		t.Fatalf("Pi replacement reference accepted: %v", err)
	}
	lastTurn, last, _ := nativeClaim(t, f, f.groupID, f.a, "pi-topic")
	lastBinding := nativePiBinding(lastTurn)
	lastBinding.TopicID = last.TopicID
	latest, err := f.s.SelectNativeSession(ctx, last, lastBinding)
	if err != nil || latest.Reference == nil || latest.SourceTurnID != secondTurn.ID || latest.Reference.Pi.LeafID != "metadata-2" {
		t.Fatalf("Pi latest successful append: %+v, %v", latest, err)
	}
	after, _ := json.Marshal(r.NativeRef)
	if !bytes.Equal(input, after) {
		t.Fatal("Store mutated input Pi reference")
	}
}

func TestNativeSessionPiProfileAndBoundedMetadata(t *testing.T) {
	b := nativePiBinding(domain.Turn{ID: "turn", GroupID: "group", MemberID: "member", ConversationID: "conversation"})
	good := nativePiReference(t, b)
	if !validNativeReference(good, b, nativePiTestID) {
		t.Fatal("complete Pi metadata rejected")
	}
	for name, change := range map[string]func(*provider.NativeSessionReference){
		"version":     func(r *provider.NativeSessionReference) { r.Version++ },
		"mixed":       func(r *provider.NativeSessionReference) { r.Codex = &provider.CodexNativeSessionReference{} },
		"adapter":     func(r *provider.NativeSessionReference) { r.Pi.Adapter = "other" },
		"protocol":    func(r *provider.NativeSessionReference) { r.Pi.Protocol = "pi-json-session/v2" },
		"cli-path":    func(r *provider.NativeSessionReference) { r.Pi.CLIPath = "/other/pi" },
		"cli-version": func(r *provider.NativeSessionReference) { r.Pi.CLIVersion = "1.4.1" },
		"model":       func(r *provider.NativeSessionReference) { r.Pi.Model = "other/model" },
		"thinking":    func(r *provider.NativeSessionReference) { r.Pi.Thinking = "high" },
		"cwd":         func(r *provider.NativeSessionReference) { r.Pi.CWD = "/other" },
		"tools":       func(r *provider.NativeSessionReference) { r.Pi.ToolsProfileSHA256 = strings.Repeat("b", 64) },
		"id": func(r *provider.NativeSessionReference) {
			r.Pi.NativeSessionID = "019f0000-0000-7000-8000-000000000002"
		},
		"file-relative": func(r *provider.NativeSessionReference) { r.Pi.SessionFile = "relative.jsonl" },
		"file-unclean":  func(r *provider.NativeSessionReference) { r.Pi.SessionFile = "/tmp/../session.jsonl" },
		"file-large":    func(r *provider.NativeSessionReference) { r.Pi.SessionFile = "/" + strings.Repeat("x", 4096) },
		"file-control":  func(r *provider.NativeSessionReference) { r.Pi.SessionFile += "\x00" },
		"device":        func(r *provider.NativeSessionReference) { r.Pi.Device = 0 },
		"inode":         func(r *provider.NativeSessionReference) { r.Pi.Inode = 0 },
		"uint-overflow": func(r *provider.NativeSessionReference) { r.Pi.Inode = 1 << 63 },
		"size-zero":     func(r *provider.NativeSessionReference) { r.Pi.Size = 0 },
		"size-negative": func(r *provider.NativeSessionReference) { r.Pi.Size = -1 },
		"size-large":    func(r *provider.NativeSessionReference) { r.Pi.Size = 32<<20 + 1 },
		"mtime":         func(r *provider.NativeSessionReference) { r.Pi.MTimeNS = 0 },
		"sha":           func(r *provider.NativeSessionReference) { r.Pi.FileSHA256 = strings.Repeat("A", 64) },
		"leaf":          func(r *provider.NativeSessionReference) { r.Pi.LeafID = strings.Repeat("a", 129) },
		"assistant":     func(r *provider.NativeSessionReference) { r.Pi.LastCompletedAssistantID = "bad/id" },
	} {
		t.Run(name, func(t *testing.T) {
			r := nativePiReference(t, b)
			change(r)
			if validNativeReference(r, b, nativePiTestID) {
				t.Fatal("invalid Pi metadata accepted")
			}
		})
	}
	for _, model := range []string{"model", "/model", "provider/", "provider/model*", "provider/model:high", "provider/model space", "p/" + strings.Repeat("x", 255)} {
		p := b.Profile
		p.Model = model
		if _, err := NativeRunProfileSHA256(p); !errors.Is(err, ErrInvalid) {
			t.Fatalf("implicit/fuzzy Pi model accepted: %q, %v", model, err)
		}
	}
	for _, thinking := range []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"} {
		p := b.Profile
		p.Thinking = thinking
		if _, err := NativeRunProfileSHA256(p); err != nil {
			t.Fatalf("frozen Pi thinking rejected: %q, %v", thinking, err)
		}
	}
	for _, change := range []func(*NativeRunProfile){
		func(p *NativeRunProfile) { p.Thinking = "" },
		func(p *NativeRunProfile) { p.Thinking = "auto" },
		func(p *NativeRunProfile) { p.NativePolicy.Sandbox = "read-only" },
		func(p *NativeRunProfile) { p.NativePolicy.ApprovalPolicy = "on-request" },
		func(p *NativeRunProfile) { p.NativePolicy.NetworkAccess = true },
		func(p *NativeRunProfile) { p.NativePolicy.NativeFileAccess = true },
		func(p *NativeRunProfile) { p.NativePolicy.NativeArtifacts = true },
		func(p *NativeRunProfile) { p.Tools = []string{"write_workspace_file"} },
	} {
		p := b.Profile
		change(&p)
		if _, err := NativeRunProfileSHA256(p); !errors.Is(err, ErrInvalid) {
			t.Fatalf("non-text Pi policy accepted: %+v, %v", p, err)
		}
	}
	codex := nativeBindingFixture(domain.Turn{}).Profile
	codex.NativePolicy = b.Profile.NativePolicy
	if _, err := NativeRunProfileSHA256(codex); !errors.Is(err, ErrInvalid) {
		t.Fatal("Pi policy relaxed Codex read-only/never")
	}
}

func TestNativeSessionPiInvalidResumeNeverSkipsBarrier(t *testing.T) {
	for _, invalid := range []string{"file", "id", "device", "inode", "size", "sha", "leaf", "assistant", "not-started", "not-accepted", "mixed"} {
		t.Run(invalid, func(t *testing.T) {
			f := nativeFixture(t)
			nativePiMember(t, f)
			firstTurn, first, _ := nativeClaim(t, f, f.groupID, f.a, "pi-topic")
			firstBinding := nativePiBinding(firstTurn)
			firstBinding.TopicID = first.TopicID
			if _, err := f.s.ApplyTurnReceipt(ctx, first.Claim, nativePiReceipt(t, firstBinding)); err != nil {
				t.Fatal(err)
			}
			turn, scope, _ := nativeClaim(t, f, f.groupID, f.a, "pi-topic")
			b := nativePiBinding(turn)
			b.TopicID, b.ResumedFromTurnID = scope.TopicID, firstTurn.ID
			r := nativePiReceipt(t, b)
			r.NativeRef = nativePiNextReference(t, b)
			switch invalid {
			case "file":
				r.NativeRef.Pi.SessionFile = "/different/native/session.jsonl"
			case "id":
				r.NativeRef.Pi.NativeSessionID = "019f0000-0000-7000-8000-000000000002"
				r.NativeSessionID = r.NativeRef.Pi.NativeSessionID
			case "device":
				r.NativeRef.Pi.Device++
			case "inode":
				r.NativeRef.Pi.Inode++
			case "size":
				r.NativeRef.Pi.Size = 2048
			case "sha":
				r.NativeRef.Pi.FileSHA256 = strings.Repeat("a", 64)
			case "leaf":
				r.NativeRef.Pi.LeafID = "metadata-1"
			case "assistant":
				r.NativeRef.Pi.LastCompletedAssistantID = "assistant-1"
			case "not-started":
				r = provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, NativeRef: r.NativeRef}
			case "not-accepted":
				r.Accepted = false
			case "mixed":
				r.NativeRef.Codex = &provider.CodexNativeSessionReference{}
			}
			settled, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, r)
			if err != nil {
				t.Fatal(err)
			}
			_, _, _, ref := nativeStored(t, f.s, turn.ID)
			var linked bool
			if err := f.s.db.QueryRow("SELECT resumed_from_turn_id IS NOT NULL FROM native_run_bindings WHERE turn_id=?", turn.ID).Scan(&linked); err != nil || linked || ref.Valid {
				t.Fatalf("invalid Pi candidate became an actual resume: linked=%t ref=%t err=%v", linked, ref.Valid, err)
			}
			if settled.State == domain.TurnStateUnknown {
				// Real reconciliation may settle, but may not upgrade the barrier.
				r = nativePiReceipt(t, b)
				r.NativeRef = nativePiNextReference(t, b)
				if _, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, r); err != nil {
					t.Fatal(err)
				}
			}
			nextTurn, next, _ := nativeClaim(t, f, f.groupID, f.a, "pi-topic")
			nextBinding := nativePiBinding(nextTurn)
			nextBinding.TopicID = next.TopicID
			selected, err := f.s.SelectNativeSession(ctx, next, nextBinding)
			if err != nil || selected.Reference != nil || selected.SourceTurnID != turn.ID || selected.Reason == "" {
				t.Fatalf("invalid latest Pi barrier skipped: %+v, %v", selected, err)
			}
		})
	}
}

func TestNativeSessionPiLatestNoRefFailedAndUnknown(t *testing.T) {
	for _, latest := range []string{"binding", "none", "failed", "unknown"} {
		t.Run(latest, func(t *testing.T) {
			f := nativeFixture(t)
			nativePiMember(t, f)
			firstTurn, first, _ := nativeClaim(t, f, f.groupID, f.a, "pi-topic")
			b := nativePiBinding(firstTurn)
			b.TopicID = first.TopicID
			if _, err := f.s.ApplyTurnReceipt(ctx, first.Claim, nativePiReceipt(t, b)); err != nil {
				t.Fatal(err)
			}
			turn, current, _ := nativeClaim(t, f, f.groupID, f.a, "pi-topic")
			b = nativePiBinding(turn)
			b.TopicID = current.TopicID
			r := nativePiReceipt(t, b)
			switch latest {
			case "binding":
				r.NativeRef.Pi = nil
			case "none":
				r.NativeRef = nil
			case "failed":
				r.Outcome, r.Terminal = provider.OutcomeFailed, provider.OutcomeFailed
			case "unknown":
				r.Outcome = provider.OutcomeUnknown
			}
			if _, err := f.s.ApplyTurnReceipt(ctx, current.Claim, r); err != nil {
				t.Fatal(err)
			}
			if latest == "unknown" {
				if held, err := f.s.HasResourceClaim(ctx, "shared"); err != nil || !held {
					t.Fatalf("unknown Pi execution released resources: %t, %v", held, err)
				}
				r.Outcome, r.Terminal = provider.OutcomeSucceeded, provider.OutcomeSucceeded
				if _, err := f.s.ApplyTurnReceipt(ctx, current.Claim, r); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.s.Close(); err != nil {
				t.Fatal(err)
			}
			f.s = queueHandle(t, f.path, false)
			nextTurn, next, _ := nativeClaim(t, f, f.groupID, f.a, "pi-topic")
			b = nativePiBinding(nextTurn)
			b.TopicID = next.TopicID
			selected, err := f.s.SelectNativeSession(ctx, next, b)
			if err != nil || selected.Reference != nil || selected.SourceTurnID != turn.ID || selected.Reason == "" {
				t.Fatalf("latest %s was filtered before qualification: %+v, %v", latest, selected, err)
			}
		})
	}
}
