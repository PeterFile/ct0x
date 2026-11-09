package httpapi

import (
	"context"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestDispatchRequestHTTPPreservesRealUnknownCancelAndResourceEvidence(t *testing.T) {
	for _, state := range []domain.TurnState{domain.TurnStateUnknown, domain.TurnStateCancelRequested} {
		t.Run(string(state), func(t *testing.T) {
			f := dispatchRequestHTTPSetup(t)
			ctx := context.Background()
			message, err := f.a.db.CreateMessage(ctx, store.MessageRequest{GroupID: f.group.ID, TopicID: "execution topic", ActorID: f.lead.ID, Content: "existing uncertain execution"}, []string{f.lead.ID})
			if err != nil {
				t.Fatal(err)
			}
			turn, err := f.a.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: message.Deliveries[0].ID, ResourceKeys: []string{"workspace:held"}, MaxActive: 1})
			if err != nil {
				t.Fatal(err)
			}
			claim := store.TurnClaim{GroupID: f.group.ID, TurnID: turn.ID, Token: turn.ClaimToken}
			for _, event := range []provider.Event{{Kind: provider.AcceptedEvent, NativeSessionID: "private-native-session"}, {Kind: provider.ResponseEvent, NativeSessionID: "private-native-session", Text: "private candidate"}} {
				if err := f.a.db.RecordTurnEvent(ctx, claim, event); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.a.db.RequestTurnCancel(ctx, f.group.ID, turn.ID); err != nil {
				t.Fatal(err)
			}
			if state == domain.TurnStateUnknown {
				if _, err := f.a.db.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Started: true, Accepted: true, NativeSessionID: "private-native-session", StdoutPath: "/private/unknown.log"}); err != nil {
					t.Fatal(err)
				}
			}
			beforeTurn, err := f.a.db.GetTurn(ctx, f.group.ID, turn.ID)
			if err != nil || beforeTurn.State != state || beforeTurn.CancelRequestedAt == nil || beforeTurn.AcceptedAt == nil {
				t.Fatalf("fixture lacks real uncertain execution: %+v, %v", beforeTurn, err)
			}
			raw := messageSQL(t, f.a)
			before := coordinationHTTPSnapshot(t, raw)
			for _, table := range []string{"messages", "deliveries", "turns", "resource_claims", "tasks", "mandates", "task_coordination"} {
				if len(before[table]) == 0 {
					t.Fatalf("fixture lacks nonempty %s evidence", table)
				}
			}
			request := dispatchRequestHTTPValue[domain.DispatchRequest](t, f.a.request("POST", f.path, f.body, "original"), 201)
			dispatchRequestHTTPAdded(t, raw, before, 1)
			before = coordinationHTTPSnapshot(t, raw)
			view := dispatchRequestHTTPValue[domain.DispatchView](t, f.a.request("GET", dispatchRequestHTTPGetPath(f.group.ID, request.ID), ""), 200)
			if view.Request != request || view.State.Status != domain.DispatchStatusPending || view.State.Delivery != nil {
				t.Fatalf("registration claimed delivery/execution: %+v", view)
			}
			if got := dispatchRequestHTTPValue[domain.DispatchRequest](t, f.a.request("POST", f.path, f.body, "original"), 201); got != request {
				t.Fatal("replay changed immutable receipt")
			}
			dispatchRequestHTTPError(t, f.a.request("POST", f.path, `{"actor":"user"}`, "invalid"), 400, "invalid_json")
			coordinationHTTPUnchanged(t, raw, before)
			if got, err := f.a.db.GetTurn(ctx, f.group.ID, turn.ID); err != nil || !reflect.DeepEqual(got, beforeTurn) {
				t.Fatalf("HTTP dispatch changed unknown/cancel execution: %+v, %v", got, err)
			}
		})
	}
}
