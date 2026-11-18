package store

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestTaskWorkFollowupImplicitEligibility(t *testing.T) {
	for _, state := range []string{"cancel_requested", "unknown"} {
		t.Run(state, func(t *testing.T) {
			f := chatSetup(t, "user", false)
			f.s.EnableTaskWorkExecution()
			worker := chatAuthorityWorker(t, f)
			p := worker.Provenance
			co := taskWorkTestCoordinator(t, f.s, p.GroupID, p.AssignmentID)
			taskWorkTestReceipt(t, f.s, co, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)

			// A real, same-topic Task/Mandate without a worker Assignment.
			m := chatLocalMessage(t, f.s, MessageRequest{GroupID: p.GroupID, TopicID: p.TopicID, ActorID: "user", Content: "整理旧资料。"}, []string{})
			other, err := f.s.RequestWork(ctx, p.GroupID, domain.RequestWorkRequest{TopicID: p.TopicID, MessageID: m.ID,
				Mode: domain.RoutingDecisionCreateNew, Reason: "独立工作", NewTask: &domain.NewTaskRequest{Title: "旧资料", Acceptance: "列出索引"}}, "other-task")
			if err != nil {
				t.Fatal(err)
			}
			queueEvent(t, f.s, worker.Member.Claim, provider.AcceptedEvent, "", "")
			if state == "unknown" {
				otherTask, err := f.s.GetTask(ctx, p.GroupID, other.Task.ID)
				if err != nil {
					t.Fatal(err)
				}
				attempt, err := f.s.ActivateTaskAttempt(ctx, taskAttemptInput(otherTask))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.s.SubmitTask(ctx, taskSubmissionInput(attempt)); err != nil {
					t.Fatal(err)
				}
				unknown := queueReceipt(provider.OutcomeUnknown)
				unknown.DirectChildReaped = false
				taskWorkTestReceipt(t, f.s, worker.Member, unknown, domain.TurnStateUnknown)
			} else if _, err := f.s.RequestTurnCancel(ctx, p.GroupID, worker.Member.Claim.TurnID); err != nil {
				t.Fatal(err)
			}

			const body = "继续当前正在执行的任务，注明来源。"
			scope, c := newWorkInputCandidate(t, f, body)
			protected := make(map[string][][]any)
			for _, table := range []string{"resource_claims", "task_attempts", "turns", "tasks", "mandates", "coordination_assignments"} {
				protected[table] = messageSequenceTableRows(t, f.s, table)
			}
			r, err := f.s.ApplyChatAssessment(ctx, scope, c.ID, domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork,
				Reason: "继续当前工作"}, "记录补充。", "implicit")
			if err != nil || r.Outcome != "continued" || r.TaskID != p.TaskID || r.WorkInputID == "" || r.WorkDeliveryID == "" ||
				!slices.Equal(c.FollowupTaskIDs, []string{p.TaskID}) {
				t.Fatalf("ineligible Task affected implicit routing: outcome=%q reason=%q hints=%v err=%v", r.Outcome, r.ReasonCode, c.FollowupTaskIDs, err)
			}
			d, err := f.s.ResolveTaskWorkDelivery(ctx, p.GroupID, r.WorkDeliveryID)
			if err != nil || d.Blocked != "prior_turn_unsettled" {
				t.Fatalf("unsettled owner did not block the wake: %+v %v", d, err)
			}
			if turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: p.GroupID, DeliveryID: r.WorkDeliveryID, MaxActive: 8}); !errors.Is(err, ErrTurnBusy) || turn.ID != "" {
				t.Fatalf("wake replaced the unsettled owner: %+v %v", turn, err)
			}
			page, err := f.s.ListTaskWorkInputs(ctx, p.GroupID, p.TaskID, ListOptions{})
			if err != nil || len(page.Items) != 1 || page.Items[0].ID != r.WorkInputID || page.Items[0].Content != body ||
				page.Items[0].Return != nil || page.Items[0].Consumption != nil {
				t.Fatalf("unsettled owner lost its pending input: %+v %v", page, err)
			}
			if got, err := f.s.HandBackWorkInput(ctx, worker.Member, r.WorkInputID, "交回额外要求"); !errors.Is(err, ErrMemberToolDenied) || got.ID != "" {
				t.Fatalf("unsettled Turn could handle the pending input: %+v %v", got, err)
			}
			request := domain.RequestWorkRequest{TopicID: c.TopicID, MessageID: c.MessageID, Mode: domain.RoutingDecisionCreateNew,
				Reason: "同一来源", NewTask: &domain.NewTaskRequest{Title: f.input.Title, Acceptance: f.input.Acceptance}}
			if got, err := f.s.RequestWork(ctx, c.GroupID, request, "same-source"); !errors.Is(err, ErrConflict) || got.Task.ID != "" {
				t.Fatalf("unsettled owner let the same source create new work: %+v %v", got, err)
			}
			messageSequencePreserved(t, f.s, protected)
		})
	}
}

func TestTaskWorkFollowupImplicitAmbiguity(t *testing.T) {
	f := chatSetup(t, "user", false)
	worker := chatAuthorityWorker(t, f)
	p := worker.Provenance
	co := taskWorkTestCoordinator(t, f.s, p.GroupID, p.AssignmentID)
	taskWorkTestReceipt(t, f.s, co, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	scope, c := newWorkInputCandidate(t, f, "整理会议议程。")
	other := f.input
	other.Title = "会议议程"
	r, err := f.s.ApplyChatAssessment(ctx, scope, c.ID, other, "登记独立工作。", "other")
	if err != nil || r.Outcome != "admitted" {
		t.Fatalf("second admission: %+v %v", r, err)
	}
	d, err := f.s.GetDispatchRequest(ctx, p.GroupID, r.DispatchRequestID)
	if err != nil {
		t.Fatal(err)
	}
	coord := coordinationDecideBindRequest(t, f.s, d.Request)
	decision := coordinationDecideAssign(t, requestDispatchFixture{s: f.s, worker: p.MemberID}, coord, "other-assignment")
	if decision.Assignment == nil {
		t.Fatal("second task must have its own current Assignment")
	}
	scope, c = newWorkInputCandidate(t, f, "继续当前工作。")
	got, err := f.s.ApplyChatAssessment(ctx, scope, c.ID, domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "继续原任务"}, "核对目标。", "ambiguous")
	chatAssertNotAdmitted(t, got, err, "ambiguous_followup_task")
	if len(c.FollowupTaskIDs) != 2 || !slices.Contains(c.FollowupTaskIDs, p.TaskID) || !slices.Contains(c.FollowupTaskIDs, r.TaskID) {
		t.Fatalf("two eligible tasks did not remain ambiguous: %v", c.FollowupTaskIDs)
	}
}

func TestTaskWorkLegacyRequestDoesNotMatchTitleSubstring(t *testing.T) {
	f := chatSetupSource(t, "user", false, "请汇总年度安全审计发现。")
	f.input.Title, f.input.Description = "报告", "汇总年度安全审计发现。"
	worker := chatAuthorityWorker(t, f)
	p := worker.Provenance
	sub, err := f.s.SubmitAssignmentResult(ctx, worker, AssignmentResultInput{Summary: "年度安全审计报告已交付。"}, "audit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ReviewTask(ctx, taskReviewInput(sub)); err != nil {
		t.Fatal(err)
	}
	stopped := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}
	co := taskWorkTestCoordinator(t, f.s, p.GroupID, p.AssignmentID)
	for _, scope := range []MemberToolScope{co, worker.Member} {
		taskWorkTestReceipt(t, f.s, scope, stopped, domain.TurnStateNotStarted)
	}

	proposal := f.input // No Followup field; exercise the old request_work admission.
	proposal.Title, proposal.Description, proposal.Acceptance = "下周预算报告", "起草下周预算报告", "列出费用明细"
	t.Run("unrelated-budget-report", func(t *testing.T) {
		scope, c := newWorkInputCandidate(t, f, "起草下周预算报告并追加费用明细")
		defer taskWorkTestReceipt(t, f.s, scope, stopped, domain.TurnStateNotStarted)
		r, err := f.s.ApplyChatAssessment(ctx, scope, c.ID, proposal, "登记独立预算任务。", "independent-budget")
		if err != nil || r.Outcome != "admitted" || r.TaskID == p.TaskID || r.DispatchRequestID == "" ||
			r.AuthorizationMessageID != c.MessageID || !r.AuthorityEvidenceVerified {
			t.Fatalf("generic old title captured an unrelated new request: outcome=%q reason=%q err=%v", r.Outcome, r.ReasonCode, err)
		}
	})
	t.Run("exact-quoted-title", func(t *testing.T) {
		scope, c := newWorkInputCandidate(t, f, "请补充《报告》。")
		r, err := f.s.ApplyChatAssessment(ctx, scope, c.ID, proposal, "核对原任务。", "quoted-audit")
		chatAssertNotAdmitted(t, r, err, "existing_task_reference")
		if r.ThreadRef != c.ThreadRef || r.MessageID != c.MessageID || r.WorkInputID != "" {
			t.Fatalf("title clarification escaped the original source: %+v", r)
		}
	})
}

func TestTaskWorkOtherTopicReference(t *testing.T) {
	f := chatSetup(t, "user", false)
	f.s.EnableTaskWorkExecution()
	old := chatAuthorityWorker(t, f).Provenance
	thread := domain.ThreadRef{GroupID: f.groupID, TopicID: "new-work-topic"}
	policy := f.policy.ChatPolicyRequest
	policy.MaxAdmissions = 3
	if _, err := f.s.SetChatPolicy(ctx, thread, policy, 0, "new-topic-policy"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body, denial string
		continuing         bool
	}{
		{"legacy-postposed-continue", "请帮我把 " + old.TaskID + " 接着做。", "followup_target_unavailable", false},
		{"independent", "另开一项工作整理议程，引用其它话题任务 <" + old.TaskID + "> 的结论。", "", false},
		{"optional-citation", "另开一项工作整理议程，可以参考其它话题任务 " + old.TaskID + " 的结论。", "", false},
		{"independent-framing", "另开一项独立工作整理会议议程（关联材料 " + old.TaskID + "）。", "", false},
		{"continue", "请继续任务 <" + old.TaskID + ">。", "followup_target_unavailable", true},
		{"legacy-continue", "应该继续工作 <" + old.TaskID + ">。", "followup_target_unavailable", false},
		{"legacy-choice", "应该继续工作 <" + old.TaskID + ">、<AAAAAAAAAAAAAAAAAAAAAAAAAA> 中的哪一个？", "ambiguous_followup_task", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recipients := []string{member(t, f.s, f.groupID).ID}
			m := chatLocalMessage(t, f.s, MessageRequest{GroupID: thread.GroupID, TopicID: thread.TopicID,
				ActorID: "user", Content: tc.body, RecipientIDs: recipients}, recipients)
			scope, c := workInputCandidate(t, f, m, 0)
			defer taskWorkTestReceipt(t, f.s, scope, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
			proposal := f.input
			proposal.Title, proposal.Description, proposal.Acceptance = "下周议程", "整理会议事项", "列出时间和事项"
			if tc.continuing {
				proposal = domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "继续原工作",
					Followup: &domain.ChatFollowupAssessment{TaskID: old.TaskID}}
			}
			protected := make(map[string][][]any)
			if tc.denial != "" {
				for _, table := range []string{"tasks", "mandates", "coordination_dispatch_requests", "task_work_inputs", "task_work_deliveries", "task_attempts", "turns", "resource_claims"} {
					protected[table] = messageSequenceTableRows(t, f.s, table)
				}
			}
			r, err := f.s.ApplyChatAssessment(ctx, scope, c.ID, proposal, "评估用户请求。", tc.name)
			if tc.denial != "" {
				chatAssertNotAdmitted(t, r, err, tc.denial)
				messageSequencePreserved(t, f.s, protected)
			} else if err != nil || r.Outcome != "admitted" || r.TaskID == "" || r.TaskID == old.TaskID || r.DispatchRequestID == "" ||
				!r.AuthorityEvidenceVerified || r.AuthorizationMessageID != m.ID {
				t.Fatalf("contextual citation captured an independent request: %+v %v", r, err)
			}
			if r.ThreadRef != thread || r.MessageID != m.ID || r.WorkInputID != "" || r.WorkDeliveryID != "" {
				t.Fatalf("request escaped its topic or became a continuation: %+v", r)
			}
		})
	}
}

func TestTaskWorkLegacyRequestAfterClosedTask(t *testing.T) {
	f := chatSetupSource(t, "user", false, "请整理日报。")
	f.input.Title = "日报"
	worker := chatAuthorityWorker(t, f)
	p := worker.Provenance
	sub, err := f.s.SubmitAssignmentResult(ctx, worker, AssignmentResultInput{Summary: "日报已交付。"}, "daily-report")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ReviewTask(ctx, taskReviewInput(sub)); err != nil {
		t.Fatal(err)
	}
	refusal := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}
	co := taskWorkTestCoordinator(t, f.s, p.GroupID, p.AssignmentID)
	for _, scope := range []MemberToolScope{co, worker.Member} {
		taskWorkTestReceipt(t, f.s, scope, refusal, domain.TurnStateNotStarted)
	}
	proposal := f.input
	proposal.Title, proposal.Description, proposal.Acceptance = "会议议程", "整理下周会议议程", "列出参会人员"
	t.Run("independent", func(t *testing.T) {
		scope, c := newWorkInputCandidate(t, f, "整理下周会议议程，并追加参会人员名单")
		defer taskWorkTestReceipt(t, f.s, scope, refusal, domain.TurnStateNotStarted)
		r, err := f.s.ApplyChatAssessment(ctx, scope, c.ID, proposal, "登记新文本任务。", "independent")
		if err != nil || r.Outcome != "admitted" || r.TaskID == p.TaskID || r.DispatchRequestID == "" {
			t.Fatalf("unrelated closed Task captured a new request: outcome=%q reason=%q err=%v", r.Outcome, r.ReasonCode, err)
		}
	})
	for _, tc := range []struct{ name, body, code string }{
		{"contextual", "继续做日报。", "existing_task_reference"},
		{"explicit-closed", "请整理任务 " + p.TaskID, "existing_task_reference"},
		{"mixed", "请整理任务 " + p.TaskID + "，参考编号 " + strings.Repeat("A", 26), "ambiguous_followup_task"},
		{"explicit-invalid", "请整理任务 `" + strings.Repeat("A", 26) + "`", "followup_target_unavailable"},
		{"explicit-malformed", "请整理任务编号: no-such-task", "followup_target_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recipients := []string{member(t, f.s, f.groupID).ID, member(t, f.s, f.groupID).ID}
			m := chatLocalMessage(t, f.s, MessageRequest{GroupID: p.GroupID, TopicID: p.TopicID, ActorID: "user", Content: tc.body}, recipients)
			scope, c := workInputCandidate(t, f, m, 0)
			defer taskWorkTestReceipt(t, f.s, scope, refusal, domain.TurnStateNotStarted)
			alias, other := workInputCandidate(t, f, m, 1)
			defer taskWorkTestReceipt(t, f.s, alias, refusal, domain.TurnStateNotStarted)
			r, err := f.s.ApplyChatAssessment(ctx, scope, c.ID, proposal, "核对原任务。", "reference")
			chatAssertNotAdmitted(t, r, err, tc.code)
			if r.ThreadRef != c.ThreadRef || r.MessageID != c.MessageID || r.WorkInputID != "" {
				t.Fatalf("reference clarification escaped its original source/topic: %+v", r)
			}
			if again, err := f.s.ApplyChatAssessmentDormant(ctx, alias, other.ID, proposal, "核对原任务。", "reference-alias"); err != nil || !reflect.DeepEqual(again, r) {
				t.Fatalf("another candidate changed the source clarification: %+v %v", again, err)
			}
		})
	}
}
