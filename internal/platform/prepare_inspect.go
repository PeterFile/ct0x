package platform

import (
	"context"
	"errors"
	"time"

	"agentworkspace/internal/store"
)

// RoleReadinessInspection is sampled diagnostic evidence, not an atomic
// snapshot, execution authorization, reservation or promise of a future claim.
// An ordinary delivery is explicitly non-role; Blocked=nil does not certify it.
type RoleReadinessInspection struct {
	ObservedAt      time.Time                  `json:"observed_at"`
	Role            store.CoordinationRoleKind `json:"role"`
	DeliveryID      string                     `json:"delivery_id"`
	RequestID       string                     `json:"request_id,omitempty"`
	AssignmentID    string                     `json:"assignment_id,omitempty"`
	HistoryLimit    int                        `json:"history_limit,omitempty"`
	MaxContextBytes int                        `json:"max_context_bytes,omitempty"`
	Blocked         *RolePreparationBlocked    `json:"blocked,omitempty"`
}

// InspectRoleReadiness is a trusted preclaim diagnostic, not a model tool. It
// shares authority, provider validation, capacity, packet budgets and workspace
// checks without constructing executors, claiming, issuing tokens or creating
// directories. Capacity checks may GC proven-terminal in-memory owners and
// advance their cursor; unknown/cancelled owners remain held. Known blocked is
// a diagnostic with nil error; system/context/identity failures return no value.
func (p *Preparer) InspectRoleReadiness(ctx context.Context, pending store.PendingDelivery) (RoleReadinessInspection, error) {
	var empty RoleReadinessInspection
	observed := time.Now().UTC()
	input, err := p.loadPreparationInput(ctx, pending)
	if err != nil {
		return empty, err
	}
	provenance := input.state.Provenance
	result := RoleReadinessInspection{ObservedAt: observed, Role: provenance.Kind, DeliveryID: provenance.DeliveryID,
		RequestID: provenance.RequestID, AssignmentID: provenance.AssignmentID}
	if provenance.Kind == store.CoordinationRoleOrdinary {
		return roleInspectionResult(ctx, result, nil)
	}
	result.HistoryLimit, result.MaxContextBytes = p.roleHistoryLimit, p.roleMaxContextBytes
	plan, err := p.planCoordinationRole(ctx, input.state, input.group, input.member, input.conversation, input.trigger)
	if ctx.Err() != nil {
		return empty, preparationReadError(ctx, ctx.Err())
	}
	if err != nil {
		return roleInspectionResult(ctx, result, err)
	}
	_, err = plan.format()
	if err != nil || ctx.Err() != nil {
		return roleInspectionResult(ctx, result, err)
	}
	_, _, err = p.resolvePreparationWorkspace(input.member, input.conversation, false)
	return roleInspectionResult(ctx, result, roleWorkspacePreparationError(input.state, err))
}

func roleInspectionResult(ctx context.Context, result RoleReadinessInspection, err error) (RoleReadinessInspection, error) {
	if ctx.Err() != nil {
		return RoleReadinessInspection{}, preparationReadError(ctx, ctx.Err())
	}
	if err == nil {
		return result, nil
	}
	var blocked *RolePreparationBlocked
	if errors.As(err, &blocked) {
		value := *blocked
		result.Blocked = &value
		return result, nil
	}
	return RoleReadinessInspection{}, err
}
