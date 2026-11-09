package store

import (
	"context"
	"encoding/json"
	"errors"
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
