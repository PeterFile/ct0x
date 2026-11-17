//go:build unix

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"agentworkspace/internal/contextpack"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func (p *Preparer) taskWorkspaceForPreparation(ctx context.Context, role store.CoordinationRoleProvenance) (*domain.TaskWorkspaceScope, error) {
	scope, err := p.store.GetTaskWorkspaceScope(ctx, role.GroupID, role.TaskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, preparationResolutionError(ctx, err)
	}
	if scope.TaskRevision != role.TaskRevision || scope.DispatchRequestID != role.RequestID || scope.NodeID != "local" {
		return nil, errPrepareWorkspace
	}
	return &scope, nil
}

func (p *Preparer) workspaceProtectedRoots() []string {
	roots := []string{p.privateRoot}
	if p.privateDataRoot != "" {
		roots = append(roots, p.privateDataRoot)
	}
	if r := p.memberTools; r != nil {
		roots = append(roots, r.configRoot)
		if r.roleFence != nil {
			roots = append(roots, r.roleFence.executionRoot)
		}
	}
	return roots
}

// This is preparation evidence only. Execution re-resolves under the live role
// fence and this Turn's physical resource claim before issuing any file tool.
func (p *Preparer) workspacePreparationGrant(ctx context.Context, scope domain.TaskWorkspaceScope, memberID string) (store.WorkspaceGrantReceipt, string, os.FileInfo, error) {
	current, err := p.store.GetWorkspaceGrant(ctx, scope.GroupID, memberID)
	if err != nil {
		return current, "", nil, err
	}
	for _, grant := range scope.Grants {
		if grant.MemberID == memberID && grant.GrantSchemaVersion == 19 && grant.GrantID == current.ID && grant.Root == current.Root && grant.ResolvedRoot == current.ResolvedRoot && current.ResourceKey == scope.ResourceKey && current.NodeID == scope.NodeID {
			root, info, err := memberWorkspacePhysicalGrant(current, p.workspaceProtectedRoots())
			return current, root, info, err
		}
	}
	return current, "", nil, store.ErrMemberToolDenied
}

func workspaceDispatchFact(member domain.Member, scope domain.TaskWorkspaceScope, grant store.WorkspaceGrantReceipt) store.DispatchWorkerCapabilityFact {
	return store.DispatchWorkerCapabilityFact{GroupID: member.GroupID, MemberID: member.ID, ProviderID: member.ProviderID, Model: member.Model, RequiredProfile: store.DispatchWorkspaceWorkerProfile, TaskID: scope.TaskID, TaskRevision: scope.TaskRevision, ResourceKey: scope.ResourceKey, NodeID: scope.NodeID, GrantID: grant.ID}
}

func addWorkspacePreparationContext(in *contextpack.Input, scope domain.TaskWorkspaceScope, worker bool) error {
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal([]byte(in.Responsibility), &metadata); err != nil {
		return errPrepareRead
	}
	// Do not expose absolute roots or grant/caller selectors to the model.
	wire, err := json.Marshal(struct {
		TaskID       string `json:"task_id"`
		TaskRevision int64  `json:"task_revision"`
		NodeID       string `json:"node_id"`
	}{scope.TaskID, scope.TaskRevision, scope.NodeID})
	if err != nil {
		return errPrepareRead
	}
	metadata["workspace_task"] = wire
	wire, err = json.Marshal(metadata)
	if err != nil {
		return errPrepareRead
	}
	in.Responsibility = string(wire)
	if worker {
		in.Permissions[1] = "本轮角色工具仅有 " + strings.Join(provider.WorkspaceWorkerToolNames(), "、") + "；调用者、群组、会话、话题和任务关系由服务端绑定，模型不能改选身份或授权。"
		in.Permissions[3] = "仅本轮服务端绑定的任务目录可用文件工具；path只能相对路径，workspace_list的根为点号。CWD不是目录权限。写入文本最多8192 UTF-8字节，替换须先读真实SHA256；不允许静默截断。"
		in.Permissions = append(in.Permissions, "必须先完成真实workspace_write，再用其done operation_id发布publish_workspace_artifact；submit_result引用本任务真实发布的精确artifact_id/version_id。文件已写、版本已发布、成果已提交和本机用户接纳是不同事实。pending/unknown或同步失败必须保留原key和资源先对账，不换key重写、不自动回滚；最终文字不能宣称已解决。")
	}
	return nil
}
