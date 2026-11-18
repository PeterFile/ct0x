package store

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func nativeFixture(t *testing.T) messageFixture {
	t.Helper()
	f := setupMessages(t)
	nativeInstallSchema(t, f.s)
	return f
}

func nativeInstallSchema(t testing.TB, s *Store) {
	t.Helper()
	var found int
	if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='native_run_bindings'").Scan(&found); err != nil {
		t.Fatal(err)
	}
	if found != 1 {
		t.Fatal("native fixture requires the formally registered V23 schema")
	}
}

func nativeClaim(t *testing.T, f messageFixture, groupID, memberID, topic string) (domain.Turn, MemberToolScope, NativeRunBinding) {
	t.Helper()
	d := queueDelivery(t, f.s, groupID, memberID, topic)
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: groupID, DeliveryID: d.ID, ResourceKeys: []string{"shared"}, MaxActive: 8})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	b := nativeBindingFixture(turn)
	b.TopicID = topic
	return turn, scope, b
}

func nativeBindingFixture(turn domain.Turn) NativeRunBinding {
	return NativeRunBinding{
		Version: 1, GroupID: turn.GroupID, MemberID: turn.MemberID, ConversationID: turn.ConversationID, TurnID: turn.ID,
		Purpose: "generic", Profile: NativeRunProfile{
			ProviderID: "pi", Model: "model",
			Adapter: "codex-app-server-text-member/v1", Protocol: "codex-app-server/v2@0.166.0", CLIPath: "/test/bin/codex", CLIVersion: "0.166.0",
			CWD: "/test/workspace", ConfiguredWorkspace: "/shared/workspace", WorkspaceIdentity: "private:fixture-node:inode-123",
			ResourceKeys: []string{"shared"}, Tools: []string{"list_members", "read_channel"}, MaxGroupMembers: 8,
			NativePolicy: NativeTextPolicy{Sandbox: "read-only", ApprovalPolicy: "never"},
		},
	}
}

func nativeReference(t testing.TB, binding NativeRunBinding) *provider.NativeSessionReference {
	t.Helper()
	data, err := CanonicalNativeRunBinding(binding)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := NativeToolsProfileSHA256(binding.Profile.Tools)
	if err != nil {
		t.Fatal(err)
	}
	p := binding.Profile
	return &provider.NativeSessionReference{Version: 1, Binding: data, Codex: &provider.CodexNativeSessionReference{
		Adapter: p.Adapter, Protocol: p.Protocol, CLIPath: p.CLIPath, CLIVersion: p.CLIVersion, Provider: "openai", Model: p.Model,
		ReasoningEffort: "high", CWD: p.CWD, ToolsProfileSHA256: tools, NativeHome: "/test/native-home",
		NativeThreadID: "native-process-session", NativeTurnID: "completed-native-turn", HistorySHA256: strings.Repeat("a", 64), HistoryTurns: 1, HistoryItems: 2,
	}}
}

func nativeStored(t *testing.T, s *Store, turnID string) (int64, string, sql.NullString, sql.NullString) {
	t.Helper()
	var seq int64
	var purpose string
	var binding, ref sql.NullString
	if err := s.db.QueryRow("SELECT seq,purpose,binding_json,native_ref_json FROM native_run_bindings WHERE turn_id=?", turnID).Scan(&seq, &purpose, &binding, &ref); err != nil {
		t.Fatalf("missing first-receipt association: %v", err)
	}
	return seq, purpose, binding, ref
}

func TestNativeSessionFirstReceiptPersistence(t *testing.T) {
	for _, metadata := range []string{"none", "binding", "reference"} {
		t.Run(metadata, func(t *testing.T) {
			f := nativeFixture(t)
			turn, scope, binding := nativeClaim(t, f, f.groupID, f.a, "native-topic")
			r := queueReceipt(provider.OutcomeSucceeded)
			if metadata != "none" {
				r.NativeRef = nativeReference(t, binding)
				if metadata == "binding" {
					r.NativeRef.Codex = nil
				}
			}
			want, err := json.Marshal(normalizeTurnReceipt(turn, r))
			if err != nil {
				t.Fatal(err)
			}
			if got, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, r); err != nil || got.State != domain.TurnStateSucceeded {
				t.Fatalf("Apply: %+v, %v", got, err)
			}
			_, purpose, savedBinding, ref := nativeStored(t, f.s, turn.ID)
			if ref.Valid != (metadata == "reference") || savedBinding.Valid != (metadata != "none") || (purpose == "generic") != (metadata != "none") {
				t.Fatalf("association: purpose=%s, binding=%t, reference=%t", purpose, savedBinding.Valid, ref.Valid)
			}
			if data := queueReceiptJSON(t, f.s, scope.Claim); data != string(want) || strings.Contains(data, "binding") {
				t.Fatalf("legacy canonical bytes changed: %s", data)
			}
			if err := f.s.Close(); err != nil {
				t.Fatal(err)
			}
			f.s = queueHandle(t, f.path, false)
			_, _, reopenedBinding, reopenedRef := nativeStored(t, f.s, turn.ID)
			if reopenedBinding != savedBinding || reopenedRef != ref {
				t.Fatal("association changed after actual SQLite close/reopen")
			}
		})
	}
}

// Both before/after use the same real SQLite fixture and time only Apply. Setup
// does not run a CLI; both versions exercise the same V23 schema and Apply fixture.
func BenchmarkNativeApplyTurnReceipt(b *testing.B) {
	for _, metadata := range []bool{false, true} {
		name := "no_metadata"
		if metadata {
			name = "bound_reference"
		}
		b.Run(name, func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "bench.sqlite"))
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			nativeInstallSchema(b, s)
			g, err := s.CreateGroup(ctx, CreateGroupInput{Name: "benchmark"})
			if err != nil {
				b.Fatal(err)
			}
			m, err := s.CreateMember(ctx, CreateMemberInput{GroupID: g.ID, Name: "member", Role: "member", ProviderID: "pi", Model: "model"})
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				message, err := s.CreateMessage(ctx, MessageRequest{GroupID: g.ID, ActorID: "user", Content: "new independent input"}, []string{m.ID})
				if err != nil {
					b.Fatal(err)
				}
				turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: g.ID, DeliveryID: message.Deliveries[0].ID, ResourceKeys: []string{"shared"}, MaxActive: 1})
				if err != nil {
					b.Fatal(err)
				}
				r := queueReceipt(provider.OutcomeSucceeded)
				if metadata {
					binding := nativeBindingFixture(turn)
					binding.Profile.ConfiguredWorkspace = m.Workspace
					r.NativeRef = nativeReference(b, binding)
				}
				claim := TurnClaim{GroupID: g.ID, TurnID: turn.ID, Token: turn.ClaimToken}
				b.StartTimer()
				if _, err := s.ApplyTurnReceipt(ctx, claim, r); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
