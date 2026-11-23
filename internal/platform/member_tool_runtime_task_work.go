package platform

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// FreshTaskWorkLaunch is a trusted construction seam, never a model input.
// Neither callback may start a process. Production installs only the exact Pi
// adapter; the executor must consume the issued config, Turn and tool profile.
type FreshTaskWorkLaunch struct {
	Supports  func(provider.Provider) bool
	Construct func(context.Context, provider.Provider, provider.FreshTaskWorkBinding) (provider.Executor, error)
	// A generic builder/preflight cannot be upgraded to proposal authority.
	taskDecisionAdapter *provider.PiFreshTaskWorkAdapter
}

func NewPiFreshTaskWorkLaunch(extensionPath string, timeout time.Duration) *FreshTaskWorkLaunch {
	a := provider.NewPiFreshTaskWorkAdapter(extensionPath, timeout)
	return &FreshTaskWorkLaunch{Supports: a.Supports, Construct: a.NewExecutor, taskDecisionAdapter: &a}
}

func (l *FreshTaskWorkLaunch) SupportsTaskDecision(selected provider.Provider) bool {
	return l != nil && l.taskDecisionAdapter != nil && l.taskDecisionAdapter.SupportsTaskDecision(selected)
}

type taskWorkRun struct {
	base memberToolRuntimeRun
	plan taskWorkPreparation
}

func (e *taskWorkRun) Execute(ctx context.Context, request provider.ExecuteRequest, onEvent func(provider.Event) error) provider.ExecutionReceipt {
	unknown := memberToolRuntimeReceipt(provider.OutcomeUnknown, "任务续办归属或历史结果未知，需对账后再处理")
	base, r := &e.base, e.base.runtime
	want := e.plan.request
	want.RunID, want.RuntimeDir = request.RunID, request.RuntimeDir
	if base.used.Swap(true) || !memberToolRuntimeRunID.MatchString(request.RunID) || request != want || !r.roleFence.rootValid(request.RuntimeDir) {
		return unknown
	}
	// Hold the actual directory through invocation so its inode cannot be
	// recycled. The native adapter still needs a stable trusted pathname.
	workspace := e.plan.workspace
	cwdUnknown := memberToolRuntimeReceipt(provider.OutcomeUnknown, "任务续办专用目录身份无法确认，未启动原生执行，需对账")
	if !workspace.pathMatches() {
		return cwdUnknown
	}
	cwd, err := os.OpenRoot(workspace.path)
	if err != nil {
		return cwdUnknown
	}
	defer cwd.Close()
	if !workspace.valid(cwd) {
		return cwdUnknown
	}
	s := r.tools
	turn, err := s.db.GetTurn(ctx, base.binding.GroupID, request.RunID)
	if err != nil || turn.ID != request.RunID || !base.fresh(turn) || !r.clearRunDirectory(request) {
		return unknown
	}
	claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	// The Store check fences and verifies this proposed scope before use.
	scope := store.MemberToolScope{Claim: claim, MemberID: turn.MemberID, ConversationID: turn.ConversationID, TopicID: e.plan.delivery.TopicID}
	launch := store.FreshTaskWorkLaunch{Scope: scope, Delivery: e.plan.delivery, RequestID: e.plan.provenance.RequestID,
		ProviderID: base.binding.ProviderID, Model: base.binding.Model, ResourceKey: workspace.key}
	validRunFiles := func() bool {
		return r.roleFence.rootValid(request.RuntimeDir) && r.clearRunDirectory(request) && r.rootValid() && workspace.pathMatches()
	}
	investigation := e.plan.investigation
	verifyUnbound := func() bool {
		checkCtx, cancel := context.WithTimeout(ctx, memberToolRuntimeVerificationTimeout)
		defer cancel()
		if investigation != nil {
			// The text verifier must not stand in for B's typed launch gate.
			return validRunFiles() && checkCtx.Err() == nil
		}
		return validRunFiles() && s.db.VerifyFreshTaskWorkStartup(checkCtx, launch) == nil
	}
	if !verifyUnbound() {
		return unknown
	}
	if investigation == nil {
		if !e.supportsTextProfile() {
			return unknown
		}
	} else if r.investigationTaskWork == nil || !r.investigationTaskWork.Supports(e.plan.selected, copyPiFreshInvestigationProfile(investigation.profile)) {
		return unknown
	}
	owner, err := r.roleFence.reserve(ctx, claim, request.RuntimeDir)
	if err != nil {
		return unknown
	}
	defer owner.markLocalDone()
	if !verifyUnbound() {
		return unknown
	}
	mandate, err := currentTaskWorkMandate(ctx, s.db, e.plan.delivery)
	if err != nil || !reflect.DeepEqual(mandate, e.plan.mandate) {
		return unknown
	}
	var token string
	var bound *taskWorkToolBinding
	if investigation == nil {
		token, bound, err = s.issueFreshTaskWork(ctx, scope, e.plan.delivery, e.plan.taskDecision)
		if err == nil {
			launch.AttemptID = bound.attempt.ID
		}
	} else {
		token, err = e.issueInvestigationTaskWork(ctx, &launch)
	}
	if err != nil {
		return unknown
	}
	var config *memberToolRuntimeConfigFile
	defer func() {
		if token != "" {
			s.Revoke(token)
		}
		if config != nil {
			config.cleanup()
		}
	}()
	validStartupFiles := func() bool { return validRunFiles() && workspace.valid(cwd) }
	var native provider.Executor
	if investigation == nil {
		var digest [sha256.Size]byte
		construct := s.freshTaskWork.Construct
		if grant := bound.decision; grant != nil {
			data, err := provider.PiFreshTaskDecisionConfig(r.endpoint, token, grant.proof, claim.TurnID)
			if err != nil {
				return unknown
			}
			config, err = r.writeConfig(data)
			if err != nil {
				return unknown
			}
			digest, construct = config.digest, s.freshTaskWork.taskDecisionAdapter.NewTaskDecisionExecutor
		} else {
			config, err = r.createConfig(token)
			if err != nil {
				return unknown
			}
			digest = provider.FreshTaskWorkConfigSHA256(r.endpoint, token)
		}
		bootstrap := provider.FreshTaskWorkBinding{
			ConfigPath: filepath.Join(r.configRoot, config.name), ConfigSHA256: digest,
			RunID: claim.TurnID, Operations: bound.operations(),
		}
		validTextFiles := func() bool {
			if bound.decision != nil {
				return config.digestMatches() && validStartupFiles()
			}
			return config.valid() && provider.FreshTaskWorkConfigMatches(bootstrap.ConfigPath, bootstrap.ConfigSHA256) &&
				config.valid() && validStartupFiles()
		}
		if !e.preflight(ctx, launch, token, bound, validTextFiles, nil) {
			return unknown
		}
		native, err = construct(ctx, e.plan.selected, bootstrap)
		if err != nil || nilTaskWorkExecutor(native) || !e.supportsTextProfile() {
			return unknown
		}
		if !e.preflight(ctx, launch, token, bound, validTextFiles, owner) {
			native = nil
		} else if bound.decision != nil {
			// Set only after the final commit, never on the construction/preflight
			// path. The HTTP gate additionally requires durable native acceptance.
			bound.decision.admitted.Store(true)
		}
	} else {
		issued, err := e.createInvestigationTaskWorkConfig(ctx, launch, token)
		config = issued.file
		if err != nil {
			return unknown
		}
		native = e.admitInvestigationTaskWork(ctx, launch, token, issued, owner, validStartupFiles)
	}
	if native == nil {
		if !workspace.valid(cwd) {
			return cwdUnknown
		}
		return unknown
	}
	if investigation != nil && ctx.Err() != nil {
		return unknown
	}
	raw := native.Execute(ctx, request, onEvent)
	s.Revoke(token) // Drain all authorized callbacks before evaluating the receipt.
	token = ""
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), memberToolRuntimeVerificationTimeout)
	defer cancel()
	turn, err = s.db.GetTurn(checkCtx, claim.GroupID, claim.TurnID)
	if err != nil || checkCtx.Err() != nil || !memberToolRoleFenceMatches(turn, claim) ||
		turn.DeliveryID != e.plan.delivery.DeliveryID || turn.ConversationID != scope.ConversationID || turn.MemberID != scope.MemberID ||
		!r.roleFence.rootValid(request.RuntimeDir) ||
		(investigation != nil && (raw.NativeRef != nil || investigation.managed && ctx.Err() != nil && raw.Outcome != provider.OutcomeUnknown)) ||
		(raw.Outcome == provider.OutcomeNotStarted && (ctx.Err() != nil || !base.fresh(turn))) {
		raw.Outcome, raw.Reason = unknown.Outcome, unknown.Reason
	}
	if investigation != nil {
		raw.NativeRef = nil // This profile must not record a #193 session binding.
	}
	// Revoke drained the callbacks. Even a missing native marker or later
	// successful terminal cannot turn an opaque Engine error into settlement.
	if bound != nil && bound.decision != nil && bound.decision.uncertain {
		raw.Outcome, raw.Reason = provider.OutcomeUnknown, ErrTaskDecisionProposalUnknown.Error()
	}
	return raw
}

func (e *taskWorkRun) supportsTextProfile() bool {
	launch := &e.base.runtime.tools.freshTaskWork
	if e.plan.taskDecision {
		return launch.SupportsTaskDecision(e.plan.selected)
	}
	return launch.Supports(e.plan.selected)
}

func nilTaskWorkExecutor(executor provider.Executor) bool {
	if executor == nil {
		return true
	}
	switch value := reflect.ValueOf(executor); value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	}
	return false
}

func (e *taskWorkRun) preflight(ctx context.Context, launch store.FreshTaskWorkLaunch, token string, binding *taskWorkToolBinding, validStartupFiles func() bool, owner *memberToolRoleFenceOwner) bool {
	s, plan := e.base.runtime.tools, e.plan
	checkCtx, cancel := context.WithTimeout(ctx, memberToolRuntimeVerificationTimeout)
	defer cancel()
	if owner == nil {
		// Verify immutable admission/input payloads before construction. The
		// final gate pins their exact B identities and rechecks mutable authority
		// in one transaction instead of rereading through these tool paths.
		mandate, err := s.chatEngine.ReadTaskWorkMandate(checkCtx, launch.Scope)
		if err != nil || !reflect.DeepEqual(mandate, plan.mandate) {
			return false
		}
		input, err := s.chatEngine.GetWorkInputForMember(checkCtx, launch.Scope, plan.delivery.SourceID)
		if err != nil || !pendingExactTaskWorkInput(plan.delivery, input) || s.db.VerifyFreshTaskWorkStartup(checkCtx, launch) != nil {
			return false
		}
	}
	if !validStartupFiles() {
		return false
	}
	checked := func() bool {
		// Same registry -> Store lock order as issue and tool calls. Neither
		// lock survives into file checks or native execution.
		s.mu.RLock()
		defer s.mu.RUnlock()
		entry := s.capabilities[sha256.Sum256([]byte(token))]
		if entry.scope != launch.Scope || entry.taskWork != binding || !entry.taskWorkProfileValid() || checkCtx.Err() != nil {
			return false
		}
		return owner == nil || s.db.CommitFreshTaskWorkLaunch(checkCtx, launch, owner.beginInvoke) == nil
	}()
	// Recheck filesystem identity outside the locks after the launch commit,
	// including the dedicated CWD. Authority can change after commit; live
	// tool checks and cancellation remain independent of launch authorization.
	return checked && (owner == nil || validStartupFiles()) && checkCtx.Err() == nil
}
