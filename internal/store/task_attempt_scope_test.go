package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func taskAttemptAgent(t *testing.T) (messageFixture, MemberToolScope, domain.Task, ActivateTaskAttemptInput) {
	t.Helper()
	f, _, claim := queueClaimFixture(t)
	scope, err := f.s.BindMemberTool(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	create := taskInput(f.groupID)
	create.AssigneeID = scope.MemberID
	task, err := f.s.CreateTask(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	input := taskAttemptInput(task)
	input.ActorID, input.MemberScope, input.IdempotencyKey = scope.MemberID, &scope, "agent"
	return f, scope, task, input
}

func TestTaskAttemptAgentDerivesIdentityAndKeepsQueueUntouched(t *testing.T) {
	f, scope, task, input := taskAttemptAgent(t)
	before := queueTurn(t, f.s, scope.Claim)
	attempt, err := f.s.ActivateTaskAttempt(ctx, input)
	if err != nil || attempt.TurnID != scope.Claim.TurnID || attempt.TaskID != task.ID || attempt.GroupID != scope.Claim.GroupID {
		t.Fatalf("bound activation: %+v, %v", attempt, err)
	}
	tx, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stored, actor, err := taskAttemptInTx(ctx, tx, input.GroupID, input.TaskID, attempt.ID)
	if err != nil || actor != scope.MemberID || !reflect.DeepEqual(stored, attempt) {
		t.Fatalf("stored actor binding: %+v, %q, %v", stored, actor, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if after := queueTurn(t, f.s, scope.Claim); !reflect.DeepEqual(after, before) {
		t.Fatal("task activation changed execution state")
	}
	queueCounts(t, f.s, 1, 1, 1)
	queueEvent(t, f.s, scope.Claim, provider.AcceptedEvent, "", "")
	input.IdempotencyKey = "running"
	input.ExpectedActiveAttemptID = attempt.ID
	if _, err := f.s.ActivateTaskAttempt(ctx, input); err != nil {
		t.Fatalf("running scope cannot activate: %v", err)
	}
	encoded, err := json.Marshal(input)
	if err != nil || strings.Contains(string(encoded), scope.Claim.Token) || strings.Contains(string(encoded), "MemberScope") || strings.Contains(string(encoded), "ActorID") {
		t.Fatalf("trusted fields escaped: %s, %v", encoded, err)
	}
	if err := json.Unmarshal([]byte(`{"ActorID":"user","actor_id":"user","MemberScope":{"MemberID":"forged"},"member_scope":{},"TurnID":"forged"}`), &input); err != nil || input.ActorID != scope.MemberID || (input.MemberScope == nil || *input.MemberScope != scope) {
		t.Fatalf("JSON selected actor/scope: %+v, %v", input, err)
	}
}

func TestTaskAttemptRejectsUnboundAndForeignIdentityBeforeReceipts(t *testing.T) {
	f, scope, _, input := taskAttemptAgent(t)
	if _, err := f.s.ActivateTaskAttempt(ctx, input); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ActivateTaskAttemptInput){
		func(i *ActivateTaskAttemptInput) { i.MemberScope = nil },
		func(i *ActivateTaskAttemptInput) { i.ActorID = "user" },
		func(i *ActivateTaskAttemptInput) { i.ActorID = f.b },
		func(i *ActivateTaskAttemptInput) { i.GroupID = f.otherID },
		func(i *ActivateTaskAttemptInput) {
			bad := scope
			bad.Claim.Token = "PRIVATE-FORGED-TOKEN"
			i.MemberScope = &bad
		},
		func(i *ActivateTaskAttemptInput) { bad := scope; bad.Claim.TurnID = "missing"; i.MemberScope = &bad },
	} {
		bad := input
		change(&bad)
		bad.TaskID = "missing"
		_, err := f.s.ActivateTaskAttempt(ctx, bad)
		requireMemberToolDenied(t, err)
	}
	create := taskInput(f.groupID)
	create.AssigneeID = f.b
	wrong, err := f.s.CreateTask(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	input.TaskID, input.IdempotencyKey = wrong.ID, "wrong-assignee"
	_, err = f.s.ActivateTaskAttempt(ctx, input)
	requireMemberToolDenied(t, err)
	taskAttemptCounts(t, f.s, 1, 1)
}

func TestTaskAttemptRevokedScopesCannotReplay(t *testing.T) {
	for _, state := range []string{"revoked", "unknown", "cancel", "candidate", "terminal", "left"} {
		t.Run(state, func(t *testing.T) {
			f, scope, _, input := taskAttemptAgent(t)
			if _, err := f.s.ActivateTaskAttempt(ctx, input); err != nil {
				t.Fatal(err)
			}
			var err error
			switch state {
			case "revoked":
				_, err = f.s.db.Exec("UPDATE turns SET claim_token='rotated' WHERE id=?", scope.Claim.TurnID)
			case "unknown":
				_, err = f.s.ApplyTurnReceipt(ctx, scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown})
			case "cancel":
				_, err = f.s.RequestTurnCancel(ctx, scope.Claim.GroupID, scope.Claim.TurnID)
			case "candidate":
				queueEvent(t, f.s, scope.Claim, provider.AcceptedEvent, "", "")
				queueEvent(t, f.s, scope.Claim, provider.TerminalEvent, "", provider.OutcomeSucceeded)
			case "terminal":
				_, err = f.s.ApplyTurnReceipt(ctx, scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted})
			case "left":
				_, err = f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", scope.MemberID)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := queueTurn(t, f.s, scope.Claim)
			for _, missing := range []bool{false, true} {
				bad := input
				if missing {
					bad.TaskID = "missing"
					bad.ExpectedRevision = 0
				}
				_, err := f.s.ActivateTaskAttempt(ctx, bad)
				requireMemberToolDenied(t, err)
			}
			if after := queueTurn(t, f.s, scope.Claim); !reflect.DeepEqual(after, before) {
				t.Fatal("denied activation changed Turn")
			}
			taskAttemptCounts(t, f.s, 1, 1)
			if state == "unknown" {
				queueCounts(t, f.s, 1, 1, 1)
			}
		})
	}
}

func TestTaskAttemptNewScopeReplaysOriginalTurnAcrossGenerations(t *testing.T) {
	f, scope, task, input := taskAttemptAgent(t)
	original, err := f.s.ActivateTaskAttempt(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	human := taskAttemptInput(task)
	human.ExpectedActiveAttemptID = original.ID
	if _, err := f.s.ActivateTaskAttempt(ctx, human); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.ActivateTaskAttempt(ctx, input)
	requireMemberToolDenied(t, err)
	d := queueDelivery(t, f.s, f.groupID, f.a, "next-task-scope")
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: d.ID, ResourceKeys: []string{"shared"}, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("UPDATE tasks SET revision=2,active_attempt_id=NULL,assignee_id=? WHERE id=?", f.b, task.ID); err != nil {
		t.Fatal(err)
	}
	before, err := f.s.GetTask(ctx, task.GroupID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	input.MemberScope = &fresh
	replay, err := f.s.ActivateTaskAttempt(ctx, input)
	if err != nil || !reflect.DeepEqual(replay, original) || replay.TurnID == fresh.Claim.TurnID {
		t.Fatalf("receipt rebound to new Turn: %+v, %v", replay, err)
	}
	after, err := f.s.GetTask(ctx, task.GroupID, task.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("historical replay changed current task: %+v, %v", after, err)
	}
	taskAttemptCounts(t, f.s, 2, 1)
	queueCounts(t, f.s, 2, 2, 1)
}
