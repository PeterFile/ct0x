package coordination

import (
	"context"

	"agentworkspace/internal/store"
)

// AgentRunExplanationSupported is a storage capability, not a grant to publish.
func (e *Engine) AgentRunExplanationSupported(ctx context.Context) (bool, error) {
	return e.store.AgentRunExplanationSupported(ctx)
}

// PublishAgentRunExplanation records a member statement for the bound Turn,
// without publishing to chat or changing execution state.
func (e *Engine) PublishAgentRunExplanation(ctx context.Context, scope store.MemberToolScope, text, key string) (store.AgentRunEntry, error) {
	return e.store.PublishAgentRunExplanation(ctx, scope, text, key)
}
