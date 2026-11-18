//go:build unix

package platform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestPiComposedIntakeRejectsLegacyResume(t *testing.T) {
	for _, mode := range []string{"continue-only", "combined"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			combined := mode == "combined"
			f, _ := nativeSessionPreparation(t, domain.ChatIntakePurpose)
			f.opts.RoleThinking = "" // Sample only the ordinary intake profile.
			engine := f.runtimeOptions.ChatIntake
			original, err := f.s.GetMessage(ctx, f.group.ID, f.ordinary.Delivery.MessageID)
			prepareRoleOK(t, err)
			if combined {
				engine.EnableInvestigationExecution()
				_, err = engine.SetChatPolicy(ctx, domain.ThreadRef{GroupID: f.group.ID, TopicID: original.TopicID}, domain.ChatPolicyRequest{
					Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 1,
					Investigation: &domain.ChatInvestigationPolicy{Objective: "fixture", AllowTopicMessages: true},
				}, 0, "composed-policy")
				prepareRoleOK(t, err)
			}
			legacy, err := f.preparer(t).Prepare(ctx, f.ordinary)
			prepareRoleOK(t, err)
			prior, _, _ := runNativeSessionFixture(t, f, legacy, nil, provider.OutcomeSucceeded, true)
			_, resumed, _ := runNativeSessionFixture(t, f, nextNativeSessionPreparation(t, f), nil, provider.OutcomeSucceeded, true)
			if decodeNativeFixtureBinding(t, resumed).ResumedFromTurnID != prior.TurnID || resumed.NativeRef.Pi == nil {
				t.Fatal("qualified #193 text did not resume before the intake schema changed")
			}

			entry, err := filepath.Abs("../../extensions/pi-bridge/fresh-task-work.ts")
			prepareRoleOK(t, err)
			f.runtimeOptions.FreshTaskWork = NewPiFreshTaskWorkLaunch(entry, 0)
			f.opts.MemberTools, err = NewMemberToolRuntime(f.s, f.runtimeOptions)
			prepareRoleOK(t, err)
			t.Cleanup(func() { prepareRoleOK(t, f.opts.MemberTools.Close()) })
			if combined {
				useReleasePiBridge(t, f)
				f.opts.Providers[0].Path = filepath.Join(t.TempDir(), "alternate-pi")
				prepareRoleOK(t, os.WriteFile(f.opts.Providers[0].Path, nil, 0700))
				prepareRoleOK(t, os.Mkdir(filepath.Join(f.opts.PrivateWorkspaceRoot, InvestigationCWDName), 0700))
				f.opts.Investigation = &InvestigationPreparationOptions{PiPath: f.opts.Providers[1].Path}
			}
			msg, err := NewLocalUser(f.s).CreateMessage(ctx, f.group.ID, MessageInput{TopicID: original.TopicID,
				Content: "现在请评估这条消息。", RecipientIDs: []string{f.coordinator.ID}}, "composed-intake")
			prepareRoleOK(t, err)
			delivery := msg.Deliveries[0]
			if delivery.ConversationID != f.ordinary.Delivery.ConversationID {
				t.Fatal("fixture did not reuse the conversation with an existing #193 reference")
			}
			p := f.preparer(t)
			prepared, err := p.Prepare(ctx, store.PendingDelivery{GroupID: f.group.ID, Delivery: delivery})
			prepareRoleOK(t, err)
			run := prepared.Executor.(*memberToolRuntimeRun)
			if run.nativeSession != nil || prepared.Request.Thinking != "" || prepared.Request.Model != f.coordinator.Model ||
				slices.Contains(decodePreparation(t, prepared.Request.Prompt).Permissions, nativeSessionTextPermission) {
				t.Error("changed intake offered a native session or altered the member model/fresh thinking")
			}
			caps := p.AdapterReports()[f.coordinator.ProviderID].Capabilities
			if caps == nil || caps.NativeResume != provider.SupportUnsupported {
				t.Error("typed intake reported native resume support")
			}
			prompt := prepared.Request.Prompt
			if !strings.Contains(prompt, "continue_work") || strings.Contains(prompt, "discussion_only_excerpt") != combined ||
				strings.Contains(prompt, domain.ChatActionReadOnlyInvestigation) != combined || strings.Contains(prompt, "明确续办已有任务时请澄清") {
				t.Error("intake guidance omitted an issued option or contradicted continuation")
			}
			if t.Failed() {
				return
			}

			// Pass the actual issued bytes through the real provider constructor.
			// Only the final execution is synthetic; it uses the live registry.
			calls := 0
			inspect := func(path string, digest [sha256.Size]byte) provider.Executor {
				t.Helper()
				data, err := os.ReadFile(path)
				prepareRoleOK(t, err)
				var cfg struct {
					Token            string
					ContinueWork     bool     `json:"continue_work"`
					Excerpt          bool     `json:"discussion_only_excerpt"`
					Action           string   `json:"intake_assessment_action"`
					InvestigationOps []string `json:"investigation_tools"`
				}
				prepareRoleOK(t, json.Unmarshal(data, &cfg))
				wantAction := ""
				if combined {
					wantAction = domain.ChatActionReadOnlyInvestigation
				}
				if cfg.Token == "" || !cfg.ContinueWork || cfg.Excerpt != combined || cfg.Action != wantAction || cfg.InvestigationOps != nil ||
					combined && digest != sha256.Sum256(data) {
					t.Fatal("actual Pi config lost an intake hint or leaked a strict role profile")
				}
				return memberToolRuntimeExecutorFunc(func(ctx context.Context, request provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
					calls++
					profile, err := f.opts.MemberTools.Tools().TransportProfile(ctx, cfg.Token)
					prepareRoleOK(t, err)
					if !slices.Equal(profile.Operations, chatIntakeOperations()) || !profile.ContinueWork ||
						profile.InvestigationIntake != combined || profile.InvestigationRole || request.RunID == prior.TurnID {
						t.Fatal("issued intake borrowed role authority or execution identity")
					}
					return provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}
				})
			}
			if combined {
				build := run.intakeBuilder
				if build == nil {
					t.Fatal("combined intake did not select the digest-bound builder")
				}
				run.intakeBuilder = func(config provider.NativeMemberToolConfig) (provider.Executor, error) {
					if actual, err := build(config); err != nil || actual == nil {
						t.Fatalf("provider rejected the issued combined config: %v", err)
					}
					return inspect(config.Path, config.SHA256), nil
				}
			} else {
				factory := run.factory
				run.factory = func(path, id string, names []string) (provider.Executor, error) {
					if actual, err := factory(path, id, names); err != nil || actual == nil {
						t.Fatalf("provider rejected the issued continuation config: %v", err)
					}
					return inspect(path, [sha256.Size]byte{}), nil
				}
			}
			turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: delivery.ID, MaxActive: 1, ResourceKeys: prepared.ResourceKeys})
			prepareRoleOK(t, err)
			request := prepared.Request
			request.RunID, request.RuntimeDir = turn.ID, f.runtimeOptions.RoleExecutionRoot
			receipt := run.Execute(ctx, request, nil)
			if calls != 1 || receipt.Outcome != provider.OutcomeNotStarted || receipt.NativeRef != nil {
				t.Fatal("fresh intake did not execute the fixture exactly once without a native binding")
			}
			_, err = f.s.ApplyTurnReceipt(ctx, store.TurnClaim{GroupID: f.group.ID, TurnID: turn.ID, Token: turn.ClaimToken}, receipt)
			prepareRoleOK(t, err)
		})
	}
}
