package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"slices"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// issueCoordinationRole is a trusted process-local entry, never a grant option
// on public Issue. Registration is not a startup-readiness or native-execution gate.
func (s *MemberTools) issueCoordinationRole(ctx context.Context, scope store.CoordinationToolScope) (string, error) {
	return s.issueCoordinationRoleWithCapabilities(ctx, scope, nil)
}

func (s *MemberTools) issueCoordinationRoleWithCapabilities(ctx context.Context, scope store.CoordinationToolScope, facts []store.DispatchWorkerCapabilityFact) (string, error) {
	facts = slices.Clone(facts)
	switch scope.Provenance.Kind {
	case store.CoordinationRoleCoordinator:
		if scope.Provenance.AssignmentID != "" || scope.AttemptID != "" {
			return "", store.ErrMemberToolDenied
		}
	case store.CoordinationRoleWorker:
		if scope.Provenance.AssignmentID == "" || scope.AttemptID == "" {
			return "", store.ErrMemberToolDenied
		}
	default:
		return "", store.ErrMemberToolDenied
	}
	operations := memberToolRoleOperations(scope.Provenance.Kind)
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
	if scope.Provenance.Kind == store.CoordinationRoleWorker {
		var work MemberWorkInputGrants
		if s.WorkInputsEnabled() {
			work, err = s.workInputs.WorkInputGrants(ctx, scope)
			if err != nil {
				return "", err
			}
		}
		_, err := s.db.GetTaskWorkspaceScope(ctx, scope.Provenance.GroupID, scope.Provenance.TaskID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
		if err == nil {
			if s.workspace == nil {
				return "", store.ErrMemberToolDenied
			}
			grant, err := s.workspace.openGrantLocked(ctx, scope)
			if err != nil {
				return "", err
			}
			s.workspace.grants[key] = grant
			operations = provider.WorkspaceWorkerToolNames()
		}
		if work.Read {
			operations = append(operations, "read_work_inputs")
		}
		if work.Ack {
			operations = append(operations, "ack_work_input")
		}
		if work.HandBack {
			operations = append(operations, "hand_back_work_input")
		}
	}
	s.capabilities[key] = memberToolRegistration{scope: scope.Member, role: scope,
		grants: MemberToolGrants{Operations: operations}, dispatchCapabilities: facts}
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
	return s.withCoordinationRoleRegistration(token, operation, kind, func(entry memberToolRegistration) error {
		return use(entry.role)
	})
}

func (s *MemberTools) withCoordinationRoleRegistration(token, operation string, kind store.CoordinationRoleKind, use func(memberToolRegistration) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, found := s.capabilities[sha256.Sum256([]byte(token))]
	if !found || entry.role.Provenance.Kind != kind || entry.role.Member != entry.scope || !slices.Contains(entry.grants.Operations, operation) {
		return store.ErrMemberToolDenied
	}
	// Preserve Store-owned causes and transaction outcome evidence.
	return use(entry)
}

func (s *MemberTools) DecideDispatch(ctx context.Context, token string, input store.CoordinatorDecisionInput, key string) (domain.DecisionReceipt, error) {
	var result domain.DecisionReceipt
	err := s.withCoordinationRoleRegistration(token, "decide_dispatch", store.CoordinationRoleCoordinator, func(entry memberToolRegistration) error {
		var err error
		result, err = s.db.DecideDispatchWithCapabilities(ctx, entry.role, input, key, entry.dispatchCapabilities)
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
