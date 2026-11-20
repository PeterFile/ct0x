package platform

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"

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
	return p.enableNativeSessionResume && selected.Name == provider.Pi && p.piMemberToolsExtension != "" &&
		nativeSessionWorkspacePath(p.memberTools.executionRoot.resolvedRoot)
}

func (p *Preparer) codexNativeIntakeConfigured(selected provider.Provider) bool {
	return p.enableNativeSessionResume && p.enableCodexAppServer && selected.Name == provider.Codex &&
		p.memberTools.tools.chatEngine != nil && !p.memberTools.tools.continueWorkEnabled()
}

func (p *Preparer) nativeSessionEligible(selected provider.Provider, member domain.Member, thinking string) bool {
	// Legacy grants select and preserve the raw configured root. Do not clean
	// it into a different authority spelling just to fit the native profile.
	if member.Workspace != "" && !nativeSessionWorkspacePath(member.Workspace) {
		return false
	}
	switch selected.Name {
	case provider.Pi:
		return p.piNativeSessionConfigured(selected) && provider.PiNativeTextSessionProfileSupported(selected, member.Model, thinking) &&
			provider.PiNativeSessionBridgeSupported(p.piMemberToolsExtension)
	case provider.Codex:
		return p.codexNativeIntakeConfigured(selected) && provider.CodexNativeTextSessionProfileSupported(selected, member.Model, thinking)
	}
	return false
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
		NativePolicy: store.NativeTextPolicy{ApprovalPolicy: "never"}}
	switch selected.Name {
	case provider.Pi:
		profile.Adapter, profile.Protocol = provider.PiNativeSessionAdapterVersion, provider.PiNativeSessionProtocolVersion
		profile.NativePolicy.Sandbox = "tools-only"
	case provider.Codex:
		if maxMembers != 0 || !slices.Equal(names, chatIntakeOperations()) {
			return nil
		}
		profile.Adapter, profile.Protocol = provider.CodexNativeSessionAdapterVersion, provider.CodexNativeSessionProtocolVersion
		profile.NativePolicy.Sandbox = "read-only"
		profile.MaxGroupMembers = codexV30DiscussionIntakeProfile
	default:
		return nil
	}
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
	if !ok || selected.Name == provider.Codex && (!run.intake || run.role != nil || run.intakeSchema != (memberToolIntakeSchema{})) {
		return errPrepareBinding
	}
	if selected.Name == provider.Pi && slices.Contains(names, "send_message") {
		quote, qualified := provider.PiNativeSessionBridgeQuoteSupport(p.piMemberToolsExtension)
		// Quote-capable intake should already be on the issued-config fresh path.
		if !qualified || quote && (run.intake || run.role != nil) {
			return errPrepareBinding
		}
		profile.PiSendMessageQuote = quote
	}
	run.nativeSession = &memberToolNativeSessionPlan{profile: *profile, selectSession: p.store.SelectNativeSession, factory: func(config provider.NativeMemberToolConfig, names []string, resume *provider.NativeSessionReference, binding json.RawMessage) (provider.Executor, error) {
		quote := profile.PiSendMessageQuote && slices.Contains(names, "send_message")
		registry, err := p.nativeSessionAdapterRegistry(selection, config, names, resume, binding, &quote)
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
func (p *Preparer) nativeSessionAdapterRegistry(selection provider.AdapterSelection, config provider.NativeMemberToolConfig, names []string, resume *provider.NativeSessionReference, binding json.RawMessage, piSendMessageQuote *bool) (*provider.AdapterRegistry, error) {
	var adapter provider.Adapter
	switch selection {
	case (provider.AdapterSelection{ID: provider.PiMemberToolAdapterID, Protocol: provider.PiExtensionProtocol}):
		adapter = provider.NewPiNativeSessionMemberToolAdapter(p.piMemberToolsExtension, config, names,
			provider.PiSessionOptions{Resume: resume, Binding: binding, ExpectedSendMessageQuote: piSendMessageQuote})
	case (provider.AdapterSelection{ID: provider.CodexAppServerAdapterID, Protocol: provider.CodexAppServerProtocol}):
		adapter = provider.NewCodexAppServerNativeSessionMemberToolAdapter(config, names, provider.CodexAppServerSessionOptions{Resume: resume, Binding: binding})
	default:
		return nil, errMemberToolAdapterUnsupported
	}
	return provider.NewAdapterRegistry(provider.AdapterRegistration{ID: selection.ID, Protocol: selection.Protocol, Adapter: adapter})
}

func (p *Preparer) piIntakeChangesNativeSchema() bool {
	return p.piDiscussionSchema || p.memberTools.tools.continueWorkEnabled()
}

// Provider-wide reporting describes the process configuration only. It cannot
// assert any member's model, claim, resources, or resumable history.
func (p *Preparer) nativeReportTools(selected provider.Provider) []string {
	if p.codexNativeIntakeConfigured(selected) {
		return chatIntakeOperations()
	}
	if !p.piNativeSessionConfigured(selected) {
		return nil
	}
	r := p.memberTools
	if p.nativePiThinking != "" {
		if r.tools.chatEngine == nil {
			return r.operations
		}
		if !p.piIntakeChangesNativeSchema() {
			return chatIntakeOperations()
		}
	}
	if p.roleThinking != "" && r.roleFence != nil {
		return memberToolRoleOperations(store.CoordinationRoleCoordinator)
	}
	return nil
}
