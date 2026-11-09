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
	if r.roleFence == nil || binding.ProviderName != provider.Pi || !memberToolRuntimeText(binding.ProviderID) || factory == nil || len(binding.Model) > 256 || !memberToolRuntimeText(binding.Model) ||
		expected.GroupID != binding.GroupID || expected.MemberID != binding.MemberID || expected.ConversationID != binding.ConversationID || expected.DeliveryID != binding.DeliveryID ||
		expected.TaskRevision < 1 || (expected.Kind != store.CoordinationRoleCoordinator && expected.Kind != store.CoordinationRoleWorker) ||
		(expected.Kind == store.CoordinationRoleCoordinator && expected.AssignmentID != "") || (expected.Kind == store.CoordinationRoleWorker && !memberToolRuntimeText(expected.AssignmentID)) {
		return nil, errMemberToolRuntimeBinding
	}
	for _, value := range []string{expected.GroupID, expected.MemberID, expected.ConversationID, expected.DeliveryID, expected.MessageID, expected.RequestID, expected.TaskID} {
		if !memberToolRuntimeText(value) {
			return nil, errMemberToolRuntimeBinding
		}
	}
	return &memberToolRuntimeRun{runtime: r, binding: binding, factory: factory, role: &expected}, nil
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
	state, err := db.ResolveCoordinationExecution(ctx, member, *e.role)
	if err != nil {
		return store.CoordinationToolScope{}, err
	}
	if state.Provenance != *e.role || state.Blocked != store.CoordinationReady || state.ProviderID != e.binding.ProviderID || state.Model != e.binding.Model {
		return store.CoordinationToolScope{}, errMemberToolRuntimeBinding
	}
	return store.CoordinationToolScope{Member: member, Provenance: state.Provenance, AttemptID: state.AttemptID}, nil
}

func (e *memberToolRuntimeRun) rolePreflight(ctx context.Context, request provider.ExecuteRequest, scope store.CoordinationToolScope, token string, config *memberToolRuntimeConfigFile) bool {
	r := e.runtime
	if ctx.Err() != nil || !r.roleFence.rootValid(request.RuntimeDir) || !r.clearRunDirectory(request) || !r.rootValid() || config == nil || !config.valid() {
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
	valid := found && entry.scope == scope.Member && entry.role == scope && slices.Equal(entry.grants.Operations, memberToolRoleOperations(e.role.Kind)) &&
		entry.grants.Skills == nil && entry.grants.Artifacts == nil
	r.tools.mu.RUnlock()
	return valid && r.roleFence.rootValid(request.RuntimeDir) && r.rootValid() && config.valid() && checkCtx.Err() == nil && ctx.Err() == nil
}

func (e *memberToolRuntimeRun) executeCoordinationRole(ctx context.Context, request provider.ExecuteRequest, onEvent func(provider.Event) error) provider.ExecutionReceipt {
	unknown := memberToolRuntimeReceipt(provider.OutcomeUnknown, "角色执行归属或历史结果未知，需对账后再处理")
	r := e.runtime
	if e.used.Swap(true) || !memberToolRuntimeRunID.MatchString(request.RunID) || request.Model != e.binding.Model || !r.roleFence.rootValid(request.RuntimeDir) {
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
	token, err = r.tools.issueCoordinationRole(ctx, scope)
	if err != nil {
		return unknown
	}
	config, err = r.createConfig(token)
	if err != nil || !e.rolePreflight(ctx, request, scope, token, config) {
		return unknown
	}
	native, err := e.factory(filepath.Join(r.configRoot, config.name), claim.TurnID, memberToolRoleOperations(e.role.Kind))
	if err != nil || native == nil || !e.rolePreflight(ctx, request, scope, token, config) || !owner.beginInvoke() {
		return unknown
	}
	// ExecutionReceipt currently contains only values/strings: this assignment
	// copies ALL native evidence before constructing any wrapper aggregate.
	raw := native.Execute(ctx, request, onEvent)
	aggregate := raw
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), memberToolRuntimeVerificationTimeout)
	defer cancel()
	if !e.roleHistorical(checkCtx, request, scope) {
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
	r.captureRoleReceipt(e.binding, scope, token, raw, aggregate)
	return aggregate
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
