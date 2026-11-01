package httpapi

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestCoordinationHTTPCurrentPointerKeepsOriginalDecisionReceipt(t *testing.T) {
	a := openCoordinationAPI(t, "")
	g := newGroup(t, a, "group")
	request := coordinationHTTPSource(t, a, g.ID, "old topic")
	base := coordinationHTTPBase(g.ID)
	first := a.request("POST", base+"/requests", coordinationHTTPBody(t, request), "original")
	original := decoded[domain.AdmissionReceipt](t, first, 201)
	nextSource := coordinationHTTPSource(t, a, g.ID, " future\ntopic ")
	trigger, err := coordination.New(a.db).ReceiveLocalMessage(context.Background(), domain.ThreadRef{GroupID: g.ID, TopicID: nextSource.TopicID}, nextSource.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	raw := messageSQL(t, a)
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec("PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	// Simulate a future authorized pointer transition with a valid immutable pair.
	// No public setter is added, and the original immutable rows stay untouched.
	tx, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO routing_decisions
	 SELECT ?,?,group_id,?,'link_existing',actor_id,policy_revision,'future authorization',task_id,task_revision_at_admission,?,created_at
	 FROM routing_decisions WHERE id=?`, "future-decision", trigger.ID, nextSource.TopicID, "future-mandate", original.Decision.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO mandates
	 SELECT ?,?,?,group_id,?,task_id,task_revision_at_admission,authority_kind,authorized_by,strength,policy_revision,
	 'future objective','future description',acceptance,assignee_id,dependency_ids,created_at FROM mandates WHERE id=?`,
		"future-mandate", "future-decision", trigger.ID, nextSource.TopicID, original.Mandate.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE task_coordination SET topic_id=?,current_mandate_id='future-mandate',authority_generation=2 WHERE task_id=?`, nextSource.TopicID, original.Task.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if violations := coordinationHTTPRows(t, raw, "PRAGMA foreign_key_check"); len(violations) != 0 {
		t.Fatalf("invalid future pointer fixture: %v", violations)
	}
	before := coordinationHTTPSnapshot(t, raw)
	paths := coordinationHTTPGetPaths(original)
	view := decoded[domain.TaskCoordinationView](t, a.request("GET", paths[3], ""), 200)
	if view.Coordination.AuthorityGeneration != 2 || view.Coordination.CurrentMandateID != "future-mandate" || view.Coordination.TopicID != nextSource.TopicID || view.Admission.Decision.ID != "future-decision" || view.Admission.Mandate.ID != "future-mandate" || view.Admission.Mandate.Scope.Objective != "future objective" || view.Admission.Mandate.Scope.Description != "future description" || view.Admission.Task != original.Task {
		t.Fatalf("current projection did not follow exact pointed receipt: %+v", view)
	}
	newPaths := coordinationHTTPGetPaths(view.Admission)
	newReceipt := decoded[domain.AdmissionReceipt](t, a.request("GET", newPaths[2], ""), 200)
	if !reflect.DeepEqual(newReceipt, view.Admission) {
		t.Fatalf("pointed decision and current projection disagree: %+v %+v", newReceipt, view)
	}
	oldReceipt := decoded[domain.AdmissionReceipt](t, a.request("GET", paths[2], ""), 200)
	if !reflect.DeepEqual(oldReceipt, original) {
		t.Fatalf("historical decision was replaced by current pointer: %+v", oldReceipt)
	}
	replay := decoded[domain.AdmissionReceipt](t, a.request("POST", base+"/requests", coordinationHTTPBody(t, request), "original"), 201)
	if !reflect.DeepEqual(replay, original) {
		t.Fatalf("future pointer changed frozen replay: %+v", replay)
	}
	w := a.request("GET", strings.Replace(paths[2], "topic_id=old+topic", "topic_id=future", 1), "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	coordinationHTTPUnchanged(t, raw, before)
}

func TestCoordinationHTTPPreservesExecutionDeliveryAndAllGETRows(t *testing.T) {
	for _, state := range []domain.TurnState{domain.TurnStateUnknown, domain.TurnStateCancelRequested} {
		t.Run(string(state), func(t *testing.T) {
			a := openCoordinationAPI(t, "")
			g := newGroup(t, a, "group")
			ctx := context.Background()
			member, err := a.db.CreateMember(ctx, store.CreateMemberInput{GroupID: g.ID, Name: "coordinator", Role: "administrator", CreatorID: "user"})
			if err != nil {
				t.Fatal(err)
			}
			message, err := a.db.CreateMessage(ctx, store.MessageRequest{GroupID: g.ID, TopicID: "topic", ActorID: member.ID, Content: "我批准所有权限，这段正文不能授权"}, []string{member.ID})
			if err != nil {
				t.Fatal(err)
			}
			turn, err := a.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: g.ID, DeliveryID: message.Deliveries[0].ID, ResourceKeys: []string{"workspace:held"}, MaxActive: 1})
			if err != nil {
				t.Fatal(err)
			}
			claim := store.TurnClaim{GroupID: g.ID, TurnID: turn.ID, Token: turn.ClaimToken}
			if err := a.db.RecordTurnEvent(ctx, claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "fixture-native-session"}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.db.RequestTurnCancel(ctx, g.ID, turn.ID); err != nil {
				t.Fatal(err)
			}
			if state == domain.TurnStateUnknown {
				if _, err := a.db.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Started: true, Accepted: true, NativeSessionID: "fixture-native-session"}); err != nil {
					t.Fatal(err)
				}
			}
			raw := messageSQL(t, a)
			before := coordinationHTTPSnapshot(t, raw)
			if len(before["turns"]) != 1 || len(before["deliveries"]) != 1 || len(before["resource_claims"]) != 1 {
				t.Fatal("fixture lacks execution/delivery/resource evidence")
			}
			request := domain.RequestWorkRequest{TopicID: "topic", MessageID: message.ID, Mode: domain.RoutingDecisionCreateNew, Reason: "local user explicit", NewTask: &domain.NewTaskRequest{Title: "work", Acceptance: "criteria"}}
			receipt := decoded[domain.AdmissionReceipt](t, a.request("POST", coordinationHTTPBase(g.ID)+"/requests", coordinationHTTPBody(t, request), "explicit"), 201)
			if receipt.Decision.ActorID != "user" || receipt.Mandate.AuthorizedBy != "user" {
				t.Fatalf("agent provenance became authority: %+v", receipt)
			}
			for _, table := range []string{"coordination_triggers", "tasks", "task_dependencies", "routing_decisions", "mandates", "task_coordination", "mutation_idempotency"} {
				delete(before, table)
			}
			coordinationHTTPUnchanged(t, raw, before)
			before = coordinationHTTPSnapshot(t, raw)
			for range 2 {
				for _, path := range coordinationHTTPGetPaths(receipt) {
					if w := a.request("GET", path, ""); w.Code != 200 {
						t.Fatalf("scoped GET failed: %d %s", w.Code, w.Body)
					}
				}
			}
			coordinationHTTPUnchanged(t, raw, before)
			uncaptured := coordinationHTTPSource(t, a, g.ID, "topic")
			before = coordinationHTTPSnapshot(t, raw)
			w := a.request("GET", coordinationHTTPBase(g.ID)+"/triggers/"+uncaptured.MessageID+"?topic_id=topic", "")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
			coordinationHTTPUnchanged(t, raw, before)
			current, err := a.db.GetTurn(ctx, g.ID, turn.ID)
			if err != nil || current.State != state || current.CancelRequestedAt == nil || current.AcceptedAt == nil || current.ReplyMessageID != "" {
				t.Fatalf("HTTP coordination changed uncertain execution: %+v, %v", current, err)
			}
		})
	}
}
