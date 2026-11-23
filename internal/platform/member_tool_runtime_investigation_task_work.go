package platform

import (
	"context"
	"crypto/sha256"
	"path/filepath"
	"slices"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// InvestigationTaskWorkLaunch is trusted composition, never model input. The
// adapter checks/configures Pi without invoking it. Artifacts, if needed by
// an issued B scope, borrows the existing reader through registry drain.
type InvestigationTaskWorkLaunch struct {
	Supports  func(provider.Provider, provider.PiFreshInvestigationProfile) bool
	Construct func(context.Context, provider.Provider, provider.PiFreshInvestigationBinding) (provider.Executor, error)
	Artifacts *MemberArtifactGrant
	// EnableDiscussionWorkReturn is the separate A7 slice. It never enables
	// initial-owner management or delegation for a structured admission.
	EnableDiscussionWorkReturn bool
	// Commit must call B's CommitInvestigationTaskWorkLaunch. The existing
	// descriptor is reused here only as the shape of the plan; the text Store
	// gate cannot authorize investigation. B calls admit synchronously inside
	// its transaction, and returns nil only after a confirmed commit.
	Commit func(ctx context.Context, launch store.FreshTaskWorkLaunch, admit func() bool) error
}

func NewPiInvestigationTaskWorkLaunch(extensionPath string, timeout time.Duration,
	commit func(context.Context, store.FreshTaskWorkLaunch, func() bool) error) *InvestigationTaskWorkLaunch {
	a := provider.NewPiFreshInvestigationAdapter(extensionPath, timeout)
	return &InvestigationTaskWorkLaunch{Supports: a.Supports, Construct: a.NewExecutor, Commit: commit}
}

// DelegateSupport takes an immutable startup catalog, applying the same member
// and adapter gates as child preparation. It samples no live accounts or tools.
func (l *InvestigationTaskWorkLaunch) DelegateSupport(providers []provider.Provider) InvestigationDelegateSupport {
	if l == nil || l.Supports == nil {
		return nil
	}
	catalog := make(map[string]provider.Provider, len(providers))
	for _, selected := range providers {
		if _, duplicate := catalog[selected.ID]; duplicate {
			return nil
		}
		selected.Evidence = slices.Clone(selected.Evidence)
		catalog[selected.ID] = selected
	}
	supports, artifacts := l.Supports, l.Artifacts != nil
	return func(member domain.Member, profile provider.PiFreshInvestigationProfile) bool {
		return profile.Action == domain.ChatActionReadOnlyInvestigation && profile.SourceKind == domain.TaskWorkSourceDelegation &&
			(artifacts || !slices.Contains(profile.Operations, ReadInvestigationArtifactOperation)) &&
			supportsInvestigationTaskWorkMember(member, catalog[member.ProviderID], profile, supports)
	}
}

func supportsInvestigationTaskWorkMember(member domain.Member, selected provider.Provider, profile provider.PiFreshInvestigationProfile,
	supports func(provider.Provider, provider.PiFreshInvestigationProfile) bool) bool {
	return selected.ID == member.ProviderID && selected.Name == provider.Pi && member.Status == domain.MemberStatusActive &&
		member.Workspace == "" && len(member.Model) <= 256 && memberToolRuntimeText(member.Model) &&
		supports(selected, copyPiFreshInvestigationProfile(profile))
}

func freezeInvestigationTaskWorkLaunch(db *store.Store, launch *InvestigationTaskWorkLaunch) (*InvestigationTaskWorkLaunch, error) {
	if launch == nil || launch.Supports == nil || launch.Construct == nil || launch.Commit == nil {
		return nil, errMemberToolRuntimeConfig
	}
	frozen := *launch
	if launch.Artifacts != nil {
		grant := *launch.Artifacts
		if grant.db != db || grant.open == nil {
			return nil, errMemberToolRuntimeConfig
		}
		frozen.Artifacts = &grant
	}
	return &frozen, nil
}

func copyPiFreshInvestigationProfile(profile provider.PiFreshInvestigationProfile) provider.PiFreshInvestigationProfile {
	profile.Operations = slices.Clone(profile.Operations)
	return profile
}

func (e *taskWorkRun) issueInvestigationTaskWork(ctx context.Context, launch *store.FreshTaskWorkLaunch) (string, error) {
	r := e.base.runtime
	scope, work := launch.Scope, launch.Delivery
	artifacts := r.investigationTaskWork.Artifacts
	if work.SourceKind == domain.TaskWorkSourceDelegation {
		// B derives the delegate from its stored wake; there is no owner Attempt.
		return r.IssueWorkDelegationTools(ctx, scope.Claim, artifacts)
	}
	if !work.SourceKind.IsOwnerContinuation() {
		return "", store.ErrMemberToolDenied
	}
	if err := r.tools.workReportCurrent(ctx, scope, work); err != nil {
		return "", err
	}
	attempt, err := r.tools.workReportEngine.BindTaskWorkContinuation(ctx, scope)
	if err != nil {
		return "", err
	}
	if !memberToolRuntimeText(attempt.ID) || attempt.GroupID != work.GroupID || attempt.TaskID != work.TaskID ||
		attempt.TaskRevision != work.TaskRevision || attempt.TurnID != scope.Claim.TurnID {
		return "", store.ErrMemberToolDenied
	}
	launch.AttemptID = attempt.ID
	role := store.CoordinationToolScope{Member: scope, Provenance: e.plan.provenance, AttemptID: attempt.ID}
	plan := e.plan.investigation
	token, _, err := r.issueFreshInvestigationOwnerTools(ctx, role, work, artifacts, plan.ownerPurpose, plan.invitation)
	return token, err
}

func (p *investigationTaskWorkPreparation) matchesRegistration(entry memberToolRegistration, launch store.FreshTaskWorkLaunch) bool {
	if entry.scope != launch.Scope || entry.role != (store.CoordinationToolScope{}) || entry.candidate != nil ||
		entry.purpose != p.ownerPurpose || !slices.Equal(entry.delegateCandidateIDs, p.candidateIDs) || entry.taskWork != nil || entry.grants.Skills != nil ||
		!slices.Equal(entry.grants.Operations, p.profile.Operations) || (entry.grants.Artifacts != nil) != (len(p.versions) != 0) {
		return false
	}
	d := launch.Delivery
	switch d.SourceKind {
	case domain.TaskWorkSourceDelegation:
		return launch.AttemptID == "" && entry.investigationOwner == nil && entry.workReport != nil &&
			entry.workReport.delivery == d && slices.Equal(entry.workReport.versions, p.versions)
	case domain.TaskWorkSourceFollowup, domain.TaskWorkSourceReport:
		return entry.investigationWorkOwnerValid() && entry.investigationOwner.delivery == d &&
			entry.investigationOwner.role.Provenance.RequestID == launch.RequestID && entry.investigationOwner.role.AttemptID == launch.AttemptID &&
			slices.Equal(entry.investigationOwner.versions, p.versions)
	}
	return false
}

type investigationTaskWorkConfig struct {
	file    *memberToolRuntimeConfigFile
	binding provider.PiFreshInvestigationBinding
	issued  memberToolRegistration
}

// The caller takes ownership of even a partial file before construction or
// final admission, and must revoke/drain before cleaning it up.
func (e *taskWorkRun) createInvestigationTaskWorkConfig(ctx context.Context, launch store.FreshTaskWorkLaunch, token string) (investigationTaskWorkConfig, error) {
	r, plan := e.base.runtime, e.plan.investigation
	issued, found := r.nativeRegistration(token)
	if !found || !plan.matchesRegistration(issued, launch) || !r.rootValid() || ctx.Err() != nil {
		return investigationTaskWorkConfig{}, errMemberToolRuntimeBinding
	}
	profile := plan.profile
	profile.Operations = slices.Clone(issued.grants.Operations)
	file, binding, err := r.writeInvestigationConfig(token, issued.scope.Claim.TurnID, profile)
	return investigationTaskWorkConfig{file: file, binding: binding, issued: issued}, err
}

// The caller already matched the exact registration and owns cleanup of even
// a partial file. No state, grant or candidate IDs enter the private bootstrap.
func (r *MemberToolRuntime) writeInvestigationConfig(token, runID string, profile provider.PiFreshInvestigationProfile) (*memberToolRuntimeConfigFile, provider.PiFreshInvestigationBinding, error) {
	data, digest, err := provider.PiFreshInvestigationConfig(r.endpoint, token, runID, profile)
	if err != nil {
		return nil, provider.PiFreshInvestigationBinding{}, err
	}
	file, err := r.writeConfig(data)
	if err != nil {
		return file, provider.PiFreshInvestigationBinding{}, err
	}
	return file, provider.PiFreshInvestigationBinding{ConfigPath: filepath.Join(r.configRoot, file.name), ConfigSHA256: digest,
		RunID: runID, Profile: profile}, nil
}

// Constructing the exact adapter is still not permission to invoke it.
func (e *taskWorkRun) admitInvestigationTaskWork(ctx context.Context, launch store.FreshTaskWorkLaunch, token string, config investigationTaskWorkConfig,
	owner *memberToolRoleFenceOwner, validRunFiles func() bool) provider.Executor {
	r := e.base.runtime
	cfg := r.investigationTaskWork
	validFiles := func() bool {
		return validRunFiles() && config.file.digestMatches() && r.rootValid() &&
			cfg.Supports(e.plan.selected, copyPiFreshInvestigationProfile(config.binding.Profile))
	}
	if !validFiles() || ctx.Err() != nil {
		return nil
	}
	binding := config.binding
	binding.Profile = copyPiFreshInvestigationProfile(binding.Profile)
	native, err := cfg.Construct(ctx, e.plan.selected, binding)
	if err != nil || nilTaskWorkExecutor(native) {
		return nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, memberToolRuntimeVerificationTimeout)
	defer cancel()
	if !validFiles() || !e.commitInvestigationTaskWork(checkCtx, launch, token, config.issued, owner) ||
		!validFiles() || checkCtx.Err() != nil {
		return nil
	}
	return native
}

func (e *taskWorkRun) commitInvestigationTaskWork(ctx context.Context, launch store.FreshTaskWorkLaunch, token string,
	issued memberToolRegistration, owner *memberToolRoleFenceOwner) bool {
	r := e.base.runtime
	s := r.tools
	// Keep registry -> Store -> memory-only owner ordering. Neither registry
	// nor transaction is held over filesystem checks or native invocation.
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, found := s.capabilities[sha256.Sum256([]byte(token))]
	if !found || entry.investigationOwner != issued.investigationOwner || entry.workReport != issued.workReport ||
		entry.grants.Artifacts != issued.grants.Artifacts || !e.plan.investigation.matchesRegistration(entry, launch) || ctx.Err() != nil {
		return false
	}
	admitCtx, closeAdmission := context.WithCancel(ctx)
	defer closeAdmission()
	admitted := false
	err := r.investigationTaskWork.Commit(admitCtx, launch, func() bool {
		admitted = admitCtx.Err() == nil && owner.beginInvoke()
		return admitted
	})
	// A nil result without a successful one-shot callback is not admission.
	// A consumed owner on any error remains consumed and is never retried.
	return err == nil && admitted && admitCtx.Err() == nil
}
