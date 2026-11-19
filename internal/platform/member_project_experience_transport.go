package platform

import (
	"context"
	"crypto/sha256"
	"slices"
	"unicode"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// The native projection narrows one existing bearer; it issues no operation
// or blob grant. A zero artifact pin denotes lookup only. It is immutable
// once attached and goes away with the existing registration.
type memberProjectExperienceTransport struct {
	bound bool
	pin   domain.ArtifactVersionRef
}

// BindPiProjectExperience consumes already issued authority for a fresh ordinary
// turn. The exact pin must come from trusted composition, never from lookup,
// group membership, a current-version pointer, or model arguments. Ordinary
// startup has no source of such approval and always supplies a zero pin.
func (s *MemberTools) BindPiProjectExperience(ctx context.Context, token string, claim store.TurnClaim, pin domain.ArtifactVersionRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sha256.Sum256([]byte(token))
	entry, found := s.capabilities[key]
	if !found || entry.scope.Claim != claim || entry.projectExperience.bound || entry.role != (store.CoordinationToolScope{}) ||
		(entry.purpose != "generic" && entry.purpose != domain.ChatIntakePurpose) || !slices.Contains(entry.grants.Operations, "read_history") {
		return store.ErrMemberToolDenied
	}
	if _, err := provider.PiProjectExperienceToolNames(entry.grants.Operations); err != nil {
		return store.ErrMemberToolDenied
	}
	hasArtifact := slices.Contains(entry.grants.Operations, provider.ReadArtifactTool)
	if hasArtifact != (pin != (domain.ArtifactVersionRef{})) {
		return store.ErrMemberToolDenied
	}
	if hasArtifact {
		grant := entry.grants.Artifacts
		if grant == nil || grant.db != s.db || grant.open == nil || !validProjectExperiencePinID(pin.ArtifactID) || !validProjectExperiencePinID(pin.VersionID) {
			return store.ErrMemberToolDenied
		}
		// Check that the exact tuple exists under this real claim in its group.
		// This is not an I/O/read receipt; ReadArtifact will recheck the scope,
		// tuple and actual blob independently on every call.
		if _, _, err := s.db.ReadArtifactForTool(ctx, entry.scope, pin.ArtifactID, pin.VersionID); err != nil {
			return store.ErrMemberToolDenied
		}
	} else if err := s.db.ValidateMemberTool(ctx, entry.scope); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	entry.projectExperience = memberProjectExperienceTransport{bound: true, pin: pin}
	s.capabilities[key] = entry
	return nil
}

func validProjectExperiencePinID(id string) bool {
	if len(id) > 128 || !memberToolRuntimeText(id) {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}

func (s *MemberTools) projectExperienceTransport(token string) memberProjectExperienceTransport {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.capabilities[sha256.Sum256([]byte(token))].projectExperience
}

// LookupNativeProjectExperiences is the native transport only, not a new
// grant for history. The E3 seam rechecks the live claim, history scope,
// source visibility and, independently, any existing material authority.
func (s *MemberTools) LookupNativeProjectExperiences(ctx context.Context, token string, query ProjectExperienceQuery) (ProjectExperiencePage, error) {
	if !s.projectExperienceTransport(token).bound {
		return ProjectExperiencePage{}, store.ErrMemberToolDenied
	}
	return s.LookupProjectExperiences(ctx, token, query)
}

// ReadNativeMemberArtifact consumes a narrower pin than the existing group
// grant. The pin check alone cannot authorize a read: ReadArtifact retains its
// registry/revocation exclusion and exact Store/blob checks. The projection
// cannot change after the check except by deleting the bearer, which also
// denies ReadArtifact. Typed investigation uses neither this nor its pin.
func (s *MemberTools) ReadNativeMemberArtifact(ctx context.Context, token string, input MemberArtifactReadInput) (MemberArtifactReadResult, error) {
	p := s.projectExperienceTransport(token)
	if !p.bound || p.pin.ArtifactID == "" || p.pin.ArtifactID != input.ArtifactID || p.pin.VersionID != input.VersionID {
		return MemberArtifactReadResult{}, store.ErrMemberToolDenied
	}
	return s.ReadArtifact(ctx, token, input)
}
