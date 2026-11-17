package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
		}
	}
	return provider.AdapterSelection{}, false
}

func (p *Preparer) memberToolAdapterRegistry(selection provider.AdapterSelection, configPath string, names []string) (*provider.AdapterRegistry, error) {
	var adapter provider.Adapter
	switch selection.ID {
	case provider.PiMemberToolAdapterID:
		adapter = provider.NewPiMemberToolAdapter(p.piMemberToolsExtension, configPath, names)
	case provider.CodexAppServerAdapterID:
		adapter = provider.NewCodexAppServerMemberToolAdapter(configPath, names)
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

func (p *Preparer) sampleDispatchCandidates(ctx context.Context, groupID, coordinatorID string) ([]store.DispatchWorkerCapabilityFact, error) {
	facts := []store.DispatchWorkerCapabilityFact{}
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
			_, _, err := p.sampleMemberToolMember(member, memberToolRoleOperations(store.CoordinationRoleWorker))
			if errors.Is(err, errMemberToolAdapterUnsupported) {
				continue
			}
			if err != nil {
				return nil, preparationFailureWithCause(ctx, errPrepareRead, err)
			}
			facts = append(facts, store.DispatchWorkerCapabilityFact{GroupID: groupID, MemberID: member.ID, ProviderID: member.ProviderID, Model: member.Model})
		}
		if members.NextCursor == nil {
			return facts, nil
		}
		options.Before = members.NextCursor
	}
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
