package store

import (
	"errors"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestCoordinationAssignmentResolutionV23RoleRuntimeAndWorkerBinding(t *testing.T) {
	s, _ := openTestStore(t)
	r := resolutionCandidate(t, s, "assign")
	claim := TurnClaim{GroupID: r.turn.GroupID, TurnID: r.turn.ID, Token: r.turn.ClaimToken}
	receipt := queueReceipt(provider.OutcomeSucceeded)
	queueCandidate(t, s, claim, "role candidate", receipt.Outcome)
	coordinatorMustCommit(t, s, resolutionStatements(r)...)

	cancelled, err := s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID)
	if err != nil || cancelled.State != domain.TurnStateCancelRequested || cancelled.CancelRequestedAt == nil {
		t.Fatalf("coordinator cancellation: state=%s, err=%v", cancelled.State, err)
	}
	if n, err := s.RecoverInFlight(ctx); err != nil || n != 1 {
		t.Fatalf("coordinator recovery: %d, %v", n, err)
	}
	unknown := queueTurn(t, s, claim)
	if unknown.State != domain.TurnStateUnknown || unknown.ClaimToken == "" || unknown.ClaimToken == claim.Token ||
		!reflect.DeepEqual(unknown.CancelRequestedAt, cancelled.CancelRequestedAt) || unknown.ReplyMessageID != "" {
		t.Fatal("recovery did not fence the cancelled coordinator as unpublished unknown")
	}
	if got, err := s.ApplyTurnReceipt(ctx, claim, receipt); !errors.Is(err, ErrStaleClaim) || got.ID != "" {
		t.Fatalf("old coordinator claim was not rejected: %v", err)
	}
	afterStale, err := s.GetCoordinationRoleReceipt(ctx, claim.GroupID, claim.TurnID)
	if err != nil || afterStale.Receipt != nil || !reflect.DeepEqual(afterStale.Turn, unknown) {
		t.Fatalf("stale receipt changed recovered coordinator evidence: %v", err)
	}
	resource := "resolution-" + r.request.request[0].(string)
	if held, err := s.HasResourceClaim(ctx, resource); err != nil || !held {
		t.Fatalf("unknown coordinator did not hold its resource: held=%t, err=%v", held, err)
	}

	claim.Token = unknown.ClaimToken
	settled, err := s.ApplyTurnReceipt(ctx, claim, receipt)
	if err != nil || settled.State != domain.TurnStateSucceeded || settled.ClaimToken != claim.Token ||
		settled.ReplyMessageID == "" || !reflect.DeepEqual(settled.CancelRequestedAt, cancelled.CancelRequestedAt) {
		t.Fatalf("trusted receipt did not settle the recovered coordinator: state=%s, err=%v", settled.State, err)
	}
	if held, err := s.HasResourceClaim(ctx, resource); err != nil || held {
		t.Fatalf("terminal coordinator did not release its resource: held=%t, err=%v", held, err)
	}

	assignmentID := r.assignment[0].(string)
	materialized, err := s.MaterializeAssignment(ctx, assignmentID, 1, materializeAt)
	if err != nil || materialized.Disposition != "enqueued" || materialized.Delivery == nil {
		t.Fatalf("worker materialization: %+v, %v", materialized, err)
	}
	role, err := s.ResolveCoordinationDelivery(ctx, claim.GroupID, materialized.Delivery.DeliveryID)
	p := role.Provenance
	if err != nil || p.Kind != CoordinationRoleWorker || p.AssignmentID != assignmentID || p.MemberID != r.assignment[5].(string) {
		t.Fatalf("materialized worker role: %+v, %v", role, err)
	}
	worker := coordinationRoleClaim(t, requestDispatchFixture{s: s}, p)
	binding, err := s.BindAssignmentAttempt(ctx, worker)
	if err != nil || binding.TurnID != worker.Member.Claim.TurnID || binding.AttemptID == "" {
		t.Fatalf("worker attempt binding: %+v, %v", binding, err)
	}
	view, err := s.GetAssignment(ctx, claim.GroupID, assignmentID)
	if err != nil || view.Decision.CoordinatorTurnID != claim.TurnID ||
		view.Dispatch.Delivery == nil || *view.Dispatch.Delivery != *materialized.Delivery ||
		view.Binding == nil || *view.Binding != binding || view.Task.ActiveAttemptID != binding.AttemptID {
		t.Fatalf("assignment did not retain its coordinator, worker delivery and attempt: %+v, %v", view, err)
	}

	want := receipt
	want.Reason = "执行完成"
	snapshot, err := s.GetCoordinationRoleReceipt(ctx, claim.GroupID, claim.TurnID)
	if err != nil || snapshot.Receipt == nil || *snapshot.Receipt != want || !reflect.DeepEqual(snapshot.Turn, settled) {
		t.Fatalf("worker binding did not preserve the canonical coordinator receipt: %v", err)
	}
	if _, purpose, savedBinding, ref := nativeStored(t, s, claim.TurnID); purpose != string(CoordinationRoleCoordinator) || savedBinding.Valid || ref.Valid {
		t.Fatalf("expected coordinator noRef association: purpose=%q, binding=%t, ref=%t", purpose, savedBinding.Valid, ref.Valid)
	}
}
