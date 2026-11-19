//go:build unix

package platform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/coordination"
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

// The injected preview forwards continuation to the real B resolver. The
// unused legacy worker methods are not connected to this intake fixture.
type composedIntakeWorkInputs struct {
	MemberWorkInputService
	engine *coordination.Engine
	calls  int
}

func (w *composedIntakeWorkInputs) ContinueWork(ctx context.Context, scope store.MemberToolScope, candidateID string, input MemberContinueWorkInput, content, key string) (domain.ChatAssessmentReceipt, error) {
	w.calls++
	return w.engine.AssessChatIntent(ctx, scope, candidateID, domain.ChatAssessment{
		Disposition: domain.ChatDispositionContinueWork, Reason: input.Reason,
		Followup: &domain.ChatFollowupAssessment{TaskID: input.TaskID, Constraint: input.Constraint},
	}, content, key)
}

func TestPiComposedIntakeResolvesIssuedOptions(t *testing.T) {
	launcher, nodeErr := installPiIntakeDigestReader(t)
	if nodeErr != nil {
		t.Skip("offline bridge parser fixture requires Node")
	}
	ctx := t.Context()
	f, _ := nativeSessionPreparation(t, domain.ChatIntakePurpose)
	f.opts.Providers = f.opts.Providers[1:]
	f.opts.Providers[0].Path = launcher
	f.opts.ExecutionTimeout = 10 * time.Second
	engine := f.runtimeOptions.ChatIntake
	thread := domain.ThreadRef{GroupID: f.group.ID, TopicID: "combined-resolution"}
	policy, err := engine.SetChatPolicy(ctx, thread, domain.ChatPolicyRequest{
		Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 2,
		Investigation: &domain.ChatInvestigationPolicy{Objective: "核对话题材料来源", AllowTopicMessages: true},
	}, 0, "policy")
	prepareRoleOK(t, err)
	local := NewLocalUser(f.s)
	source := func(t *testing.T, body, key string) domain.Message {
		t.Helper()
		m, err := local.CreateMessage(t.Context(), f.group.ID, MessageInput{TopicID: thread.TopicID, Content: body,
			RecipientIDs: []string{f.coordinator.ID}}, key)
		prepareRoleOK(t, err)
		return m
	}
	original := source(t, "请交付纯文本摘要。", "old-source")
	turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: original.Deliveries[0].ID, MaxActive: 1})
	prepareRoleOK(t, err)
	scope, err := f.s.BindMemberTool(ctx, store.TurnClaim{GroupID: f.group.ID, TurnID: turn.ID, Token: turn.ClaimToken})
	prepareRoleOK(t, err)
	candidate, err := engine.CaptureChatCandidate(ctx, scope)
	prepareRoleOK(t, err)
	old, err := engine.AssessChatIntent(ctx, scope, candidate.ID, domain.ChatAssessment{Disposition: "request_work", Action: domain.ChatActionTextSubmission,
		Title: "纯文本摘要", Acceptance: "说明依据", Reason: "明确请求"}, "记录原任务。", "old-admit")
	prepareRoleOK(t, err)
	_, err = f.s.ApplyTurnReceipt(ctx, scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted})
	prepareRoleOK(t, err)
	dispatch, err := f.s.MaterializeDispatchRequest(ctx, old.DispatchRequestID, 1, time.Now().UTC())
	prepareRoleOK(t, err)
	f.selectDelivery(t, dispatch.Delivery.MessageID)
	f.selectWorker(t)
	legacy, err := f.preparer(t).Prepare(ctx, f.pending)
	prepareRoleOK(t, err)
	prior, priorReceipt, priorRun := runNativeSessionFixture(t, f, legacy, nil, provider.OutcomeSucceeded, true)
	priorTurn, err := f.s.GetTurn(ctx, prior.GroupID, prior.TurnID)
	prepareRoleOK(t, err)
	if priorReceipt.NativeRef == nil || priorReceipt.NativeRef.Pi == nil || priorTurn.State != domain.TurnStateSucceeded {
		t.Fatal("fixture did not retain a #193 binding on the original worker conversation")
	}

	useReleasePiBridge(t, f)
	prepareRoleOK(t, os.Mkdir(filepath.Join(f.opts.PrivateWorkspaceRoot, InvestigationCWDName), 0700))
	f.opts.Investigation = &InvestigationPreparationOptions{PiPath: launcher}
	entry, err := filepath.Abs("../../extensions/pi-bridge/fresh-task-work.ts")
	prepareRoleOK(t, err)
	workInputs := &composedIntakeWorkInputs{engine: engine}
	f.runtimeOptions.WorkInputs, f.runtimeOptions.FreshTaskWork = workInputs, NewPiFreshTaskWorkLaunch(entry, 0)
	f.opts.MemberTools, err = NewMemberToolRuntime(f.s, f.runtimeOptions)
	prepareRoleOK(t, err)
	t.Cleanup(func() { prepareRoleOK(t, f.opts.MemberTools.Close()) })
	engine.EnableTaskWorkExecution()
	engine.EnableInvestigationExecution()
	p, tools := f.preparer(t), f.opts.MemberTools.Tools()

	const excerpt = " \n这次只讨论，不开工🙂。\n"
	const constraint = "注明原文出处"
	const missingTask = "unknown-task"
	proposal := domain.ChatAssessment{Disposition: "prepare_work", Action: domain.ChatActionReadOnlyInvestigation, Reason: "按当前范围调查"}
	for _, tc := range []struct {
		name, body, outcome, denial string
		assessment                  domain.ChatAssessment
		dropPolicy                  bool
	}{
		{"continue", "请继续任务 " + old.TaskID + "。" + constraint, "continued", "", domain.ChatAssessment{Disposition: "continue_work", Reason: "原任务续办",
			Followup: &domain.ChatFollowupAssessment{TaskID: old.TaskID, Constraint: constraint}}, false},
		{"new-investigation", "请另开只读调查，核对话题材料来源。", "admitted", "", proposal, false},
		{"discuss", "关于任务 " + old.TaskID + "：" + excerpt + "先交换想法。", "discuss", "", domain.ChatAssessment{Disposition: "discuss",
			Reason: "明确只讨论", DiscussionOnlyExcerpt: new(excerpt)}, false},
		{"unknown-task", "请继续任务编号: " + missingTask, "not_admitted", "followup_target_unavailable", domain.ChatAssessment{Disposition: "continue_work",
			Reason: "引用未识别的任务", Followup: &domain.ChatFollowupAssessment{TaskID: missingTask}}, false},
		{"revoked-policy", "请另开只读调查，核对话题材料来源。", "", "", proposal, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			m := source(t, tc.body, tc.name)
			prepared, err := p.Prepare(ctx, store.PendingDelivery{GroupID: m.GroupID, Delivery: m.Deliveries[0]})
			prepareRoleOK(t, err)
			permissions := strings.Join(decodePreparation(t, prepared.Request.Prompt).Permissions, "\n")
			if !strings.Contains(permissions, "action=read_only_investigation") || !strings.Contains(permissions, "followup.task_id") ||
				!strings.Contains(permissions, "discussion_only_excerpt") || strings.Contains(permissions, "否则只讨论/澄清") ||
				strings.Contains(permissions, "明确续办已有任务时请澄清") || strings.Contains(permissions, nativeSessionTextPermission) {
				t.Error("combined guidance contradicts or fails to distinguish the issued options")
			}
			t.Setenv("FIXTURE_INTAKE_MODE", "combined")
			if tc.dropPolicy {
				disabled := policy.ChatPolicyRequest
				disabled.Enabled = false
				_, err = engine.SetChatPolicy(ctx, thread, disabled, policy.Revision, "disable")
				prepareRoleOK(t, err)
				t.Setenv("FIXTURE_INTAKE_MODE", "excerpt-and-continue")
			}
			run := prepared.Executor.(*memberToolRuntimeRun)
			build := run.intakeBuilder
			if build == nil || run.nativeSession != nil {
				t.Fatal("combined intake did not select the issued digest builder")
			}
			var token string
			run.intakeBuilder = func(issued provider.NativeMemberToolConfig) (provider.Executor, error) {
				data, err := os.ReadFile(issued.Path)
				prepareRoleOK(t, err)
				var config struct{ Token string }
				prepareRoleOK(t, json.Unmarshal(data, &config))
				token = config.Token
				if token == "" || token == priorRun.token || issued.SHA256 != sha256.Sum256(data) {
					t.Fatal("builder did not receive a new token and the exact issued bytes digest")
				}
				return build(issued)
			}
			before := f.snapshot(t)
			req, claim := claimNativeFenceFixture(t, f, prepared)
			var result domain.ChatAssessmentReceipt
			var assessErr error
			assessed := false
			execution := run.Execute(ctx, req, func(event provider.Event) error {
				if err := f.s.RecordTurnEvent(ctx, claim, event); err != nil || event.Kind != provider.AcceptedEvent {
					return err
				}
				// The synthetic child emits Accepted only after the real bridge
				// parses the issued config. Exercise the same resolver as send_message.
				captured, err := tools.ChatCandidate(ctx, token)
				if err != nil || captured == nil || captured.MessageID != m.ID || captured.SourceID != m.SourceID ||
					captured.SourceKind != "local_user_chat" || captured.TurnID != claim.TurnID || (captured.Policy.Investigation != nil) == tc.dropPolicy {
					return errors.New("missing or mismatched captured intake source/policy")
				}
				assessed = true
				if a := tc.assessment; a.Disposition == domain.ChatDispositionContinueWork {
					result, assessErr = tools.ContinueWork(ctx, token, MemberContinueWorkInput{Reason: a.Reason, TaskID: a.Followup.TaskID, Constraint: a.Followup.Constraint}, "记录评估。", tc.name)
				} else {
					result, assessErr = tools.AssessChatIntent(ctx, token, a, "记录评估。", tc.name)
				}
				if tc.dropPolicy && errors.Is(assessErr, store.ErrMemberToolDenied) {
					return nil
				}
				return assessErr
			})
			if !assessed || execution.Outcome != provider.OutcomeSucceeded || execution.NativeRef != nil || token == claim.Token {
				t.Fatalf("combined fresh wire did not reach the live resolver: %s (%s; assessment: %v)", execution.Outcome, execution.Reason, assessErr)
			}
			_, err = f.s.ApplyTurnReceipt(ctx, claim, execution)
			prepareRoleOK(t, err)
			if _, err := tools.TransportOperations(ctx, token); !errors.Is(err, store.ErrMemberToolDenied) {
				t.Fatal("combined intake retained its capability")
			}
			after := f.snapshot(t)
			if result.Outcome != tc.outcome || result.ReasonCode != tc.denial || tc.outcome != "admitted" &&
				(!reflect.DeepEqual(before["tasks"], after["tasks"]) || !reflect.DeepEqual(before["coordination_dispatch_requests"], after["coordination_dispatch_requests"])) {
				t.Fatalf("unexpected durable work decision: outcome=%q reason=%q err=%v", result.Outcome, result.ReasonCode, assessErr)
			}
			switch tc.outcome {
			case "continued":
				work, err := f.s.ResolveTaskWorkDelivery(ctx, f.group.ID, result.WorkDeliveryID)
				prepareRoleOK(t, err)
				m, err := f.s.GetMessage(ctx, work.GroupID, work.MessageID)
				prepareRoleOK(t, err)
				d := m.Deliveries[0]
				typed, err := p.Prepare(ctx, store.PendingDelivery{GroupID: work.GroupID, Delivery: d})
				prepareRoleOK(t, err)
				newRun, ok := typed.Executor.(*taskWorkRun)
				if result.TaskID != old.TaskID || result.MandateID != old.MandateID || result.DispatchRequestID != "" ||
					work.SourceKind != domain.TaskWorkSourceFollowup || work.SourceID != result.WorkInputID || work.ConversationID != priorTurn.ConversationID ||
					work.MemberID != priorTurn.MemberID || d.State != domain.DeliveryStateQueued || d.TurnID != "" || !ok || newRun.base.nativeSession != nil ||
					typed.Request.CWD == legacy.Request.CWD || slices.Equal(typed.ResourceKeys, legacy.ResourceKeys) {
					t.Fatal("continuation did not produce a fresh wake for the original task and owner")
				}
			case "admitted":
				frozen, err := f.s.GetTaskInvestigationScope(ctx, m.GroupID, result.TaskID)
				prepareRoleOK(t, err)
				if result.TaskID == old.TaskID || result.WorkInputID != "" || result.Authority != "written_preauthorization" ||
					frozen.PolicyRevision != policy.Revision || frozen.Objective != policy.Investigation.Objective || frozen.MandateID != result.MandateID || result.DispatchRequestID == "" {
					t.Fatal("new investigation did not bind a separate Task to the actual policy")
				}
			case "discuss", "not_admitted":
				var saved domain.ChatAssessmentReceipt
				var wire []byte
				prepareRoleOK(t, f.db.QueryRowContext(ctx, "SELECT receipt_json FROM chat_assessments WHERE id=?", result.ID).Scan(&wire))
				prepareRoleOK(t, json.Unmarshal(wire, &saved))
				if saved.TaskID != "" || saved.DispatchRequestID != "" || saved.WorkInputID != "" || saved.SourceID != m.SourceID || saved.TurnID != claim.TurnID ||
					tc.outcome == "discuss" && (!saved.DiscussionOnly || saved.DiscussionOnlyExcerpt != excerpt) || tc.denial != "" && saved.Question == "" {
					t.Fatal("did not retain the exact discussion restriction or the existing B denial without work")
				}
			default:
				if !errors.Is(assessErr, store.ErrMemberToolDenied) || result.ID != "" {
					t.Fatal("policy change after preparation still allowed the unissued action")
				}
			}
		})
	}
	if workInputs.calls != 2 {
		t.Fatalf("expected two calls to the injected WorkInputs resolver, got %d", workInputs.calls)
	}
	after, err := f.s.GetTurn(ctx, prior.GroupID, prior.TurnID)
	prepareRoleOK(t, err)
	if !reflect.DeepEqual(after, priorTurn) {
		t.Fatal("combined work changed the original native turn or claim")
	}
}
