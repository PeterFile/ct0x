package platform

import (
	"context"

	"agentworkspace/internal/store"
)

// Agent run evidence is a local-user read over visible groups, never a member tool.
func (s *LocalUser) ListAgentRuns(ctx context.Context, memberID string, options store.ListOptions) (store.Page[store.AgentRunSummary], error) {
	return s.db.ListAgentRuns(ctx, memberID, options)
}

func (s *LocalUser) GetAgentRun(ctx context.Context, groupID, runID string, options store.ListOptions) (store.AgentRunDetail, error) {
	return s.db.GetAgentRun(ctx, groupID, runID, options)
}
