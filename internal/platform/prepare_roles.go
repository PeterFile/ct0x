package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	state    store.CoordinationRoleState
	selected provider.Provider
	input    contextpack.Input
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
	selected, ok := p.providers[state.ProviderID]
	if !ok || selected.Name != provider.Pi || !memberToolRuntimeText(selected.ID) ||
		selected.Installation != provider.Installed || selected.Version != "1.4.0" ||
		!filepath.IsAbs(selected.Path) || strings.ContainsRune(selected.Path, 0) ||
		len(member.Model) > 256 || !memberToolRuntimeText(member.Model) {
		return empty, rolePreparationBlocked(state, "unsupported_provider", nil)
	}
	// Keep the explicit preflight and provider validation stats separate. If
	// validation's own stat fails, classify its captured error, not a later path.
	info, err := os.Stat(selected.Path)
	if errors.Is(err, os.ErrNotExist) || err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0) {
		return empty, rolePreparationBlocked(state, "unsupported_provider", nil)
	}
	if err != nil {
		return empty, preparationFailureWithCause(ctx, errPrepareRead, err)
	}
	if err := provider.ValidateExecutor(selected, p.executionTimeout); err != nil {
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
	in, err := rolePreparationContext(state.Provenance, group, member, conversation, trigger, history, p.roleMaxContextBytes)
	if err != nil {
		return empty, err
	}
	return rolePreparationPlan{state: state, selected: selected, input: in}, nil
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
	cwd, key, err := p.workspace(member, conversation)
	if err != nil {
		return empty, roleWorkspacePreparationError(state, err)
	}
	if err := ctx.Err(); err != nil {
		return empty, preparationReadError(ctx, err)
	}
	selected := plan.selected
	executor, err := p.memberTools.WrapCoordinationRole(MemberToolRunBinding{GroupID: group.ID, MemberID: member.ID,
		ConversationID: conversation.ID, DeliveryID: state.Provenance.DeliveryID, ProviderID: selected.ID,
		ProviderName: selected.Name, Model: member.Model}, state.Provenance,
		func(configPath, _ string, names []string) (provider.Executor, error) {
			return provider.NewPiMemberToolExecutorWithTools(selected, p.piMemberToolsExtension, configPath, names, p.executionTimeout)
		})
	if err != nil {
		return empty, preparationFailureWithCause(ctx, errPrepareBinding, err)
	}
	return scheduler.PreparedExecution{Executor: executor, ResourceKeys: []string{key},
		Request: provider.ExecuteRequest{CWD: cwd, Prompt: string(packet), Model: member.Model}}, nil
}
