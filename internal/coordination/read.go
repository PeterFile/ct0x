package coordination

import (
	"context"

	"agentworkspace/internal/domain"
)

func (e *Engine) GetAdmissionReceipt(ctx context.Context, thread domain.ThreadRef, decisionID string) (domain.AdmissionReceipt, error) {
	return e.store.GetAdmissionReceipt(ctx, thread, decisionID)
}

func (e *Engine) GetTaskCoordination(ctx context.Context, groupID, taskID string) (domain.TaskCoordinationView, error) {
	return e.store.GetTaskCoordination(ctx, groupID, taskID)
}
