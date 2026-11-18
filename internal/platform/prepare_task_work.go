package platform

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"agentworkspace/internal/contextpack"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

const taskWorkProfileUnavailable = "typed_profile_unavailable"

// A successful typed peek is evidence of a wake, not permission to start it.
func (p *Preparer) resolveQueuedTaskWork(ctx context.Context, input preparationInput, deliveryID string, coordinationErr error) (preparationInput, error) {
	var empty preparationInput
	work, err := p.store.ResolveTaskWorkDelivery(ctx, input.group.ID, deliveryID)
	if err != nil {
		// Keep the original denial on absent or damaged typed provenance.
		if coordinationErr != nil && (errors.Unwrap(err) == store.ErrNotFound || errors.Unwrap(err) == store.ErrMemberToolDenied) {
			err = coordinationErr
		}
		return empty, preparationResolutionError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return empty, preparationReadError(ctx, err)
	}
	matches := work.GroupID == input.group.ID && work.DeliveryID == deliveryID && work.MemberID == input.member.ID &&
		work.ConversationID == input.conversation.ID && work.MessageID == input.trigger.ID && work.TopicID == input.trigger.TopicID &&
		work.SenderMemberID == input.trigger.SenderID && work.Blocked != "already_claimed"
	prov := taskWorkWakeProvenance(work)
	if coordinationErr != nil {
		if !matches || work.SourceKind != domain.TaskWorkSourceDelegation {
			return empty, preparationResolutionError(ctx, coordinationErr)
		}
		// The delegate has no owner role, but the typed peek alone does not
		// prove that no Turn targets the wake. Verify before overriding denial.
		if err := p.store.VerifyQueuedTaskWorkDelivery(ctx, work); err != nil {
			if err == store.ErrMemberToolDenied {
				err = coordinationErr
			}
			return empty, preparationResolutionError(ctx, err)
		}
	} else {
		state := input.state
		prov.Kind, prov.RequestID = store.CoordinationRoleWorker, state.Provenance.RequestID
		if !matches || work.SourceKind != domain.TaskWorkSourceReport || prov != state.Provenance ||
			state.ProviderID != input.member.ProviderID || state.Model != input.member.Model || state.AttemptID != "" {
			return empty, errPrepareBinding
		}
	}
	input.state.Provenance = prov
	if p.memberTools == nil || p.memberTools.investigationTaskWork == nil {
		input.state.Blocked = taskWorkProfileUnavailable
	} else {
		input.typedWork = &work
		if input.state.Blocked == store.CoordinationReady {
			input.state.Blocked = store.CoordinationReadinessCode(work.Blocked)
		}
	}
	return input, nil
}

func taskWorkWakeProvenance(work domain.TaskWorkDelivery) store.CoordinationRoleProvenance {
	return store.CoordinationRoleProvenance{
		GroupID: work.GroupID, MemberID: work.MemberID, ConversationID: work.ConversationID, MessageID: work.MessageID,
		DeliveryID: work.DeliveryID, TopicID: work.TopicID, SourceKind: work.SourceKind, SourceID: work.SourceID,
		AssignmentID: work.AssignmentID, TaskID: work.TaskID, TaskRevision: work.TaskRevision,
	}
}

type taskWorkPreparation struct {
	delivery      domain.TaskWorkDelivery
	mandate       domain.Mandate
	provenance    store.CoordinationRoleProvenance
	selected      provider.Provider
	request       provider.ExecuteRequest
	workspace     taskWorkWorkspace
	investigation *investigationTaskWorkPreparation // Nil means the existing pure-text profile.
}

func currentTaskWorkMandate(ctx context.Context, db *store.Store, work domain.TaskWorkDelivery) (domain.Mandate, error) {
	view, err := db.GetTaskCoordination(ctx, work.GroupID, work.TaskID)
	if err != nil {
		return domain.Mandate{}, preparationResolutionError(ctx, err)
	}
	c, m := view.Coordination, view.Admission.Mandate
	if c.GroupID != work.GroupID || c.TopicID != work.TopicID || c.TaskID != work.TaskID ||
		c.CurrentMandateID != work.MandateID || c.AuthorityGeneration != work.AuthorityGeneration ||
		m.ID != work.MandateID || m.GroupID != work.GroupID || m.TopicID != work.TopicID ||
		m.TaskID != work.TaskID || m.TaskRevisionAtAdmission != work.TaskRevision {
		return domain.Mandate{}, errPrepareBinding
	}
	return m, nil
}

func (p *Preparer) prepareFreshTaskWork(ctx context.Context, input preparationInput) (scheduler.PreparedExecution, error) {
	plan, err := p.planFreshTaskWork(ctx, input, true)
	if err != nil {
		return scheduler.PreparedExecution{}, err
	}
	d := plan.delivery
	base := memberToolRuntimeRun{runtime: p.memberTools, binding: MemberToolRunBinding{
		GroupID: d.GroupID, MemberID: d.MemberID, ConversationID: d.ConversationID, DeliveryID: d.DeliveryID,
		ProviderID: plan.selected.ID, Model: plan.request.Model,
	}}
	return scheduler.PreparedExecution{Executor: &taskWorkRun{base: base, plan: plan},
		Request: plan.request, ResourceKeys: []string{plan.workspace.key}}, nil
}

func (p *Preparer) planFreshTaskWork(ctx context.Context, input preparationInput, createWorkspace bool) (taskWorkPreparation, error) {
	var empty taskWorkPreparation
	state, r := input.state, p.memberTools
	prov := state.Provenance
	if state.Blocked != store.CoordinationReady {
		return empty, rolePreparationBlocked(state, string(state.Blocked), nil)
	}
	if r != nil && r.investigationTaskWork != nil {
		// Classify the original persisted admission, not the wake text or the
		// current Policy's action. Only B's verified absence permits text planning.
		scope, err := p.store.GetTaskInvestigationScope(ctx, prov.GroupID, prov.TaskID)
		switch {
		case err == nil:
			return p.planInvestigationTaskWork(ctx, input, scope, createWorkspace)
		case ctx.Err() == nil && errors.Is(err, store.ErrNotFound):
			// The existing text plan still requires an admitted text followup.
		case ctx.Err() == nil && errors.Is(err, store.ErrTaskInvestigationNotReady):
			return empty, rolePreparationBlocked(state, "investigation_not_ready", nil)
		default:
			return empty, preparationResolutionError(ctx, err)
		}
	}
	if r == nil || r.roleFence == nil || !r.tools.FreshTaskWorkEnabled() || input.member.Workspace != "" ||
		prov.SourceKind != domain.TaskWorkSourceFollowup {
		return empty, rolePreparationBlocked(state, taskWorkProfileUnavailable, nil)
	}
	if !r.rootValid() {
		return empty, rolePreparationBlocked(state, "transport_root_unavailable", nil)
	}
	selected, ok := p.providers[input.member.ProviderID]
	if !ok || selected.Name != provider.Pi || input.member.Status != domain.MemberStatusActive || len(input.member.Model) > 256 ||
		!memberToolRuntimeText(input.member.Model) || !r.tools.freshTaskWork.Supports(selected) {
		return empty, rolePreparationBlocked(state, "unsupported_provider", nil)
	}
	if err := r.CheckCoordinationRoleCapacity(ctx); err != nil {
		if errors.Is(err, errMemberToolRoleFenceCapacity) || errors.Is(err, errMemberToolRoleFenceConfig) {
			return empty, rolePreparationBlocked(state, "role_runtime_unavailable", nil)
		}
		return empty, preparationFailureWithCause(ctx, errPrepareRead, err)
	}
	// The existing B role resolver classified this as typed. A missing or
	// inconsistent typed delivery is an error, never a legacy fallback.
	work, err := r.tools.chatEngine.ResolveTaskWorkDelivery(ctx, prov.GroupID, prov.DeliveryID)
	if err != nil {
		return empty, preparationResolutionError(ctx, err)
	}
	if work.SourceKind != prov.SourceKind || work.SourceID != prov.SourceID || work.GroupID != prov.GroupID ||
		work.AssignmentID != prov.AssignmentID || work.TaskID != prov.TaskID || work.TaskRevision != prov.TaskRevision ||
		work.MemberID != prov.MemberID || work.OwnerMemberID != prov.MemberID || work.TopicID != prov.TopicID ||
		work.ConversationID != prov.ConversationID || work.DeliveryID != prov.DeliveryID || work.MessageID != prov.MessageID ||
		work.SenderMemberID != input.trigger.SenderID || !memberToolRuntimeText(work.MandateID) || work.AuthorityGeneration < 1 {
		return empty, errPrepareBinding
	}
	if work.Blocked != "" {
		return empty, rolePreparationBlocked(state, work.Blocked, nil)
	}
	mandate, err := currentTaskWorkMandate(ctx, p.store, work)
	if err != nil {
		return empty, err
	}
	view, err := p.store.GetAssignment(ctx, work.GroupID, work.AssignmentID)
	if err != nil {
		return empty, preparationResolutionError(ctx, err)
	}
	original, task := view.Dispatch.Delivery, view.Task
	if view.Request.ID != prov.RequestID || original == nil || view.Binding == nil ||
		original.ConversationID != work.ConversationID || original.DeliveryID == work.DeliveryID || task.GroupID != work.GroupID || task.ID != work.TaskID {
		return empty, errPrepareBinding
	}
	if task.Revision != work.TaskRevision || task.Status != domain.TaskStatusOpen || task.CurrentSubmissionID != "" {
		return empty, rolePreparationBlocked(state, "task_changed", nil)
	}
	admission, err := p.store.GetChatAdmission(ctx, work.GroupID, work.TaskID)
	if err != nil {
		return empty, preparationResolutionError(ctx, err)
	}
	if admission.GroupID != work.GroupID || admission.TopicID != work.TopicID || admission.TaskID != work.TaskID ||
		admission.MandateID != work.MandateID || admission.DispatchRequestID != prov.RequestID {
		return empty, errPrepareBinding
	}
	if admission.Action != domain.ChatActionTextSubmission || admission.WorkspaceScope != nil ||
		admission.Outcome != "admitted" || !admission.AuthorityEvidenceVerified {
		return empty, rolePreparationBlocked(state, "unsupported_work_scope", nil)
	}
	plan := taskWorkPreparation{delivery: work, mandate: mandate, provenance: prov, selected: selected}
	prompt, err := plan.prompt(p.roleMaxContextBytes)
	if err != nil {
		return empty, err
	}
	return p.finishTaskWorkPlan(ctx, state, plan, prompt, input.member.Model, createWorkspace)
}

func (p *Preparer) finishTaskWorkPlan(ctx context.Context, state store.CoordinationRoleState, plan taskWorkPreparation, prompt, model string, createWorkspace bool) (taskWorkPreparation, error) {
	var empty taskWorkPreparation
	var err error
	plan.workspace, err = p.freshTaskWorkWorkspace(ctx, plan.delivery, createWorkspace)
	if ctx.Err() == nil && (errors.Is(err, errPrepareWorkspace) || errors.Is(err, scheduler.ErrNotReady)) {
		return empty, rolePreparationBlocked(state, "typed_private_cwd_unavailable", nil)
	}
	if err != nil {
		return empty, err
	}
	if plan.workspace.key != "" {
		held, err := p.store.HasResourceClaim(ctx, plan.workspace.key)
		if err != nil {
			return empty, preparationFailureWithCause(ctx, errPrepareRead, err)
		}
		if held {
			return empty, rolePreparationBlocked(state, "workspace_resource_held", nil)
		}
	}
	plan.request = provider.ExecuteRequest{CWD: plan.workspace.path, Prompt: prompt, Model: model, Thinking: p.roleThinking}
	return plan, ctx.Err()
}

// Supply only the original Mandate and the B-selected input handle. The
// original user text is retrieved through the claim-scoped UTF-8 segment tool.
func (p taskWorkPreparation) prompt(budget int) (string, error) {
	m, d := p.mandate, p.delivery
	for _, text := range []string{m.Scope.Objective, m.Scope.Description, m.Scope.Acceptance} {
		if !utf8.ValidString(text) {
			return "", errPrepareBinding
		}
	}
	metadata, err := json.Marshal(struct {
		Purpose      string `json:"purpose"`
		InputID      string `json:"required_input_id"`
		TaskID       string `json:"task_id"`
		TaskRevision int64  `json:"task_revision"`
		MandateID    string `json:"mandate_id"`
		Objective    string `json:"objective"`
		Description  string `json:"description"`
		Acceptance   string `json:"acceptance"`
	}{"task_work_text_continuation", d.SourceID, d.TaskID, d.TaskRevision, m.ID, m.Scope.Objective, m.Scope.Description, m.Scope.Acceptance})
	if err != nil {
		return "", errPrepareRead
	}
	packet, err := contextpack.Build(contextpack.Input{Scope: contextpack.Scope{GroupID: d.GroupID, ConversationID: d.ConversationID},
		Responsibility: string(metadata), MaxBytes: budget, Permissions: []string{
			"全新纯文本续办，不恢复旧会话，不读取话题历史、系统上下文、旧工作目录或主机文件。本轮专用目录不代表主机读取沙箱。",
			"使用 read_work_inputs 读取服务器指定的 required_input_id；按 next_offset 取齐原文。constraint 仅表示本条连续原文中的补充，不扩大原授权。",
			"disposition 区分待处理（pending）、已确认处理（confirmed）和已交回（returned）；已交回不提供正文，不得作为继续执行的要求。读取不等于确认。",
			"实际处理后调用 ack_work_input；超出授权时用 hand_back_work_input 说明原因。这只交回一条输入，不交回整项任务。",
			"submit_result 仅提交文本，不传 artifact_versions；未处理输入会阻止提交，提交不等于接纳。未知结果保留原调用标识和内容先对账，不换键重试。",
		}})
	var tooLarge *contextpack.BudgetError
	if errors.As(err, &tooLarge) {
		return "", rolePreparationBlocked(store.CoordinationRoleState{Provenance: p.provenance}, "context_budget", tooLarge)
	}
	if err != nil {
		return "", errPrepareBinding
	}
	return string(packet), nil
}

func pendingExactTaskWorkInput(d domain.TaskWorkDelivery, i domain.TaskWorkInput) bool {
	return i.ID == d.SourceID && i.GroupID == d.GroupID && i.TopicID == d.TopicID && i.TaskID == d.TaskID &&
		i.TaskRevision == d.TaskRevision && i.MandateID == d.MandateID && i.AssignmentID == d.AssignmentID && i.WorkerMemberID == d.MemberID &&
		i.Consumption == nil && i.Return == nil && strings.TrimSpace(i.Content) != "" && utf8.ValidString(i.Content)
}
