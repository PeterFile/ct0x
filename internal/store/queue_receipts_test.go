package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func queueReceipt(outcome provider.Outcome) provider.ExecutionReceipt {
	exit := 0
	if outcome == provider.OutcomeFailed {
		exit = 7 // A normalized Codex turn.failed may have a nonzero process exit.
	}
	return provider.ExecutionReceipt{
		Started: true, Accepted: true, Outcome: outcome, Terminal: outcome,
		NativeSessionID: "native-process-session", DirectChildReaped: true, OutputComplete: true,
		ExitCode: exit, StdoutPath: "/internal/run/stdout.jsonl", StderrPath: "/internal/run/stderr.log",
		Reason: "PRIVATE-RAW-ERROR: SELECT credential FROM secret",
	}
}

func queueCandidate(t *testing.T, s *Store, claim TurnClaim, text string, outcome provider.Outcome) {
	t.Helper()
	queueEvent(t, s, claim, provider.AcceptedEvent, "", "")
	queueEvent(t, s, claim, provider.ResponseEvent, text, "")
	queueEvent(t, s, claim, provider.TerminalEvent, "", outcome)
}

func queueReceiptJSON(t *testing.T, s *Store, claim TurnClaim) string {
	t.Helper()
	var data string
	if err := s.db.QueryRow("SELECT receipt_json FROM turns WHERE group_id=? AND id=?", claim.GroupID, claim.TurnID).Scan(&data); err != nil {
		t.Fatal(err)
	}
	return data
}

func queueAudit(t *testing.T, s *Store, claim TurnClaim, outcome provider.Outcome) provider.ExecutionReceipt {
	t.Helper()
	data := queueReceiptJSON(t, s, claim)
	var receipt provider.ExecutionReceipt
	if len(data) > 512<<10 || json.Unmarshal([]byte(data), &receipt) != nil || receipt.Outcome != outcome || receipt.Reason == "" || strings.Contains(data, "PRIVATE") || strings.Contains(data, "SELECT") {
		t.Fatalf("receipt audit is absent, unbounded, unsanitized, or wrong: length=%d, receipt=%+v", len(data), receipt)
	}
	return receipt
}

func TestQueueTrustworthyReceiptsPublishOnceWithBoundIdentity(t *testing.T) {
	for _, outcome := range []provider.Outcome{provider.OutcomeSucceeded, provider.OutcomeFailed, provider.OutcomeAborted, provider.OutcomeTruncated} {
		t.Run(string(outcome), func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			queueEvent(t, f.s, claim, provider.AcceptedEvent, "", "")
			queueEvent(t, f.s, claim, provider.ResponseEvent, "obsolete retry", "")
			queueEvent(t, f.s, claim, provider.ResponseEvent, " final persisted answer ", "")
			queueEvent(t, f.s, claim, provider.TerminalEvent, "", outcome)
			before := queueTurn(t, f.s, claim)
			if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", f.a); err != nil {
				t.Fatal(err)
			}
			receipt := queueReceipt(outcome)
			receipt.LogsTruncated = true // Bounded logs do not contradict complete stream draining.
			turn, err := f.s.ApplyTurnReceipt(ctx, claim, receipt)
			if err != nil || turn.State != domain.TurnState(outcome) || turn.ReplyMessageID == "" || !reflect.DeepEqual(turn.AcceptedAt, before.AcceptedAt) || turn.NativeSessionID != before.NativeSessionID || strings.Contains(turn.Reason, "PRIVATE") {
				t.Fatalf("trustworthy receipt not settled: %+v, %v", turn, err)
			}
			message, err := f.s.GetMessage(ctx, f.groupID, turn.ReplyMessageID)
			if err != nil || message.GroupID != f.groupID || message.SenderID != f.a || message.TopicID != "bound-topic" || message.Content != before.CandidateText || message.Deliveries == nil || len(message.Deliveries) != 0 || message.CreatedAt.Location().String() != "UTC" {
				t.Fatalf("unbound or waking reply: %+v, %v", message, err)
			}
			queueDeliveryState(t, f.s, turn, domain.DeliveryStateAccepted)
			queueCounts(t, f.s, 1, 1, 0)
			audit := queueAudit(t, f.s, claim, outcome)
			if audit.ExitCode != receipt.ExitCode || audit.StdoutPath != receipt.StdoutPath || audit.StderrPath != receipt.StderrPath || !audit.LogsTruncated {
				t.Fatalf("internal evidence discarded: %+v", audit)
			}
			receipt.Reason = "PRIVATE-DIFFERENT-RAW-ERROR"
			again, err := f.s.ApplyTurnReceipt(ctx, claim, receipt)
			if err != nil || !reflect.DeepEqual(again, turn) {
				t.Fatalf("normalized replay changed history: %+v, %v", again, err)
			}
			conflict := receipt
			conflict.ExitCode++
			if got, err := f.s.ApplyTurnReceipt(ctx, claim, conflict); !errors.Is(err, ErrConflict) || got.ID != "" {
				t.Fatalf("conflicting settlement replaced history: %+v, %v", got, err)
			}
			if err := f.s.RecordTurnEvent(ctx, claim, provider.Event{Kind: provider.ResponseEvent, NativeSessionID: receipt.NativeSessionID, Text: "PRIVATE-LATE-REPLY"}); !errors.Is(err, ErrConflict) {
				t.Fatalf("late event rewrote terminal history: %v", err)
			}
			cancelled, err := f.s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID)
			if err != nil || !reflect.DeepEqual(cancelled, turn) {
				t.Fatalf("terminal cancel changed history: %+v, %v", cancelled, err)
			}
			var messages, deliveries int
			if err := f.s.db.QueryRow("SELECT (SELECT count(*) FROM messages),(SELECT count(*) FROM deliveries)").Scan(&messages, &deliveries); err != nil || messages != 2 || deliveries != 1 {
				t.Fatalf("reply duplicated or created notifications: %d, %d, %v", messages, deliveries, err)
			}
			if err := f.s.Close(); err != nil {
				t.Fatal(err)
			}
			f.s = queueHandle(t, f.path, false)
			if got, err := f.s.ApplyTurnReceipt(ctx, claim, receipt); err != nil || !reflect.DeepEqual(got, turn) {
				t.Fatalf("reopen lost settlement replay: %+v, %v", got, err)
			}
			if _, err := f.s.db.Exec("UPDATE members SET status='active' WHERE id=?", f.a); err != nil {
				t.Fatal(err)
			}
			d := queueDelivery(t, f.s, f.groupID, f.a, "bound-topic")
			if _, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: d.ID, ResourceKeys: []string{"shared"}, MaxActive: 1}); err != nil {
				t.Fatalf("settlement retained conversation/member/resource/capacity occupancy: %v", err)
			}
		})
	}
}

func TestQueueReceiptEmptyFinalSnapshotPublishesNothing(t *testing.T) {
	for _, text := range []string{"", " \n\t"} {
		t.Run(fmt.Sprintf("length=%d", len(text)), func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			queueEvent(t, f.s, claim, provider.AcceptedEvent, "", "")
			queueEvent(t, f.s, claim, provider.ResponseEvent, "obsolete nonempty retry", "")
			queueEvent(t, f.s, claim, provider.ResponseEvent, text, "")
			queueEvent(t, f.s, claim, provider.TerminalEvent, "", provider.OutcomeSucceeded)
			turn, err := f.s.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeSucceeded))
			if err != nil || turn.State != domain.TurnStateSucceeded || turn.ReplyMessageID != "" || turn.CandidateText != text {
				t.Fatalf("empty snapshot fabricated a reply: %+v, %v", turn, err)
			}
			var count int
			if err := f.s.db.QueryRow("SELECT count(*) FROM messages").Scan(&count); err != nil || count != 1 {
				t.Fatalf("empty snapshot published obsolete text: %d, %v", count, err)
			}
			queueCounts(t, f.s, 1, 1, 0)
		})
	}
}

func TestQueueUntrustworthyReceiptsRemainUnknownAndHoldEveryClaim(t *testing.T) {
	cases := []struct {
		name   string
		change func(*provider.ExecutionReceipt)
	}{
		{"incomplete-output", func(r *provider.ExecutionReceipt) { r.OutputComplete = false }},
		{"unreaped-child", func(r *provider.ExecutionReceipt) { r.DirectChildReaped = false }},
		{"delivery-failed", func(r *provider.ExecutionReceipt) { r.DeliveryFailed = true }},
		{"unknown-with-terminal", func(r *provider.ExecutionReceipt) { r.Outcome = provider.OutcomeUnknown }},
		{"unknown-false-observations", func(r *provider.ExecutionReceipt) { *r = provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown} }},
		{"exit-zero-only", func(r *provider.ExecutionReceipt) {
			*r = provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeSucceeded, ExitCode: 0}
		}},
		{"reaped-child-only", func(r *provider.ExecutionReceipt) {
			*r = provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeSucceeded, DirectChildReaped: true}
		}},
		{"terminal-only", func(r *provider.ExecutionReceipt) {
			*r = provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded}
		}},
		{"missing-terminal", func(r *provider.ExecutionReceipt) { r.Terminal = "" }},
		{"contradictory-terminal", func(r *provider.ExecutionReceipt) { r.Terminal = provider.OutcomeFailed }},
		{"contradictory-candidate", func(r *provider.ExecutionReceipt) {
			r.Outcome, r.Terminal = provider.OutcomeFailed, provider.OutcomeFailed
		}},
		{"conflicting-session", func(r *provider.ExecutionReceipt) { r.NativeSessionID = "other-process-session" }},
		{"missing-session", func(r *provider.ExecutionReceipt) { r.NativeSessionID = "" }},
		{"invalid-session", func(r *provider.ExecutionReceipt) { r.NativeSessionID = "PRIVATE\nSESSION" }},
		{"not-started", func(r *provider.ExecutionReceipt) { r.Started = false }},
		{"not-accepted", func(r *provider.ExecutionReceipt) { r.Accepted = false }},
		{"unsupported-outcome", func(r *provider.ExecutionReceipt) { r.Outcome = "PRIVATE-OUTCOME" }},
		{"unsupported-terminal", func(r *provider.ExecutionReceipt) { r.Terminal = "PRIVATE-TERMINAL" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			queueCandidate(t, f.s, claim, "private candidate", provider.OutcomeSucceeded)
			before := queueTurn(t, f.s, claim)
			receipt := queueReceipt(provider.OutcomeSucceeded)
			tc.change(&receipt)
			turn, err := f.s.ApplyTurnReceipt(ctx, claim, receipt)
			if err != nil || turn.State != domain.TurnStateUnknown || turn.NativeSessionID != before.NativeSessionID || !reflect.DeepEqual(turn.AcceptedAt, before.AcceptedAt) || turn.CandidateText != before.CandidateText || turn.CandidateTerminal != before.CandidateTerminal || strings.Contains(turn.Reason, "PRIVATE") {
				t.Fatalf("untrustworthy evidence settled or overwrote facts: %+v, %v", turn, err)
			}
			queueAudit(t, f.s, claim, provider.OutcomeUnknown)
			queueDeliveryState(t, f.s, turn, domain.DeliveryStateAccepted)
			queuePrivateAndHeld(t, f, claim)
		})
	}
}

func TestQueueUnknownCannotBeDisprovedByLaterNotStarted(t *testing.T) {
	for _, prior := range []string{"unknown-false", "unknown-started", "recovered", "known-session", "accepted"} {
		t.Run(prior, func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			switch prior {
			case "unknown-false", "unknown-started":
				r := provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Started: prior == "unknown-started"}
				if _, err := f.s.ApplyTurnReceipt(ctx, claim, r); err != nil {
					t.Fatal(err)
				}
			case "recovered":
				if _, err := f.s.RecoverInFlight(ctx); err != nil {
					t.Fatal(err)
				}
				claim.Token = queueTurn(t, f.s, claim).ClaimToken
			case "known-session":
				queueEvent(t, f.s, claim, provider.SessionEvent, "", "")
			case "accepted":
				queueEvent(t, f.s, claim, provider.AcceptedEvent, "", "")
			}
			before := queueTurn(t, f.s, claim)
			if _, err := f.s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID); err != nil {
				t.Fatal(err)
			}
			turn, err := f.s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1})
			if err != nil || turn.State != domain.TurnStateUnknown || turn.NativeSessionID != before.NativeSessionID || !reflect.DeepEqual(turn.AcceptedAt, before.AcceptedAt) || turn.CancelRequestedAt == nil {
				t.Fatalf("new invocation's not_started released prior uncertain work: %+v, %v", turn, err)
			}
			queueAudit(t, f.s, claim, provider.OutcomeUnknown)
			queuePrivateAndHeld(t, f, claim)
		})
	}
}

func TestQueueOriginalNotStartedFinishesWithoutReplyOrRetry(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint(cancel), func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			if cancel {
				if _, err := f.s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID); err != nil {
					t.Fatal(err)
				}
			}
			r := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1, Reason: "PRIVATE-START-ERROR"}
			turn, err := f.s.ApplyTurnReceipt(ctx, claim, r)
			if err != nil || turn.State != domain.TurnStateNotStarted || turn.ReplyMessageID != "" || turn.AcceptedAt != nil || (turn.CancelRequestedAt != nil) != cancel {
				t.Fatalf("proven original not_started not finalized: %+v, %v", turn, err)
			}
			queueDeliveryState(t, f.s, turn, domain.DeliveryStateFailed)
			queueCounts(t, f.s, 1, 1, 0)
			queueAudit(t, f.s, claim, provider.OutcomeNotStarted)
			again, err := f.s.ApplyTurnReceipt(ctx, claim, r)
			if err != nil || !reflect.DeepEqual(again, turn) {
				t.Fatalf("not_started replay changed terminal history: %+v, %v", again, err)
			}
			pending, err := f.s.ListPendingDeliveries(ctx, PendingOptions{})
			if err != nil || len(pending.Items) != 0 {
				t.Fatalf("not_started automatically retried: %+v, %v", pending, err)
			}
			var count int
			if err := f.s.db.QueryRow("SELECT count(*) FROM messages").Scan(&count); err != nil || count != 1 {
				t.Fatalf("not_started fabricated reply: %d, %v", count, err)
			}
		})
	}
}

func TestQueueContradictoryNotStartedRetainsOriginalClaim(t *testing.T) {
	for _, change := range []struct {
		name string
		edit func(*provider.ExecutionReceipt)
	}{
		{"started", func(r *provider.ExecutionReceipt) { r.Started = true }},
		{"accepted", func(r *provider.ExecutionReceipt) { r.Accepted = true }},
		{"native-session", func(r *provider.ExecutionReceipt) { r.NativeSessionID = "native-process-session" }},
		{"terminal", func(r *provider.ExecutionReceipt) { r.Terminal = provider.OutcomeSucceeded }},
		{"delivery-failed", func(r *provider.ExecutionReceipt) { r.DeliveryFailed = true }},
		{"reaped-child", func(r *provider.ExecutionReceipt) { r.DirectChildReaped = true }},
		{"complete-output", func(r *provider.ExecutionReceipt) { r.OutputComplete = true }},
	} {
		t.Run(change.name, func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			r := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}
			change.edit(&r)
			turn, err := f.s.ApplyTurnReceipt(ctx, claim, r)
			if err != nil || turn.State != domain.TurnStateUnknown || turn.AcceptedAt != nil {
				t.Fatalf("contradictory not_started released work or invented acceptance: %+v, %v", turn, err)
			}
			queueAudit(t, f.s, claim, provider.OutcomeUnknown)
			queuePrivateAndHeld(t, f, claim)
		})
	}
}

func TestQueueCancelledTurnRequiresTrustworthyReceiptToSettle(t *testing.T) {
	f, _, claim := queueClaimFixture(t)
	cancelled, err := f.s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	queueCandidate(t, f.s, claim, "final answer after cancel request", provider.OutcomeAborted)
	if turn := queueTurn(t, f.s, claim); turn.State != domain.TurnStateCancelRequested {
		t.Fatalf("terminal callback cleared cancel intent: %+v", turn)
	}
	queueCounts(t, f.s, 1, 1, 1)
	turn, err := f.s.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeAborted))
	if err != nil || turn.State != domain.TurnStateAborted || turn.ReplyMessageID == "" || !reflect.DeepEqual(turn.CancelRequestedAt, cancelled.CancelRequestedAt) {
		t.Fatalf("trustworthy terminal lost cancellation intent: %+v, %v", turn, err)
	}
	queueDeliveryState(t, f.s, turn, domain.DeliveryStateAccepted)
	queueCounts(t, f.s, 1, 1, 0)
	if again, err := f.s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID); err != nil || !reflect.DeepEqual(again, turn) {
		t.Fatalf("terminal cancellation was not idempotent: %+v, %v", again, err)
	}
}

func TestQueueReceiptRestoresAcceptanceAfterCallbackPersistenceFailure(t *testing.T) {
	f, _, claim := queueClaimFixture(t)
	queueEvent(t, f.s, claim, provider.SessionEvent, "", "")
	before := queueTurn(t, f.s, claim)
	if _, err := f.s.db.Exec(`CREATE TRIGGER queue_accept_fail BEFORE UPDATE ON deliveries
	 WHEN NEW.state='accepted' BEGIN SELECT RAISE(ABORT,'PRIVATE-ACCEPT-SQL'); END`); err != nil {
		t.Fatal(err)
	}
	err := f.s.RecordTurnEvent(ctx, claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "native-process-session"})
	if err == nil || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("callback failure leaked or succeeded: %v", err)
	}
	if got := queueTurn(t, f.s, claim); !reflect.DeepEqual(got, before) {
		t.Fatalf("failed acceptance partially committed: %+v", got)
	}
	queueDeliveryState(t, f.s, before, domain.DeliveryStateQueued)
	if _, err := f.s.db.Exec("DROP TRIGGER queue_accept_fail"); err != nil {
		t.Fatal(err)
	}
	r := queueReceipt(provider.OutcomeSucceeded)
	r.Outcome, r.DeliveryFailed = provider.OutcomeUnknown, true
	turn, err := f.s.ApplyTurnReceipt(ctx, claim, r)
	if err != nil || turn.State != domain.TurnStateUnknown || turn.AcceptedAt == nil || turn.NativeSessionID != before.NativeSessionID {
		t.Fatalf("matching receipt lost a known accepted fact: %+v, %v", turn, err)
	}
	queueDeliveryState(t, f.s, turn, domain.DeliveryStateAccepted)
	queueAudit(t, f.s, claim, provider.OutcomeUnknown)
	queuePrivateAndHeld(t, f, claim)
}

func TestQueueReceiptConflictingBindingCannotInventAcceptance(t *testing.T) {
	f, _, claim := queueClaimFixture(t)
	queueEvent(t, f.s, claim, provider.SessionEvent, "", "")
	r := queueReceipt(provider.OutcomeSucceeded)
	r.NativeSessionID = "another-native-session"
	turn, err := f.s.ApplyTurnReceipt(ctx, claim, r)
	if err != nil || turn.State != domain.TurnStateUnknown || turn.NativeSessionID != "native-process-session" || turn.AcceptedAt != nil {
		t.Fatalf("conflicting receipt rebound identity or invented acceptance: %+v, %v", turn, err)
	}
	queueDeliveryState(t, f.s, turn, domain.DeliveryStateQueued)
	queuePrivateAndHeld(t, f, claim)
}

func TestQueueReceiptFencesRecoveryAndCrossGroup(t *testing.T) {
	f, _, claim := queueClaimFixture(t)
	queueCandidate(t, f.s, claim, "persisted answer", provider.OutcomeSucceeded)
	old := claim
	if _, err := f.s.RecoverInFlight(ctx); err != nil {
		t.Fatal(err)
	}
	before := queueTurn(t, f.s, claim)
	for _, bad := range []TurnClaim{old, {GroupID: f.otherID, TurnID: claim.TurnID, Token: before.ClaimToken}, {GroupID: claim.GroupID, TurnID: claim.TurnID}, {GroupID: claim.GroupID, TurnID: "missing", Token: before.ClaimToken}} {
		if got, err := f.s.ApplyTurnReceipt(ctx, bad, queueReceipt(provider.OutcomeSucceeded)); !errors.Is(err, ErrStaleClaim) || got.ID != "" {
			t.Fatalf("receipt was not fenced: %+v, %v", got, err)
		}
	}
	if got := queueTurn(t, f.s, claim); !reflect.DeepEqual(got, before) || queueReceiptJSON(t, f.s, claim) != "" {
		t.Fatalf("stale receipt changed recovered turn: %+v", got)
	}
	queueCounts(t, f.s, 1, 1, 1)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.s.ApplyTurnReceipt(cancelled, old, provider.ExecutionReceipt{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("receipt lost context cancellation: %v", err)
	}
	claim.Token = before.ClaimToken
	turn, err := f.s.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeSucceeded))
	if err != nil || turn.State != domain.TurnStateSucceeded || turn.ReplyMessageID == "" {
		t.Fatalf("current reconciled receipt could not finish recovered execution: %+v, %v", turn, err)
	}
}

func TestQueueReceiptRollsBackReplyDeliveryTurnAndClaims(t *testing.T) {
	for _, failure := range []string{"after-reply", "turn-update", "delivery-update", "lock-delete", "commit"} {
		t.Run(failure, func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			queueCandidate(t, f.s, claim, "persisted answer", provider.OutcomeSucceeded)
			before := queueTurn(t, f.s, claim)
			trigger := "AFTER INSERT ON messages WHEN NEW.sender_id='" + f.a + "'"
			body := "SELECT RAISE(ABORT,'PRIVATE-SQL-SECRET');"
			switch failure {
			case "turn-update":
				trigger = "BEFORE UPDATE ON turns WHEN NEW.state='succeeded'"
			case "delivery-update":
				trigger = "BEFORE UPDATE ON deliveries WHEN NEW.state='accepted'"
			case "lock-delete":
				trigger = "BEFORE DELETE ON resource_claims"
			case "commit":
				if _, err := f.s.db.Exec("CREATE TABLE queue_receipt_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)"); err != nil {
					t.Fatal(err)
				}
				trigger = "AFTER DELETE ON resource_claims"
				body = "INSERT INTO queue_receipt_guard VALUES('PRIVATE-COMMIT-SECRET');"
			}
			if _, err := f.s.db.Exec("CREATE TRIGGER queue_receipt_fail " + trigger + " BEGIN " + body + " END"); err != nil {
				t.Fatal(err)
			}
			turn, err := f.s.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeSucceeded))
			if err == nil || turn.ID != "" || strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), "RAISE") || strings.Contains(err.Error(), "INSERT") {
				t.Fatalf("failed settlement invented success or leaked SQL: %+v, %v", turn, err)
			}
			if got := queueTurn(t, f.s, claim); !reflect.DeepEqual(got, before) || queueReceiptJSON(t, f.s, claim) != "" {
				t.Fatalf("failed settlement partially changed turn: %+v", got)
			}
			queueDeliveryState(t, f.s, before, domain.DeliveryStateAccepted)
			queuePrivateAndHeld(t, f, claim)
			if _, err := f.s.db.Exec("DROP TRIGGER queue_receipt_fail"); err != nil {
				t.Fatal(err)
			}
			if turn, err := f.s.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeSucceeded)); err != nil || turn.State != domain.TurnStateSucceeded {
				t.Fatalf("rollback left connection or claim unusable: %+v, %v", turn, err)
			}
		})
	}
}

func TestQueueReceiptAuditIsBoundedAndDiscardsRawDiagnostics(t *testing.T) {
	f, _, claim := queueClaimFixture(t)
	r := queueReceipt(provider.OutcomeSucceeded)
	r.Reason = strings.Repeat("PRIVATE-RAW-ERROR", 1<<16)
	r.StdoutPath = strings.Repeat("PRIVATE-PATH", 1<<16)
	r.StderrPath = "PRIVATE\nPATH"
	r.NativeSessionID = strings.Repeat("x", 64<<10)
	turn, err := f.s.ApplyTurnReceipt(ctx, claim, r)
	if err != nil || turn.State != domain.TurnStateSucceeded || turn.ReplyMessageID != "" || turn.AcceptedAt == nil {
		t.Fatalf("trusted receipt without candidate fabricated reply or lost acceptance: %+v, %v", turn, err)
	}
	audit := queueAudit(t, f.s, claim, provider.OutcomeSucceeded)
	if audit.StdoutPath != "" || audit.StderrPath != "" || audit.NativeSessionID != r.NativeSessionID {
		t.Fatalf("unsafe path retained or valid bounded identity lost: %+v", audit)
	}
}
