package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"agentworkspace/internal/contextpack"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

// RolePreparationBlocked describes observed preclaim readiness, not execution
// authorization. Byte metrics exist only for an actual contextpack BudgetError.
// Prepare's separate reads are not one snapshot; Execute must revalidate them.
type RolePreparationBlocked struct {
	Code         string                     `json:"code"`
	Role         store.CoordinationRoleKind `json:"role"`
	DeliveryID   string                     `json:"delivery_id"`
	RequestID    string                     `json:"request_id"`
	AssignmentID string                     `json:"assignment_id,omitempty"`
	Required     int                        `json:"required,omitempty"`
	Limit        int                        `json:"limit,omitempty"`
}

func (e *RolePreparationBlocked) Error() string {
	if e.Code == "context_budget" {
		return fmt.Sprintf("角色上下文预算不足：需要 %d 字节，上限 %d 字节", e.Required, e.Limit)
	}
	return fmt.Sprintf("角色执行准备尚未就绪（%s）", e.Code)
}
func (e *RolePreparationBlocked) Unwrap() error { return scheduler.ErrNotReady }

func rolePreparationBlocked(state store.CoordinationRoleState, code string, budget *contextpack.BudgetError) error {
	p := state.Provenance
	e := &RolePreparationBlocked{Code: code, Role: p.Kind, DeliveryID: p.DeliveryID, RequestID: p.RequestID, AssignmentID: p.AssignmentID}
	if budget != nil {
		e.Required, e.Limit = budget.Required, budget.Limit
	}
	return e
}

// Keep internal causes inspectable without rendering SQL, paths or prompt data.
type preparationFailure struct{ category, cause error }

func (e *preparationFailure) Error() string   { return e.category.Error() }
func (e *preparationFailure) Unwrap() []error { return []error{e.category, e.cause} }

func preparationFailureWithCause(ctx context.Context, category, cause error) error {
	if ctx.Err() != nil {
		cause = ctx.Err()
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		category = preparationReadError(ctx, cause)
	}
	return &preparationFailure{category: category, cause: cause}
}

func preparationResolutionError(ctx context.Context, err error) error {
	category := errPrepareRead
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalid) || errors.Is(err, store.ErrMemberToolDenied) {
		category = errPrepareBinding
	}
	return preparationFailureWithCause(ctx, category, err)
}

func roleExecutorPreparationError(ctx context.Context, state store.CoordinationRoleState, err error) error {
	if ctx.Err() == nil && errors.Is(err, os.ErrNotExist) {
		return rolePreparationBlocked(state, "unsupported_provider", nil)
	}
	return preparationFailureWithCause(ctx, errPrepareRead, err)
}

func roleWorkspacePreparationError(state store.CoordinationRoleState, err error) error {
	if errors.Is(err, scheduler.ErrNotReady) {
		return rolePreparationBlocked(state, "workspace_not_ready", nil)
	}
	return err
}

func validatePreparationRole(state store.CoordinationRoleState, group domain.Group, member domain.Member,
	conversation domain.Conversation, trigger domain.Message, delivery domain.Delivery) error {
	p := state.Provenance
	if p.GroupID != group.ID || p.MemberID != member.ID || p.ConversationID != conversation.ID ||
		p.MessageID != trigger.ID || p.DeliveryID != delivery.ID || p.TopicID != trigger.TopicID ||
		state.ProviderID != member.ProviderID || state.Model != member.Model || state.AttemptID != "" {
		return errPrepareBinding
	}
	if (p.SourceKind != "" || p.SourceID != "") &&
		(p.Kind != store.CoordinationRoleWorker || p.SourceKind != domain.TaskWorkSourceFollowup || !memberToolRuntimeText(p.SourceID)) {
		return errPrepareBinding
	}
	switch p.Kind {
	case store.CoordinationRoleOrdinary:
		if p.RequestID != "" || p.TaskID != "" || p.TaskRevision != 0 || p.AssignmentID != "" ||
			(state.Blocked != store.CoordinationReady && state.Blocked != store.CoordinationMemberInactive) {
			return errPrepareBinding
		}
	case store.CoordinationRoleCoordinator, store.CoordinationRoleWorker:
		for _, id := range []string{p.GroupID, p.MemberID, p.ConversationID, p.MessageID, p.DeliveryID} {
			if !memberToolRuntimeText(id) {
				return errPrepareBinding
			}
		}
		if !memberToolRuntimeText(p.RequestID) || !memberToolRuntimeText(p.TaskID) || p.TaskRevision < 1 ||
			(p.Kind == store.CoordinationRoleCoordinator && p.AssignmentID != "") ||
			(p.Kind == store.CoordinationRoleWorker && !memberToolRuntimeText(p.AssignmentID)) {
			return errPrepareBinding
		}
		if p.SourceKind == domain.TaskWorkSourceFollowup {
			return nil // Typed readiness codes are owned by B, not the legacy role profile.
		}
		switch state.Blocked {
		case store.CoordinationReady, store.CoordinationDependencyNotAccepted, store.CoordinationTaskStale,
			store.CoordinationAuthorityStale, store.CoordinationBindingStale, store.CoordinationPolicyStale,
			store.CoordinationMemberInactive, store.CoordinationAlreadyDecided, store.CoordinationAttemptConflict:
		default:
			return errPrepareBinding
		}
	default:
		return errPrepareBinding
	}
	return nil
}

// The plan and formatter are the single private computation seam for future
// read-only diagnostics. They never promise a later claim or reserve capacity.
type rolePreparationPlan struct {
	state                store.CoordinationRoleState
	selected             provider.Provider
	selection            provider.AdapterSelection
	dispatchCapabilities []store.DispatchWorkerCapabilityFact
	workspace            *domain.TaskWorkspaceScope
	investigation        *investigationRolePreparation
	nativeText           bool
	input                contextpack.Input
}

func (p *Preparer) planCoordinationRole(ctx context.Context, state store.CoordinationRoleState, group domain.Group,
	member domain.Member, conversation domain.Conversation, trigger domain.Message) (rolePreparationPlan, error) {
	var empty rolePreparationPlan
	if err := ctx.Err(); err != nil {
		return empty, preparationReadError(ctx, err)
	}
	if state.Blocked != store.CoordinationReady {
		return empty, rolePreparationBlocked(state, string(state.Blocked), nil)
	}
	r := p.memberTools
	if r == nil || r.roleFence == nil {
		return empty, rolePreparationBlocked(state, "role_disabled", nil)
	}
	if r.tools == nil || r.tools.db != p.store || !r.rootValid() {
		return empty, preparationFailureWithCause(ctx, errPrepareConfig, errMemberToolRuntimeConfig)
	}
	investigation, err := taskInvestigationForRole(ctx, p.store, state.Provenance)
	if errors.Is(err, errInvestigationFirstRoot) {
		return empty, rolePreparationBlocked(state, "investigation_first_root_required", nil)
	}
	if err != nil {
		return empty, preparationResolutionError(ctx, err)
	}
	if investigation != nil {
		return p.planInvestigationRole(ctx, state, member, *investigation)
	}
	workspace, err := p.taskWorkspaceForPreparation(ctx, state.Provenance)
	if err != nil {
		return empty, err
	}
	names := memberToolRoleOperations(state.Provenance.Kind)
	if workspace != nil && state.Provenance.Kind == store.CoordinationRoleWorker {
		if !r.workspaceConfigured() {
			return empty, rolePreparationBlocked(state, "workspace_not_ready", nil)
		}
		if _, _, _, err := p.workspacePreparationGrant(ctx, *workspace, member.ID); err != nil {
			return empty, roleWorkspacePreparationError(state, err)
		}
		names = provider.WorkspaceWorkerToolNames()
	}
	selected, selection, err := p.sampleMemberToolMember(member, names)
	if errors.Is(err, errMemberToolAdapterUnsupported) {
		return empty, rolePreparationBlocked(state, "unsupported_provider", nil)
	}
	if err != nil {
		return empty, roleExecutorPreparationError(ctx, state, err)
	}
	if err := r.CheckCoordinationRoleCapacity(ctx); err != nil {
		if errors.Is(err, errMemberToolRoleFenceCapacity) {
			return empty, rolePreparationBlocked(state, "role_runtime_capacity", nil)
		}
		return empty, preparationFailureWithCause(ctx, errPrepareRead, err)
	}
	history, err := p.store.ListPreparationHistory(ctx, group.ID, trigger.ID, p.roleHistoryLimit)
	if err != nil {
		return empty, preparationResolutionError(ctx, err)
	}
	var facts []store.DispatchWorkerCapabilityFact
	if state.Provenance.Kind == store.CoordinationRoleCoordinator {
		facts, err = p.sampleDispatchCandidates(ctx, group.ID, member.ID, workspace)
		if err != nil {
			return empty, err
		}
	}
	in, err := rolePreparationContext(state.Provenance, group, member, conversation, trigger, history, p.roleMaxContextBytes, facts)
	if err != nil {
		return empty, err
	}
	if workspace != nil {
		if err := addWorkspacePreparationContext(&in, *workspace, state.Provenance.Kind == store.CoordinationRoleWorker); err != nil {
			return empty, err
		}
	}
	if err := p.addRepairRolePreparationContext(ctx, state, &in); err != nil {
		return empty, err
	}
	if selection.Protocol == provider.CodexAppServerProtocol {
		in.Permissions[0] = "仅支持所选 Codex AppServer 版本的全新单轮角色执行；不支持恢复或运行中追加输入。"
		in.Permissions[1] = "本轮角色平台工具仅有 " + strings.Join(memberToolRoleOperations(state.Provenance.Kind), "、") + "；调用者、群组、会话、话题和任务关系由服务端绑定，模型不能改选身份或授权。"
		in.Permissions = append(in.Permissions, codexNativeTextPermission)
	}
	workInputs := state.Provenance.Kind == store.CoordinationRoleWorker && r.tools.WorkInputsEnabled()
	if workInputs {
		in.Permissions[1] = "本轮基础角色工具为 " + strings.Join(names, "、") + "；仅在服务器实际提供工作输入工具时使用。读取不会确认已处理；处理后显式确认，无法处理则交回说明。调用者、群组、会话、话题和任务关系由服务端绑定。"
	}
	var nativeText bool
	// Work-input grants and workspace tasks are outside the frozen text profile.
	if workspace == nil && !workInputs {
		thinking := ""
		if selected.Name == provider.Pi {
			thinking = p.roleThinking
		}
		nativeText, err = p.previewNativeSession(ctx, selected, member, conversation, thinking, names, 0)
		if err != nil {
			return empty, roleWorkspacePreparationError(state, err)
		}
		if nativeText {
			in.Permissions[0] = nativeSessionTextPermission
		}
	}
	return rolePreparationPlan{state: state, selected: selected, selection: selection, dispatchCapabilities: facts, workspace: workspace, nativeText: nativeText, input: in}, nil
}

func (plan rolePreparationPlan) format() ([]byte, error) {
	packet, err := contextpack.Build(plan.input)
	var budget *contextpack.BudgetError
	if errors.As(err, &budget) {
		return nil, rolePreparationBlocked(plan.state, "context_budget", budget)
	}
	if err != nil {
		return nil, preparationFailureWithCause(context.Background(), errPrepareBinding, err)
	}
	return packet, nil
}

func (p *Preparer) prepareCoordinationRole(ctx context.Context, state store.CoordinationRoleState, group domain.Group,
	member domain.Member, conversation domain.Conversation, trigger domain.Message) (scheduler.PreparedExecution, error) {
	var empty scheduler.PreparedExecution
	plan, err := p.planCoordinationRole(ctx, state, group, member, conversation, trigger)
	if ctx.Err() != nil {
		return empty, preparationReadError(ctx, ctx.Err())
	}
	if err != nil {
		return empty, err
	}
	packet, err := plan.format()
	if ctx.Err() != nil {
		return empty, preparationReadError(ctx, ctx.Err())
	}
	if err != nil {
		return empty, err
	}
	if plan.investigation != nil {
		return p.prepareInvestigationRole(ctx, plan, member, string(packet))
	}
	var workspace preparationWorkspace
	if plan.workspace != nil && state.Provenance.Kind == store.CoordinationRoleWorker {
		_, workspace.cwd, _, err = p.workspacePreparationGrant(ctx, *plan.workspace, member.ID)
		workspace.key = plan.workspace.ResourceKey
	} else {
		workspace, err = p.resolvePreparationWorkspaceSnapshot(ctx, member, conversation, true)
	}
	if err != nil {
		return empty, roleWorkspacePreparationError(state, err)
	}
	if err := ctx.Err(); err != nil {
		return empty, preparationReadError(ctx, err)
	}
	selected := plan.selected
	executor, err := p.memberTools.WrapCoordinationRoleWithCapabilities(MemberToolRunBinding{GroupID: group.ID, MemberID: member.ID,
		ConversationID: conversation.ID, DeliveryID: state.Provenance.DeliveryID, ProviderID: selected.ID,
		ProviderName: selected.Name, Model: member.Model}, state.Provenance, plan.dispatchCapabilities,
		p.memberToolExecutorFactory(selected, plan.selection))
	if err != nil {
		return empty, preparationFailureWithCause(ctx, errPrepareBinding, err)
	}
	thinking := ""
	if plan.selection.Protocol == provider.PiExtensionProtocol {
		thinking = p.roleThinking
	}
	request := provider.ExecuteRequest{CWD: workspace.cwd, Prompt: string(packet), Model: member.Model, Thinking: thinking}
	if plan.nativeText {
		if err := p.bindNativeSessionPreparation(executor, selected, plan.selection, member, workspace, thinking, memberToolRoleOperations(state.Provenance.Kind), 0, &request); err != nil {
			return empty, roleWorkspacePreparationError(state, err)
		}
	}
	return scheduler.PreparedExecution{Executor: executor, ResourceKeys: []string{workspace.key}, Request: request}, nil
}
