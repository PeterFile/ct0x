package platform_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func TestAgyExplicitSuccessorPreparesAndBindsFreshTaskWork(t *testing.T) {
	f := newTaskWorkRoleRuntime(t)
	ctx := t.Context()
	engine := coordination.New(f.db)
	thread := domain.ThreadRef{GroupID: f.group, TopicID: "compatibility"}
	policy := domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 1}
	_, err := engine.SetChatPolicy(ctx, thread, policy, 0, "topic-policy")
	roleOK(t, err)
	message := func(text, key string, recipients []string) domain.Message {
		t.Helper()
		m, err := f.local.CreateMessage(ctx, f.group, platform.MessageInput{TopicID: thread.TopicID, Content: text, RecipientIDs: recipients}, key)
		roleOK(t, err)
		return m
	}
	work := func(source domain.Message, title string) domain.RequestWorkRequest {
		return domain.RequestWorkRequest{TopicID: thread.TopicID, MessageID: source.ID, Mode: domain.RoutingDecisionCreateNew,
			Reason: source.Content, NewTask: &domain.NewTaskRequest{Title: title, Acceptance: "交付可追溯的结论"}}
	}
	original, err := engine.RequestWork(ctx, f.group, work(message("授权原交付。", "original-source", []string{}), "旧结论"), "original-admission")
	roleOK(t, err)
	originalAttempt, err := f.local.ActivateTaskAttempt(ctx, f.group, original.Task.ID, platform.TaskAttemptInput{ExpectedRevision: 1}, "original-attempt")
	roleOK(t, err)
	oldResult, err := f.local.SubmitTask(ctx, f.group, original.Task.ID, platform.TaskSubmissionInput{ExpectedRevision: 1, AttemptID: originalAttempt.ID, Summary: "旧结论 v1"}, "old-result")
	roleOK(t, err)
	review, err := f.local.ReviewTask(ctx, f.group, original.Task.ID, platform.TaskReviewInput{ExpectedRevision: 1, SubmissionID: oldResult.ID, Outcome: domain.ReviewOutcomeAccepted}, "accept-original")
	roleOK(t, err)
	request := work(message("授权关联续办并交付文本。", "successor-source", []string{}), "续办结论")
	request.Predecessor = &domain.TaskSuccessorInput{TaskID: original.Task.ID, ExpectedRevision: 1, SubmissionID: oldResult.ID, ReviewID: review.ID}
	var installed bool
	roleOK(t, f.raw.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='task_successors' AND type='table')`).Scan(&installed))
	if installed {
		f.worker, err = f.local.CreateMember(ctx, f.group, platform.MemberInput{Name: "AGY worker", Role: "执行成员", ProviderID: "agy", Model: "selected"}, "agy-worker")
		roleOK(t, err)
		_, err = f.db.SetDispatchPolicy(ctx, f.group, []string{f.worker.ID}, 1, "agy-dispatch-policy")
		roleOK(t, err)
	}
	successor, err := engine.RequestWork(ctx, f.group, request, "successor-admission")
	if !installed {
		if !errors.Is(err, store.ErrConflict) || successor.Task.ID != "" || successor.DispatchRequestID != "" {
			t.Fatalf("unregistered successor must fail closed: %+v %v", successor, err)
		}
		return
	}
	roleOK(t, err)
	if _, err := f.db.GetChatAdmission(ctx, f.group, successor.Task.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("explicit continuation unexpectedly has a chat assessment: %v", err)
	}
	worker := f.worker
	// The runtime's provider inventory selects AGY for the Worker already bound by the Store.
	root := t.TempDir()
	private, cli := filepath.Join(root, "workspaces"), filepath.Join(root, "offline-agy")
	roleOK(t, os.Mkdir(private, 0700))
	roleOK(t, os.WriteFile(cli, nil, 0700))
	selected := provider.Provider{ID: worker.ProviderID, Name: provider.Agy, Installation: provider.Installed, Path: cli, Version: "1.5.16"}
	if !provider.SupportsAgyFreshTaskWork(selected) {
		t.Fatal("fixture does not bind the AGY text continuation profile")
	}
	dispatch, err := f.db.MaterializeDispatchRequest(ctx, successor.DispatchRequestID, 1, time.Now().UTC())
	roleOK(t, err)
	coordinator := f.claim(t, dispatch.Delivery.DeliveryID)
	scope, err := f.db.BindMemberTool(ctx, coordinator.claim)
	roleOK(t, err)
	decision, err := f.db.DecideDispatch(ctx, store.CoordinationToolScope{Member: scope, Provenance: coordinator.provenance},
		store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: worker.ID, Reason: "执行文本续办"}, "assign-successor")
	roleOK(t, err)
	_, err = f.db.ApplyTurnReceipt(ctx, coordinator.claim, roleNotStarted())
	roleOK(t, err)
	assignment, err := f.db.MaterializeAssignment(ctx, decision.Assignment.ID, 1, time.Now().UTC())
	roleOK(t, err)
	initial := f.claim(t, assignment.Delivery.DeliveryID)
	scope, err = f.db.BindMemberTool(ctx, initial.claim)
	roleOK(t, err)
	prior, err := f.db.BindAssignmentAttempt(ctx, store.CoordinationToolScope{Member: scope, Provenance: initial.provenance})
	roleOK(t, err)
	_, err = f.db.ApplyTurnReceipt(ctx, initial.claim, roleNotStarted())
	roleOK(t, err)

	const constraint = "保持旧格式兼容"
	engine.EnableTaskWorkExecution()
	wakeFor := func(constraint, key string) (domain.ChatAssessmentReceipt, store.PendingDelivery) {
		t.Helper()
		input := message(constraint, "constraint-"+key, []string{f.lead.ID})
		intake, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group, DeliveryID: input.Deliveries[0].ID, MaxActive: 1})
		roleOK(t, err)
		intakeClaim := store.TurnClaim{GroupID: f.group, TurnID: intake.ID, Token: intake.ClaimToken}
		scope, err := f.db.BindMemberTool(ctx, intakeClaim)
		roleOK(t, err)
		chat, err := engine.CaptureChatCandidate(ctx, scope)
		roleOK(t, err)
		wake, err := engine.AssessChatIntent(ctx, scope, chat.ID, domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork,
			Reason: "原授权内的约束", Followup: &domain.ChatFollowupAssessment{Constraint: constraint}}, "记录补充。", "followup-"+key)
		roleOK(t, err)
		if wake.WorkDeliveryID == "" || wake.WorkInputID == "" || wake.TaskID != successor.Task.ID {
			t.Fatalf("the ordinary message did not create the exact typed successor wake: %+v", wake)
		}
		_, err = f.db.ApplyTurnReceipt(ctx, intakeClaim, roleNotStarted())
		roleOK(t, err)
		wakeMessage, err := f.db.GetMessage(ctx, f.group, wake.WorkWakeMessageID)
		roleOK(t, err)
		return wake, store.PendingDelivery{GroupID: f.group, SenderID: wakeMessage.SenderID, Delivery: wakeMessage.Deliveries[0]}
	}
	wake, queued := wakeFor(constraint, "first")
	resolved, err := f.db.ResolveTaskWorkDelivery(ctx, f.group, queued.Delivery.ID)
	roleOK(t, err)
	if resolved.Blocked != "" || resolved.SourceID != wake.WorkInputID || resolved.MandateID != successor.Mandate.ID {
		t.Fatalf("Store did not resolve the exact successor wake: %+v", resolved)
	}

	f.options.ChatIntake = engine
	started := false
	f.options.FreshTaskWork = &platform.FreshTaskWorkLaunch{Supports: provider.SupportsAgyFreshTaskWork,
		Construct: func(_ context.Context, p provider.Provider, b provider.FreshTaskWorkBinding) (provider.Executor, error) {
			if p.ID != selected.ID || p.Name != selected.Name || p.Path != selected.Path || !slices.Equal(b.Operations, provider.PiFreshTaskWorkToolNames()) {
				t.Fatal("incorrect AGY bootstrap")
			}
			data, err := os.ReadFile(b.ConfigPath)
			roleOK(t, err)
			var config struct{ Token, Endpoint string }
			roleOK(t, json.Unmarshal(data, &config))
			return roleNative(func(ctx context.Context, req provider.ExecuteRequest, emit func(provider.Event) error) provider.ExecutionReceipt {
				started = true
				if config.Token == "" || config.Endpoint != f.options.Endpoint || req.RunID != b.RunID || !strings.Contains(req.Prompt, successor.Mandate.ID) ||
					!strings.Contains(req.Prompt, wake.WorkInputID) || strings.Contains(req.Prompt, constraint) {
					t.Fatal("incorrect prompt or session-scoped token")
				}
				roleOK(t, emit(provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: req.RunID}))
				for i, op := range []string{"read_work_inputs", "ack_work_input"} {
					params := fmt.Sprintf(`{"name":%q,"arguments":{"input_id":%q}}`, op, wake.WorkInputID)
					text := memberMCPRoleText(t, memberMCPRoleHTTP(t, f, config.Token, fmt.Sprint(i+1), "tools/call", params), false)
					if i == 0 && !strings.Contains(text, constraint) || i == 1 && !strings.Contains(text, `"disposition":"confirmed"`) {
						t.Fatalf("unexpected %s result: %s", op, text)
					}
				}
				roleOK(t, emit(provider.Event{Kind: provider.TerminalEvent, NativeSessionID: req.RunID, Terminal: provider.OutcomeSucceeded}))
				return provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded,
					Started: true, Accepted: true, NativeSessionID: req.RunID, DirectChildReaped: true, OutputComplete: true}
			}), nil
		}}
	f.runtime, err = platform.NewMemberToolRuntime(f.db, f.options)
	roleOK(t, err)
	defer f.runtime.Close()
	mcp, err := httpapi.NewServer(httpapi.DefaultAddress, httpapi.MemberMCPRoutes(f.runtime.Tools())...)
	roleOK(t, err)
	f.handler = mcp.Handler
	prep, err := platform.NewPreparer(f.db, platform.PrepareOptions{Providers: []provider.Provider{selected}, PrivateWorkspaceRoot: private,
		MemberTools: f.runtime, EnableAgyACP: true})
	roleOK(t, err)
	otherGroup := queued
	otherGroup.GroupID = f.other
	if result, err := prep.Prepare(ctx, otherGroup); result.Executor != nil || err == nil {
		t.Fatal("successor wake escaped its group")
	}
	prepared, err := prep.Prepare(ctx, queued)
	if err != nil || prepared.Executor == nil || len(prepared.ResourceKeys) != 1 {
		t.Fatalf("normally queued explicit successor must prepare a fresh AGY executor: %v", err)
	}
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group, DeliveryID: queued.Delivery.ID, MaxActive: 1, ResourceKeys: prepared.ResourceKeys})
	roleOK(t, err)
	claim := store.TurnClaim{GroupID: f.group, TurnID: turn.ID, Token: turn.ClaimToken}
	prepared.Request.RunID, prepared.Request.RuntimeDir = turn.ID, f.root
	receipt := prepared.Executor.Execute(ctx, prepared.Request, func(e provider.Event) error { return f.db.RecordTurnEvent(ctx, claim, e) })
	if !started || receipt.Outcome != provider.OutcomeSucceeded {
		t.Fatalf("claimed successor did not enter the AGY profile: started=%t outcome=%v", started, receipt.Outcome)
	}
	settled, err := f.db.ApplyTurnReceipt(ctx, claim, receipt)
	roleOK(t, err)
	inputs, err := f.db.ListTaskWorkInputs(ctx, f.group, successor.Task.ID, store.ListOptions{})
	roleOK(t, err)
	if settled.State != domain.TurnStateSucceeded || len(inputs.Items) != 1 || inputs.Items[0].Consumption == nil {
		t.Fatalf("confirmation receipt was not stored on the successor: %+v", inputs)
	}
	consumption := inputs.Items[0].Consumption
	attempt, err := f.db.GetTaskAttempt(ctx, f.group, successor.Task.ID, consumption.AttemptID)
	roleOK(t, err)
	held, err := f.db.HasResourceClaim(ctx, prepared.ResourceKeys[0])
	roleOK(t, err)
	if held || consumption.TurnID != turn.ID || attempt.TurnID != turn.ID || attempt.ID == prior.AttemptID || attempt.TaskRevision != resolved.TaskRevision {
		t.Fatalf("successor ACK used the wrong Attempt or retained a claim: %+v", attempt)
	}
	_, beforeDrift := wakeFor("保留更改原因", "policy-drift")
	policy.Enabled = false
	_, err = engine.SetChatPolicy(ctx, thread, policy, 1, "withdraw-policy")
	roleOK(t, err)
	if result, err := prep.Prepare(ctx, beforeDrift); result.Executor != nil || !errors.Is(err, scheduler.ErrNotReady) {
		t.Fatalf("successor wake prepared after its text policy changed: %v", err)
	}
}
