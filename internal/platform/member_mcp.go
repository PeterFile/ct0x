package platform

import (
	"context"
	"crypto/sha256"
	"slices"

	"agentworkspace/internal/store"
)

type MemberToolTransportProfile struct {
	Operations            []string
	ContinueWork          bool
	DiscussionOnlyExcerpt bool
	InvestigationIntake   bool
	InvestigationRole     bool
}

func (s *MemberTools) TransportOperations(ctx context.Context, token string) ([]string, error) {
	profile, err := s.TransportProfile(ctx, token)
	return profile.Operations, err
}

// TransportProfile returns a private snapshot for discovery, not authority for
// a later call. Native handlers retain their same-tx, purpose and replay checks.
func (s *MemberTools) TransportProfile(ctx context.Context, token string) (MemberToolTransportProfile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, found := s.capabilities[sha256.Sum256([]byte(token))]
	if !found {
		return MemberToolTransportProfile{}, store.ErrMemberToolDenied
	}
	if binding := entry.workReport; binding != nil {
		if err := s.workReportCurrent(ctx, entry.scope, binding.delivery); err != nil {
			return MemberToolTransportProfile{}, err
		}
		return MemberToolTransportProfile{Operations: slices.Clone(entry.grants.Operations)}, nil
	}
	if err := s.db.ValidateMemberTool(ctx, entry.scope); err != nil {
		return MemberToolTransportProfile{}, memberRoleError(ctx, err)
	}
	if entry.purpose == memberInvestigationRolePurpose {
		names, err := s.investigationTransportOperations(ctx, entry)
		if err != nil {
			return MemberToolTransportProfile{}, err
		}
		return MemberToolTransportProfile{Operations: names, InvestigationRole: true}, nil
	}
	if entry.role.Provenance.Kind != "" {
		if entry.role.Member != entry.scope {
			return MemberToolTransportProfile{}, store.ErrMemberToolDenied
		}
		state, err := s.db.ResolveCoordinationExecution(ctx, entry.scope, entry.role.Provenance)
		if err != nil {
			return MemberToolTransportProfile{}, err
		}
		if state.AttemptID != entry.role.AttemptID {
			return MemberToolTransportProfile{}, store.ErrMemberToolDenied
		}
		// Blocked is startup readiness, not historical replay authorization.
	}
	return s.textTransportProfile(entry), nil
}

// Derive the MCP schema from the issued entry. This is not an authorization
// check; native text binding and MCP discovery must use the same projection.
func (s *MemberTools) textTransportProfile(entry memberToolRegistration) MemberToolTransportProfile {
	return MemberToolTransportProfile{Operations: slices.Clone(entry.grants.Operations), ContinueWork: s.advertisesContinueWork(entry),
		DiscussionOnlyExcerpt: s.isChatIntake(entry), InvestigationIntake: entry.investigationIntake()}
}
