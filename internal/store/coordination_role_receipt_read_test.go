package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestCoordinationRoleReceiptSnapshot(t *testing.T) {
	f, turn, claim := queueClaimFixture(t)
	snapshot, err := f.s.GetCoordinationRoleReceipt(context.Background(), claim.GroupID, claim.TurnID)
	if err != nil || snapshot.Turn.ID != turn.ID || snapshot.Receipt != nil {
		t.Fatalf("old/unapplied receipt must be explicit absence with current Turn: %v", err)
	}
	if _, err := f.s.GetCoordinationRoleReceipt(context.Background(), "other-group", claim.TurnID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-group read: %v", err)
	}
	raw := provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, ExitCode: -1, Reason: "PRIVATE-RAW-REASON"}
	if _, err := f.s.ApplyTurnReceipt(context.Background(), claim, raw); err != nil {
		t.Fatal(err)
	}
	snapshot, err = f.s.GetCoordinationRoleReceipt(context.Background(), claim.GroupID, claim.TurnID)
	if err != nil || snapshot.Receipt == nil || snapshot.Turn.State != domain.TurnStateUnknown || snapshot.Receipt.Reason == raw.Reason || snapshot.Receipt.Outcome != provider.OutcomeUnknown {
		t.Fatalf("current normalized snapshot unavailable: %v", err)
	}
	data, _ := json.Marshal(snapshot)
	if string(data) != "{}" {
		t.Fatal("trusted snapshot became a transport DTO")
	}
	for _, malformed := range []string{"{", "null", "{} {}", `{"outcome":null}`, `{"outcome":"unknown","private":"PRIVATE-SQL"}`, `{"outcome":"unknown","reason":"incomplete"}`} {
		if _, err := f.s.db.Exec("UPDATE turns SET receipt_json=? WHERE id=?", malformed, claim.TurnID); err != nil {
			t.Fatal(err)
		}
		got, err := f.s.GetCoordinationRoleReceipt(context.Background(), claim.GroupID, claim.TurnID)
		if err == nil || got.Receipt != nil || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("malformed receipt was accepted or leaked")
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.s.GetCoordinationRoleReceipt(cancelled, claim.GroupID, claim.TurnID); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost context cause: %v", err)
	}
}

func TestCoordinationRoleReceiptRejectsNonNormalized(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*provider.ExecutionReceipt)
	}{
		{"private-reason", func(r *provider.ExecutionReceipt) { r.Reason = "PRIVATE-SQL: SELECT credential FROM secret" }},
		{"wrong-fixed-reason", func(r *provider.ExecutionReceipt) { r.Reason = "执行完成" }},
		{"incomplete-terminal", func(r *provider.ExecutionReceipt) {
			r.Outcome, r.Reason = provider.OutcomeSucceeded, "执行完成"
		}},
		{"unnormalized-path", func(r *provider.ExecutionReceipt) { r.StderrPath = "PRIVATE-RAW\npath" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			receipt := queueReceipt(provider.OutcomeSucceeded)
			receipt.OutputComplete = false
			turn, err := f.s.ApplyTurnReceipt(ctx, claim, receipt)
			if err != nil || turn.State != domain.TurnStateUnknown || turn.Reason != "执行结果未知，需对账后再处理" {
				t.Fatalf("invalid normalized fixture: %+v, %v", turn, err)
			}
			queueCounts(t, f.s, 1, 1, 1)
			receipt.Outcome, receipt.Reason = provider.OutcomeUnknown, "执行结果未知，需对账后再处理"
			tc.change(&receipt)
			data, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.db.Exec("UPDATE turns SET receipt_json=? WHERE group_id=? AND id=?", string(data), claim.GroupID, claim.TurnID); err != nil {
				t.Fatal(err)
			}
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.GetCoordinationRoleReceipt(ctx, claim.GroupID, claim.TurnID)
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("non-normalized canonical receipt must be rejected with ErrInvalid: %v", err)
			}
			if !reflect.DeepEqual(got, CoordinationRoleReceiptSnapshot{}) {
				t.Error("rejected receipt must return a zero snapshot")
			}
			if err == nil || err.Error() != "读取角色当前回执失败" {
				t.Error("rejected receipt must return only the generic Chinese error")
			}
			if queueReceiptJSON(t, f.s, claim) != string(data) {
				t.Error("read rewrote non-normalized receipt bytes")
			}
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}

func TestCoordinationRoleReceiptAppliedSnapshots(t *testing.T) {
	unknown := queueReceipt(provider.OutcomeSucceeded)
	unknown.Outcome, unknown.DeliveryFailed = provider.OutcomeUnknown, true
	downgraded := queueReceipt(provider.OutcomeSucceeded)
	downgraded.OutputComplete = false
	notStarted := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1, Reason: "PRIVATE-START-ERROR"}
	for _, tc := range []struct {
		name      string
		raw       provider.ExecutionReceipt
		outcome   provider.Outcome
		reason    string
		cancel    bool
		candidate bool
	}{
		{name: "unknown", raw: unknown, outcome: provider.OutcomeUnknown, reason: "执行结果未知，需对账后再处理"},
		{name: "downgraded-terminal", raw: downgraded, outcome: provider.OutcomeUnknown, reason: "执行结果未知，需对账后再处理", candidate: true},
		{name: "not-started", raw: notStarted, outcome: provider.OutcomeNotStarted, reason: "执行未启动"},
		{name: "cancelled-not-started", raw: notStarted, outcome: provider.OutcomeNotStarted, reason: "执行未启动", cancel: true},
		{name: "succeeded", raw: queueReceipt(provider.OutcomeSucceeded), outcome: provider.OutcomeSucceeded, reason: "执行完成"},
		{name: "failed", raw: queueReceipt(provider.OutcomeFailed), outcome: provider.OutcomeFailed, reason: "模型执行失败", candidate: true},
		{name: "aborted", raw: queueReceipt(provider.OutcomeAborted), outcome: provider.OutcomeAborted, reason: "模型执行已中止", cancel: true, candidate: true},
		{name: "truncated", raw: queueReceipt(provider.OutcomeTruncated), outcome: provider.OutcomeTruncated, reason: "模型输出被截断", candidate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			if tc.cancel {
				turn, err := f.s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID)
				if err != nil || turn.State != domain.TurnStateCancelRequested || turn.CancelRequestedAt == nil {
					t.Fatalf("cancel intent unavailable: %+v, %v", turn, err)
				}
			}
			if tc.candidate {
				queueCandidate(t, f.s, claim, "persisted candidate", tc.raw.Terminal)
			}
			turn, err := f.s.ApplyTurnReceipt(ctx, claim, tc.raw)
			if err != nil || turn.State != domain.TurnState(tc.outcome) || turn.Reason != tc.reason || turn.ClaimToken != claim.Token || (turn.CancelRequestedAt != nil) != tc.cancel {
				t.Fatalf("unexpected applied outcome: %+v, %v", turn, err)
			}
			if turn.NativeSessionID != tc.raw.NativeSessionID || (turn.AcceptedAt != nil) != tc.raw.Accepted || (turn.ReplyMessageID != "") != (tc.candidate && tc.outcome != provider.OutcomeUnknown) {
				t.Fatalf("Apply lost binding/acceptance/publication evidence: %+v", turn)
			}
			claims := 0
			if tc.outcome == provider.OutcomeUnknown {
				claims = 1
			}
			queueCounts(t, f.s, 1, 1, claims)
			want := tc.raw
			want.Outcome, want.Reason = tc.outcome, tc.reason
			canonical, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			for _, stage := range []string{"applied", "reopened"} {
				if stage == "reopened" {
					if err := f.s.Close(); err != nil {
						t.Fatal(err)
					}
					f.s = queueHandle(t, f.path, false)
					if tc.outcome != provider.OutcomeUnknown {
						replay := tc.raw
						replay.Reason = "different raw diagnostic"
						if got, err := f.s.ApplyTurnReceipt(ctx, claim, replay); err != nil || !reflect.DeepEqual(got, turn) {
							t.Fatalf("normalized replay changed terminal history: %+v, %v", got, err)
						}
					}
				}
				if queueReceiptJSON(t, f.s, claim) != string(canonical) {
					t.Fatalf("%s: Apply did not persist the expected canonical evidence", stage)
				}
				before := coordinationDecideSnapshot(t, f.s)
				got, err := f.s.GetCoordinationRoleReceipt(ctx, claim.GroupID, claim.TurnID)
				if err != nil || !reflect.DeepEqual(got.Turn, turn) || !reflect.DeepEqual(got.Receipt, &want) {
					t.Errorf("%s: current normalized snapshot changed: %+v, %v", stage, got, err)
				}
				coordinationDecidePreserved(t, f.s, before)
			}
		})
	}
}

func TestCoordinationRoleReceiptRecoveredSnapshots(t *testing.T) {
	for _, name := range []string{"unknown", "unapplied"} {
		t.Run(name, func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			var want *provider.ExecutionReceipt
			if name == "unknown" {
				queueCandidate(t, f.s, claim, "private candidate", provider.OutcomeSucceeded)
				raw := queueReceipt(provider.OutcomeSucceeded)
				raw.OutputComplete = false
				turn, err := f.s.ApplyTurnReceipt(ctx, claim, raw)
				if err != nil || turn.State != domain.TurnStateUnknown || turn.Reason != "执行结果未知，需对账后再处理" {
					t.Fatalf("normalized unknown unavailable: %+v, %v", turn, err)
				}
				raw.Outcome, raw.Reason = provider.OutcomeUnknown, "执行结果未知，需对账后再处理"
				want = &raw
			} else {
				queueEvent(t, f.s, claim, provider.AcceptedEvent, "", "")
			}
			previous := queueTurn(t, f.s, claim)
			stored := queueReceiptJSON(t, f.s, claim)
			if want == nil {
				if stored != "" {
					t.Fatal("unapplied fixture has a receipt")
				}
			} else if data, err := json.Marshal(want); err != nil || stored != string(data) {
				t.Fatalf("unexpected persisted normalized evidence: %v", err)
			}
			claims := messageSequenceTableRows(t, f.s, "resource_claims")
			if count, err := f.s.RecoverInFlight(ctx); err != nil || count != 1 {
				t.Fatalf("recovery did not fence the active turn: %d, %v", count, err)
			}
			current := queueTurn(t, f.s, claim)
			if current.State != domain.TurnStateUnknown || current.ClaimToken == "" || current.ClaimToken == claim.Token || current.UpdatedAt.Before(previous.UpdatedAt) {
				t.Fatalf("recovery did not rotate the current claim/state: %+v", current)
			}
			previous.State, previous.ClaimToken, previous.UpdatedAt = domain.TurnStateUnknown, current.ClaimToken, current.UpdatedAt
			if !reflect.DeepEqual(current, previous) || queueReceiptJSON(t, f.s, claim) != stored || !reflect.DeepEqual(messageSequenceTableRows(t, f.s, "resource_claims"), claims) {
				t.Fatal("recovery changed persisted evidence or resource claims")
			}
			queueCounts(t, f.s, 1, 1, 1)
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.GetCoordinationRoleReceipt(ctx, claim.GroupID, claim.TurnID)
			if err != nil || !reflect.DeepEqual(got.Turn, current) || !reflect.DeepEqual(got.Receipt, want) {
				t.Errorf("recovered current snapshot changed: %+v, %v", got, err)
			}
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}
