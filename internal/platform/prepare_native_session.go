package platform

import (
	"context"
	"encoding/json"
	"path/filepath"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

const nativeSessionTextPermission = "本轮使用实验性原生文本会话：服务端仅续接同用途最新且合格的引用，否则新建；续接出错不改为新会话重试，不支持运行中追加。身份、工具和权限只以本轮实际发证为准。"

func validNativePiThinking(thinking string) bool {
	switch thinking {
	case "off", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

func (p *Preparer) piNativeSessionConfigured(selected provider.Provider) bool {
	return p.enableNativeSessionResume && selected.Name == provider.Pi &&
		nativeSessionWorkspacePath(p.memberTools.executionRoot.resolvedRoot)
}

func (p *Preparer) nativeSessionEligible(selected provider.Provider, member domain.Member, thinking string) bool {
	// Legacy grants select and preserve the raw configured root. Do not clean
	// it into a different authority spelling just to fit the v23 profile.
	return p.piNativeSessionConfigured(selected) && (member.Workspace == "" || nativeSessionWorkspacePath(member.Workspace)) &&
		provider.PiNativeTextSessionProfileSupported(selected, member.Model, thinking) && provider.PiNativeSessionBridgeSupported(p.piMemberToolsExtension)
}

func nativeSessionWorkspacePath(path string) bool {
	return len(path) <= 4096 && memberToolRuntimeText(path) && filepath.IsAbs(path) && filepath.Clean(path) == path
}

// Use the Store's actual profile contract, not just a check on the configured
// workspace spelling. Tools/capacity here are static expectations for readiness;
// the live binding still takes its authority and tools from actual issuance.
func nativeSessionStaticProfile(selected provider.Provider, member domain.Member, workspace preparationWorkspace, thinking string, names []string, maxMembers int) *store.NativeRunProfile {
	profile := store.NativeRunProfile{ProviderID: selected.ID, Model: member.Model, Thinking: thinking,
		CLIPath: selected.Path, CLIVersion: selected.Version, CWD: workspace.cwd, ConfiguredWorkspace: member.Workspace,
		WorkspaceIdentity: workspace.key, ResourceKeys: []string{workspace.key}, Tools: names, MaxGroupMembers: maxMembers,
		Adapter: provider.PiNativeSessionAdapterVersion, Protocol: provider.PiNativeSessionProtocolVersion,
		NativePolicy: store.NativeTextPolicy{Sandbox: "tools-only", ApprovalPolicy: "never"}}
	if grant := workspace.grant; grant.ID != "" {
		profile.WorkspaceGrantID, profile.WorkspaceRoot, profile.WorkspaceResolvedRoot = grant.ID, grant.Root, grant.ResolvedRoot
		switch grant.Scope {
		case "read_only":
			profile.WorkspaceGrantVersion = 15
		case "read_write":
			profile.WorkspaceGrantVersion = 19
		default:
			return nil
		}
	}
	if _, err := store.NativeRunProfileSHA256(profile); err != nil {
		return nil
	}
	return &profile
}

func physicalNativeWorkspace(workspace preparationWorkspace) (preparationWorkspace, error) {
	cwd, err := filepath.EvalSymlinks(workspace.cwd)
	if err != nil {
		return workspace, errPrepareWorkspace
	}
	keys, err := scheduler.WorkspaceResourceKeys([]string{cwd})
	if err != nil || keys[0] != workspace.key {
		return workspace, errPrepareWorkspace
	}
	workspace.cwd = cwd
	return workspace, nil
}

// Choose permissions before building the packet, without creating any private
// directory on a budget failure or during role inspection. For a missing private
// scope only the path and key grammar are projected; no identity is reserved.
func (p *Preparer) previewNativeSession(ctx context.Context, selected provider.Provider, member domain.Member, conversation domain.Conversation, thinking string, names []string, maxMembers int) (bool, error) {
	if !p.nativeSessionEligible(selected, member, thinking) {
		return false, nil
	}
	workspace, err := p.resolvePreparationWorkspaceSnapshot(ctx, member, conversation, false)
	if err != nil {
		return false, err
	}
	if workspace.cwd == "" {
		root, err := filepath.EvalSymlinks(p.privateRoot)
		if err != nil {
			return false, errPrepareWorkspace
		}
		workspace = preparationWorkspace{cwd: filepath.Join(root, filepath.Base(p.privatePreparationPath(member, conversation))), key: p.rootKeys[0]}
	} else {
		workspace, err = physicalNativeWorkspace(workspace)
		if err != nil {
			return false, err
		}
	}
	return nativeSessionStaticProfile(selected, member, workspace, thinking, names, maxMembers) != nil, nil
}

// Revalidate the actual scope before claim. A previously qualified profile that
// changed must not launch under the now-mismatched prompt or a weaker transport.
func (p *Preparer) bindNativeSessionPreparation(executor provider.Executor, selected provider.Provider, selection provider.AdapterSelection, member domain.Member, workspace preparationWorkspace, thinking string, names []string, maxMembers int, request *provider.ExecuteRequest) error {
	workspace, err := physicalNativeWorkspace(workspace)
	if err != nil {
		return err
	}
	profile := nativeSessionStaticProfile(selected, member, workspace, thinking, names, maxMembers)
	if profile == nil {
		return errPrepareWorkspace
	}
	run, ok := executor.(*memberToolRuntimeRun)
	if !ok {
		return errPrepareBinding
	}
	run.nativeSession = &memberToolNativeSessionPlan{profile: *profile, selectSession: p.store.SelectNativeSession, factory: func(config provider.NativeMemberToolConfig, names []string, resume *provider.NativeSessionReference, binding json.RawMessage) (provider.Executor, error) {
		registry, err := p.nativeSessionAdapterRegistry(selection, config, names, resume, binding)
		if err != nil {
			return nil, err
		}
		return registry.NewExecutor(selection, selected, p.executionTimeout, provider.CapabilityPlatformTools, provider.CapabilityTerminalEvidence, provider.CapabilityNativeResume)
	}}
	request.CWD, request.Thinking = profile.CWD, thinking
	return nil
}

// Like the legacy factory, the per-turn registry samples implementation
// capabilities and constructs an executor. All native work stays in Execute.
func (p *Preparer) nativeSessionAdapterRegistry(selection provider.AdapterSelection, config provider.NativeMemberToolConfig, names []string, resume *provider.NativeSessionReference, binding json.RawMessage) (*provider.AdapterRegistry, error) {
	if selection.ID != provider.PiMemberToolAdapterID || selection.Protocol != provider.PiExtensionProtocol {
		return nil, errMemberToolAdapterUnsupported
	}
	adapter := provider.NewPiNativeSessionMemberToolAdapter(p.piMemberToolsExtension, config, names, provider.PiSessionOptions{Resume: resume, Binding: binding})
	return provider.NewAdapterRegistry(provider.AdapterRegistration{ID: selection.ID, Protocol: selection.Protocol, Adapter: adapter})
}

// Provider-wide reporting describes the process configuration only. It cannot
// assert any member's model, claim, resources, or resumable history.
func (p *Preparer) nativeReportTools(selected provider.Provider) []string {
	if !p.piNativeSessionConfigured(selected) {
		return nil
	}
	r := p.memberTools
	if p.nativePiThinking != "" {
		if r.tools.chatEngine == nil {
			return r.operations
		}
		if !p.piDiscussionSchema && !r.tools.WorkInputsEnabled() {
			return chatIntakeOperations()
		}
	}
	if p.roleThinking != "" && r.roleFence != nil {
		return memberToolRoleOperations(store.CoordinationRoleCoordinator)
	}
	return nil
}
