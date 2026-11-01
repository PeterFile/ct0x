package platform

import (
	"context"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// SearchHistory delegates literal search and scoped pagination to the store.
func (s *LocalUser) SearchHistory(ctx context.Context, groupID string, query store.HistoryQuery) (store.HistoryPage, error) {
	return s.db.SearchHistory(ctx, groupID, query)
}

// ReadHistory delegates group visibility and exact topic filtering to the store.
func (s *LocalUser) ReadHistory(ctx context.Context, groupID string, topicID *string, messageID string) (domain.Message, error) {
	return s.db.ReadHistory(ctx, groupID, topicID, messageID)
}
