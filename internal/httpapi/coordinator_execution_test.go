package httpapi

import (
	"context"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestCoordinatorConfigurationHTTPPreservesRealUnknownCancelAndAllReadRows(t *testing.T) {
	for _, state := range []domain.TurnState{domain.TurnStateUnknown, domain.TurnStateCancelRequested} {
		t.Run(string(state), func(t *testing.T) {
			a := openCoordinatorConfigurationAPI(t, "")
			g := newGroup(t, a, "group")
			lead, worker := coordinatorHTTPMember(t, a, g.ID), coordinatorHTTPMember(t, a, g.ID)
			ctx := context.Background()
			message, err := a.db.CreateMessage(ctx, store.MessageRequest{GroupID: g.ID, TopicID: "topic", ActorID: lead.ID, Content: "普通聊天不是配置或派发"}, []string{lead.ID})
			if err != nil {
				t.Fatal(err)
			}
			turn, err := a.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: g.ID, DeliveryID: message.Deliveries[0].ID, ResourceKeys: []string{"workspace:held"}, MaxActive: 1})
			if err != nil {
				t.Fatal(err)
			}
			claim := store.TurnClaim{GroupID: g.ID, TurnID: turn.ID, Token: turn.ClaimToken}
			for _, event := range []provider.Event{
				{Kind: provider.AcceptedEvent, NativeSessionID: "private-native-session"},
				{Kind: provider.ResponseEvent, NativeSessionID: "private-native-session", Text: "private response candidate"},
			} {
				if err := a.db.RecordTurnEvent(ctx, claim, event); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := a.db.RequestTurnCancel(ctx, g.ID, turn.ID); err != nil {
				t.Fatal(err)
			}
			if state == domain.TurnStateUnknown {
				if _, err := a.db.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Started: true, Accepted: true, NativeSessionID: "private-native-session", StdoutPath: "/private/unknown.log"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := platform.NewLocalUser(a.db).CreateTask(ctx, g.ID, platform.TaskInput{Title: "existing manual task", Acceptance: "preserve exact state"}, "manual-task"); err != nil {
				t.Fatal(err)
			}
			beforeTurn, err := a.db.GetTurn(ctx, g.ID, turn.ID)
			if err != nil || beforeTurn.State != state || beforeTurn.CancelRequestedAt == nil || beforeTurn.AcceptedAt == nil {
				t.Fatalf("fixture lacks real uncertain execution: %+v, %v", beforeTurn, err)
			}
			raw := messageSQL(t, a)
			before := coordinationHTTPSnapshot(t, raw)
			for _, table := range []string{"messages", "deliveries", "turns", "resource_claims", "tasks"} {
				if len(before[table]) != 1 {
					t.Fatalf("fixture lacks nonempty %s evidence", table)
				}
			}
			base := coordinationHTTPBase(g.ID)
			bindingPath, policyPath := base+"/coordinator", base+"/dispatch-policy"
			w := a.request("GET", bindingPath, "")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
			coordinatorHTTPValue[domain.GroupDispatchPolicy](t, a.request("GET", policyPath, ""))
			coordinationHTTPUnchanged(t, raw, before)
			bindingBody := coordinatorHTTPBindingBody(lead.ID, 0)
			policyBody := coordinatorHTTPPolicyBody(t, []string{worker.ID}, 0)
			originalBinding := coordinatorHTTPValue[domain.GroupCoordinatorBinding](t, a.request("PUT", bindingPath, bindingBody, "original"))
			originalPolicy := coordinatorHTTPValue[domain.GroupDispatchPolicy](t, a.request("PUT", policyPath, policyBody, "original"))
			coordinatorHTTPValue[domain.GroupDispatchPolicy](t, a.request("PUT", policyPath, coordinatorHTTPPolicyBody(t, []string{}, 1), "clear"))
			latestBinding := coordinatorHTTPValue[domain.GroupCoordinatorBinding](t, a.request("PUT", bindingPath, coordinatorHTTPBindingBody(worker.ID, 1), "replace"))
			latestPolicy := coordinatorHTTPValue[domain.GroupDispatchPolicy](t, a.request("PUT", policyPath, coordinatorHTTPPolicyBody(t, []string{lead.ID}, 2), "replace"))
			if got := coordinatorHTTPValue[domain.GroupCoordinatorBinding](t, a.request("PUT", bindingPath, bindingBody, "original")); got != originalBinding {
				t.Fatalf("replay changed binding: %+v", got)
			}
			if got := coordinatorHTTPValue[domain.GroupDispatchPolicy](t, a.request("PUT", policyPath, policyBody, "original")); !reflect.DeepEqual(got, originalPolicy) {
				t.Fatalf("replay changed policy: %+v", got)
			}
			coordinatorHTTPConfigurationOnly(t, raw, before)
			before = coordinationHTTPSnapshot(t, raw)
			for range 2 {
				if got := coordinatorHTTPValue[domain.GroupCoordinatorBinding](t, a.request("GET", bindingPath, "")); got != latestBinding {
					t.Fatalf("binding GET: %+v", got)
				}
				if got := coordinatorHTTPValue[domain.GroupDispatchPolicy](t, a.request("GET", policyPath, "")); !reflect.DeepEqual(got, latestPolicy) {
					t.Fatalf("policy GET: %+v", got)
				}
			}
			coordinationHTTPUnchanged(t, raw, before)
			if got, err := a.db.GetTurn(ctx, g.ID, turn.ID); err != nil || !reflect.DeepEqual(got, beforeTurn) {
				t.Fatalf("configuration changed uncertain execution/resources: %+v, %v", got, err)
			}
		})
	}
}
