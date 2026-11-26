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

func TestTaskWorkFollowupHandback(t *testing.T) {
	for _, tc := range []struct {
		name         string
		continuation bool
		stale        bool
	}{
		{name: "original"},
		{name: "continuation", continuation: true},
		{name: "policy-changed-before-return", continuation: true, stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := chatSetup(t, "user", false)
			f.s.EnableTaskWorkExecution()
			old := chatAuthorityWorker(t, f)
			p := old.Provenance
			const text = "还需要将摘要写入项目目录"
			s, c := newWorkInputCandidate(t, f, text)
			cmd := domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "由执行成员核对是否属于原授权",
				Followup: &domain.ChatFollowupAssessment{Constraint: text}}
			r, err := f.s.ApplyChatAssessment(ctx, s, c.ID, cmd, "记录要求。", "handback-fixture")
			if err != nil || r.WorkDeliveryID == "" {
				t.Fatalf("work input: %+v %v", r, err)
			}
			scope, attemptID := old.Member, old.AttemptID
			if tc.continuation {
				co := taskWorkTestCoordinator(t, f.s, p.GroupID, p.AssignmentID)
				for _, prior := range []MemberToolScope{co, old.Member} {
					taskWorkTestReceipt(t, f.s, prior, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
				}
				d, err := f.s.ResolveTaskWorkDelivery(ctx, p.GroupID, r.WorkDeliveryID)
				if err != nil || d.Blocked != "" {
					t.Fatalf("followup not ready: %+v %v", d, err)
				}
				scope = taskWorkTestClaim(t, f.s, d, "worker-resource")
				a, err := f.s.BindTaskWorkContinuation(ctx, scope)
				if err != nil {
					t.Fatal(err)
				}
				attemptID = a.ID
			}
			changePolicy := func(enabled bool, expected int64) domain.ChatPolicy {
				t.Helper()
				policy, err := f.s.SetChatPolicy(ctx, c.ThreadRef, domain.ChatPolicyRequest{Enabled: enabled,
					Action: domain.ChatActionTextSubmission, MaxAdmissions: 2}, expected, "return-policy")
				if err != nil {
					t.Fatal(err)
				}
				return policy
			}
			policy := f.policy
			if tc.stale {
				// Enabled is not enough: the original authorization was bound to its earlier policy.
				policy = changePolicy(true, policy.Revision)
			}
			// A worker's reason does not decide which authority is available.
			const reason = "我认为可以直接按现有授权写入目录"
			for _, denied := range []MemberToolScope{s, {Claim: scope.Claim, MemberID: f.foreign, ConversationID: scope.ConversationID, TopicID: scope.TopicID}} {
				if _, err := f.s.HandBackWorkInput(ctx, denied, r.WorkInputID, reason); !errors.Is(err, ErrMemberToolDenied) {
					t.Fatalf("another scope could return the owner's input: %v", err)
				}
			}
			protected := []string{"tasks", "task_attempts", "turns", "resource_claims", "coordination_assignments", "mandates", "message_command_sources"}
			before := messageSequenceSnapshot(t, f.s)
			const key = "return-with-policy"
			got, err := f.s.HandBackWorkInputWithKey(ctx, scope, r.WorkInputID, reason, key)
			if err != nil || got.Return == nil || got.Return.MemberID != p.MemberID || got.Return.AttemptID != attemptID || got.Return.TurnID != scope.Claim.TurnID || got.Return.Reason != reason || got.Content != text || got.ReplyMessageID != c.MessageID || got.Consumption != nil || got.AfterConstraint != text {
				t.Fatalf("handback merged authority or lost the human source: %+v %v", got, err)
			}
			phrase, disposition := "单独授权", domain.TaskWorkInputReturnSeparateAuthorizationRequired
			if tc.stale {
				phrase, disposition = "当前授权不可用", domain.TaskWorkInputReturnOriginalAuthorizationUnavailable
			}
			if !strings.Contains(got.Return.Question, phrase) || got.Return.Disposition != disposition ||
				got.Return.PolicyRevision != policy.Revision || got.Return.PolicySourceID != policy.ConfigurationSourceID {
				t.Fatalf("handback ignored active policy revision %d: %+v", policy.Revision, got.Return)
			}
			after := messageSequenceSnapshot(t, f.s)
			for _, table := range protected {
				if !reflect.DeepEqual(before[table], after[table]) {
					t.Fatalf("handback rewrote protected facts in %s", table)
				}
			}
			notice, err := f.s.GetMessage(ctx, p.GroupID, got.Return.NoticeMessageID)
			if err != nil || notice.TopicID != p.TopicID || notice.SourceKind != "platform" || !strings.Contains(notice.Content, c.MessageID) ||
				!strings.HasSuffix(notice.Content, got.Return.Question) || strings.Count(notice.Content, "？") != 1 || len(notice.Deliveries) != 0 {
				t.Fatalf("handback was not one scoped clarification: %+v %v", notice, err)
			}
			if again, err := f.s.HandBackWorkInput(ctx, scope, r.WorkInputID, reason); err != nil || !reflect.DeepEqual(again, got) {
				t.Fatalf("return replay changed reason, actor or notice: %+v %v", again, err)
			}
			if _, err := f.s.HandBackWorkInput(ctx, scope, r.WorkInputID, "不同原因"); !errors.Is(err, ErrConflict) {
				t.Fatalf("return history was replaceable: %v", err)
			}
			if !tc.continuation {
				if wake, err := f.s.ResolveTaskWorkDelivery(ctx, p.GroupID, r.WorkDeliveryID); err != nil || wake.Blocked != "followup_handled" {
					t.Fatalf("returned input remained executable: %+v %v", wake, err)
				}
			}
			if !tc.stale {
				if _, err := f.s.AcknowledgeWorkInput(ctx, scope, r.WorkInputID); !errors.Is(err, ErrConflict) {
					t.Fatalf("handback was converted into confirmation: %v", err)
				}
				changePolicy(false, policy.Revision)
			}
			after = messageSequenceSnapshot(t, f.s)
			if again, err := f.s.HandBackWorkInputWithKey(ctx, scope, r.WorkInputID, reason, key); err != nil || !reflect.DeepEqual(again, got) {
				t.Fatalf("policy drift changed the exact keyed handback: %+v %v", again, err)
			}
			page, err := f.s.ListTaskWorkInputs(ctx, p.GroupID, p.TaskID, ListOptions{Limit: 1})
			if err != nil || len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0], got) || page.NextCursor != nil {
				t.Fatalf("work history lost returned input: %+v %v", page, err)
			}
			messageSequencePreserved(t, f.s, after)
			dispatchAssertFK(t, f.s)
		})
	}
}

func TestTaskWorkInvestigationHandbackWithoutRuntime(t *testing.T) {
	f, policy, owner := investigationUserWorker(t)
	f.s.EnableTaskWorkExecution()
	p, scope := owner.Provenance, owner.Member
	queueEvent(t, f.s, scope.Claim, provider.AcceptedEvent, "", "")
	r, wake := taskWorkTestFollowup(t, f, "请同时将结论写入项目目录。")
	original, err := f.s.GetWorkInputForMember(ctx, scope, r.WorkInputID)
	if err != nil || original.ID != r.WorkInputID || original.Return != nil || original.Consumption != nil {
		t.Fatalf("no durable unhandled input: %+v %v", original, err)
	}
	authorized, err := f.s.GetTaskInvestigationScope(ctx, p.GroupID, p.TaskID)
	if err != nil || authorized.MandateID != original.MandateID || authorized.PolicyRevision != policy.Revision || authorized.ConfigurationSourceID != policy.ConfigurationSourceID {
		t.Fatalf("fixture lacks the original investigation authority: %+v %v", authorized, err)
	}
	if err := f.s.CheckTaskInvestigationReady(ctx, p.GroupID, p.TaskID); err != nil {
		t.Fatal(err)
	}
	cold := queueHandle(t, f.path, false)
	cold.EnableTaskWorkExecution() // Isolate the investigation startup flag.
	if err := cold.CheckTaskInvestigationReady(ctx, p.GroupID, p.TaskID); !errors.Is(err, ErrTaskInvestigationNotReady) {
		t.Fatalf("fixture unexpectedly ready on the second handle: %v", err)
	}
	before := messageSequenceSnapshot(t, cold)
	if got, err := cold.ReadInvestigationChannel(ctx, owner, ListOptions{}); !errors.Is(err, ErrMemberToolDenied) || len(got.Items) != 0 {
		t.Fatalf("read acquired investigation rights without the runtime: %+v %v", got, err)
	}
	if got, err := cold.GetWorkInputForMember(ctx, scope, original.ID); !errors.Is(err, ErrMemberToolDenied) || got.ID != "" {
		t.Fatalf("input read bypassed readiness: %+v %v", got, err)
	}
	if got, err := cold.AcknowledgeWorkInputWithKey(ctx, scope, original.ID, "cold-ack"); !errors.Is(err, ErrMemberToolDenied) || got.ID != "" {
		t.Fatalf("acknowledgement bypassed readiness: %+v %v", got, err)
	}
	if got, err := cold.ResolveInvestigationExecution(ctx, scope, p); !errors.Is(err, ErrMemberToolDenied) || got != (CoordinationRoleState{}) {
		t.Fatalf("role resolved without investigation startup: %+v %v", got, err)
	}
	const reason, key = "原授权只允许读取和提交文本。", "investigation-return"
	wrong := scope
	wrong.Claim.Token += "-wrong"
	if got, err := cold.HandBackWorkInputWithKey(ctx, wrong, original.ID, reason, key); !errors.Is(err, ErrMemberToolDenied) || got.ID != "" {
		t.Fatalf("return accepted the wrong claim: %+v %v", got, err)
	}
	messageSequencePreserved(t, cold, before)

	got, err := cold.HandBackWorkInputWithKey(ctx, scope, original.ID, reason, key)
	h := got.Return
	if err != nil || h == nil || h.Disposition != domain.TaskWorkInputReturnSeparateAuthorizationRequired ||
		h.PolicyRevision != authorized.PolicyRevision || h.PolicySourceID != authorized.ConfigurationSourceID || h.TurnID != scope.Claim.TurnID ||
		h.AttemptID != owner.AttemptID || h.MemberID != p.MemberID || !strings.Contains(h.Question, "单独授权") {
		t.Fatalf("runtime readiness changed the durable return disposition: %+v %v", h, err)
	}
	projected := got
	projected.Return = nil
	if !reflect.DeepEqual(projected, original) {
		t.Fatalf("return changed the projected original input: %+v", projected)
	}
	after := messageSequenceSnapshot(t, cold)
	if slices.Contains(migrations, taskImpactSchema) && slices.Contains(migrations, workInputDecisionSchema) {
		page, err := cold.ListPendingTaskDecisions(ctx, ListOptions{})
		if err != nil || len(page.Items) != 1 || page.NextCursor != nil {
			t.Fatalf("fresh return must expose one pending task decision: %+v %v", page, err)
		}
		q := page.Items[0]
		if !strings.HasPrefix(q.ID, workInputDecisionPrefix) || q.Version != 1 ||
			q.Kind != domain.DecisionRequestAuthorization || q.Relay != domain.DecisionRelayTaskOwner || q.RecipientID != "user" ||
			q.ThreadRef != got.ThreadRef || q.Task == nil || q.Task.WorkInputID != got.ID || q.Task.TaskID != got.TaskID ||
			q.Task.TaskRevision != got.TaskRevision || q.Task.MandateID != got.MandateID || q.Task.AssignmentID != got.AssignmentID ||
			q.Task.OwnerMemberID != got.WorkerMemberID || q.Task.OriginTriggerID != got.TriggerID ||
			q.SourceMessageID != got.ReplyMessageID || q.SourceSequence != got.SourceSequence ||
			q.AuthorMemberID != h.MemberID || q.AuthorTurnID != h.TurnID || q.AuthorAttemptID != h.AttemptID ||
			q.Facts != h.Reason || q.Question != h.Question {
			t.Fatalf("pending decision lost the returned authority or source: %+v (task %+v), return %+v", q, q.Task, h)
		}
		for _, table := range []string{"decision_requests", "decision_request_versions"} {
			if len(after[table]) != len(before[table])+1 {
				t.Fatalf("return must add exactly one %s row", table)
			}
			remaining := slices.DeleteFunc(slices.Clone(after[table]), func(row []any) bool {
				return len(row) != 0 && row[0] == q.ID
			})
			if len(remaining) != len(before[table]) || !slices.EqualFunc(remaining, before[table], func(a, b []any) bool {
				return reflect.DeepEqual(a, b)
			}) {
				t.Fatalf("return changed original %s rows", table)
			}
			delete(before, table)
		}
	}
	tables := []string{"task_work_input_returns", "mutation_idempotency", "messages", "message_log"}
	if slices.Contains(migrations, taskImpactSchema) {
		tables = append(tables, "task_constraint_impacts")
	}
	for _, table := range tables {
		if len(after[table]) != len(before[table])+1 {
			t.Fatalf("return must add exactly one %s row", table)
		}
		delete(before, table)
	}
	messageSequencePreserved(t, cold, before)
	taskWorkTestPeek(t, cold, wake, "followup_handled")
	if sub, err := cold.SubmitAssignmentResult(ctx, owner, AssignmentResultInput{Summary: "按原范围交付调查结论。"}, "cold-submit"); !errors.Is(err, ErrMemberToolDenied) || sub.ID != "" {
		t.Fatalf("return conferred submission rights without readiness: %+v %v", sub, err)
	}
	if again, err := f.s.HandBackWorkInputWithKey(ctx, scope, original.ID, reason, key); err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("enabled handle changed the stored disposition: %+v %v", again, err)
	}
	messageSequencePreserved(t, cold, after)
	investigationDisablePolicy(t, f.s, policy)
	after = messageSequenceSnapshot(t, cold)
	if again, err := cold.HandBackWorkInputWithKey(ctx, scope, original.ID, reason, key); err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("policy drift changed the stored disposition: %+v %v", again, err)
	}
	messageSequencePreserved(t, cold, after)
}

func TestTaskWorkFollowupDormantAndAtomic(t *testing.T) {
	f := chatSetup(t, "user", false)
	old := chatAuthorityWorker(t, f)
	cmd := domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "保持原任务范围"}
	s, c := newWorkInputCandidate(t, f, "保持旧格式兼容")
	before := messageSequenceSnapshot(t, f.s)
	if r, err := f.s.ApplyChatAssessment(ctx, s, c.ID, cmd, "记录要求。", "dormant"); !errors.Is(err, ErrTaskWorkExecutionNotReady) || r.ID != "" {
		t.Fatalf("unwired process silently recorded work: %+v %v", r, err)
	}
	messageSequencePreserved(t, f.s, before)
	dormant, err := f.s.ApplyChatAssessmentDormant(ctx, s, c.ID, cmd, "记录要求。", "dormant")
	if err != nil || !dormant.WorkInputPending || dormant.WorkDeliveryID != "" || dormant.WorkInputID == "" {
		t.Fatalf("explicit dormant receipt: %+v %v", dormant, err)
	}
	f.s.EnableTaskWorkExecution()
	if got, err := f.s.ApplyChatAssessment(ctx, s, c.ID, cmd, "记录要求。", "dormant"); err != nil || !reflect.DeepEqual(dormant, got) {
		t.Fatalf("startup opt-in retroactively woke a replay: %+v %v", got, err)
	}
	if _, err := f.s.AcknowledgeWorkInput(ctx, old.Member, dormant.WorkInputID); err != nil {
		t.Fatal(err)
	}
	taskWorkTestReceipt(t, f.s, s, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	s, c = newWorkInputCandidate(t, f, "列出兼容事项")
	before = messageSequenceSnapshot(t, f.s)
	coordinationDecideExec(t, f.s, `CREATE TEMP TRIGGER suppress_followup_wake BEFORE INSERT ON task_work_deliveries BEGIN SELECT RAISE(IGNORE); END`)
	if r, err := f.s.ApplyChatAssessment(ctx, s, c.ID, cmd, "记录要求。", "atomic"); err == nil || r.ID != "" {
		t.Fatalf("undelivered input was reported as official and queued: %+v %v", r, err)
	}
	coordinationDecideExec(t, f.s, `DROP TRIGGER suppress_followup_wake`)
	messageSequencePreserved(t, f.s, before)
	if r, err := f.s.ApplyChatAssessment(ctx, s, c.ID, cmd, "记录要求。", "atomic"); err != nil || r.WorkDeliveryID == "" {
		t.Fatalf("rolled-back transport key could not be retried: %+v %v", r, err)
	}
}
