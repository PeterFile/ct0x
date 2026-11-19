package platform

import (
	"context"
	"fmt"
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
		var grant *MemberArtifactGrant
		if slices.Contains(grants.Operations, "read_artifact") {
			grant = grants.Artifacts
		}
		var err error
		page, err = s.db.SearchProjectExperiencesForTool(ctx, scope, grant != nil, query)
		if err == nil {
			err = s.verifyProjectExperienceArtifacts(ctx, scope, grant, page)
		}
		// A committed scope change during blob I/O wins even over a read error.
		if authErr := s.db.ValidateMemberTool(ctx, scope); authErr != nil {
			return authErr
		}
		return err
	})
	if err != nil {
		return ProjectExperiencePage{}, err
	}
	return page, nil
}

// Every check hashes a full blob (up to 64 MiB for an existing grant). Fail the
// page rather than dropping only an unreadable retirement or altering ranking.
const maxProjectExperienceArtifactChecks = 4

func (s *MemberTools) verifyProjectExperienceArtifacts(ctx context.Context, scope store.MemberToolScope, grant *MemberArtifactGrant, page ProjectExperiencePage) error {
	seen := make(map[domain.ArtifactVersionRef]bool)
	var refs []domain.ArtifactVersionRef
	add := func(pin *domain.ProjectExperienceOutcome) {
		if pin != nil && !seen[pin.ArtifactVersionRef] {
			seen[pin.ArtifactVersionRef] = true
			refs = append(refs, pin.ArtifactVersionRef)
		}
	}
	for _, hit := range page.Items {
		add(hit.AcceptedOutcome)
		if hit.Retirement != nil {
			add(hit.Retirement.ReplacementOutcome)
		}
	}
	if len(refs) > maxProjectExperienceArtifactChecks {
		return fmt.Errorf("%w：项目经验包含过多成果版本，请缩小检索范围", store.ErrInvalid)
	}
	for _, ref := range refs {
		if grant == nil {
			return store.ErrMemberToolDenied
		}
		if _, err := s.readMemberArtifact(ctx, scope, grant, MemberArtifactReadInput{
			ArtifactID: ref.ArtifactID, VersionID: ref.VersionID, MaxBytes: 1,
		}); err != nil {
			return err
		}
	}
	return nil
}
