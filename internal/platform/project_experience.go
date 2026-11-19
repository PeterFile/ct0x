package platform

import (
	"context"
	"slices"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

type SelectProjectExperienceInput = store.SelectProjectExperienceInput
type RetireProjectExperienceInput = store.RetireProjectExperienceInput
type ProjectExperienceQuery = store.ProjectExperienceQuery
type ProjectExperiencePage = store.ProjectExperiencePage

func (s *LocalUser) SelectProjectExperience(ctx context.Context, groupID string, input SelectProjectExperienceInput, key string) (domain.ProjectExperience, error) {
	return s.db.SelectProjectExperience(ctx, groupID, input, key)
}

func (s *LocalUser) RetireProjectExperience(ctx context.Context, groupID, experienceID string, input RetireProjectExperienceInput, key string) (domain.ProjectExperienceRetirement, error) {
	return s.db.RetireProjectExperience(ctx, groupID, experienceID, input, key)
}

func (s *LocalUser) SearchProjectExperiences(ctx context.Context, groupID string, query ProjectExperienceQuery) (ProjectExperiencePage, error) {
	return s.db.SearchProjectExperiences(ctx, groupID, query)
}

// LookupProjectExperiences is an in-process native composition seam after claim
// and capability issuance. It accepts neither identity nor grant selectors and
// is not registered as an HTTP/MCP tool. Lookup is not authorization for later
// source reads; use the unchanged exact readers with their own fences.
func (s *MemberTools) LookupProjectExperiences(ctx context.Context, token string, query ProjectExperienceQuery) (ProjectExperiencePage, error) {
	var page ProjectExperiencePage
	err := s.withAuthorized(ctx, token, "read_history", func(scope store.MemberToolScope, grants MemberToolGrants) error {
		allowArtifacts := grants.Artifacts != nil && slices.Contains(grants.Operations, "read_artifact")
		var err error
		page, err = s.db.SearchProjectExperiencesForTool(ctx, scope, allowArtifacts, query)
		return err
	})
	if err != nil {
		return ProjectExperiencePage{}, err
	}
	return page, nil
}
