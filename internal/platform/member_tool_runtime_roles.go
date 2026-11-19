package platform

import (
	"context"
	"crypto/sha256"
	"path/filepath"
	"slices"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// WrapCoordinationRole is an explicit trusted process-local entry. It captures
// immutable server selections without claiming, issuing, creating files or calling
// the factory. Role support is opt-in; no failure falls back to ordinary tools.
func (r *MemberToolRuntime) WrapCoordinationRole(binding MemberToolRunBinding, expected store.CoordinationRoleProvenance, factory MemberToolExecutorFactory) (provider.Executor, error) {
	if binding.ProviderName != provider.Pi {
		return nil, errMemberToolRuntimeBinding
	}
	return r.WrapCoordinationRoleWithCapabilities(binding, expected, nil, factory)
}

// WrapCoordinationRoleWithCapabilities freezes server-sampled worker support,
// never grants. The caller must sample adapters before entering this runtime.
// The legacy wrapper supplies no facts and cannot create new assignments.
func (r *MemberToolRuntime) WrapCoordinationRoleWithCapabilities(binding MemberToolRunBinding, expected store.CoordinationRoleProvenance, facts []store.DispatchWorkerCapabilityFact, factory MemberToolExecutorFactory) (provider.Executor, error) {
	return r.wrapCoordinationRole(binding, expected, facts, factory, nil)
}

func (r *MemberToolRuntime) wrapCoordinationRole(binding MemberToolRunBinding, expected store.CoordinationRoleProvenance, facts []store.DispatchWorkerCapabilityFact,
	factory MemberToolExecutorFactory, investigation *investigationRolePreparation) (provider.Executor, error) {
	if r.roleFence == nil || (binding.ProviderName != provider.Pi && binding.ProviderName != provider.Codex) || !memberToolRuntimeText(binding.ProviderID) || len(binding.Model) > 256 || !memberToolRuntimeText(binding.Model) ||
		expected.GroupID != binding.GroupID || expected.MemberID != binding.MemberID || expected.ConversationID != binding.ConversationID || expected.DeliveryID != binding.DeliveryID ||
		expected.SourceKind != "" || expected.SourceID != "" || expected.TaskRevision < 1 || (expected.Kind != store.CoordinationRoleCoordinator && expected.Kind != store.CoordinationRoleWorker) ||
		(expected.Kind == store.CoordinationRoleCoordinator && expected.AssignmentID != "") || (expected.Kind == store.CoordinationRoleWorker && !memberToolRuntimeText(expected.AssignmentID)) {
		return nil, errMemberToolRuntimeBinding
	}
	for _, value := range []string{expected.GroupID, expected.MemberID, expected.ConversationID, expected.DeliveryID, expected.MessageID, expected.RequestID, expected.TaskID} {
		if !memberToolRuntimeText(value) {
			return nil, errMemberToolRuntimeBinding
		}
	}
	if investigation != nil {
		if factory != nil || binding.ProviderName != provider.Pi || investigation.providerID != binding.ProviderID || investigation.request.Model != binding.Model {
			return nil, errMemberToolRuntimeBinding
		}
		frozen := *investigation
		frozen.scope.ArtifactVersions = slices.Clone(frozen.scope.ArtifactVersions)
		if managed := investigation.owner; managed != nil {
			if !r.investigationOwnerLaunchEnabled() || expected.Kind != store.CoordinationRoleWorker || investigation.build != nil ||
				managed.selected.ID != binding.ProviderID || managed.selected.Name != binding.ProviderName || len(facts) != 0 {
				return nil, errMemberToolRuntimeBinding
			}
			copy := *managed
			copy.Operations = slices.Clone(managed.Operations)
			copy.DelegateCandidateIDs = slices.Clone(managed.DelegateCandidateIDs)
			frozen.owner = &copy
		} else if investigation.build == nil {
			return nil, errMemberToolRuntimeBinding
		}
		investigation = &frozen
	} else if factory == nil {
		return nil, errMemberToolRuntimeBinding
	}
	return &memberToolRuntimeRun{runtime: r, binding: binding, factory: factory, role: &expected, investigation: investigation,
		dispatchCapabilities: slices.Clone(facts)}, nil
}

// CheckCoordinationRoleCapacity only performs bounded memory GC and Store reads;
// it is neither a reservation nor overall startup readiness. Preserve error causes.
func (r *MemberToolRuntime) CheckCoordinationRoleCapacity(ctx context.Context) error {
	if r.roleFence == nil {
		return errMemberToolRoleFenceConfig
	}
	return r.roleFence.checkCapacity(ctx)
}

func memberToolRoleOperations(kind store.CoordinationRoleKind) []string {
	if kind == store.CoordinationRoleCoordinator {
		return []string{"list_members", "read_channel", "search_history", "read_history", "decide_dispatch"}
	}
	return []string{"read_channel", "search_history", "read_history", "submit_result"}
}

func (e *memberToolRuntimeRun) roleStartup(ctx context.Context, claim store.TurnClaim) (store.CoordinationToolScope, error) {
	db := e.runtime.tools.db
	turn, err := db.GetTurn(ctx, claim.GroupID, claim.TurnID)
	if err != nil {
		return store.CoordinationToolScope{}, err
	}
	if !memberToolRoleFenceMatches(turn, claim) || !e.fresh(turn) {
		return store.CoordinationToolScope{}, errMemberToolRuntimeBinding
	}
	member, err := db.BindMemberTool(ctx, claim)
	if err != nil {
		return store.CoordinationToolScope{}, err
	}
	if member.MemberID != e.binding.MemberID || member.ConversationID != e.binding.ConversationID || member.TopicID != e.role.TopicID {
		return store.CoordinationToolScope{}, errMemberToolRuntimeBinding
	}
	resolve := db.ResolveCoordinationExecution
	if e.investigation != nil {
		resolve = db.ResolveInvestigationExecution
	}
	state, err := resolve(ctx, member, *e.role)
	if err != nil {
		return store.CoordinationToolScope{}, err
	}
	if state.Provenance != *e.role || state.Blocked != store.CoordinationReady || state.ProviderID != e.binding.ProviderID || state.Model != e.binding.Model {
		return store.CoordinationToolScope{}, errMemberToolRuntimeBinding
	}
	return store.CoordinationToolScope{Member: member, Provenance: state.Provenance, AttemptID: state.AttemptID}, nil
}

func (e *memberToolRuntimeRun) rolePreflight(ctx context.Context, request provider.ExecuteRequest, scope store.CoordinationToolScope, token string, operations []string, config *memberToolRuntimeConfigFile) bool {
	r := e.runtime
	if ctx.Err() != nil || !r.roleFence.rootValid(request.RuntimeDir) || !r.clearRunDirectory(request) || !r.rootValid() || !e.roleConfigValid(config) {
		return false
	}
	// Bound database verification independently of caller cancellation, including
	// pool waits. This does not hard-bound filesystem or registry mutex waits.
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), memberToolRuntimeVerificationTimeout)
	defer cancel()
	current, err := e.roleStartup(checkCtx, scope.Member.Claim)
	if err != nil || current != scope || checkCtx.Err() != nil || ctx.Err() != nil {
		return false
	}
	// Compare the actual registry snapshot, not an ordinary operation or a stale
	// successful Issue. Lock order stays registry -> Store, never fence -> registry.
	r.tools.mu.RLock()
	entry, found := r.tools.capabilities[sha256.Sum256([]byte(token))]
	valid := found && e.matchesRoleRegistration(entry, scope, operations)
	r.tools.mu.RUnlock()
	return valid && r.roleFence.rootValid(request.RuntimeDir) && r.rootValid() && e.roleConfigValid(config) &&
		e.investigation.validateRequest(request) && checkCtx.Err() == nil && ctx.Err() == nil
}

func (e *memberToolRuntimeRun) managedInitialOwner() bool {
	return e.investigation != nil && e.investigation.owner != nil
}

func (e *memberToolRuntimeRun) roleConfigValid(config *memberToolRuntimeConfigFile) bool {
	return e.configValid(config) && (!e.managedInitialOwner() || config.digestMatches())
}

func (e *memberToolRuntimeRun) executeCoordinationRole(ctx context.Context, request provider.ExecuteRequest, onEvent func(provider.Event) error) provider.ExecutionReceipt {
	unknown := e.startupUnknown("角色执行归属或历史结果未知，需对账后再处理")
	r := e.runtime
	if e.used.Swap(true) || !memberToolRuntimeRunID.MatchString(request.RunID) || request.Model != e.binding.Model ||
		!r.roleFence.rootValid(request.RuntimeDir) || !e.investigation.validateRequest(request) {
		return unknown
	}
	turn, err := r.tools.db.GetTurn(ctx, e.binding.GroupID, request.RunID)
	if err != nil || turn.ID != request.RunID || !e.fresh(turn) || !r.clearRunDirectory(request) {
		return unknown
	}
	claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	owner, err := r.roleFence.reserve(ctx, claim, request.RuntimeDir)
	if err != nil {
		return unknown
	}
	var token string
	var config *memberToolRuntimeConfigFile
	defer func() {
		if token != "" {
			r.tools.Revoke(token)
		}
		if config != nil {
			config.cleanup()
		}
		owner.markLocalDone()
	}()
	scope, err := e.roleStartup(ctx, claim)
	if err != nil {
		return unknown
	}
	if e.role.Kind == store.CoordinationRoleWorker {
		binding, err := r.tools.db.BindAssignmentAttempt(ctx, scope)
		if err != nil || binding.GroupID != claim.GroupID || binding.TurnID != claim.TurnID || binding.AssignmentID != e.role.AssignmentID ||
			binding.TaskID != e.role.TaskID || binding.TaskRevision != e.role.TaskRevision || binding.WorkerMemberID != e.binding.MemberID || binding.AttemptID == "" {
			return unknown
		}
		scope.AttemptID = binding.AttemptID
	}
	switch {
	case e.managedInitialOwner():
		// Match the same immutable scope that shaped the #200 preparation.
		// Issuance and the final role transaction independently check live authority.
		current, scopeErr := r.tools.db.GetTaskInvestigationScope(ctx, scope.Provenance.GroupID, scope.Provenance.TaskID)
		if scopeErr != nil || !e.investigation.matchesScope(&current) {
			return unknown
		}
		token, _, err = r.IssueInitialInvestigationOwnerTools(ctx, scope, e.investigation.artifacts)
	case e.investigation != nil:
		token, err = r.tools.issueInvestigationRole(ctx, scope, e.investigation, e.dispatchCapabilities)
	default:
		token, err = r.tools.issueCoordinationRoleWithCapabilities(ctx, scope, e.dispatchCapabilities)
	}
	if err != nil {
		return unknown
	}
	r.tools.mu.RLock()
	entry, found := r.tools.capabilities[sha256.Sum256([]byte(token))]
	operations := slices.Clone(entry.grants.Operations)
	r.tools.mu.RUnlock()
	workspace := slices.Contains(operations, "workspace_write")
	if !found || workspace && e.binding.ProviderName == provider.Codex {
		return unknown
	}
	startupFailure := func() provider.ExecutionReceipt {
		if e.nativeSession != nil {
			return e.startupFailure(ctx, request, claim, token, "")
		}
		return unknown
	}
	var typed provider.PiFreshInvestigationBinding
	if e.managedInitialOwner() {
		config, typed, err = r.writeInvestigationConfig(token, scope.Member.Claim.TurnID, initialInvestigationOwnerProfile(operations))
	} else {
		config, err = r.createConfig(token)
	}
	if err != nil || !e.rolePreflight(ctx, request, scope, token, operations, config) {
		return startupFailure()
	}
	var native provider.Executor
	var session *memberToolNativeTurn
	nativeRequest := request
	if e.investigation != nil {
		if e.managedInitialOwner() {
			selected, launch := e.investigation.owner.selected, r.investigationTaskWork
			if !launch.Supports(selected, copyPiFreshInvestigationProfile(typed.Profile)) || ctx.Err() != nil {
				return startupFailure()
			}
			typed.Profile = copyPiFreshInvestigationProfile(typed.Profile)
			native, err = launch.Construct(ctx, selected, typed)
		} else {
			issued := provider.NativeMemberToolConfig{Path: filepath.Join(r.configRoot, config.name), SHA256: config.digest}
			native, err = e.investigation.build(issued, slices.Clone(operations))
		}
		if err != nil || nilTaskWorkExecutor(native) || !e.commitInvestigationLaunch(ctx, request, scope, token, operations, config, owner) {
			return startupFailure()
		}
	} else {
		native, session, err = e.constructExecutor(ctx, request, claim, token, config, slices.Clone(operations))
		if err != nil || native == nil || e.nativeSession == nil && !e.rolePreflight(ctx, request, scope, token, operations, config) {
			return startupFailure()
		}
		nativeRequest, err = e.executionRequest(request, config)
		if err != nil || !e.commitNativeLaunch(ctx, token, session, owner) {
			return startupFailure()
		}
	}
	// Freeze native evidence independently from the wrapper's metadata/aggregate.
	raw := cloneMemberToolReceipt(native.Execute(ctx, nativeRequest, onEvent))
	// Drain every authorized callback before auditing unresolved filesystem IO.
	// Native success/cancellation is not evidence that file operations settled.
	r.tools.Revoke(token)
	aggregate := raw
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), memberToolRuntimeVerificationTimeout)
	defer cancel()
	if !e.roleHistorical(checkCtx, request, scope) || e.managedInitialOwner() && (raw.NativeRef != nil || ctx.Err() != nil && raw.Outcome != provider.OutcomeUnknown) {
		aggregate.Outcome = provider.OutcomeUnknown
		aggregate.Reason = "角色执行历史身份或执行根无法确认，需对账后再处理"
	} else if raw.Outcome == provider.OutcomeNotStarted {
		current, err := r.tools.db.GetTurn(checkCtx, claim.GroupID, claim.TurnID)
		if err != nil || (current.State != domain.TurnStateStarting && current.State != domain.TurnStateCancelRequested && current.State != domain.TurnStateNotStarted) ||
			current.AcceptedAt != nil || current.NativeSessionID != "" || current.CandidateText != "" || current.CandidateTerminal != "" {
			aggregate.Outcome = provider.OutcomeUnknown
			aggregate.Reason = "本次调用未启动，但整轮未执行证据不足，需对账后再处理"
		}
	}
	if workspace {
		unresolved, err := r.tools.db.HasUnresolvedWorkspaceOperations(checkCtx, claim.GroupID, claim.TurnID)
		if err != nil || unresolved {
			aggregate.Outcome = provider.OutcomeUnknown
			aggregate.Reason = "本轮文件操作结果未确认或无法核验，须保留资源并先对账；原生回执不代表目录操作已完成"
		}
	}
	if workspace || e.investigation != nil {
		aggregate.NativeRef = nil // File and investigation roles never get text-session metadata.
	} else if session != nil {
		aggregate = session.receipt(ctx, e, claim, aggregate)
	} else {
		aggregate = cloneMemberToolReceipt(aggregate)
	}
	r.captureRoleReceipt(e.binding, scope, token, raw, aggregate)
	return aggregate
}

// A first-turn investigation never enters the v23 text-session selector. Use
// the existing one-shot owner, then make the serialized B role/policy check
// the final authority decision. Registry revocation cannot interleave with it.
func (e *memberToolRuntimeRun) commitInvestigationLaunch(ctx context.Context, request provider.ExecuteRequest, scope store.CoordinationToolScope,
	token string, operations []string, config *memberToolRuntimeConfigFile, owner *memberToolRoleFenceOwner) bool {
	r := e.runtime
	validFiles := func() bool {
		return r.roleFence.rootValid(request.RuntimeDir) && r.clearRunDirectory(request) && r.rootValid() &&
			e.roleConfigValid(config) && e.investigation.validateRequest(request)
	}
	if ctx.Err() != nil || !validFiles() || !owner.beginInvoke() {
		return false
	}
	checkCtx, cancel := context.WithTimeout(ctx, memberToolRuntimeVerificationTimeout)
	defer cancel()
	checked := func() bool {
		r.tools.mu.RLock()
		defer r.tools.mu.RUnlock()
		entry, found := r.tools.capabilities[sha256.Sum256([]byte(token))]
		if !found || !e.matchesRoleRegistration(entry, scope, operations) {
			return false
		}
		current, err := e.roleStartup(checkCtx, scope.Member.Claim)
		return err == nil && current == scope && checkCtx.Err() == nil
	}()
	// As for typed wakes, construction and the last authority read do not
	// waive the digest/physical root checks at invocation. No legacy fallback.
	return checked && (!e.managedInitialOwner() || validFiles() && checkCtx.Err() == nil)
}

func (e *memberToolRuntimeRun) matchesRoleRegistration(entry memberToolRegistration, scope store.CoordinationToolScope, operations []string) bool {
	return entry.scope == scope.Member && entry.role == scope && slices.Equal(entry.grants.Operations, operations) &&
		entry.grants.Skills == nil && e.investigation.matchesRegistration(entry) && slices.Equal(entry.dispatchCapabilities, e.dispatchCapabilities)
}

// Evidence acceptance deliberately does NOT call Bind/Resolve/Blocked or inspect
// current policy/provider settings. Revoked permission is not execution evidence.
func (e *memberToolRuntimeRun) roleHistorical(ctx context.Context, request provider.ExecuteRequest, scope store.CoordinationToolScope) bool {
	r := e.runtime
	if !r.roleFence.rootValid(request.RuntimeDir) {
		return false
	}
	turn, err := r.tools.db.GetTurn(ctx, scope.Member.Claim.GroupID, scope.Member.Claim.TurnID)
	return err == nil && memberToolRoleFenceMatches(turn, scope.Member.Claim) &&
		r.roleHistoricalProvenance(ctx, turn, scope.Provenance, scope.AttemptID)
}

// Historical diagnostics validate immutable relations without claiming that a
// recovered token still matches. Execute separately requires its exact claim.
func (r *MemberToolRuntime) roleHistoricalProvenance(ctx context.Context, turn domain.Turn, p store.CoordinationRoleProvenance, attemptID string) bool {
	if (p.Kind != store.CoordinationRoleCoordinator && p.Kind != store.CoordinationRoleWorker) ||
		turn.GroupID != p.GroupID || turn.DeliveryID != p.DeliveryID || turn.MemberID != p.MemberID || turn.ConversationID != p.ConversationID {
		return false
	}
	view, err := r.tools.db.GetDispatchRequest(ctx, p.GroupID, p.RequestID)
	if err != nil || view.Request.GroupID != p.GroupID || view.Request.TaskID != p.TaskID || view.Request.TaskRevision != p.TaskRevision || view.Request.TopicID != p.TopicID {
		return false
	}
	if p.Kind == store.CoordinationRoleCoordinator {
		d := view.State.Delivery
		return d != nil && d.DeliveryID == p.DeliveryID && d.MessageID == p.MessageID && d.ConversationID == p.ConversationID && d.CoordinatorMemberID == p.MemberID && view.Request.CoordinatorMemberID == p.MemberID && p.AssignmentID == "" && attemptID == ""
	}
	assignment, err := r.tools.db.GetAssignment(ctx, p.GroupID, p.AssignmentID)
	if err != nil || assignment.Assignment.RequestID != p.RequestID || assignment.Assignment.TaskID != p.TaskID || assignment.Assignment.WorkerMemberID != p.MemberID {
		return false
	}
	d, b := assignment.Dispatch.Delivery, assignment.Binding
	return d != nil && b != nil && d.DeliveryID == p.DeliveryID && d.MessageID == p.MessageID && d.ConversationID == p.ConversationID && d.WorkerMemberID == p.MemberID &&
		b.TurnID == turn.ID && b.AttemptID == attemptID && b.TaskRevision == p.TaskRevision
}
