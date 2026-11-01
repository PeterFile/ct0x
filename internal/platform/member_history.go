package platform

import (
	"context"

	"agentworkspace/internal/store"
)

// Member history DTOs contain no caller, group, topic or grant selectors.
type MemberHistorySearchInput = store.MemberHistorySearchInput
type MemberHistoryReadInput = store.MemberHistoryReadInput
type MemberHistorySearchResult = store.MemberHistorySearchResult
type MemberHistoryReadResult = store.MemberHistoryReadResult

func (s *MemberTools) SearchHistory(ctx context.Context, token string, input MemberHistorySearchInput) (MemberHistorySearchResult, error) {
	var result MemberHistorySearchResult
	err := s.withAuthorized(ctx, token, "search_history", func(scope store.MemberToolScope, _ MemberToolGrants) error {
		var err error
		result, err = s.db.SearchHistoryForTool(ctx, scope, input)
		return err
	})
	if err != nil {
		return MemberHistorySearchResult{}, err
	}
	return result, nil
}

func (s *MemberTools) ReadHistory(ctx context.Context, token string, input MemberHistoryReadInput) (MemberHistoryReadResult, error) {
	var result MemberHistoryReadResult
	err := s.withAuthorized(ctx, token, "read_history", func(scope store.MemberToolScope, _ MemberToolGrants) error {
		var err error
		result, err = s.db.ReadHistoryForTool(ctx, scope, input)
		return err
	})
	if err != nil {
		return MemberHistoryReadResult{}, err
	}
	return result, nil
}
