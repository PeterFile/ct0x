package platform

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// FreshTaskWorkLaunch is a trusted construction seam, never a model input.
// Neither callback may start a process. Production installs only the exact Pi
// adapter; the executor must consume the issued config, Turn and tool profile.
type FreshTaskWorkLaunch struct {
	Supports  func(provider.Provider) bool
	Construct func(context.Context, provider.Provider, provider.FreshTaskWorkBinding) (provider.Executor, error)
}

func NewPiFreshTaskWorkLaunch(extensionPath string, timeout time.Duration) *FreshTaskWorkLaunch {
	a := provider.NewPiFreshTaskWorkAdapter(extensionPath, timeout)
	return &FreshTaskWorkLaunch{Supports: a.Supports, Construct: a.NewExecutor}
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
	scope, ok := e.live(ctx, request, claim)
	if !ok || scope.AttemptID != "" || !s.freshTaskWork.Supports(e.plan.selected) {
		return unknown
	}
	owner, err := r.roleFence.reserve(ctx, claim, request.RuntimeDir)
	if err != nil {
		return unknown
	}
	defer owner.markLocalDone()
	if current, ok := e.live(ctx, request, claim); !ok || current != scope {
		return unknown
	}
	mandate, err := currentTaskWorkMandate(ctx, s.db, e.plan.delivery)
	if err != nil || !reflect.DeepEqual(mandate, e.plan.mandate) {
		return unknown
	}
	token, bound, err := s.issueFreshTaskWork(ctx, scope.Member, e.plan.delivery)
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
	config, err = r.createConfig(token)
	if err != nil {
		return unknown
	}
	bootstrap := provider.FreshTaskWorkBinding{
		ConfigPath: filepath.Join(r.configRoot, config.name), ConfigSHA256: provider.FreshTaskWorkConfigSHA256(r.endpoint, token),
		RunID: claim.TurnID, Operations: provider.PiFreshTaskWorkToolNames(),
	}
	validStartupFiles := func() bool {
		return config.valid() && provider.FreshTaskWorkConfigMatches(bootstrap.ConfigPath, bootstrap.ConfigSHA256) &&
			config.valid() && r.rootValid() && workspace.valid(cwd)
	}
	if !e.preflight(ctx, request, scope.Member, token, bound, validStartupFiles, nil) {
		return unknown
	}
	native, err := s.freshTaskWork.Construct(ctx, e.plan.selected, bootstrap)
	if err != nil || nilTaskWorkExecutor(native) || !s.freshTaskWork.Supports(e.plan.selected) {
		return unknown
	}
	if !e.preflight(ctx, request, scope.Member, token, bound, validStartupFiles, owner.beginInvoke) {
		if !workspace.valid(cwd) {
			return cwdUnknown
		}
		return unknown
	}
	raw := native.Execute(ctx, request, onEvent)
	s.Revoke(token) // Drain all authorized callbacks before evaluating the receipt.
	token = ""
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), memberToolRuntimeVerificationTimeout)
	defer cancel()
	turn, err = s.db.GetTurn(checkCtx, claim.GroupID, claim.TurnID)
	if err != nil || checkCtx.Err() != nil || !memberToolRoleFenceMatches(turn, claim) ||
		turn.DeliveryID != e.plan.delivery.DeliveryID || turn.ConversationID != scope.Member.ConversationID || turn.MemberID != scope.Member.MemberID ||
		!r.roleFence.rootValid(request.RuntimeDir) ||
		(raw.Outcome == provider.OutcomeNotStarted && (ctx.Err() != nil || !base.fresh(turn))) {
		raw.Outcome, raw.Reason = unknown.Outcome, unknown.Reason
	}
	return raw
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

func (e *taskWorkRun) live(ctx context.Context, request provider.ExecuteRequest, claim store.TurnClaim) (store.CoordinationToolScope, bool) {
	var empty store.CoordinationToolScope
	ctx, cancel := context.WithTimeout(ctx, memberToolRuntimeVerificationTimeout)
	defer cancel()
	r, plan := e.base.runtime, e.plan
	scope, err := e.base.roleStartup(ctx, claim)
	if err != nil || !taskWorkMatchesScope(scope.Member, plan.delivery) {
		return empty, false
	}
	current, err := r.tools.chatEngine.GetTaskWorkExecution(ctx, scope.Member)
	if err != nil || current != plan.delivery {
		return empty, false
	}
	db := r.tools.db
	member, err := db.GetMember(ctx, plan.delivery.GroupID, plan.delivery.MemberID)
	if err != nil || member.Workspace != "" || member.Status != domain.MemberStatusActive ||
		member.ProviderID != e.base.binding.ProviderID || member.Model != e.base.binding.Model {
		return empty, false
	}
	if request.CWD != plan.workspace.path || !r.roleFence.rootValid(request.RuntimeDir) ||
		!r.clearRunDirectory(request) || !r.rootValid() || db.VerifyFreshTaskWorkResource(ctx, scope.Member, plan.workspace.key) != nil ||
		!plan.workspace.pathMatches() {
		return empty, false
	}
	return scope, ctx.Err() == nil
}

func (e *taskWorkRun) preflight(ctx context.Context, request provider.ExecuteRequest, scope store.MemberToolScope, token string, binding *taskWorkToolBinding, validStartupFiles, invoke func() bool) bool {
	s, plan := e.base.runtime.tools, e.plan
	checkCtx, cancel := context.WithTimeout(ctx, memberToolRuntimeVerificationTimeout)
	defer cancel()
	mandate, err := s.chatEngine.ReadTaskWorkMandate(checkCtx, scope)
	if err != nil || !reflect.DeepEqual(mandate, plan.mandate) {
		return false
	}
	input, err := s.chatEngine.GetWorkInputForMember(checkCtx, scope, plan.delivery.SourceID)
	if err != nil || !pendingExactTaskWorkInput(plan.delivery, input) {
		return false
	}
	if current, ok := e.live(checkCtx, request, scope.Claim); !ok || current.Member != scope || current.AttemptID != binding.attempt.ID || !validStartupFiles() {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry := s.capabilities[sha256.Sum256([]byte(token))]
	return entry.scope == scope && entry.taskWork == binding && entry.taskWorkProfileValid() &&
		checkCtx.Err() == nil && (invoke == nil || invoke())
}
