package platform

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func codexNativeIntakePreparation(t *testing.T) (*rolePrepareFixture, scheduler.PreparedExecution) {
	t.Helper()
	f := newRolePrepareFixture(t)
	f.enableRuntime(t)
	f.opts.Providers[1].Name, f.opts.Providers[1].Version = provider.Codex, "0.166.0"
	f.opts.PiMemberToolsExtension = ""
	f.opts.EnableCodexAppServer, f.opts.EnableNativeSessionResume, f.opts.RoleThinking = true, true, "max"
	f.runtimeOptions.ChatIntake = coordination.New(f.s)
	var err error
	f.opts.MemberTools, err = NewMemberToolRuntime(f.s, f.runtimeOptions)
	prepareRoleOK(t, err)
	t.Cleanup(func() { _ = f.opts.MemberTools.Close() })
	f.pending = f.ordinary
	prepared, err := f.preparer(t).Prepare(t.Context(), f.pending)
	prepareRoleOK(t, err)
	if run := prepared.Executor.(*memberToolRuntimeRun); !run.intake || run.nativeSession == nil || prepared.Request.Thinking != "" {
		t.Fatal("ordinary Codex intake did not select the native text profile without Pi thinking")
	}
	return f, prepared
}

func TestCodexNativeIntakePrepareScope(t *testing.T) {
	f, native := codexNativeIntakePreparation(t)
	ctx := t.Context()
	p := f.preparer(t)
	plan := native.Executor.(*memberToolRuntimeRun).nativeSession.profile
	if plan.Adapter != provider.CodexNativeSessionAdapterVersion || plan.Protocol != provider.CodexNativeSessionProtocolVersion ||
		plan.NativePolicy != (store.NativeTextPolicy{Sandbox: "read-only", ApprovalPolicy: "never"}) || plan.MaxGroupMembers != codexV30DiscussionIntakeProfile ||
		!slices.Equal(plan.Tools, chatIntakeOperations()) || !slices.Equal(plan.ResourceKeys, native.ResourceKeys) {
		t.Fatal("bad scoped Codex intake profile")
	}
	report := p.AdapterReports()[f.member.ProviderID]
	if report.Protocol != provider.CodexAppServerProtocol || report.Capabilities == nil || report.Capabilities.NativeResume != provider.SupportSupported || report.Capabilities.Steer != provider.SupportUnsupported {
		t.Fatal("report did not sample the opted-in Codex constructor")
	}
	// Real role provenance, Task and Mandate exist here, but the intake opt-in
	// does not change their fresh adapter, prompt, resources or tool claims.
	role := store.PendingDelivery{GroupID: f.group.ID, Delivery: f.trigger.Deliveries[0]}
	withOptIn, err := p.Prepare(ctx, role)
	prepareRoleOK(t, err)
	f.opts.EnableNativeSessionResume = false
	p = f.preparer(t)
	without, err := p.Prepare(ctx, role)
	prepareRoleOK(t, err)
	if withOptIn.Request != without.Request || withOptIn.Request.Thinking != "" || !slices.Equal(withOptIn.ResourceKeys, without.ResourceKeys) ||
		withOptIn.Executor.(*memberToolRuntimeRun).nativeSession != nil {
		t.Fatal("Codex intake opt-in changed a Task role")
	}
	fresh, err := p.Prepare(ctx, f.ordinary)
	prepareRoleOK(t, err)
	if fresh.Executor.(*memberToolRuntimeRun).nativeSession != nil || fresh.Request.Model != native.Request.Model ||
		!slices.Equal(fresh.ResourceKeys, native.ResourceKeys) || p.AdapterReports()[f.member.ProviderID].Capabilities.NativeResume != provider.SupportUnsupported {
		t.Fatal("AppServer alone selected native resume or changed the configured member")
	}
	f.opts.EnableNativeSessionResume, f.opts.EnableCodexAppServer = true, false
	// A readable Pi extension permits Pi-only opt-in, not Codex resume.
	useLegacyNativePiBridge(t, f)
	p = f.preparer(t)
	plain, err := p.Prepare(ctx, f.ordinary)
	prepareRoleOK(t, err)
	if _, wrapped := plain.Executor.(*memberToolRuntimeRun); wrapped || p.AdapterReports()[f.member.ProviderID].ID != provider.CodexOneShotAdapterID {
		t.Fatal("resume opt-in alone selected the Codex AppServer")
	}
}

func TestCodexNativeIntakeTypedAndUnknownRemainUnqualified(t *testing.T) {
	f, _ := codexNativeIntakePreparation(t)
	prepareRoleOK(t, f.opts.MemberTools.Close())
	// The continuation variant has the same tool names but a different MCP
	// schema. The service is never invoked for adapter sampling.
	f.runtimeOptions.WorkInputs = struct{ MemberWorkInputService }{}
	var err error
	f.opts.MemberTools, err = NewMemberToolRuntime(f.s, f.runtimeOptions)
	prepareRoleOK(t, err)
	p := f.preparer(t)
	typed, err := p.Prepare(t.Context(), f.ordinary)
	prepareRoleOK(t, err)
	if typed.Executor.(*memberToolRuntimeRun).nativeSession != nil || p.AdapterReports()[f.member.ProviderID].Capabilities.NativeResume != provider.SupportUnsupported ||
		strings.Contains(typed.Request.Prompt, nativeSessionTextPermission) {
		t.Fatal("typed Codex intake borrowed the stable discussion profile")
	}
	f.opts.EnableNativeSessionResume = false
	fresh, err := f.preparer(t).Prepare(t.Context(), f.ordinary)
	prepareRoleOK(t, err)
	if typed.Request != fresh.Request || !slices.Equal(typed.ResourceKeys, fresh.ResourceKeys) {
		t.Fatal("typed intake lost its fresh prompt or resources")
	}
	f.opts.EnableNativeSessionResume = true
	f.opts.Providers[1].Version = "0.166.1"
	p = f.preparer(t)
	if _, err := p.Prepare(t.Context(), f.ordinary); !errors.Is(err, scheduler.ErrNotReady) ||
		p.AdapterReports()[f.member.ProviderID].Capabilities != nil {
		t.Fatal("unverified AppServer version selected a native or one-shot fallback")
	}
}

// Write a real Store receipt via the existing claim/candidate/binding APIs. The
// synthetic proof is never used to start a process. A zero marker represents
// the old v24 same-names/different-schema profile, not an imported session.
func seedCodexIntakeReference(t *testing.T, f *rolePrepareFixture, prepared scheduler.PreparedExecution, marker int) store.TurnClaim {
	t.Helper()
	ctx := t.Context()
	request, claim := claimNativeFenceFixture(t, f, prepared)
	run := prepared.Executor.(*memberToolRuntimeRun)
	token, err := run.runtime.tools.IssueChatIntake(ctx, claim)
	prepareRoleOK(t, err)
	defer run.runtime.tools.Revoke(token)
	entry, found := run.runtime.nativeRegistration(token)
	if !found {
		t.Fatal("fixture did not issue a live candidate")
	}
	binding, err := run.nativeSessionBinding(ctx, request, claim, entry)
	prepareRoleOK(t, err)
	binding.Profile.MaxGroupMembers = marker
	canonical, err := store.CanonicalNativeRunBinding(binding)
	prepareRoleOK(t, err)
	p := binding.Profile
	tools, err := store.NativeToolsProfileSHA256(p.Tools)
	prepareRoleOK(t, err)
	const id = "synthetic-intake-thread"
	ref := &provider.NativeSessionReference{Version: provider.NativeSessionReferenceVersion, Binding: canonical, Codex: &provider.CodexNativeSessionReference{
		Adapter: p.Adapter, Protocol: p.Protocol, CLIPath: p.CLIPath, CLIVersion: p.CLIVersion, Model: p.Model, CWD: p.CWD,
		Provider: "synthetic", NativeHome: "/synthetic/home", ToolsProfileSHA256: tools, NativeThreadID: id, NativeTurnID: "synthetic-turn-1",
		HistorySHA256: strings.Repeat("a", 64), HistoryTurns: 1, HistoryItems: 1,
	}}
	for _, event := range []provider.Event{
		{Kind: provider.SessionEvent, NativeSessionID: id}, {Kind: provider.AcceptedEvent, NativeSessionID: id},
		{Kind: provider.ResponseEvent, NativeSessionID: id, Text: "synthetic answer"}, {Kind: provider.TerminalEvent, NativeSessionID: id, Terminal: provider.OutcomeSucceeded},
	} {
		prepareRoleOK(t, f.s.RecordTurnEvent(ctx, claim, event))
	}
	turn, err := f.s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Started: true, Accepted: true, NativeSessionID: id,
		Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded, OutputComplete: true, DirectChildReaped: true, NativeRef: ref})
	prepareRoleOK(t, err)
	var stored bool
	prepareRoleOK(t, f.db.QueryRow("SELECT native_ref_json IS NOT NULL FROM native_run_bindings WHERE turn_id=?", claim.TurnID).Scan(&stored))
	if !stored || turn.State != domain.TurnStateSucceeded {
		t.Fatal("fixture requires an otherwise qualified historical reference")
	}
	return claim
}

func TestCodexNativeIntakeOldSchemaStaysFresh(t *testing.T) {
	f, first := codexNativeIntakePreparation(t)
	old := seedCodexIntakeReference(t, f, first, 0)
	var original string
	prepareRoleOK(t, f.db.QueryRow("SELECT native_ref_json FROM native_run_bindings WHERE turn_id=?", old.TurnID).Scan(&original))
	prepared := nextNativeSessionPreparation(t, f)
	_, receipt, call := runNativeSessionFixture(t, f, prepared, func(call *nativeSessionFixtureCall) {
		if call.resume != nil || call.binding.Profile.MaxGroupMembers != codexV30DiscussionIntakeProfile {
			t.Fatal("a v24 same-names reference was sent to the adapter")
		}
	}, provider.OutcomeSucceeded, false)
	var after string
	prepareRoleOK(t, f.db.QueryRow("SELECT native_ref_json FROM native_run_bindings WHERE turn_id=?", old.TurnID).Scan(&after))
	if receipt.Outcome != provider.OutcomeSucceeded || call.binding.TurnID == old.TurnID || after != original || decodeNativeFixtureBinding(t, receipt).ResumedFromTurnID != "" {
		t.Fatal("fresh v30 turn changed or adopted old history")
	}
}

func TestCodexNativeIntakeLaunchFences(t *testing.T) {
	for _, change := range []string{"stale-source", "stale-grant", "stale-plan", "issued-schema", "issued-grants", "cancel"} {
		t.Run(change, func(t *testing.T) {
			ctx := t.Context()
			f, prepared := codexNativeIntakePreparation(t)
			first := seedCodexIntakeReference(t, f, prepared, codexV30DiscussionIntakeProfile)
			prepared = nextNativeSessionPreparation(t, f)
			request, claim := claimNativeFenceFixture(t, f, prepared)
			run := prepared.Executor.(*memberToolRuntimeRun)
			if change == "stale-source" {
				selectSession := run.nativeSession.selectSession
				run.nativeSession.selectSession = func(ctx context.Context, scope store.MemberToolScope, binding store.NativeRunBinding) (store.NativeSessionSelection, error) {
					selection, err := selectSession(ctx, scope, binding)
					if err != nil || selection.SourceTurnID != first.TurnID {
						t.Fatal("fixture requires a real qualified source")
					}
					// Simulate a stale construction snapshot without modifying history.
					selection.Reference.Codex.HistorySHA256 = strings.Repeat("b", 64)
					return selection, nil
				}
			}
			calls := 0
			factory := run.nativeSession.factory
			run.nativeSession.factory = func(config provider.NativeMemberToolConfig, tools []string, ref *provider.NativeSessionReference, binding json.RawMessage) (provider.Executor, error) {
				calls++
				if ref == nil || ref.Codex == nil || ref.Codex.NativeThreadID != "synthetic-intake-thread" {
					t.Fatal("fixture did not begin with a qualified source")
				}
				_, err := factory(config, tools, ref, binding)
				prepareRoleOK(t, err)
				switch change {
				case "stale-grant":
					_, err := f.preparer(t).AuthorizeWorkspaceGrant(ctx, f.group.ID, f.member.ID, WorkspaceGrantInput{Root: t.TempDir(), Scope: "read_only"}, "codex-fence")
					prepareRoleOK(t, err)
				case "stale-plan":
					_, err := f.s.SetChatPolicy(ctx, domain.ThreadRef{GroupID: f.group.ID, TopicID: f.trigger.TopicID}, domain.ChatPolicyRequest{Action: domain.ChatActionTextSubmission, MaxAdmissions: 1}, 0, "changed-policy")
					prepareRoleOK(t, err)
				case "issued-schema":
					// Synthetic drift of normally immutable trusted composition.
					run.runtime.tools.workInputs = struct{ MemberWorkInputService }{}
				case "issued-grants":
					registry := run.runtime.tools
					registry.mu.Lock()
					for key, entry := range registry.capabilities {
						if entry.scope.Claim == claim {
							entry.grants.MaxGroupMembers++
							registry.capabilities[key] = entry
						}
					}
					registry.mu.Unlock()
				case "cancel":
					_, err := f.s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID)
					prepareRoleOK(t, err)
				}
				return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
					t.Fatal("stale Codex binding reached the provider")
					return provider.ExecutionReceipt{}
				}), nil
			}
			run.factory = func(string, string, []string) (provider.Executor, error) {
				t.Fatal("stale Codex binding fell back to fresh execution")
				return nil, nil
			}
			receipt := run.Execute(ctx, request, nil)
			want, held := provider.OutcomeNotStarted, 0
			if change == "cancel" {
				want, held = provider.OutcomeUnknown, 1
			}
			if calls != 1 || receipt.Started || receipt.Outcome != want || !receipt.NativePreflightFailure {
				t.Fatalf("bad preflight outcome after %s: %s (factories=%d)", change, receipt.Outcome, calls)
			}
			assertNativeFenceBarrier(t, f, claim, held)
			memberToolRuntimeEmpty(t, request.RuntimeDir)
			memberToolRuntimeEmpty(t, run.runtime.configRoot)
		})
	}
}
