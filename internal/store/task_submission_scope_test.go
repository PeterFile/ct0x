package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func taskSubmissionAgent(t *testing.T) (messageFixture, MemberToolScope, domain.Task, domain.TaskAttempt, SubmitTaskInput) {
	t.Helper()
	f, scope, task, activation := taskAttemptAgent(t)
	attempt, err := f.s.ActivateTaskAttempt(ctx, activation)
	if err != nil {
		t.Fatal(err)
	}
	input := taskSubmissionInput(attempt)
	input.ActorID, input.MemberScope = scope.MemberID, &scope
	return f, scope, task, attempt, input
}

func TestTaskSubmissionAgentUsesOwnLiveTurnAndHiddenIdentity(t *testing.T) {
	f, scope, _, _, input := taskSubmissionAgent(t)
	before := queueTurn(t, f.s, scope.Claim)
	bad := input
	bad.ActorID, bad.MemberScope = "user", nil
	_, err := f.s.SubmitTask(ctx, bad)
	requireMemberToolDenied(t, err)
	d := queueDelivery(t, f.s, f.groupID, f.b, "other-member")
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: d.ID, MaxActive: 2})
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	bad = input
	bad.ActorID, bad.MemberScope = other.MemberID, &other
	_, err = f.s.SubmitTask(ctx, bad)
	requireMemberToolDenied(t, err)
	taskSubmissionCounts(t, f.s, 0, 0)
	encoded, err := json.Marshal(input)
	if err != nil || strings.Contains(string(encoded), scope.Claim.Token) || strings.Contains(string(encoded), "ActorID") || strings.Contains(string(encoded), "MemberScope") {
		t.Fatalf("trusted identity escaped: %s, %v", encoded, err)
	}
	if err := json.Unmarshal([]byte(`{"ActorID":"user","actor_id":"user","MemberScope":{"MemberID":"forged"},"member_scope":{},"SubmittedBy":"forged","TurnID":"forged"}`), &input); err != nil || input.ActorID != scope.MemberID || input.MemberScope == nil || *input.MemberScope != scope {
		t.Fatalf("JSON chose submission identity: %+v, %v", input, err)
	}
	submission, err := f.s.SubmitTask(ctx, input)
	if err != nil || submission.SubmittedBy != scope.MemberID || submission.AttemptID != input.AttemptID {
		t.Fatalf("bound submission: %+v, %v", submission, err)
	}
	if after := queueTurn(t, f.s, scope.Claim); !reflect.DeepEqual(after, before) {
		t.Fatal("submission changed execution state")
	}
	queueCounts(t, f.s, 2, 2, 1)
	taskSubmissionCounts(t, f.s, 1, 1)
}

func TestTaskSubmissionNewScopeReplaysButCannotSubmitOldTurn(t *testing.T) {
	f, scope, _, _, input := taskSubmissionAgent(t)
	pendingTask := createTestTask(t, f.s, f.groupID)
	activation := taskAttemptInput(pendingTask)
	activation.ActorID, activation.MemberScope = scope.MemberID, &scope
	pending, err := f.s.ActivateTaskAttempt(ctx, activation)
	if err != nil {
		t.Fatal(err)
	}
	original, err := f.s.SubmitTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	taskSubmissionRejectFixture(t, f.s, original)
	if _, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.SubmitTask(ctx, input)
	requireMemberToolDenied(t, err)
	d := queueDelivery(t, f.s, f.groupID, scope.MemberID, "fresh-submission-scope")
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: d.ID, ResourceKeys: []string{"shared"}, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	input.MemberScope = &fresh
	replay, err := f.s.SubmitTask(ctx, input)
	if err != nil || !reflect.DeepEqual(replay, original) {
		t.Fatalf("fresh scope rebound evidence: %+v, %v", replay, err)
	}
	request := taskSubmissionInput(pending)
	request.ActorID, request.MemberScope = scope.MemberID, &fresh
	request.IdempotencyKey = "pending"
	_, err = f.s.SubmitTask(ctx, request)
	requireMemberToolDenied(t, err)
	taskSubmissionCounts(t, f.s, 1, 1)
	if current, err := f.s.GetTask(ctx, pending.GroupID, pending.TaskID); err != nil || current.Status != domain.TaskStatusOpen || current.ActiveAttemptID != pending.ID {
		t.Fatalf("new Turn altered old pending attempt: %+v, %v", current, err)
	}
	queueCounts(t, f.s, 2, 2, 1)
}

func TestTaskSubmissionAuthorizationPrecedesReplayAndConflict(t *testing.T) {
	f, scope, _, _, input := taskSubmissionAgent(t)
	original, err := f.s.SubmitTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("UPDATE turns SET claim_token='rotated-private-token' WHERE id=?", scope.Claim.TurnID); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.SubmitTask(ctx, input)
	requireMemberToolDenied(t, err)
	fresh, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: scope.Claim.TurnID, Token: "rotated-private-token"})
	if err != nil {
		t.Fatal(err)
	}
	input.MemberScope = &fresh
	if replay, err := f.s.SubmitTask(ctx, input); err != nil || !reflect.DeepEqual(replay, original) {
		t.Fatalf("new claim token changed receipt: %+v, %v", replay, err)
	}
	if _, err := f.s.RequestTurnCancel(ctx, f.groupID, scope.Claim.TurnID); err != nil {
		t.Fatal(err)
	}
	denied := func() {
		t.Helper()
		before := queueTurn(t, f.s, fresh.Claim)
		for _, changed := range []bool{false, true} {
			bad := input
			if changed {
				bad.TaskID = "missing"
				bad.ExpectedRevision = 0
				bad.ArtifactVersions = []domain.ArtifactVersionRef{{}}
			}
			_, err := f.s.SubmitTask(ctx, bad)
			requireMemberToolDenied(t, err)
		}
		if after := queueTurn(t, f.s, fresh.Claim); !reflect.DeepEqual(after, before) {
			t.Fatal("denied submit changed Turn")
		}
		taskSubmissionCounts(t, f.s, 1, 1)
		queueCounts(t, f.s, 1, 1, 1)
	}
	denied()
	if _, err := f.s.ApplyTurnReceipt(ctx, fresh.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}); err != nil {
		t.Fatal(err)
	}
	denied()
}
