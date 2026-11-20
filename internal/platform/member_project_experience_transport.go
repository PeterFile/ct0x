package platform

import (
	"context"
	"crypto/sha256"
	"slices"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// The native projection and independently approved pin are frozen together
// at issuance. A zero pin on an enabled projection denotes lookup only.
type memberProjectExperienceTransport struct {
	enabled    bool
	pin        domain.ArtifactVersionRef
	deliveryID string // Nonempty only on the production durable-approval path.
}

type preparedDeliveryArtifactRead struct {
	pin   domain.ArtifactVersionRef
	owner *MemberArtifactGrant
}

// issueFreshPiDeliveryArtifactRead uses the same issuance gate as the
// reviewed projection, but requires the current claim to re-prove the pin
// prepared from the triggering Delivery. Neither preparation nor config is authority.
func (e *memberToolRuntimeRun) issueFreshPiDeliveryArtifactRead(ctx context.Context, claim store.TurnClaim, grants MemberToolGrants) (string, error) {
	purpose := "generic"
	if e.intake {
		purpose = domain.ChatIntakePurpose
		grants = MemberToolGrants{Operations: chatIntakeOperations()}
	}
	approval := e.projectExperienceArtifactRead
	grants.Operations = append(slices.Clone(grants.Operations), provider.ReadArtifactTool)
	grants.Artifacts = approval.owner
	return e.runtime.tools.issue(ctx, claim, grants, purpose, e.intakeSchema,
		memberProjectExperienceTransport{enabled: true, pin: approval.pin, deliveryID: e.binding.DeliveryID})
}

// IssueFreshPiProjectExperience is for trusted fresh Pi composition before
// adapter construction or invocation. It never changes an issued bearer.
// An artifact operation requires a separately approved exact version and the
// existing blob grant. The caller must supply that approval independently of
// any requested pin, lookup result, group membership or current-version pointer.
// An empty approval issues lookup-only authority.
func (s *MemberTools) IssueFreshPiProjectExperience(ctx context.Context, claim store.TurnClaim, grants MemberToolGrants, approvedArtifact domain.ArtifactVersionRef) (string, error) {
	return s.issue(ctx, claim, grants, "generic", memberToolIntakeSchema{}, memberProjectExperienceTransport{enabled: true, pin: approvedArtifact})
}

func (p memberProjectExperienceTransport) permits(operations []string) bool {
	if _, err := provider.PiProjectExperienceToolNames(operations); err != nil {
		return false
	}
	if !slices.Contains(operations, provider.ReadArtifactTool) {
		return p.pin == (domain.ArtifactVersionRef{})
	}
	return store.ValidArtifactReadPin(p.pin)
}

// This rejects recorded launch evidence at issuance, not invocation before an
// event. The trusted runtime issues once, before constructing the adapter.
func (s *MemberTools) projectExperiencePrelaunch(ctx context.Context, scope store.MemberToolScope) bool {
	turn, err := s.db.GetTurn(ctx, scope.Claim.GroupID, scope.Claim.TurnID)
	return err == nil && turn.ClaimToken == scope.Claim.Token && turn.MemberID == scope.MemberID &&
		turn.ConversationID == scope.ConversationID && memberToolTurnPrelaunch(turn)
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
	if !s.projectExperienceTransport(token).enabled {
		return ProjectExperiencePage{}, store.ErrMemberToolDenied
	}
	return s.LookupProjectExperiences(ctx, token, query)
}

// ReadNativeMemberArtifact requires the issued native projection. ReadArtifact
// retains revocation exclusion and rechecks the frozen grant pin, Store and blob.
// Typed investigation uses neither this transport nor its pin.
func (s *MemberTools) ReadNativeMemberArtifact(ctx context.Context, token string, input MemberArtifactReadInput) (MemberArtifactReadResult, error) {
	p := s.projectExperienceTransport(token)
	if !p.enabled || p.pin.ArtifactID == "" || p.pin.ArtifactID != input.ArtifactID || p.pin.VersionID != input.VersionID {
		return MemberArtifactReadResult{}, store.ErrMemberToolDenied
	}
	return s.ReadArtifact(ctx, token, input)
}
