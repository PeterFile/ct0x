package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"slices"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// issueCoordinationRole is a trusted process-local entry, never a grant option
// on public Issue. Registration is not a startup-readiness or native-execution gate.
func (s *MemberTools) issueCoordinationRole(ctx context.Context, scope store.CoordinationToolScope) (string, error) {
	var operations []string
	switch scope.Provenance.Kind {
	case store.CoordinationRoleCoordinator:
		if scope.Provenance.AssignmentID != "" || scope.AttemptID != "" {
			return "", store.ErrMemberToolDenied
		}
		operations = []string{"list_members", "read_channel", "search_history", "read_history", "decide_dispatch"}
	case store.CoordinationRoleWorker:
		if scope.Provenance.AssignmentID == "" || scope.AttemptID == "" {
			return "", store.ErrMemberToolDenied
		}
		operations = []string{"read_channel", "search_history", "read_history", "submit_result"}
	default:
		return "", store.ErrMemberToolDenied
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.capabilities) >= maxMemberToolRegistrations {
		return "", store.ErrMemberToolDenied
	}
	for _, entry := range s.capabilities {
		if entry.scope.Claim.TurnID == scope.Member.Claim.TurnID {
			return "", store.ErrMemberToolDenied
		}
	}
	state, err := s.db.ResolveCoordinationExecution(ctx, scope.Member, scope.Provenance)
	if err != nil {
		return "", memberRoleError(ctx, err)
	}
	if state.AttemptID != scope.AttemptID {
		return "", store.ErrMemberToolDenied
	}
	token := rand.Text()
	key := sha256.Sum256([]byte(token))
	if _, exists := s.capabilities[key]; exists || token == scope.Member.Claim.Token {
		return "", store.ErrMemberToolDenied
	}
	s.capabilities[key] = memberToolRegistration{scope: scope.Member, role: scope,
		grants: MemberToolGrants{Operations: operations}}
	return token, nil
}

func memberRoleError(ctx context.Context, err error) error {
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, store.ErrMemberToolDenied) {
			return store.ErrMemberToolDenied
		}
	}
	return err
}

// withCoordinationRole excludes revocation through the entire Store command.
// Store owns the same-tx live/provenance fence and replay-before-new-eligibility
// order; neither Bind nor a separate startup-readiness preflight belongs here.
func (s *MemberTools) withCoordinationRole(ctx context.Context, token, operation string, kind store.CoordinationRoleKind, use func(store.CoordinationToolScope) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, found := s.capabilities[sha256.Sum256([]byte(token))]
	if !found || entry.role.Provenance.Kind != kind || entry.role.Member != entry.scope || !slices.Contains(entry.grants.Operations, operation) {
		return store.ErrMemberToolDenied
	}
	// Preserve Store-owned causes and transaction outcome evidence.
	return use(entry.role)
}

func (s *MemberTools) DecideDispatch(ctx context.Context, token string, input store.CoordinatorDecisionInput, key string) (domain.DecisionReceipt, error) {
	var result domain.DecisionReceipt
	err := s.withCoordinationRole(ctx, token, "decide_dispatch", store.CoordinationRoleCoordinator, func(scope store.CoordinationToolScope) error {
		var err error
		result, err = s.db.DecideDispatch(ctx, scope, input, key)
		return err
	})
	if err != nil {
		return domain.DecisionReceipt{}, err
	}
	return result, nil
}

func (s *MemberTools) SubmitAssignmentResult(ctx context.Context, token string, input store.AssignmentResultInput, key string) (domain.Submission, error) {
	var result domain.Submission
	err := s.withCoordinationRole(ctx, token, "submit_result", store.CoordinationRoleWorker, func(scope store.CoordinationToolScope) error {
		var err error
		result, err = s.db.SubmitAssignmentResult(ctx, scope, input, key)
		return err
	})
	if err != nil {
		return domain.Submission{}, err
	}
	return result, nil
}
