package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/preflight"
	"agentworkspace/internal/provider"
)

func TestMemberToolRuntimeContextFactoryCancellation(t *testing.T) {
	f := setupMemberToolRuntime(t)
	profile := provider.ACPOpenCodeProfile{}
	f.binding.Model, f.request.Model, f.request.Thinking = "opencode/big-pickle", "opencode/big-pickle", ""
	// Constructor validates an executable's metadata first. Never execute it.
	image := filepath.Join(t.TempDir(), "synthetic-executable")
	if os.WriteFile(image, []byte("not a CLI"), 0700) != nil {
		t.Fatal("synthetic executable creation failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var memberReadCanceled, runtimeChecked bool
	ctx = preflight.WithObserver(ctx, func(event preflight.Observation) {
		switch event.Stage {
		case "acp_constructor_member_config":
			if event.Status == "begin" {
				cancel()
			}
			memberReadCanceled = memberReadCanceled || event.Status == "canceled"
		case "member_runtime_post_factory":
			runtimeChecked = runtimeChecked || event.Status == "canceled"
		case "acp_os_start":
			t.Error("constructor cancellation reached the native start boundary")
		}
	})
	var token string
	executor, err := f.runtime.WrapWithContext(f.binding, func(factoryCtx context.Context, path, _ string, names []string) (provider.Executor, error) {
		if factoryCtx != ctx {
			t.Fatal("factory did not receive the execution context")
		}
		_, token = memberToolRuntimeConfig(t, path)
		acp, err := provider.NewACPExecutorContext(factoryCtx, provider.ACPConfig{
			AgentPath: image, Version: "1.19.6", VendorProfile: provider.ACPOpenCode119,
			Model: f.binding.Model, OpenCode: &profile, MemberToolConfigPath: path, ToolNames: names,
		}, time.Second)
		if acp != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("canceled constructor returned an executor or unexpected error")
		}
		// Even if a factory returns an executor on cancellation, the existing
		// runtime revalidation must prevent the handoff.
		return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
			t.Error("runtime handed off after canceled construction")
			return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}
		}), nil
	})
	if err != nil {
		t.Fatal("context factory not admitted")
	}
	receipt := executor.Execute(ctx, f.request, func(provider.Event) error {
		t.Error("preflight reported native acceptance")
		return nil
	})
	f.requirePreparationReceipt(t, receipt, provider.OutcomeNotStarted)
	if token == "" || !memberReadCanceled || !runtimeChecked {
		t.Fatal("missing canceled constructor or runtime revalidation evidence")
	}
	requireCapabilityDenied(t, capabilityUse(context.Background(), f.runtime.Tools(), token, "read_channel"))
	memberToolRuntimeEmpty(t, f.options.ConfigRoot)
	f.requireEmptyRunReservation(t)
	// A separate wrapper in legacy mode cannot acquire the RunID, even after
	// capability cleanup and before the non-start has been applied to Store.
	legacy := f.wrap(t, func(string, string, []string) (provider.Executor, error) {
		t.Fatal("legacy factory reached a RunID reserved by context-mode cancellation")
		return nil, nil
	})
	f.requirePreparationReceipt(t, legacy.Execute(context.Background(), f.request, nil), provider.OutcomeUnknown)
	f.requireUnsettled(t, domain.TurnStateStarting)
	turn, err := f.db.ApplyTurnReceipt(context.Background(), f.claim, receipt)
	if err != nil || turn.State != domain.TurnStateNotStarted || turn.AcceptedAt != nil {
		t.Fatal("trusted non-start did not settle the synthetic claim without acceptance")
	}
	var count int
	if err := f.raw.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", f.claim.TurnID).Scan(&count); err != nil || count != 0 {
		t.Fatal("settled non-start did not release the synthetic claim resources")
	}
}

func TestMemberToolRuntimePreHandoffDuplicateRunID(t *testing.T) {
	for _, scenario := range []string{"cancel-in-factory", "cancel-after-recheck", "factory-error"} {
		t.Run(scenario, func(t *testing.T) {
			f := setupMemberToolRuntime(t)
			// A separate registry cannot see the first runtime's capability. Both
			// modes must still contend on the adapter's exact filesystem reservation.
			other := f
			var err error
			other.runtime, err = NewMemberToolRuntime(f.db, f.options)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(f.request.RuntimeDir, f.request.RunID)
			marker := filepath.Join(dir, "owner-marker")
			winner := memberToolRuntimeReceipt(provider.OutcomeUnknown, "合成执行结果未知，需对账")
			legacy := other.wrap(t, func(string, string, []string) (provider.Executor, error) {
				return memberToolRuntimeExecutorFunc(func(_ context.Context, request provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
					if err := os.Mkdir(filepath.Join(request.RuntimeDir, request.RunID), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(marker, []byte("owner"), 0600); err != nil {
						t.Fatal(err)
					}
					return winner // No child, session, acceptance, or Store event.
				}), nil
			})
			var competingReceipt provider.ExecutionReceipt
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			status := "ok"
			if scenario == "cancel-in-factory" {
				status = "canceled"
			}
			ctx = preflight.WithObserver(ctx, func(o preflight.Observation) {
				if o.Stage == "member_runtime_post_factory" && o.Status == status {
					// Win after the absence check, before the other mode settles.
					competingReceipt = legacy.Execute(context.Background(), f.request, nil)
					if scenario == "cancel-after-recheck" {
						cancel()
					}
				}
			})
			executor, err := f.runtime.WrapWithContext(f.binding, func(context.Context, string, string, []string) (provider.Executor, error) {
				if scenario == "cancel-in-factory" {
					cancel()
				}
				if scenario == "factory-error" {
					return nil, errors.New("synthetic factory error")
				}
				return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
					t.Fatal("canceled invocation reached native Execute")
					return provider.ExecutionReceipt{}
				}), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			receipt := executor.Execute(ctx, f.request, nil)
			if competingReceipt != winner {
				t.Fatal("legacy mode did not reserve the run after the context-mode recheck")
			}
			f.requirePreparationReceipt(t, receipt, provider.OutcomeUnknown)
			if data, err := os.ReadFile(marker); err != nil || string(data) != "owner" {
				t.Fatal("canceled or failed preflight changed the winning directory")
			}
			memberToolRuntimeEmpty(t, f.options.ConfigRoot)
			f.requireUnsettled(t, domain.TurnStateStarting)
			if _, err := f.db.ApplyTurnReceipt(context.Background(), f.claim, receipt); err != nil {
				t.Fatal(err)
			}
			f.requireUnsettled(t, domain.TurnStateUnknown)
		})
	}
}
