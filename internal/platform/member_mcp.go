package platform

import (
	"context"
	"crypto/sha256"
	"slices"

	"agentworkspace/internal/store"
)

// TransportOperations returns a private snapshot for transport discovery, not
// authority for a later call. Native handlers still own their same-tx fences,
// operation/purpose checks and receipt replay order. No identity is exported.
func (s *MemberTools) TransportOperations(ctx context.Context, token string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, found := s.capabilities[sha256.Sum256([]byte(token))]
	if !found {
		return nil, store.ErrMemberToolDenied
	}
	if err := s.db.ValidateMemberTool(ctx, entry.scope); err != nil {
		return nil, memberRoleError(ctx, err)
	}
	if entry.role.Provenance.Kind != "" {
		if entry.role.Member != entry.scope {
			return nil, store.ErrMemberToolDenied
		}
		state, err := s.db.ResolveCoordinationExecution(ctx, entry.scope, entry.role.Provenance)
		if err != nil {
			return nil, err
		}
		if state.AttemptID != entry.role.AttemptID {
			return nil, store.ErrMemberToolDenied
		}
		// Blocked is startup readiness, not historical replay authorization.
	}
	return slices.Clone(entry.grants.Operations), nil
}
