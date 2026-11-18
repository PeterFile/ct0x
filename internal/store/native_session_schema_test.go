package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/provider"
)

func TestNativeSessionV23HistoricalBarriersAndImmutableIdentity(t *testing.T) {
	nativeV23Migrations(t)
	f := nativeV22Fixture(t)
	var existing int
	if err := f.s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='native_run_bindings'").Scan(&existing); err != nil {
		t.Fatal(err)
	}
	if existing != 0 {
		t.Fatal("V22 fixture already contains native associations")
	}
	d := queueDelivery(t, f.s, f.groupID, f.a, "topic")
	old, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: d.ID, MaxActive: 8})
	if err != nil {
		t.Fatal(err)
	}
	r := normalizeTurnReceipt(old, queueReceipt(provider.OutcomeSucceeded))
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	// An old canonical receipt is fixture data; the new Apply hook is never
	// disabled or taught to skip missing schema in production.
	if _, err := f.s.db.Exec("UPDATE turns SET state='succeeded',native_session_id=?,accepted_at=?,receipt_json=? WHERE id=?",
		r.NativeSessionID, time.Now().UTC().Format(timestampLayout), string(data), old.ID); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.s = queueHandle(t, f.path, false)
	messageSequenceLedger(t, f.s, 23)
	messageSequencePreserved(t, f.s, before)
	seq, purpose, binding, ref := nativeStored(t, f.s, old.ID)
	if seq < 1 || purpose != "unknown" || binding.Valid || ref.Valid || queueReceiptJSON(t, f.s, TurnClaim{GroupID: old.GroupID, TurnID: old.ID}) != string(data) {
		t.Fatal("historical migration invented qualifications or changed canonical receipt bytes")
	}
	for _, query := range []string{
		"UPDATE native_run_bindings SET purpose='generic' WHERE turn_id='" + old.ID + "'",
		"DELETE FROM native_run_bindings WHERE turn_id='" + old.ID + "'",
		"INSERT OR REPLACE INTO native_run_bindings(turn_id,group_id,member_id,conversation_id,purpose,binding_version) SELECT turn_id,group_id,member_id,conversation_id,'generic',0 FROM native_run_bindings",
	} {
		if _, err := f.s.db.Exec(query); err == nil {
			t.Fatal("immutable association was modified/replaced/deleted")
		}
	}
	currentTurn, next, nextBinding := nativeClaim(t, f, f.groupID, f.a, "topic")
	// The composite FK rejects cross-group identity even with a real Turn ID.
	if _, err := f.s.db.Exec(`INSERT INTO native_run_bindings(turn_id,group_id,member_id,conversation_id,purpose,binding_version)
	 VALUES(?,?,?,?,?,0)`, currentTurn.ID, f.otherID, f.foreign, currentTurn.ConversationID, "unknown"); err == nil {
		t.Fatal("association accepted an unbound Turn identity")
	}
	got, err := f.s.SelectNativeSession(ctx, next, nextBinding)
	if err != nil || got.Reference != nil || got.SourceTurnID != old.ID || !strings.Contains(got.Reason, "用途") {
		t.Fatalf("legacy unknown barrier did not fence resume: %+v, %v", got, err)
	}
	nativeApply(t, f.s, next, nextBinding, provider.OutcomeSucceeded, "reference")
	newSeq, _, _, _ := nativeStored(t, f.s, next.Claim.TurnID)
	if newSeq <= seq {
		t.Fatal("new receipts did not follow durable historical barriers")
	}
	var bad int
	if err := f.s.db.QueryRow("SELECT count(*) FROM pragma_foreign_key_check").Scan(&bad); err != nil || bad != 0 {
		t.Fatalf("foreign key check: %d, %v", bad, err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.s = queueHandle(t, f.path, false)
	if seq2, _, _, _ := nativeStored(t, f.s, next.Claim.TurnID); seq2 != newSeq {
		t.Fatal("seq changed after reopen")
	}
}

func TestNativeSessionResumedFromIsSameScopeAndLatest(t *testing.T) {
	f := nativeFixture(t)
	firstTurn, first, firstBinding := nativeClaim(t, f, f.groupID, f.a, "topic")
	nativeApply(t, f.s, first, firstBinding, provider.OutcomeSucceeded, "reference")
	_, current, binding := nativeClaim(t, f, f.groupID, f.a, "topic")
	selected, err := f.s.SelectNativeSession(ctx, current, binding)
	if err != nil || selected.Reference == nil || selected.SourceTurnID != firstTurn.ID {
		t.Fatalf("source selection: %+v, %v", selected, err)
	}
	binding.ResumedFromTurnID = selected.SourceTurnID
	r := queueReceipt(provider.OutcomeSucceeded)
	r.NativeRef = nativeReference(t, binding)
	r.NativeRef.Codex.NativeTurnID = "second-native-turn"
	r.NativeRef.Codex.HistoryTurns = 2
	r.NativeRef.Codex.HistoryItems = 4
	if _, err := f.s.ApplyTurnReceipt(ctx, current.Claim, r); err != nil {
		t.Fatal(err)
	}
	var resumed string
	if err := f.s.db.QueryRow("SELECT resumed_from_turn_id FROM native_run_bindings WHERE turn_id=?", binding.TurnID).Scan(&resumed); err != nil || resumed != firstTurn.ID {
		t.Fatalf("actual resumed-from link lost: %s, %v", resumed, err)
	}
	_, next, nextBinding := nativeClaim(t, f, f.groupID, f.a, "topic")
	nextBinding.ResumedFromTurnID = firstTurn.ID // Not latest; no stale-source adoption.
	nativeApply(t, f.s, next, nextBinding, provider.OutcomeSucceeded, "reference")
	_, _, _, ref := nativeStored(t, f.s, nextBinding.TurnID)
	if ref.Valid {
		t.Fatal("non-latest resumed-from source granted eligibility")
	}
}
