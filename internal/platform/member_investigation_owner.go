package platform

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

const (
	DelegateWorkOperation = provider.DelegateWorkTool
	HandbackWorkOperation = provider.HandbackWorkTool

	memberInitialInvestigationOwnerPurpose    = "investigation_initial_owner/v1"
	memberContinuingInvestigationOwnerPurpose = "investigation_continuing_owner/v1"
)

// InvestigationDelegateSupport samples the trusted installed provider for the
// exact fresh child profile. It must be side-effect free, never invoke a
// provider or enter the tool registry, and cover only approved installations.
type InvestigationDelegateSupport func(domain.Member, provider.PiFreshInvestigationProfile) bool

// NewMemberToolRuntimeWithInvestigationOwnerManagement is a separate trusted
// opt-in. It requires Roles and the same Store as the reports Engine. With no
// launcher it only enables explicit postclaim issuance, never native startup.
func NewMemberToolRuntimeWithInvestigationOwnerManagement(db *store.Store, options MemberToolRuntimeOptions, engine *coordination.Engine,
	sample InvestigationDelegateSupport, launches ...*InvestigationTaskWorkLaunch) (*MemberToolRuntime, error) {
	if sample == nil {
		return nil, errMemberToolRuntimeConfig
	}
	r, err := NewMemberToolRuntimeWithTaskWorkReports(db, options, engine, launches...)
	if err != nil {
		return nil, err
	}
	r.tools.investigationDelegateSupport = sample
	return r, nil
}

func (s *MemberTools) InvestigationOwnerManagementEnabled() bool {
	return s != nil && s.investigationDelegateSupport != nil
}

// InvestigationOwnerToolProfile is an issuance snapshot for a trusted caller.
// It describes the HTTP capability, not startup readiness or launch approval.
// Candidate IDs are the provider-supported ceiling, not B's live authorization.
type InvestigationOwnerToolProfile struct {
	Operations           []string
	DelegateCandidateIDs []string
}

// IssueInitialInvestigationOwnerTools binds management to the original scoped
// worker's live, already-bound B Attempt. The existing #200 issuer remains
// unchanged by the runtime opt-in; only this exact startup purpose adds writes.
// Borrow artifacts through revocation, which drains the registered calls.
func (r *MemberToolRuntime) IssueInitialInvestigationOwnerTools(ctx context.Context, role store.CoordinationToolScope, artifacts *MemberArtifactGrant) (string, InvestigationOwnerToolProfile, error) {
	return r.tools.issueInitialInvestigationTools(ctx, role, nil, nil, artifacts)
}

// IssueFreshInvestigationOwnerManagementTools adds the management commands to
// an exact, already-bound report or followup owner. The read-only report and
// delegate issuers cannot issue this purpose. Revoke the token on exit.
func (r *MemberToolRuntime) IssueFreshInvestigationOwnerManagementTools(ctx context.Context, role store.CoordinationToolScope, expected domain.TaskWorkDelivery, artifacts *MemberArtifactGrant) (string, InvestigationOwnerToolProfile, error) {
	return r.issueFreshInvestigationOwnerTools(ctx, role, expected, artifacts, true)
}

func (entry memberToolRegistration) investigationOwnerProfile() InvestigationOwnerToolProfile {
	return InvestigationOwnerToolProfile{Operations: slices.Clone(entry.grants.Operations), DelegateCandidateIDs: slices.Clone(entry.delegateCandidateIDs)}
}

func (r *MemberToolRuntime) investigationOwnerLaunchEnabled() bool {
	return r.investigationTaskWork != nil && r.tools.InvestigationOwnerManagementEnabled()
}

func investigationOwnerOperations(operations, candidates []string, continuing bool) []string {
	if len(candidates) != 0 {
		operations = append(operations, DelegateWorkOperation)
	}
	if continuing {
		operations = append(operations, HandbackWorkOperation)
	}
	return operations
}

func (entry memberToolRegistration) withOwnerManagementOperations(operations []string) []string {
	if entry.purpose == memberInitialInvestigationOwnerPurpose || entry.purpose == memberContinuingInvestigationOwnerPurpose {
		return investigationOwnerOperations(operations, entry.delegateCandidateIDs, entry.purpose == memberContinuingInvestigationOwnerPurpose)
	}
	return operations
}

func investigationOwnerPermissions(candidates []string, continuing bool) []string {
	var permissions []string
	if len(candidates) != 0 {
		permissions = append(permissions, "delegate_work 只能请求 delegate_candidate_ids 中的同伴核对一个问题，材料仅限确切获准的子集；不确定时省略材料。当前授权和余额由平台核验。若要等待报告，不要先提交成果。")
	}
	if continuing {
		permissions = append(permissions, "无法继续整项调查时，用 handback_work 说明原因，通知绑定的协调员；不改派或唤醒。hand_back_work_input 只交回单条输入，两者不可替代。")
	}
	return permissions
}

func (entry memberToolRegistration) initialInvestigationOwnerValid() bool {
	p := entry.role.Provenance
	return entry.purpose == memberInitialInvestigationOwnerPurpose && entry.role.Member == entry.scope &&
		p.Kind == store.CoordinationRoleWorker && p.SourceKind == "" && p.SourceID == "" && validInvestigationRoleScope(entry.role) &&
		entry.taskWork == nil && entry.workReport == nil && entry.investigationOwner == nil
}

// Like investigationDispatchCandidates, use the original request's exact
// PolicyRevision and a bounded configured set, never an unscoped member list.
// This freezes only transport support. B checks the current policy, target,
// material subset, and remaining budget in its own mutation transaction.
func (s *MemberTools) investigationDelegateCandidates(ctx context.Context, role store.CoordinationRoleProvenance, scope domain.TaskInvestigationScope) ([]string, error) {
	view, err := s.db.GetDispatchRequest(ctx, role.GroupID, role.RequestID)
	if err != nil {
		return nil, investigationReadError(ctx, err)
	}
	request := view.Request
	if request.ID != role.RequestID || request.GroupID != role.GroupID || request.TopicID != role.TopicID ||
		request.TaskID != role.TaskID || request.TaskRevision != role.TaskRevision || request.MandateID != scope.MandateID || request.CoordinatorMemberID == "" {
		return nil, store.ErrMemberToolDenied
	}
	policy, err := s.db.GetDispatchPolicy(ctx, role.GroupID)
	if err != nil {
		return nil, investigationReadError(ctx, err)
	}
	if policy.GroupID != role.GroupID || policy.Revision != request.PolicyRevision || len(policy.AllowedWorkerIDs) > investigationMaxDispatchCandidates {
		return nil, store.ErrMemberToolDenied
	}

	// A child can have none, some, or all of the material read operations;
	// sample each possible shape (at most four), always with report_work only
	// as its command. In particular no owner's submit or inbox is inherited.
	reads := investigationReadOperations(scope)
	var candidates []string
	for _, id := range policy.AllowedWorkerIDs {
		if id == role.MemberID || id == request.CoordinatorMemberID {
			continue
		}
		member, err := s.db.GetMember(ctx, role.GroupID, id)
		if errors.Is(err, store.ErrNotFound) && ctx.Err() == nil {
			continue
		}
		if err != nil {
			return nil, investigationReadError(ctx, err)
		}
		if member.GroupID != role.GroupID || member.ID != id {
			return nil, store.ErrMemberToolDenied
		}
		if member.Status == domain.MemberStatusActive && s.supportsInvestigationDelegate(member, reads) {
			candidates = append(candidates, id)
		}
	}
	return candidates, ctx.Err()
}

func (s *MemberTools) supportsInvestigationDelegate(member domain.Member, reads []string) bool {
	for mask := 0; mask < 1<<len(reads); mask++ {
		operations := make([]string, 0, len(reads)+1)
		for i, read := range reads {
			if mask&(1<<i) != 0 {
				operations = append(operations, read)
			}
		}
		if !s.investigationDelegateSupport(member, provider.PiFreshInvestigationProfile{Action: domain.ChatActionReadOnlyInvestigation,
			SourceKind: domain.TaskWorkSourceDelegation, Operations: append(operations, ReportWorkOperation)}) {
			return false
		}
	}
	return true
}

func (s *MemberTools) withInvestigationOwnerManagement(token, op string, use func(memberToolRegistration) error) error {
	if !s.InvestigationOwnerManagementEnabled() {
		return store.ErrMemberToolDenied
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry := s.capabilities[sha256.Sum256([]byte(token))]
	if !slices.Contains(entry.grants.Operations, op) {
		return store.ErrMemberToolDenied
	}
	if !(op == DelegateWorkOperation && entry.initialInvestigationOwnerValid()) &&
		!(entry.purpose == memberContinuingInvestigationOwnerPurpose && entry.investigationWorkOwnerValid()) {
		return store.ErrMemberToolDenied
	}
	// No live readiness preflight here. B re-proves the claim and owner in
	// the write transaction, then reconciles the original key before checking
	// new-command eligibility. Revoke drains this whole call.
	return use(entry)
}

func (s *MemberTools) DelegateWork(ctx context.Context, token string, input domain.DelegateWorkInput, key string) (out domain.TaskWorkDelegation, err error) {
	err = s.withInvestigationOwnerManagement(token, DelegateWorkOperation, func(entry memberToolRegistration) error {
		if !slices.Contains(entry.delegateCandidateIDs, input.MemberID) {
			return store.ErrMemberToolDenied
		}
		out, err = s.workReportEngine.DelegateWork(ctx, entry.scope, input, key)
		return err
	})
	return out, err
}

// HandbackWork records B's whole-task notice, not a return of a WorkInput.
func (s *MemberTools) HandbackWork(ctx context.Context, token string, input domain.HandbackWorkInput, key string) (out domain.TaskWorkHandback, err error) {
	err = s.withInvestigationOwnerManagement(token, HandbackWorkOperation, func(entry memberToolRegistration) error {
		out, err = s.workReportEngine.HandbackWork(ctx, entry.scope, input, key)
		return err
	})
	return out, err
}
