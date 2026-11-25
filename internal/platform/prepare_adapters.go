package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

var errMemberToolAdapterUnsupported = errors.New("所选适配器的成员工具能力未经确认")

// Selection is immutable trusted process configuration, never member text,
// provider account state or a model-supplied capability claim.
func (p *Preparer) memberToolAdapter(selected provider.Provider) (provider.AdapterSelection, bool) {
	if p.memberTools != nil {
		switch {
		case selected.Name == provider.Pi && p.piMemberToolsExtension != "":
			return provider.AdapterSelection{ID: provider.PiMemberToolAdapterID, Protocol: provider.PiExtensionProtocol}, true
		case selected.Name == provider.Codex && p.enableCodexAppServer:
			return provider.AdapterSelection{ID: provider.CodexAppServerAdapterID, Protocol: provider.CodexAppServerProtocol}, true
		case selected.Name == provider.Agy && p.enableAgyACP:
			return provider.AdapterSelection{ID: provider.AgyACPAdapterID, Protocol: provider.ACPV1Protocol}, true
		}
	}
	return provider.AdapterSelection{}, false
}

func (p *Preparer) memberToolAdapterRegistry(selection provider.AdapterSelection, configPath string, names []string) (*provider.AdapterRegistry, error) {
	var adapter provider.Adapter
	switch selection.ID {
	case provider.PiMemberToolAdapterID:
		if slices.Contains(names, "workspace_write") {
			base := provider.WorkspaceWorkerToolNames()
			if len(names) < len(base) || !slices.Equal(names[:len(base)], base) {
				return nil, errMemberToolAdapterUnsupported
			}
			adapter = provider.NewPiWorkspaceMemberToolAdapter(p.piMemberToolsExtension, configPath, names[len(base):]...)
		} else {
			adapter = provider.NewPiMemberToolAdapter(p.piMemberToolsExtension, configPath, names)
		}
	case provider.CodexAppServerAdapterID:
		adapter = provider.NewCodexAppServerMemberToolAdapter(configPath, names)
	case provider.AgyACPAdapterID:
		adapter = provider.NewAgyACPMemberToolAdapter(configPath, names)
	default:
		return nil, errMemberToolAdapterUnsupported
	}
	return provider.NewAdapterRegistry(provider.AdapterRegistration{ID: selection.ID, Protocol: selection.Protocol, Adapter: adapter})
}

func memberToolCapabilitiesSupported(c provider.AdapterCapabilities) bool {
	return c.Supports(provider.CapabilityPlatformTools) && c.Supports(provider.CapabilityTerminalEvidence)
}

// This predicate is shared by the executing role and unassigned worker samples.
// It has no Store callbacks, CLI invocations, credential or paid model probes.
func (p *Preparer) sampleMemberToolAdapter(selected provider.Provider, names []string) (provider.AdapterSelection, provider.AdapterCapabilities, error) {
	selection, ok := p.memberToolAdapter(selected)
	if !ok || !memberToolRuntimeText(selected.ID) || selected.Installation != provider.Installed ||
		!filepath.IsAbs(selected.Path) || strings.ContainsRune(selected.Path, 0) {
		return selection, provider.AdapterCapabilities{}, errMemberToolAdapterUnsupported
	}
	// Preserve unexpected filesystem causes separately from unsupported metadata.
	info, err := os.Stat(selected.Path)
	if errors.Is(err, os.ErrNotExist) || err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0) {
		return selection, provider.AdapterCapabilities{}, errMemberToolAdapterUnsupported
	}
	if err != nil {
		return selection, provider.AdapterCapabilities{}, err
	}
	registry, err := p.memberToolAdapterRegistry(selection, "", names)
	if err != nil {
		return selection, provider.AdapterCapabilities{}, errMemberToolAdapterUnsupported
	}
	caps, err := registry.Capabilities(selection, selected)
	if err != nil {
		var pathError *os.PathError
		if errors.As(err, &pathError) && !errors.Is(err, os.ErrNotExist) {
			return selection, provider.AdapterCapabilities{}, err
		}
		return selection, provider.AdapterCapabilities{}, errMemberToolAdapterUnsupported
	}
	if !memberToolCapabilitiesSupported(caps) {
		return selection, provider.AdapterCapabilities{}, errMemberToolAdapterUnsupported
	}
	return selection, caps, nil
}

func (p *Preparer) sampleMemberToolMember(member domain.Member, names []string) (provider.Provider, provider.AdapterSelection, error) {
	selected, ok := p.providers[member.ProviderID]
	if !ok || member.Status != domain.MemberStatusActive || len(member.Model) > 256 || !memberToolRuntimeText(member.Model) {
		return selected, provider.AdapterSelection{}, errMemberToolAdapterUnsupported
	}
	selection, _, err := p.sampleMemberToolAdapter(selected, names)
	return selected, selection, err
}

// The factory constructs its config-bound adapter only after Runtime has claimed
// and issued its closed 0600 config. Capabilities themselves never issue grants.
func (p *Preparer) memberToolExecutorFactory(selected provider.Provider, selection provider.AdapterSelection) MemberToolExecutorFactory {
	return func(configPath, _ string, names []string) (provider.Executor, error) {
		registry, err := p.memberToolAdapterRegistry(selection, configPath, names)
		if err != nil {
			return nil, err
		}
		return registry.NewExecutor(selection, selected, p.executionTimeout, provider.CapabilityPlatformTools, provider.CapabilityTerminalEvidence)
	}
}

func (p *Preparer) sampleDispatchCandidates(ctx context.Context, groupID, coordinatorID string, workspace ...*domain.TaskWorkspaceScope) ([]store.DispatchWorkerCapabilityFact, error) {
	facts := []store.DispatchWorkerCapabilityFact{}
	var scope *domain.TaskWorkspaceScope
	if len(workspace) > 0 {
		scope = workspace[0]
	}
	if scope != nil && (p.memberTools == nil || !p.memberTools.workspaceConfigured()) {
		return facts, nil
	}
	options := store.ListOptions{Limit: 200}
	for {
		members, err := p.store.ListMembers(ctx, groupID, options)
		if err != nil {
			return nil, preparationResolutionError(ctx, err)
		}
		for _, member := range members.Items {
			if member.GroupID != groupID || member.ID == coordinatorID {
				continue
			}
			names := memberToolRoleOperations(store.CoordinationRoleWorker)
			var grant store.WorkspaceGrantReceipt
			if scope != nil {
				var err error
				grant, _, _, err = p.workspacePreparationGrant(ctx, *scope, member.ID)
				if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrMemberToolDenied) {
					continue
				}
				if err != nil {
					return nil, preparationResolutionError(ctx, err)
				}
				names = provider.WorkspaceWorkerToolNames()
			}
			_, _, err := p.sampleMemberToolMember(member, names)
			if errors.Is(err, errMemberToolAdapterUnsupported) {
				continue
			}
			if err != nil {
				return nil, preparationFailureWithCause(ctx, errPrepareRead, err)
			}
			fact := store.DispatchWorkerCapabilityFact{GroupID: groupID, MemberID: member.ID, ProviderID: member.ProviderID, Model: member.Model}
			if scope != nil {
				fact = workspaceDispatchFact(member, *scope, grant)
			}
			facts = append(facts, fact)
		}
		if members.NextCursor == nil {
			return facts, nil
		}
		options.Before = members.NextCursor
	}
}

// Automatic text currently composes only the verified Pi text role profiles.
// No directory grants, capacity reservations, or model/credential probes.
func (p *Preparer) sampleAutomaticTextRoles(ctx context.Context, intake domain.Member) store.ChatAutomaticTextFacts {
	var facts store.ChatAutomaticTextFacts
	r := p.memberTools
	if intake.Workspace != "" || r == nil || r.roleFence == nil || !r.rootValid() || !r.roleFence.rootValid(r.roleFence.executionRoot) {
		return facts
	}
	selected, _, err := p.sampleMemberToolMember(intake, memberToolRoleOperations(store.CoordinationRoleCoordinator))
	if err != nil || selected.Name != provider.Pi {
		return facts
	}
	facts.Coordinator = store.DispatchWorkerCapabilityFact{GroupID: intake.GroupID, MemberID: intake.ID, ProviderID: intake.ProviderID, Model: intake.Model}
	workers, err := p.sampleDispatchCandidates(ctx, intake.GroupID, intake.ID)
	if err == nil {
		facts.Workers = slices.DeleteFunc(workers, func(w store.DispatchWorkerCapabilityFact) bool {
			return p.providers[w.ProviderID].Name != provider.Pi
		})
	}
	return facts
}

// AdapterReports describes the actually selected process adapter, not account,
// network, authorization or model readiness. Unknown support is represented nil.
// Every returned map, report and capability pointer is detached from the preparer.
func (p *Preparer) AdapterReports() map[string]ProviderAdapterReport {
	reports := make(map[string]ProviderAdapterReport, len(p.providers))
	for id, selected := range p.providers {
		if selection, enabled := p.memberToolAdapter(selected); enabled {
			report := ProviderAdapterReport{ID: selection.ID, Protocol: selection.Protocol, Enabled: true}
			if _, caps, err := p.sampleMemberToolAdapter(selected, p.memberTools.operations); err == nil {
				if tools := p.nativeReportTools(selected); len(tools) > 0 {
					if registry, err := p.nativeSessionAdapterRegistry(selection, provider.NativeMemberToolConfig{}, tools, nil, nil, nil); err == nil {
						if nativeCaps, err := registry.Capabilities(selection, selected); err == nil {
							caps = nativeCaps
						}
					}
				}
				report.Capabilities = &caps
			}
			reports[id] = report
			continue
		}
		selection := provider.AdapterSelection{Protocol: provider.OneShotProtocol}
		switch selected.Name {
		case provider.Pi:
			selection.ID = provider.PiOneShotAdapterID
		case provider.Codex:
			selection.ID = provider.CodexOneShotAdapterID
		default:
			continue
		}
		report := ProviderAdapterReport{ID: selection.ID, Protocol: selection.Protocol, Enabled: true}
		if caps, err := provider.DefaultAdapterRegistry().Capabilities(selection, selected); err == nil {
			report.Capabilities = &caps
		}
		reports[id] = report
	}
	return reports
}

const codexNativeTextPermission = "实验性 Codex AppServer 仅启用受限只读原生文本配置：无可写目录、审批设为 never；原生 apply_patch 仍可能注册，但写入须被原生安全边界拒绝，不能据此开展文件任务或发布 Artifact。view_image 已关闭，原生文件读取不承诺隔离；继承 MCP 已禁用，唯一平台 MCP 不提供 resources。"

const agyNativeTextPermission = "实验性 Agy ACP 仅启用受限只读原生文本配置：运行于隔离沙箱、独立设置与严格 PreToolUse 工具白名单下；不提供文件系统写作任务或 Artifact 授权，不支持原生恢复或运行中追加输入。"
