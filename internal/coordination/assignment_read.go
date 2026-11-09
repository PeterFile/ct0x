package coordination

import (
	"context"

	"agentworkspace/internal/domain"
)

// These exact reads are for trusted local users, not member tools.
func (e *Engine) GetDispatchDecision(ctx context.Context, groupID, requestID string) (domain.DecisionReceipt, error) {
	return e.store.GetDispatchDecision(ctx, groupID, requestID)
}

func (e *Engine) GetAssignment(ctx context.Context, groupID, assignmentID string) (domain.AssignmentView, error) {
	return e.store.GetAssignment(ctx, groupID, assignmentID)
}
