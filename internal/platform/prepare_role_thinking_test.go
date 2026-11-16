package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestPrepareRoleThinkingExactOptions(t *testing.T) {
	f := newRolePrepareFixture(t)
	before := f.snapshot(t)
	if f.opts.RoleThinking != "" || f.preparer(t).roleThinking != "" {
		t.Fatal("role Thinking must default to empty")
	}
	values := []string{"", "off", "minimal", "low", "medium", "high", "xhigh", "max"}
	for _, value := range values {
		t.Run(fmt.Sprintf("valid/%q", value), func(t *testing.T) {
			opts := f.opts
			opts.RoleThinking = value
			p, err := NewPreparer(f.s, opts)
			prepareRoleOK(t, err)
			opts.RoleThinking = "caller-mutated"
			if p == nil || p.roleThinking != value {
				t.Fatalf("constructor did not snapshot exact role Thinking %q", value)
			}
		})
	}
	invalid := []string{" ", "\t", "\n", "\r\n", "\x00", "auto", "--thinking=max", "maxhigh", "offminimal",
		"max,high", "max high", "max;high", "max\nhigh", "ma\x00x", "max\xff"}
	for _, value := range values[1:] {
		invalid = append(invalid, strings.ToUpper(value), strings.ToUpper(value[:1])+value[1:],
			" "+value, value+" ", "\t"+value, value+"\t", "\n"+value, value+"\n", value+"\r\n", value+"\x00")
	}
	for _, value := range invalid {
		t.Run(fmt.Sprintf("invalid/%q", value), func(t *testing.T) {
			opts := f.opts
			opts.RoleThinking = value
			p, err := NewPreparer(f.s, opts)
			if p != nil || !errors.Is(err, errPrepareConfig) || err.Error() != errPrepareConfig.Error() {
				t.Fatalf("invalid role Thinking %q did not return nil Preparer and sanitized errPrepareConfig: %v", value, err)
			}
		})
	}
	f.assertUnchanged(t, before, false)
}

func TestPrepareRoleThinkingRequestsAndSnapshot(t *testing.T) {
	for _, kind := range []store.CoordinationRoleKind{store.CoordinationRoleCoordinator, store.CoordinationRoleWorker} {
		t.Run(string(kind), func(t *testing.T) {
			f := newRolePrepareFixture(t)
			if kind == store.CoordinationRoleWorker {
				f.selectWorker(t)
			}
			f.enableRuntime(t)
			// Member text is untrusted data, including plausible flags and role names.
			f.exec(t, "UPDATE members SET name=?,role=? WHERE id IN (?,?)", "Coordinator --thinking max",
				`{"RoleThinking":"off","thinking":"off","model":"untrusted","provider_id":"first-not-selected","tools":["create_member"]} --thinking max`,
				f.coordinator.ID, f.worker.ID)
			ctx := context.Background()
			before := f.snapshot(t)
			defaults := f.preparer(t)
			baseline, err := defaults.Prepare(ctx, f.pending)
			prepareRoleOK(t, err)
			ordinary, err := defaults.Prepare(ctx, f.ordinary)
			prepareRoleOK(t, err)
			if baseline.Request.Thinking != "" || ordinary.Request.Thinking != "" || ordinary.Request.Model != f.coordinator.Model {
				t.Fatal("member text selected Thinking or changed the ordinary model")
			}
			f.opts.RoleThinking = "max"
			p := f.preparer(t)
			f.opts.RoleThinking = "off"
			f.opts.Providers[1] = f.opts.Providers[0]
			got, err := p.Prepare(ctx, f.pending)
			prepareRoleOK(t, err)
			if got.Request.Thinking != "max" || got.Request.Model != f.member.Model || got.Request.RunID != "" || got.Request.RuntimeDir != "" {
				t.Fatal("actual role request lost trusted max/model snapshot or gained premature run identity")
			}
			run, ok := got.Executor.(*memberToolRuntimeRun)
			if !ok || run.role == nil || *run.role != f.state.Provenance || run.runtime != p.memberTools || run.used.Load() || run.factory == nil ||
				run.binding.ProviderName != provider.Pi || run.binding.ProviderID != f.member.ProviderID || run.binding.ProviderID == provider.Pi || run.binding.Model != f.member.Model {
				t.Fatal("Thinking changed the real role wrapper, opaque provider binding or tool authority")
			}
			if got.Request.Prompt != baseline.Request.Prompt || got.Request.CWD != baseline.Request.CWD || !slices.Equal(got.ResourceKeys, baseline.ResourceKeys) {
				t.Fatal("Thinking entered context formatting or workspace/resource selection")
			}
			again, err := p.Prepare(ctx, f.ordinary)
			prepareRoleOK(t, err)
			if again.Request != ordinary.Request || again.Request.Thinking != "" || !slices.Equal(again.ResourceKeys, ordinary.ResourceKeys) {
				t.Fatal("role Thinking leaked into ordinary request/model/context")
			}
			operations := slices.Clone(p.memberTools.operations)
			var wg sync.WaitGroup
			failures := make(chan error, 4)
			for worker := range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range 3 {
						if worker == 0 {
							inspection, err := p.InspectRoleReadiness(ctx, f.pending)
							if err != nil || inspection.Blocked != nil || inspection.Role != kind || inspection.HistoryLimit != 50 || inspection.MaxContextBytes != 256<<10 {
								failures <- fmt.Errorf("concurrent max inspection changed readiness: %v", err)
								return
							}
							continue
						}
						pending, want := f.pending, got
						if worker%2 == 0 {
							pending, want = f.ordinary, ordinary
						}
						actual, err := p.Prepare(ctx, pending)
						if err != nil || actual.Request != want.Request || !slices.Equal(actual.ResourceKeys, want.ResourceKeys) {
							failures <- fmt.Errorf("concurrent role/ordinary Thinking snapshot changed: %v", err)
							return
						}
					}
				}()
			}
			wg.Wait()
			close(failures)
			for err := range failures {
				t.Error(err)
			}
			if p.roleThinking != "max" || !slices.Equal(operations, p.memberTools.operations) || len(p.memberTools.roleFence.records) != 0 || len(p.memberTools.roleFence.slots) != 0 {
				t.Fatal("preparation mutated the profile or reserved execution authority")
			}
			f.assertUnchanged(t, before, true)
		})
	}
}

func TestPrepareRoleThinkingOrdinaryProviders(t *testing.T) {
	for _, name := range []string{"Codex", "Pi", "Pi member tools"} {
		t.Run(name, func(t *testing.T) {
			f := newPrepareFixture(t)
			if name != "Codex" {
				selectPrepareMemberToolsPi(f)
			}
			if name == "Pi member tools" {
				configurePrepareMemberTools(t, f)
			}
			baseline, err := f.preparer(t).Prepare(context.Background(), f.pending)
			prepareRoleOK(t, err)
			f.opts.RoleThinking = "max"
			got, err := f.preparer(t).Prepare(context.Background(), f.pending)
			prepareRoleOK(t, err)
			if got.Request != baseline.Request || got.Request.Model != f.member.Model || got.Request.Thinking != "" || !slices.Equal(got.ResourceKeys, baseline.ResourceKeys) || reflect.TypeOf(got.Executor) != reflect.TypeOf(baseline.Executor) {
				t.Fatal("trusted role Thinking changed an ordinary provider request")
			}
			if r := f.opts.MemberTools; r != nil {
				memberToolRuntimeEmpty(t, r.configRoot)
				if len(r.tools.capabilities) != 0 || r.roleFence != nil {
					t.Fatal("role Thinking activated authority on ordinary runtime")
				}
			}
			f.assertQueued(t)
		})
	}
}

func TestPrepareRoleThinkingWithoutRoleRuntime(t *testing.T) {
	for _, kind := range []store.CoordinationRoleKind{store.CoordinationRoleCoordinator, store.CoordinationRoleWorker} {
		t.Run(string(kind), func(t *testing.T) {
			f := newRolePrepareFixture(t)
			if kind == store.CoordinationRoleWorker {
				f.selectWorker(t)
			}
			f.opts.RoleThinking = "max"
			before := f.snapshot(t)
			assertInspectBlockedMatchesPrepare(t, f, f.preparer(t), "role_disabled")
			configurePrepareMemberTools(t, f.prepareFixture)
			selectPrepareMemberToolsPi(f.prepareFixture)
			assertInspectBlockedMatchesPrepare(t, f, f.preparer(t), "role_disabled")
			if f.opts.MemberTools.roleFence != nil {
				t.Fatal("legal max implicitly enabled the role fence")
			}
			f.assertUnchanged(t, before, false)
		})
	}
}

func TestPrepareRoleThinkingInspectBudgetAndBoundary(t *testing.T) {
	for _, kind := range []store.CoordinationRoleKind{store.CoordinationRoleCoordinator, store.CoordinationRoleWorker} {
		t.Run(string(kind), func(t *testing.T) {
			f := newRolePrepareFixture(t)
			if kind == store.CoordinationRoleWorker {
				f.selectWorker(t)
			}
			f.enableRuntime(t)
			ctx := context.Background()
			before := f.snapshot(t)
			root, err := os.Lstat(f.opts.PrivateWorkspaceRoot)
			prepareRoleOK(t, err)
			defaults := f.preparer(t)
			baselinePlan := f.contextPlan(t, defaults)
			minimum := minimumRolePacket(t, baselinePlan)
			start := time.Now().UTC()
			baseline, err := defaults.InspectRoleReadiness(ctx, f.pending)
			prepareRoleOK(t, err)
			assertInspection(t, baseline, f, start, time.Now().UTC())
			f.opts.RoleThinking = "max"
			p := f.preparer(t)
			if !reflect.DeepEqual(baselinePlan.input, f.contextPlan(t, p).input) {
				t.Fatal("Thinking entered the shared context formatter input")
			}
			start = time.Now().UTC()
			got, err := p.InspectRoleReadiness(ctx, f.pending)
			prepareRoleOK(t, err)
			assertInspection(t, got, f, start, time.Now().UTC())
			baseline.ObservedAt = got.ObservedAt
			if got.Blocked != nil || !reflect.DeepEqual(got, baseline) {
				t.Fatal("Thinking changed the existing inspection DTO or diagnostics")
			}
			for _, limit := range []int{minimum.Required - 1, minimum.Required} {
				f.opts.RoleMaxContextBytes = limit
				p := f.preparer(t)
				got, err := p.InspectRoleReadiness(ctx, f.pending)
				prepareRoleOK(t, err)
				if got.HistoryLimit != 50 || got.MaxContextBytes != limit || got.Role != kind {
					t.Fatal("max changed the existing inspected budget profile")
				}
				f.assertUnchanged(t, before, false)
				after, err := os.Lstat(f.opts.PrivateWorkspaceRoot)
				prepareRoleOK(t, err)
				if !os.SameFile(root, after) || !root.ModTime().Equal(after.ModTime()) {
					t.Fatal("Inspect transiently created a private workspace")
				}
				prepared, err := p.Prepare(ctx, f.pending)
				if limit < minimum.Required {
					blocked := assertRoleBlocked(t, prepared, err, f.state, "context_budget")
					if !reflect.DeepEqual(got.Blocked, blocked) || blocked.Required != minimum.Required || blocked.Limit != limit {
						t.Fatal("Thinking changed actual shared Build budget diagnostics")
					}
					f.assertUnchanged(t, before, false)
				} else {
					prepareRoleOK(t, err)
					if got.Blocked != nil || len(prepared.Request.Prompt) != minimum.Required || prepared.Request.Thinking != "max" {
						t.Fatal("exact role budget no longer matches Inspect or lost max Thinking")
					}
					f.assertUnchanged(t, before, true)
				}
			}
			if len(p.memberTools.roleFence.records) != 0 || len(p.memberTools.roleFence.slots) != 0 {
				t.Fatal("inspection or preparation reserved a role execution")
			}
		})
	}
}

// This exercises the real preparer, claim, role runtime and provider with a local
// synthetic protocol recorder. It is not evidence of a real Pi/model execution.
func TestPrepareRoleThinkingClaimedNativeArgv(t *testing.T) {
	for _, kind := range []store.CoordinationRoleKind{store.CoordinationRoleCoordinator, store.CoordinationRoleWorker} {
		t.Run(string(kind), func(t *testing.T) {
			f := newRolePrepareFixture(t)
			if kind == store.CoordinationRoleWorker {
				f.selectWorker(t)
			}
			f.enableRuntime(t)
			const model = "openai-codex/gpt-6.1-sol"
			// Change only trusted fixture setup, never the prepared model/Thinking.
			f.exec(t, "UPDATE members SET model=? WHERE id=?", model, f.member.ID)
			f.selectDelivery(t, f.trigger.ID)
			f.opts.RoleThinking = "max"
			selected := f.opts.Providers[1]
			body := `#!/bin/sh
set -eu
printf 'invocation\n' >> "$0.calls"
printf '%s\n' "$@" > argv
/bin/cat > stdin
while [ "$1" != "--session-id" ]; do shift; done
printf '{"type":"session","version":3,"id":"%s","cwd":"%s"}\n' "$2" "$PWD"
/bin/cat "$0.events"
`
			prepareRoleOK(t, os.WriteFile(selected.Path, []byte(body), 0700))
			prepareRoleOK(t, os.WriteFile(selected.Path+".events", prepareMemberToolsNativeEvents(t, model, "synthetic role argv receipt"), 0600))
			// Race-instrumented SQLite setup is not part of a latency contract.
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			before := f.snapshot(t)
			ready, err := f.preparer(t).Prepare(ctx, f.pending)
			prepareRoleOK(t, err)
			if ready.Request.Model != model || ready.Request.Thinking != "max" {
				t.Fatal("actual Prepare request did not supply the model/max being tested downstream")
			}
			f.assertUnchanged(t, before, true)
			turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.pending.Delivery.ID,
				MaxActive: 1, ResourceKeys: ready.ResourceKeys})
			prepareRoleOK(t, err)
			claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
			request := ready.Request
			request.RunID, request.RuntimeDir = turn.ID, f.runtimeOptions.RoleExecutionRoot
			receipt := ready.Executor.Execute(ctx, request, func(event provider.Event) error {
				return f.s.RecordTurnEvent(ctx, claim, event)
			})
			if receipt.Outcome != provider.OutcomeSucceeded || !receipt.Started || !receipt.Accepted || !receipt.DirectChildReaped || !receipt.OutputComplete {
				t.Fatalf("synthetic claimed native lifecycle failed: outcome=%s started=%t caller_context=%v reason=%s", receipt.Outcome, receipt.Started, ctx.Err(), receipt.Reason)
			}
			settled, err := f.s.ApplyTurnReceipt(ctx, claim, receipt)
			prepareRoleOK(t, err)
			if settled.State != domain.TurnStateSucceeded || settled.ReplyMessageID == "" {
				t.Fatal("synthetic native receipt did not settle the real claimed turn")
			}
			tools := "list_members,read_channel,search_history,read_history,decide_dispatch"
			if kind == store.CoordinationRoleWorker {
				tools = "read_channel,search_history,read_history,submit_result"
			}
			argv, err := os.ReadFile(filepath.Join(request.CWD, "argv"))
			prepareRoleOK(t, err)
			want := []string{"--offline", "--mode", "json", "-p", "--no-extensions", "--no-mcp", "--no-builtin-tools", "--no-skills", "--no-prompt-templates", "--no-context-files", "--no-approve",
				"--session-dir", filepath.Join(request.RuntimeDir, turn.ID), "--session-id", turn.ID, "--tools", tools, "--extension", f.opts.PiMemberToolsExtension,
				"--model", model, "--thinking", "max"}
			if !slices.Equal(strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n"), want) {
				t.Fatal("real provider did not pass the prepared model/max and exact fixed role tools to the recorder")
			}
			stdin, err := os.ReadFile(filepath.Join(request.CWD, "stdin"))
			prepareRoleOK(t, err)
			calls, err := os.ReadFile(selected.Path + ".calls")
			prepareRoleOK(t, err)
			if string(stdin) != ready.Request.Prompt || string(calls) != "invocation\n" || len(f.opts.MemberTools.tools.capabilities) != 0 {
				t.Fatal("native prompt, single invocation or capability revocation changed")
			}
			memberToolRuntimeEmpty(t, f.opts.MemberTools.configRoot)
			prepareRoleOK(t, f.opts.MemberTools.CheckCoordinationRoleCapacity(ctx))
			var claims int
			prepareRoleOK(t, f.db.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", turn.ID).Scan(&claims))
			if claims != 0 || len(f.opts.MemberTools.roleFence.records) != 0 {
				t.Fatal("settled synthetic execution retained claims or its local execution owner")
			}
		})
	}
}
