package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func nativeApply(t *testing.T, s *Store, scope MemberToolScope, binding NativeRunBinding, outcome provider.Outcome, metadata string) provider.ExecutionReceipt {
	t.Helper()
	r := queueReceipt(outcome)
	if metadata != "none" {
		r.NativeRef = nativeReference(t, binding)
		if metadata == "binding" {
			r.NativeRef.Codex = nil
		}
	}
	if _, err := s.ApplyTurnReceipt(ctx, scope.Claim, r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNativeSessionLatestGuardAndIndependentReopen(t *testing.T) {
	for _, latest := range []string{"eligible", "none", "binding", "failed", "mismatch", "damaged-receipt", "malformed-bound-ref"} {
		t.Run(latest, func(t *testing.T) {
			f := nativeFixture(t)
			_, first, firstBinding := nativeClaim(t, f, f.groupID, f.a, "topic")
			nativeApply(t, f.s, first, firstBinding, provider.OutcomeSucceeded, "reference")
			secondTurn, second, secondBinding := nativeClaim(t, f, f.groupID, f.a, "topic")
			outcome, metadata := provider.OutcomeSucceeded, "reference"
			if latest == "none" || latest == "binding" {
				metadata = latest
			}
			if latest == "failed" {
				outcome = provider.OutcomeFailed
			}
			if latest == "mismatch" {
				secondBinding.Profile.Model = "different-historical-model"
			}
			if latest == "malformed-bound-ref" {
				r := queueReceipt(provider.OutcomeSucceeded)
				r.NativeRef = nativeReference(t, secondBinding)
				r.NativeRef.Binding = bytes.Replace(r.NativeRef.Binding, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1)
				if _, err := f.s.ApplyTurnReceipt(ctx, second.Claim, r); err != nil {
					t.Fatal(err)
				}
			} else {
				nativeApply(t, f.s, second, secondBinding, outcome, metadata)
			}
			if latest == "damaged-receipt" {
				data := queueReceiptJSON(t, f.s, second.Claim)
				data = strings.Replace(data, `"reason":"执行完成"`, `"reason":"PRIVATE-noncanonical-reason"`, 1)
				if _, err := f.s.db.Exec("UPDATE turns SET receipt_json=? WHERE id=?", data, secondTurn.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.s.Close(); err != nil {
				t.Fatal(err)
			}
			f.s = queueHandle(t, f.path, false)
			_, current, binding := nativeClaim(t, f, f.groupID, f.a, "topic")
			got, err := f.s.SelectNativeSession(ctx, current, binding)
			if err != nil || got.SourceTurnID != secondTurn.ID || (got.Reference != nil) != (latest == "eligible") || (got.Reason == "") != (latest == "eligible") {
				t.Fatalf("latest %s: source=%s ref=%t reason=%q err=%v", latest, got.SourceTurnID, got.Reference != nil, got.Reason, err)
			}
			if got.Reference != nil {
				got.Reference.Binding[0] = '!'
				got.Reference.Codex.NativeThreadID = "caller-mutation"
				again, err := f.s.SelectNativeSession(ctx, current, binding)
				if err != nil || again.Reference == nil || again.Reference.Codex.NativeThreadID != "native-process-session" || again.Reference.Binding[0] != '{' {
					t.Fatal("selector reference aliases previous returned value or persisted evidence")
				}
			}
		})
	}
}

func TestNativeSessionReceiptReplayCannotReplaceOrLateUpgrade(t *testing.T) {
	for _, metadata := range []string{"none", "binding", "reference"} {
		t.Run(metadata, func(t *testing.T) {
			f := nativeFixture(t)
			turn, scope, binding := nativeClaim(t, f, f.groupID, f.a, "topic")
			r := nativeApply(t, f.s, scope, binding, provider.OutcomeSucceeded, metadata)
			seq, purpose, savedBinding, savedRef := nativeStored(t, f.s, turn.ID)
			data := queueReceiptJSON(t, f.s, scope.Claim)
			if _, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, r); err != nil {
				t.Fatalf("identical replay: %v", err)
			}
			oldStyle := r
			oldStyle.NativeRef = nil
			if _, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, oldStyle); err != nil {
				t.Fatalf("old caller removed/rejected existing reference: %v", err)
			}
			late := r
			late.NativeRef = nativeReference(t, binding)
			if metadata == "reference" {
				late.NativeRef.Codex.HistorySHA256 = strings.Repeat("b", 64)
			}
			if _, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, late); !errors.Is(err, ErrConflict) {
				t.Fatalf("late/replaced reference accepted: %v", err)
			}
			if seq2, purpose2, binding2, ref2 := nativeStored(t, f.s, turn.ID); seq2 != seq || purpose2 != purpose || binding2 != savedBinding || ref2 != savedRef || queueReceiptJSON(t, f.s, scope.Claim) != data {
				t.Fatal("replay changed canonical receipt or immutable association")
			}
			if metadata == "reference" {
				// Mutating caller-owned raw/aggregate metadata after Apply cannot
				// affect SQLite or another returned selector snapshot.
				r.NativeRef.Codex.NativeThreadID = "mutated-after-apply"
				r.NativeRef.Binding[0] = '!'
				_, _, _, after := nativeStored(t, f.s, turn.ID)
				if after != savedRef {
					t.Fatal("Store retained caller-owned reference memory")
				}
			}
		})
	}
}

func TestNativeSessionApplyRollbackAndRetry(t *testing.T) {
	for _, fail := range []string{"association-insert", "commit"} {
		t.Run(fail, func(t *testing.T) {
			f := nativeFixture(t)
			turn, scope, binding := nativeClaim(t, f, f.groupID, f.a, "topic")
			queueCandidate(t, f.s, scope.Claim, "final answer", provider.OutcomeSucceeded)
			before := queueTurn(t, f.s, scope.Claim)
			trigger := `CREATE TRIGGER native_test_fail BEFORE INSERT ON native_run_bindings BEGIN SELECT RAISE(ABORT,'PRIVATE-SQL'); END`
			if fail == "commit" {
				if _, err := f.s.db.Exec("CREATE TABLE native_test_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)"); err != nil {
					t.Fatal(err)
				}
				trigger = `CREATE TRIGGER native_test_fail AFTER INSERT ON native_run_bindings BEGIN INSERT INTO native_test_guard VALUES('PRIVATE-SQL'); END`
			}
			if _, err := f.s.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			r := queueReceipt(provider.OutcomeSucceeded)
			r.NativeRef = nativeReference(t, binding)
			input, _ := json.Marshal(r.NativeRef)
			if got, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, r); err == nil || got.ID != "" || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("failed association reported success/leaked data: %+v, %v", got, err)
			}
			var rows int
			if err := f.s.db.QueryRow("SELECT count(*) FROM native_run_bindings").Scan(&rows); err != nil || rows != 0 || queueReceiptJSON(t, f.s, scope.Claim) != "" || !reflect.DeepEqual(before, queueTurn(t, f.s, scope.Claim)) {
				t.Fatal("association failure partially committed receipt/turn")
			}
			queuePrivateAndHeld(t, f, scope.Claim)
			if _, err := f.s.db.Exec("DROP TRIGGER native_test_fail"); err != nil {
				t.Fatal(err)
			}
			if got, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, r); err != nil || got.ReplyMessageID == "" || got.State != domain.TurnStateSucceeded {
				t.Fatalf("retry after whole-transaction rollback: %+v, %v", got, err)
			}
			_, _, _, ref := nativeStored(t, f.s, turn.ID)
			if !ref.Valid {
				t.Fatal("retry lost reference")
			}
			after, _ := json.Marshal(r.NativeRef)
			if !bytes.Equal(input, after) {
				t.Fatal("Store mutated the caller's side channel")
			}
			var locks int
			if err := f.s.db.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", turn.ID).Scan(&locks); err != nil || locks != 0 {
				t.Fatal("trustworthy terminal receipt did not release original resource claim")
			}
		})
	}
}

func TestNativeSessionUnknownRecoveryKeepsBarrierAndFencesClaim(t *testing.T) {
	f := nativeFixture(t)
	turn, scope, binding := nativeClaim(t, f, f.groupID, f.a, "topic")
	r := nativeApply(t, f.s, scope, binding, provider.OutcomeUnknown, "reference")
	seq, _, originalBinding, ref := nativeStored(t, f.s, turn.ID)
	if ref.Valid {
		t.Fatal("unknown outcome acquired resume eligibility")
	}
	queuePrivateAndHeld(t, f, scope.Claim)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.s = queueHandle(t, f.path, false)
	if count, err := f.s.RecoverInFlight(ctx); err != nil || count != 1 {
		t.Fatalf("recover: %d, %v", count, err)
	}
	if got, err := f.s.SelectNativeSession(ctx, scope, binding); !errors.Is(err, ErrMemberToolDenied) || got.Reference != nil || got.Reason == "" {
		t.Fatalf("old claim could select a session after recovery: %+v, %v", got, err)
	}
	r.Outcome, r.Terminal = provider.OutcomeSucceeded, provider.OutcomeSucceeded
	if _, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, r); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("old receipt claim survived recovery: %v", err)
	}
	currentTurn := queueTurn(t, f.s, scope.Claim)
	scope.Claim.Token = currentTurn.ClaimToken
	if got, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, r); err != nil || got.State != domain.TurnStateSucceeded {
		t.Fatalf("real reconciliation could not settle unknown: %+v, %v", got, err)
	}
	if seq2, _, binding2, ref2 := nativeStored(t, f.s, turn.ID); seq2 != seq || binding2 != originalBinding || ref2.Valid {
		t.Fatal("reconciliation upgraded/replaced first no-reference barrier")
	}
	_, next, nextBinding := nativeClaim(t, f, f.groupID, f.a, "topic")
	if got, err := f.s.SelectNativeSession(ctx, next, nextBinding); err != nil || got.Reference != nil || got.SourceTurnID != turn.ID || got.Reason == "" {
		t.Fatalf("reconciled no-reference barrier was skipped: %+v, %v", got, err)
	}
}

func TestNativeSessionCurrentClaimAndCrossGroupFence(t *testing.T) {
	f := nativeFixture(t)
	_, source, sourceBinding := nativeClaim(t, f, f.otherID, f.foreign, "topic")
	nativeApply(t, f.s, source, sourceBinding, provider.OutcomeSucceeded, "reference")
	turn, current, binding := nativeClaim(t, f, f.groupID, f.a, "topic")
	if got, err := f.s.SelectNativeSession(ctx, current, binding); err != nil || got.Reference != nil || got.SourceTurnID != "" || got.Reason == "" {
		t.Fatalf("cross-group history leaked: %+v, %v", got, err)
	}
	for _, change := range []func(*MemberToolScope, *NativeRunBinding){
		func(s *MemberToolScope, _ *NativeRunBinding) { s.Claim.Token = "old-claim" },
		func(s *MemberToolScope, _ *NativeRunBinding) { s.Claim.GroupID = f.otherID },
		func(_ *MemberToolScope, b *NativeRunBinding) { b.GroupID = f.otherID; b.MemberID = f.foreign },
		func(_ *MemberToolScope, b *NativeRunBinding) { b.Profile.ResourceKeys = []string{} },
		func(_ *MemberToolScope, b *NativeRunBinding) { b.Profile.Model = "stale-model" },
	} {
		s, b := current, binding
		change(&s, &b)
		got, err := f.s.SelectNativeSession(ctx, s, b)
		if !errors.Is(err, ErrMemberToolDenied) || got.Reference != nil || got.Reason == "" {
			t.Fatalf("forged/stale current binding selected/fell back: %+v, %v", got, err)
		}
	}
	if _, err := f.s.db.Exec("UPDATE members SET workspace='/changed-workspace' WHERE id=?", f.a); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.SelectNativeSession(ctx, current, binding); !errors.Is(err, ErrMemberToolDenied) || got.Reference != nil {
		t.Fatalf("selector ignored changed current workspace: %+v, %v", got, err)
	}
	if _, err := f.s.db.Exec("UPDATE members SET workspace=? WHERE id=?", binding.Profile.ConfiguredWorkspace, f.a); err != nil {
		t.Fatal(err)
	}
	queueEvent(t, f.s, current.Claim, provider.SessionEvent, "", "")
	if got, err := f.s.SelectNativeSession(ctx, current, binding); !errors.Is(err, ErrMemberToolDenied) || got.Reference != nil {
		t.Fatalf("non-fresh current turn selected a source: %+v, %v", got, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got, err := f.s.SelectNativeSession(cancelled, current, binding); !errors.Is(err, context.Canceled) || got.Reference != nil {
		t.Fatalf("lost selector cancellation: %+v, %v", got, err)
	}
	if stored := queueTurn(t, f.s, current.Claim); stored.ID != turn.ID || stored.State != domain.TurnStateStarting {
		t.Fatal("selector changed current execution lifecycle")
	}
}
