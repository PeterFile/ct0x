package coordination

import (
	"context"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// Engine borrows the Store; its trusted server-local Source is not a model/tool entry point.
type Engine struct {
	store *store.Store
}

func New(s *store.Store) *Engine { return &Engine{store: s} }

func (e *Engine) ReceiveLocalMessage(ctx context.Context, thread domain.ThreadRef, messageID string) (domain.Trigger, error) {
	return e.store.RecordLocalMessageTrigger(ctx, thread, messageID)
}

func (e *Engine) GetTrigger(ctx context.Context, thread domain.ThreadRef, triggerID string) (domain.Trigger, error) {
	return e.store.GetTrigger(ctx, thread, triggerID)
}

func (e *Engine) ListTriggers(ctx context.Context, thread domain.ThreadRef, options store.ListOptions) (store.Page[domain.Trigger], error) {
	return e.store.ListTriggers(ctx, thread, options)
}
