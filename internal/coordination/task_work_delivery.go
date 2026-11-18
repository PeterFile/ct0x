package coordination

import (
	"context"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// EnableTaskWorkExecution must be called by trusted startup only once the
// typed worker profile, scoped tools and resolver are installed.
func (e *Engine) EnableTaskWorkExecution() { e.store.EnableTaskWorkExecution() }

// ResolveTaskWorkDelivery peeks at a typed wake in the native delivery queue.
// The runtime must check the live claim (and applicable investigation policy)
// separately before launch. Only ErrNotFound permits the old routing path.
func (e *Engine) ResolveTaskWorkDelivery(ctx context.Context, groupID, deliveryID string) (domain.TaskWorkDelivery, error) {
	return e.store.ResolveTaskWorkDelivery(ctx, groupID, deliveryID)
}

func (e *Engine) GetTaskWorkExecution(ctx context.Context, scope store.MemberToolScope) (domain.TaskWorkDelivery, error) {
	return e.store.GetTaskWorkExecution(ctx, scope)
}

func (e *Engine) BindTaskWorkContinuation(ctx context.Context, scope store.MemberToolScope) (domain.TaskAttempt, error) {
	return e.store.BindTaskWorkContinuation(ctx, scope)
}

// ReadTaskWorkMandate reads the immutable scope for an already bound owner.
func (e *Engine) ReadTaskWorkMandate(ctx context.Context, scope store.MemberToolScope) (domain.Mandate, error) {
	return e.store.ReadTaskWorkMandate(ctx, scope)
}

func (e *Engine) SubmitTaskWorkResult(ctx context.Context, scope store.MemberToolScope, input store.AssignmentResultInput, key string) (domain.Submission, error) {
	return e.store.SubmitTaskWorkResult(ctx, scope, input, key)
}
