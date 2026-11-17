package platform

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"agentworkspace/internal/contextpack"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// The immutable server source selects a reader only. Its label/notice text is
// not authority; Store resolves and verifies the actual Review relationships.
func preparationRepairReviewID(trigger domain.Message) string {
	if trigger.SourceKind != "platform" || !strings.HasPrefix(trigger.SourceID, "review:") {
		return ""
	}
	return strings.TrimPrefix(trigger.SourceID, "review:")
}

func (p *Preparer) checkRepairPreparationReady(ctx context.Context, state store.CoordinationRoleState, trigger domain.Message) error {
	if state.Provenance.Kind != store.CoordinationRoleOrdinary || preparationRepairReviewID(trigger) == "" {
		return nil
	}
	err := p.store.CheckChatRepairIntakeReady(ctx, state.Provenance.GroupID, state.Provenance.DeliveryID)
	if errors.Is(err, store.ErrChatRepairPending) {
		return rolePreparationBlocked(state, "repair_parent_pending", nil)
	}
	if err != nil {
		return preparationResolutionError(ctx, err)
	}
	return nil
}

func (p *Preparer) repairPreparationSource(ctx context.Context, trigger domain.Message) (*domain.ChatRepairSource, error) {
	id := preparationRepairReviewID(trigger)
	if id == "" {
		return nil, nil
	}
	source, err := p.store.GetChatRepairSource(ctx, domain.ThreadRef{GroupID: trigger.GroupID, TopicID: trigger.TopicID}, id)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
		// A legacy/non-current Review may still be discussed. It is not eligible
		// work evidence; runtime separately captures the full historical candidate
		// and admission rechecks current state. Never grant from a notice summary.
		return nil, nil
	}
	if err != nil {
		return nil, preparationResolutionError(ctx, err)
	}
	if source.GroupID != trigger.GroupID || source.TopicID != trigger.TopicID || source.ParentReviewID != id {
		return nil, errPrepareBinding
	}
	for _, text := range []string{source.ReviewReason, source.OriginalAuthorization.Content, source.ParentMandate.Scope.Objective, source.ParentMandate.Scope.Description, source.ParentMandate.Scope.Acceptance} {
		if !utf8.ValidString(text) {
			return nil, errPrepareBinding
		}
	}
	return &source, nil
}

func (p *Preparer) addRepairRolePreparationContext(ctx context.Context, state store.CoordinationRoleState, in *contextpack.Input) error {
	role := state.Provenance
	link, err := p.store.GetChatRepairLink(ctx, role.GroupID, role.TaskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return preparationResolutionError(ctx, err)
	}
	if link.GroupID != role.GroupID || link.TopicID != role.TopicID || link.RepairTaskID != role.TaskID || link.DispatchRequestID != role.RequestID || link.ParentTaskID == link.RepairTaskID || link.ArtifactVersions == nil {
		return errPrepareBinding
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal([]byte(in.Responsibility), &metadata); err != nil {
		return errPrepareBinding
	}
	encoded, err := json.Marshal(link)
	if err != nil {
		return errPrepareBinding
	}
	metadata["repair_link"] = encoded
	encoded, err = json.Marshal(metadata)
	if err != nil {
		return errPrepareBinding
	}
	in.Responsibility = string(encoded)
	in.Permissions = append(in.Permissions,
		"repair_link是已登记的新Task不可变来源，不是重开父Assignment/Attempt或扩大权限；每次执行仍须当前Task、许可与资源围栏。",
		"repair_link.artifact_versions是用户实际拒收的冻结成果基线，不能改成当前head。目录修复须沿这些原Artifact发布，expected_version_id使用对应精确version_id，不另建成果或覆盖旧版；文本修复仍不得引用Artifact。")
	return nil
}
