//go:build unix

package platform

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestNativeSessionPrepareDefaultOffAndKnownSelectionOnly(t *testing.T) {
	f, _ := nativeSessionPreparation(t, "generic")
	f.opts.EnableNativeSessionResume = false
	p := f.preparer(t)
	legacy, err := p.Prepare(context.Background(), f.pending)
	prepareRoleOK(t, err)
	if legacy.Executor.(*memberToolRuntimeRun).nativeSession != nil || p.AdapterReports()[f.member.ProviderID].Capabilities.NativeResume != provider.SupportUnsupported {
		t.Fatal("native sessions were enabled without the explicit flag")
	}
	f.opts.EnableNativeSessionResume = true
	p = f.preparer(t)
	enabled, err := p.Prepare(context.Background(), f.pending)
	prepareRoleOK(t, err)
	if enabled.Executor.(*memberToolRuntimeRun).nativeSession == nil || !slices.Equal(enabled.ResourceKeys, legacy.ResourceKeys) || enabled.Request.Model != legacy.Request.Model || enabled.Request.Thinking != legacy.Request.Thinking {
		t.Fatal("opt-in lost existing resources/model/thinking or failed to select its private plan")
	}
	if p.AdapterReports()[f.member.ProviderID].Capabilities.NativeResume != provider.SupportSupported {
		t.Fatal("explicit native adapter report does not sample its actual builder")
	}
	f.opts.EnableNativeSessionResume = false
	again, err := f.preparer(t).Prepare(context.Background(), f.pending)
	prepareRoleOK(t, err)
	if again.Request != legacy.Request || !slices.Equal(again.ResourceKeys, legacy.ResourceKeys) || again.Executor.(*memberToolRuntimeRun).nativeSession != nil {
		t.Fatal("disabling flag changed the legacy preparation behavior")
	}
	f.opts.EnableNativeSessionResume = true
	f.opts.Providers[1].Version = "future"
	if f.preparer(t).nativeSessionEligible(f.opts.Providers[1], f.member, "max") {
		t.Fatal("unknown selected version promoted native resume")
	}
	f.opts.Providers[1].Version, f.opts.Providers[1].Path = "0.166.0", "relative"
	if f.preparer(t).nativeSessionEligible(f.opts.Providers[1], f.member, "max") {
		t.Fatal("relative selected CLI path promoted native resume")
	}
}

func TestNativeSessionPreparePiExplicitTextProfile(t *testing.T) {
	for _, thinking := range []string{"", "max"} {
		t.Run("thinking-"+thinking, func(t *testing.T) {
			f := newRolePrepareFixture(t)
			f.enableRuntime(t)
			useReleasePiBridge(t, f)
			f.opts.EnableNativeSessionResume, f.opts.RoleThinking, f.opts.NativePiThinking = true, thinking, "low"
			f.exec(t, "UPDATE members SET model='openai-codex/gpt-6.1-sol' WHERE id=?", f.member.ID)
			prepared, err := f.preparer(t).Prepare(context.Background(), f.pending)
			prepareRoleOK(t, err)
			run := prepared.Executor.(*memberToolRuntimeRun)
			if thinking == "" {
				if run.nativeSession != nil {
					t.Fatal("Pi native profile guessed personal/default thinking")
				}
				return
			}
			if run.nativeSession == nil || run.nativeSession.profile.Thinking != thinking || run.nativeSession.profile.Protocol != provider.PiNativeSessionProtocolVersion || run.nativeSession.profile.Adapter != provider.PiNativeSessionAdapterVersion || run.nativeSession.profile.NativePolicy.Sandbox != "tools-only" {
				t.Fatal("Pi profile did not freeze the trusted exact version/model/thinking/policy")
			}
			_, receipt, _ := runNativeSessionFixture(t, f, prepared, nil, provider.OutcomeSucceeded, false)
			if receipt.Outcome != provider.OutcomeSucceeded || receipt.NativeRef.Pi != nil || receipt.NativeRef.Codex != nil {
				t.Fatal("missing Pi capture fabricated a native payload or changed execution truth")
			}
		})
	}
}

func TestNativeSessionPreparePiNonRoleThinkingIsIndependent(t *testing.T) {
	for _, intake := range []bool{false, true} {
		name := "generic"
		if intake {
			name = "intake"
		}
		t.Run(name, func(t *testing.T) {
			f := newRolePrepareFixture(t)
			f.enableRuntime(t)
			useReleasePiBridge(t, f)
			f.pending = f.ordinary
			if intake {
				f.runtimeOptions.ChatIntake = coordination.New(f.s)
				var err error
				f.opts.MemberTools, err = NewMemberToolRuntime(f.s, f.runtimeOptions)
				prepareRoleOK(t, err)
			}
			const model = "openai-codex/gpt-6.1-sol"
			f.exec(t, "UPDATE members SET model=? WHERE id=?", model, f.member.ID)
			f.opts.EnableNativeSessionResume, f.opts.RoleThinking = true, "max"
			prepare := func() (thinking, selectedModel string, native bool) {
				prepared, err := f.preparer(t).Prepare(context.Background(), f.pending)
				prepareRoleOK(t, err)
				return prepared.Request.Thinking, prepared.Request.Model, prepared.Executor.(*memberToolRuntimeRun).nativeSession != nil
			}
			if thinking, _, native := prepare(); thinking != "" || native {
				t.Fatal("generic/intake borrowed role thinking or guessed a default")
			}
			f.opts.NativePiThinking = "off"
			if thinking, selectedModel, native := prepare(); thinking != "off" || selectedModel != model || !native {
				t.Fatal("generic/intake lost explicitly opted-in Pi thinking or changed the selected member's model")
			}
			f.exec(t, "UPDATE members SET model='model-alias' WHERE id=?", f.member.ID)
			if thinking, selectedModel, native := prepare(); thinking != "" || selectedModel != "model-alias" || native {
				t.Fatal("legacy-supported model alias was not left on the fresh legacy path")
			}
		})
	}
}

func TestNativeSessionPrepareUnqualifiedPiBridgeKeepsLegacy(t *testing.T) {
	f := newRolePrepareFixture(t)
	f.enableRuntime(t) // Readable legacy extension, not the audited digest pair.
	f.pending = f.ordinary
	f.exec(t, "UPDATE members SET model='fixture/model' WHERE id=?", f.member.ID)
	ctx := context.Background()
	legacy, err := f.preparer(t).Prepare(ctx, f.pending)
	prepareRoleOK(t, err)
	f.opts.EnableNativeSessionResume, f.opts.NativePiThinking = true, "off"
	p := f.preparer(t)
	prepared, err := p.Prepare(ctx, f.pending)
	prepareRoleOK(t, err)
	if prepared.Executor.(*memberToolRuntimeRun).nativeSession != nil || prepared.Request != legacy.Request || !slices.Equal(prepared.ResourceKeys, legacy.ResourceKeys) ||
		p.AdapterReports()[f.member.ProviderID].Capabilities.NativeResume != provider.SupportUnsupported {
		t.Fatal("readable non-qualified Pi extension changed fresh execution or its prompt/report")
	}
}

func TestNativeSessionPrepareSelectedGrantContractAndIdentity(t *testing.T) {
	for _, scope := range []string{"read_only", "read_write"} {
		t.Run(scope, func(t *testing.T) {
			f, _ := nativeSessionPreparation(t, "generic")
			grant, err := f.preparer(t).AuthorizeWorkspaceGrant(context.Background(), f.group.ID, f.member.ID, WorkspaceGrantInput{Root: t.TempDir(), Scope: scope}, "native-grant")
			prepareRoleOK(t, err)
			prepared, err := f.preparer(t).Prepare(context.Background(), f.pending)
			prepareRoleOK(t, err)
			profile := prepared.Executor.(*memberToolRuntimeRun).nativeSession.profile
			version := 15
			if scope == "read_write" {
				version = 19
			}
			if profile.WorkspaceGrantID != grant.ID || profile.WorkspaceGrantVersion != version || profile.WorkspaceRoot != grant.Root || profile.WorkspaceResolvedRoot != grant.ResolvedRoot || profile.WorkspaceIdentity != grant.ResourceKey || profile.ConfiguredWorkspace != grant.Root || profile.NativePolicy.NativeFileAccess || profile.NativePolicy.NativeArtifacts {
				t.Fatal("workspace grant was lost, given schema23, or relabeled as native file authority")
			}
			f.opts.WorkspaceGrants = []WorkspaceGrant{{GroupID: f.group.ID, MemberID: f.member.ID, Root: grant.Root}}
			prepared, err = f.preparer(t).Prepare(context.Background(), f.pending)
			prepareRoleOK(t, err)
			if prepared.Executor.(*memberToolRuntimeRun).nativeSession.profile.WorkspaceGrantID != "" {
				t.Fatal("trusted config selection was relabeled as a different durable grant")
			}
		})
	}
}

func TestNativeSessionPrepareLegacyUncleanWorkspaceGrants(t *testing.T) {
	for _, tc := range []struct{ purpose, scope, suffix string }{
		{"generic", "read_only", "/"}, {domain.ChatIntakePurpose, "read_write", "/."},
		{"coordinator", "read_only", "/"}, {"worker", "read_write", "/."},
	} {
		t.Run(tc.purpose, func(t *testing.T) {
			f, _ := nativeSessionPreparation(t, tc.purpose)
			ctx := context.Background()
			root := t.TempDir() + tc.suffix
			grant, err := f.preparer(t).AuthorizeWorkspaceGrant(ctx, f.group.ID, f.member.ID, WorkspaceGrantInput{Root: root, Scope: tc.scope}, "legacy-grant")
			prepareRoleOK(t, err)
			if grant.Root != root || filepath.Clean(root) == root {
				t.Fatal("fixture did not preserve a valid unclean grant root")
			}
			f.opts.EnableNativeSessionResume = false
			legacy, err := f.preparer(t).Prepare(ctx, f.pending)
			prepareRoleOK(t, err)
			f.opts.EnableNativeSessionResume = true
			prepared, err := f.preparer(t).Prepare(ctx, f.pending)
			prepareRoleOK(t, err)
			if prepared.Executor.(*memberToolRuntimeRun).nativeSession != nil || prepared.Request != legacy.Request || !slices.Equal(prepared.ResourceKeys, []string{grant.ResourceKey}) {
				t.Fatal("an existing grant changed its legacy execution path, permissions or claim keys")
			}
		})
	}
}

func TestNativeSessionPrepareTrustedGrantUsesPhysicalCWDContract(t *testing.T) {
	for _, purpose := range []string{"generic", domain.ChatIntakePurpose, "coordinator", "worker"} {
		t.Run(purpose, func(t *testing.T) {
			f, _ := nativeSessionPreparation(t, purpose)
			ctx := context.Background()
			for _, target := range []string{"physical", "physical "} {
				root := t.TempDir()
				physical, alias := filepath.Join(root, target), filepath.Join(root, "clean-alias")
				prepareRoleOK(t, os.Mkdir(physical, 0700))
				prepareRoleOK(t, os.Symlink(physical, alias))
				f.opts.WorkspaceGrants = []WorkspaceGrant{{GroupID: f.group.ID, MemberID: f.member.ID, Root: alias}}
				f.exec(t, "UPDATE members SET workspace=? WHERE id=?", alias, f.member.ID)
				f.opts.EnableNativeSessionResume = false
				legacy, err := f.preparer(t).Prepare(ctx, f.pending)
				prepareRoleOK(t, err)
				f.opts.EnableNativeSessionResume = true
				prepared, err := f.preparer(t).Prepare(ctx, f.pending)
				prepareRoleOK(t, err)
				plan := prepared.Executor.(*memberToolRuntimeRun).nativeSession
				physical, err = filepath.EvalSymlinks(physical)
				prepareRoleOK(t, err)
				if prepared.Request.CWD != physical || !slices.Equal(legacy.ResourceKeys, prepared.ResourceKeys) {
					t.Fatal("profile validation changed the legacy grant's actual directory or claim keys")
				}
				hasPermission := slices.Contains(decodePreparation(t, prepared.Request.Prompt).Permissions, nativeSessionTextPermission)
				if target == "physical " {
					if plan != nil || hasPermission || prepared.Request != legacy.Request {
						t.Fatal("clean alias with trailing-space physical CWD advertised a native plan instead of exact legacy")
					}
				} else if plan == nil || !hasPermission || plan.profile.CWD != physical || plan.profile.ConfiguredWorkspace != alias || plan.profile.WorkspaceGrantID != "" {
					t.Fatal("qualified clean alias did not bind both the physical CWD and original trusted grant spelling")
				}
			}
		})
	}
}

func TestNativeSessionPrepareFileWorkerRemainsIsolated(t *testing.T) {
	for _, name := range []string{provider.Pi, provider.Codex} {
		t.Run(name, func(t *testing.T) {
			f, workspace, _ := newWorkspacePreparation(t)
			f.opts.EnableNativeSessionResume, f.opts.RoleThinking = true, "max"
			f.exec(t, "UPDATE members SET model='openai-codex/gpt-6.1-sol' WHERE id=?", f.worker.ID)
			ctx := context.Background()
			p := f.preparer(t)
			f.exec(t, "UPDATE members SET model='openai-codex/gpt-6.1-sol' WHERE id=?", f.coordinator.ID)
			coordinator, err := p.Prepare(ctx, f.pending)
			prepareRoleOK(t, err)
			if coordinator.Executor.(*memberToolRuntimeRun).nativeSession != nil {
				t.Fatal("workspace coordinator acquired a native text profile")
			}
			facts, err := p.sampleDispatchCandidates(ctx, f.group.ID, f.coordinator.ID, &workspace)
			prepareRoleOK(t, err)
			turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.pending.Delivery.ID, MaxActive: 1})
			prepareRoleOK(t, err)
			claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
			scope, err := f.s.BindMemberTool(ctx, claim)
			prepareRoleOK(t, err)
			decision, err := f.s.DecideDispatchWithCapabilities(ctx, store.CoordinationToolScope{Member: scope, Provenance: f.state.Provenance}, store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker.ID, Reason: "offline file profile fixture"}, "file-worker", facts)
			prepareRoleOK(t, err)
			_, err = f.s.ApplyTurnReceipt(ctx, claim, memberToolRuntimeReceipt(provider.OutcomeNotStarted, "fixture setup"))
			prepareRoleOK(t, err)
			materialized, err := f.s.MaterializeAssignment(ctx, decision.Assignment.ID, 1, time.Now().UTC())
			prepareRoleOK(t, err)
			f.selectDelivery(t, materialized.Delivery.MessageID)
			prepared, err := f.preparer(t).Prepare(ctx, f.pending)
			prepareRoleOK(t, err)
			run := prepared.Executor.(*memberToolRuntimeRun)
			if run.nativeSession != nil {
				t.Fatal("Pi/file profile acquired a text native-session plan")
			}
			if strings.Contains(strings.Join(decodePreparation(t, prepared.Request.Prompt).Permissions, "\n"), nativeSessionTextPermission) {
				t.Fatal("file worker prompt falsely claimed native text resume")
			}
			run.binding.ProviderName = name
			calls := 0
			run.factory = func(path, _ string, names []string) (provider.Executor, error) {
				calls++
				_, token := memberToolRuntimeConfig(t, path)
				if !slices.Equal(names, provider.WorkspaceWorkerToolNames()) {
					t.Fatal("file worker lost its exact eight actually issued tools")
				}
				issued, err := f.opts.MemberTools.Tools().TransportOperations(ctx, token)
				prepareRoleOK(t, err)
				if !slices.Equal(issued, names) {
					t.Fatal("file issuance and executor tools diverged")
				}
				return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
					return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, NativeRef: &provider.NativeSessionReference{Version: 1, Binding: json.RawMessage(`{"unexpected":1}`)}}
				}), nil
			}
			workerTurn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: run.binding.DeliveryID, MaxActive: 1, ResourceKeys: prepared.ResourceKeys})
			prepareRoleOK(t, err)
			req := prepared.Request
			req.RunID, req.RuntimeDir = workerTurn.ID, f.runtimeOptions.RoleExecutionRoot
			receipt := run.Execute(ctx, req, nil)
			if receipt.NativeRef != nil || name == provider.Codex && calls != 0 || name == provider.Pi && calls != 1 {
				t.Fatal("file profile bypassed Codex rejection or emitted a text-only reference")
			}
			if _, err := os.Stat(req.RuntimeDir + "/" + req.RunID); !os.IsNotExist(err) {
				t.Fatal("fixture or rejected file worker launched native execution")
			}
		})
	}
}
