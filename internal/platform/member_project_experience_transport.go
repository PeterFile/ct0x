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

// This is frozen when a separate fresh Pi launch registration is issued. The
// approved tuple is independent of the one supplied later to the binder.
type memberProjectExperienceAuthority struct {
	eligible         bool
	approvedArtifact domain.ArtifactVersionRef
}

// IssueFreshPiProjectExperience is for trusted fresh Pi launch composition only.
// An artifact operation requires a separately approved exact version and the
// existing blob grant. The caller must supply that approval independently of
// any requested pin, lookup result, group membership or current-version pointer.
// An empty approval issues lookup-only authority; ordinary startup uses this path.
func (s *MemberTools) IssueFreshPiProjectExperience(ctx context.Context, claim store.TurnClaim, grants MemberToolGrants, approvedArtifact domain.ArtifactVersionRef) (string, error) {
	return s.issue(ctx, claim, grants, "generic", memberToolIntakeSchema{}, memberProjectExperienceAuthority{eligible: true, approvedArtifact: approvedArtifact})
}

func (a memberProjectExperienceAuthority) permits(operations []string) bool {
	if _, err := provider.PiProjectExperienceToolNames(operations); err != nil {
		return false
	}
	if !slices.Contains(operations, provider.ReadArtifactTool) {
		return a.approvedArtifact == (domain.ArtifactVersionRef{})
	}
	return validProjectExperiencePinID(a.approvedArtifact.ArtifactID) && validProjectExperiencePinID(a.approvedArtifact.VersionID)
}

func (s *MemberTools) projectExperiencePrelaunch(ctx context.Context, scope store.MemberToolScope) bool {
	turn, err := s.db.GetTurn(ctx, scope.Claim.GroupID, scope.Claim.TurnID)
	return err == nil && turn.ClaimToken == scope.Claim.Token && turn.MemberID == scope.MemberID &&
		turn.ConversationID == scope.ConversationID && memberToolTurnPrelaunch(turn)
}

// The native projection narrows one existing bearer; it issues no operation
// or blob grant. A zero artifact pin denotes lookup only. It is immutable
// once attached and goes away with the existing registration.
type memberProjectExperienceTransport struct {
	bound bool
	pin   domain.ArtifactVersionRef
}

// BindPiProjectExperience consumes already issued authority for a fresh ordinary
// Pi turn. The supplied pin must match the issuance exactly, even for the first
// bind. Ordinary startup has no artifact approval and always supplies a zero pin.
func (s *MemberTools) BindPiProjectExperience(ctx context.Context, token string, claim store.TurnClaim, pin domain.ArtifactVersionRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sha256.Sum256([]byte(token))
	entry, found := s.capabilities[key]
	authority := entry.projectExperienceAuthority
	if !found || !authority.eligible || entry.scope.Claim != claim || entry.projectExperience.bound ||
		entry.role != (store.CoordinationToolScope{}) || (entry.purpose != "generic" && entry.purpose != domain.ChatIntakePurpose) ||
		pin != authority.approvedArtifact {
		return store.ErrMemberToolDenied
	}
	approved := authority.approvedArtifact
	if approved != (domain.ArtifactVersionRef{}) {
		// Check existence only after checking the independent approval. The
		// issuance already verified the operation and the same-Store blob grant.
		// This is not an I/O/read receipt; ReadArtifact will recheck the scope,
		// tuple and actual blob independently on every call.
		if _, _, err := s.db.ReadArtifactForTool(ctx, entry.scope, approved.ArtifactID, approved.VersionID); err != nil {
			return store.ErrMemberToolDenied
		}
	} else if err := s.db.ValidateMemberTool(ctx, entry.scope); err != nil {
		return err
	}
	if !s.projectExperiencePrelaunch(ctx, entry.scope) {
		return store.ErrMemberToolDenied
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	entry.projectExperience = memberProjectExperienceTransport{bound: true, pin: approved}
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
