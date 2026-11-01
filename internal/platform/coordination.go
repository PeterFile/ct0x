package platform

import (
	"context"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// Coordinations is explicitly local-user only, never a member capability.
// It borrows one Engine and owns no Store, execution, or lifecycle.
type Coordinations struct{ engine *coordination.Engine }

func NewCoordinations(engine *coordination.Engine) *Coordinations { return &Coordinations{engine} }

func (s *Coordinations) RequestWork(ctx context.Context, groupID string, request domain.RequestWorkRequest, key string) (domain.AdmissionReceipt, error) {
	return s.engine.RequestWork(ctx, groupID, request, key)
}

func (s *Coordinations) GetTrigger(ctx context.Context, thread domain.ThreadRef, triggerID string) (domain.Trigger, error) {
	return s.engine.GetTrigger(ctx, thread, triggerID)
}

func (s *Coordinations) ListTriggers(ctx context.Context, thread domain.ThreadRef, options store.ListOptions) (store.Page[domain.Trigger], error) {
	return s.engine.ListTriggers(ctx, thread, options)
}

func (s *Coordinations) GetAdmissionReceipt(ctx context.Context, thread domain.ThreadRef, decisionID string) (domain.AdmissionReceipt, error) {
	return s.engine.GetAdmissionReceipt(ctx, thread, decisionID)
}

func (s *Coordinations) GetTaskCoordination(ctx context.Context, groupID, taskID string) (domain.TaskCoordinationView, error) {
	return s.engine.GetTaskCoordination(ctx, groupID, taskID)
}
