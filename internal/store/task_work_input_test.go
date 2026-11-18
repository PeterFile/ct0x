package store

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func workInputCandidate(t *testing.T, f chatFixture, message domain.Message, delivery int) (MemberToolScope, domain.ChatCandidate) {
	t.Helper()
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: message.Deliveries[delivery].ID, MaxActive: 4})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := f.s.CaptureChatCandidate(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	return scope, candidate
}

func newWorkInputCandidate(t *testing.T, f chatFixture, body string, recipients ...string) (MemberToolScope, domain.ChatCandidate) {
	t.Helper()
	if len(recipients) == 0 {
		recipients = []string{member(t, f.s, f.groupID).ID}
	}
	message := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: f.scope.TopicID, ActorID: "user",
		Content: body, RecipientIDs: recipients}, recipients)
	return workInputCandidate(t, f, message, 0)
}

func TestTaskWorkInputUnsafeRouting(t *testing.T) {
	for _, kind := range []string{"legacy-unknown", "legacy-original-task", "two-real-ids", "mixed-invalid-ids", "mixed-foreign-ids-with-hint", "list-invalid-ids-with-hint", "list-foreign-ids-with-hint", "unquoted-constraint", "workspace", "discussion-only"} {
		t.Run(kind, func(t *testing.T) {
			f := chatSetup(t, "user", false)
			worker := chatAuthorityWorker(t, f)
			body := "继续做刚才的摘要。"
			input := domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "原摘要仍在处理"}
			var reason string
			var discussionLimit string
			switch kind {
			case "legacy-unknown", "legacy-original-task":
				queueEvent(t, f.s, worker.Member.Claim, provider.AcceptedEvent, "", "")
				unknown := queueReceipt(provider.OutcomeUnknown)
				unknown.DirectChildReaped = false
				taskWorkTestReceipt(t, f.s, worker.Member, unknown, domain.TurnStateUnknown)
				if kind == "legacy-original-task" {
					body = "请给原任务加一条引用"
				}
				input = f.input // Pre-B1a caller with no Followup field.
				reason = "existing_task_reference"
			case "two-real-ids":
				other, err := f.s.CreateTask(ctx, taskInput(f.groupID))
				if err != nil {
					t.Fatal(err)
				}
				body = "应继续任务 " + worker.Provenance.TaskID + " 还是 " + other.ID + "？"
				input.Followup = &domain.ChatFollowupAssessment{TaskID: worker.Provenance.TaskID}
				reason = "ambiguous_followup_task"
			case "mixed-invalid-ids", "mixed-foreign-ids-with-hint", "list-invalid-ids-with-hint", "list-foreign-ids-with-hint":
				f.s.EnableTaskWorkExecution() // A misroute would actually record and wake work.
				id, otherID := worker.Provenance.TaskID, strings.Repeat("A", 26)
				if strings.Contains(kind, "foreign") {
					other, err := f.s.CreateTask(ctx, taskInput(f.otherID))
					if err != nil {
						t.Fatal(err)
					}
					otherID = other.ID
				}
				body = "继续任务 <" + id + "> 还是 <" + otherID + ">？"
				if strings.HasPrefix(kind, "list-") {
					body = "应该继续任务 <" + id + ">、<" + otherID + "> 中的哪一个？"
				}
				if kind != "mixed-invalid-ids" {
					input.Followup = &domain.ChatFollowupAssessment{TaskID: id}
				}
				reason = "ambiguous_followup_task"
			case "unquoted-constraint":
				input.Followup = &domain.ChatFollowupAssessment{Constraint: "删除全部引用"}
				reason = "unconfirmed_followup_constraint"
			case "workspace":
				input.Action, reason = domain.ChatActionWorkspaceChange, "followup_text_only"
			case "discussion-only":
				discussionLimit, reason = "仅讨论，不执行。", "discussion_only"
				body += discussionLimit
			}
			var scope MemberToolScope
			var candidate domain.ChatCandidate
			if discussionLimit == "" {
				scope, candidate = newWorkInputCandidate(t, f, body)
			} else {
				recipients := []string{member(t, f.s, f.groupID).ID, member(t, f.s, f.groupID).ID}
				message := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: f.scope.TopicID, ActorID: "user", Content: body, RecipientIDs: recipients}, recipients)
				first, c := workInputCandidate(t, f, message, 0)
				limited, err := f.s.ApplyChatAssessment(ctx, first, c.ID, domain.ChatAssessment{Disposition: "discuss", Reason: "遵守用户限制",
					DiscussionOnlyExcerpt: &discussionLimit}, "仅讨论范围。", "discussion-limit")
				if err != nil || !limited.DiscussionOnly {
					t.Fatalf("discussion-only proof: %+v %v", limited, err)
				}
				scope, candidate = workInputCandidate(t, f, message, 1)
			}
			protected := make(map[string][][]any)
			for _, table := range []string{"tasks", "mandates", "coordination_dispatch_requests", "task_work_inputs", "task_work_deliveries", "task_attempts", "resource_claims", "turns"} {
				protected[table] = messageSequenceTableRows(t, f.s, table)
			}
			const note, key = "记录原要求。", "followup-guard"
			got, err := f.s.ApplyChatAssessment(ctx, scope, candidate.ID, input, note, key)
			chatAssertNotAdmitted(t, got, err, reason)
			if got.MessageID != candidate.MessageID || got.ThreadRef != candidate.ThreadRef || got.WorkInputID != "" || got.WorkDeliveryID != "" {
				t.Fatalf("clarification escaped the original topic/source: %+v", got)
			}
			if again, err := f.s.ApplyChatAssessment(ctx, scope, candidate.ID, input, note, key); err != nil || !reflect.DeepEqual(again, got) {
				t.Fatalf("clarification did not replay: %+v %v", again, err)
			}
			messageSequencePreserved(t, f.s, protected)
		})
	}
}

func TestTaskWorkInputSubmissionRequiresEveryChangedConstraint(t *testing.T) {
	f := chatSetup(t, "user", false)
	worker := chatAuthorityWorker(t, f)
	var ids []string
	for _, constraint := range []string{"最多一百字", "注明结论来源"} {
		scope, candidate := newWorkInputCandidate(t, f, "继续做摘要，请"+constraint+"。")
		input := domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "补充原摘要", Followup: &domain.ChatFollowupAssessment{Constraint: constraint}}
		r, err := f.s.ApplyChatAssessmentDormant(ctx, scope, candidate.ID, input, "记录补充。", "supplement")
		if err != nil || r.Outcome != "continued" || r.WorkInputID == "" {
			t.Fatalf("failed to route supplement: %+v %v", r, err)
		}
		ids = append(ids, r.WorkInputID)
	}
	if _, err := f.s.AcknowledgeWorkInput(ctx, worker.Member, ids[1]); err != nil {
		t.Fatal(err)
	}
	result := AssignmentResultInput{Summary: "原任务交付。"}
	if got, err := f.s.SubmitAssignmentResult(ctx, worker, result, "result"); !errors.Is(err, ErrConflict) || got.ID != "" {
		t.Fatalf("acknowledging only the newest constraint unlocked submission: %+v %v", got, err)
	}
	if _, err := f.s.AcknowledgeWorkInput(ctx, worker.Member, ids[0]); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.SubmitAssignmentResult(ctx, worker, result, "result"); err != nil || got.ID == "" {
		t.Fatalf("handling all changed constraints did not unlock fresh submission: %+v %v", got, err)
	}
}

func TestTaskWorkInputSourceReplayDoesNotEvaluateSecondCandidate(t *testing.T) {
	f := chatSetup(t, "user", false)
	chatAuthorityWorker(t, f)
	recipients := []string{member(t, f.s, f.groupID).ID, member(t, f.s, f.groupID).ID}
	message := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: f.scope.TopicID, ActorID: "user",
		Content: "这份摘要继续做。", RecipientIDs: recipients}, recipients)
	firstScope, first := workInputCandidate(t, f, message, 0)
	otherScope, other := workInputCandidate(t, f, message, 1)
	input := domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "继续原任务"}
	got, err := f.s.ApplyChatAssessmentDormant(ctx, firstScope, first.ID, input, "后续输入。", "first")
	if err != nil || got.Outcome != "continued" {
		t.Fatalf("original route: %+v %v", got, err)
	}
	before := messageSequenceSnapshot(t, f.s)
	for _, tc := range []struct{ key, text string }{{"first", "不同载荷"}, {"new", "不同载荷"}, {"new", "后续输入。"}} {
		if _, err := f.s.ApplyChatAssessment(ctx, firstScope, first.ID, input, tc.text, tc.key); !errors.Is(err, ErrConflict) {
			t.Fatalf("same-candidate conflict escaped: %v", err)
		}
	}
	for range 2 {
		prior, err := f.s.ApplyChatAssessment(ctx, otherScope, other.ID, f.input, "旧调用。", "alias")
		if err != nil || !reflect.DeepEqual(prior, got) {
			t.Fatalf("different candidate did not get original receipt: %+v %v", prior, err)
		}
	}
	if _, err := f.s.ApplyChatAssessment(ctx, otherScope, other.ID, f.input, "不同载荷", "alias"); !errors.Is(err, ErrConflict) {
		t.Fatalf("alias key did not retain payload conflict: %v", err)
	}
	request := domain.RequestWorkRequest{TopicID: first.TopicID, MessageID: first.MessageID, Mode: domain.RoutingDecisionCreateNew,
		Reason: "消费过的来源", NewTask: &domain.NewTaskRequest{Title: f.input.Title, Acceptance: f.input.Acceptance}}
	if _, err := f.s.RequestWork(ctx, f.groupID, request, "duplicate-direct"); !errors.Is(err, ErrConflict) {
		t.Fatalf("trusted RequestWork reopened a consumed source: %v", err)
	}
	delete(before, "mutation_idempotency") // Sole new fact: alias transport key.
	messageSequencePreserved(t, f.s, before)
}

func TestTaskWorkInputInboxPreservesEarlierSourceAndPendingIDs(t *testing.T) {
	f := chatSetup(t, "user", false)
	worker := chatAuthorityWorker(t, f)
	input := domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "继续原摘要"}
	apply := func(scope MemberToolScope, c domain.ChatCandidate, quote string) domain.ChatAssessmentReceipt {
		t.Helper()
		proposal := input
		proposal.Followup = &domain.ChatFollowupAssessment{Constraint: quote}
		r, err := f.s.ApplyChatAssessmentDormant(ctx, scope, c.ID, proposal, "记录补充。", "inbox/"+c.ID)
		if err != nil || r.Outcome != "continued" {
			t.Fatalf("follow-up was not recorded: %+v %v", r, err)
		}
		return r
	}
	body := "请继续摘要：最多一百字。"
	s1, c1 := newWorkInputCandidate(t, f, body)
	s2, c2 := newWorkInputCandidate(t, f, "请继续摘要：列出依据。")
	r2 := apply(s2, c2, "列出依据")
	r1 := apply(s1, c1, "最多一百字")
	for _, settled := range []MemberToolScope{s1, s2} {
		taskWorkTestReceipt(t, f.s, settled, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	}
	assessor := member(t, f.s, f.groupID).ID
	for range 50 {
		s, c := newWorkInputCandidate(t, f, "只是补充上下文，不改变要求。", assessor)
		apply(s, c, "")
		if _, err := f.s.ApplyTurnReceipt(ctx, s.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
			t.Fatal(err)
		}
	}
	items, err := f.s.ListWorkInputsForMember(ctx, worker.Member)
	if err != nil || len(items) != 50 || items[0].ID != r2.WorkInputID || items[1].ID != r1.WorkInputID {
		t.Fatalf("context hid pending changed inputs: len=%d err=%v", len(items), err)
	}
	old := items[1]
	if old.Content != body || old.SourceSequence != c1.Generation || old.SourceSequence >= items[0].SourceSequence ||
		old.CandidateID != c1.ID || old.ReplyMessageID != c1.MessageID || old.NoticeMessageID != r1.NoticeMessageID ||
		old.BeforeConstraint != "列出依据" || old.AfterConstraint != "最多一百字" || !old.CreatedAt.After(items[0].CreatedAt) || old.Consumption != nil {
		t.Fatalf("source/confirmation provenance lost: %+v", old)
	}
}

func TestTaskWorkInputCanonicalAttemptAndDurableAcknowledgement(t *testing.T) {
	f := chatSetup(t, "user", false)
	worker := chatAuthorityWorker(t, f)
	var ids []string
	for _, quote := range []string{"最多一百字", "注明来处"} {
		s, c := newWorkInputCandidate(t, f, "请继续摘要，"+quote+"。")
		input := domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "补充原任务", Followup: &domain.ChatFollowupAssessment{Constraint: quote}}
		r, err := f.s.ApplyChatAssessmentDormant(ctx, s, c.ID, input, "记录补充。", "durable")
		if err != nil || r.Outcome != "continued" {
			t.Fatalf("follow-up: %+v %v", r, err)
		}
		ids = append(ids, r.WorkInputID)
	}
	i, err := f.s.AcknowledgeWorkInput(ctx, worker.Member, ids[0])
	if err != nil || i.Consumption == nil || i.Consumption.AttemptID != worker.AttemptID || i.Consumption.TurnID != worker.Member.Claim.TurnID {
		t.Fatalf("explicit confirmation: %+v %v", i, err)
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, worker.Member.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
		t.Fatal(err)
	}
	later, _ := newWorkInputCandidate(t, f, "后续轮次测试。", worker.Member.MemberID)
	p := worker.Provenance
	nextID, stamp := rand.Text(), time.Now().UTC().Format(timestampLayout)
	// Simulate only B1b's future canonical pointer. An ordinary scope still
	// cannot acquire the Assignment proof this slice requires at the API.
	coordinationDecideExec(t, f.s, `INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,turn_id,created_at)
	 VALUES(?,?,?,?,?,?,?)`, nextID, p.TaskID, p.GroupID, p.TaskRevision, p.MemberID, later.Claim.TurnID, stamp)
	const ack = "INSERT INTO task_work_input_consumptions(input_id,turn_id,attempt_id,consumed_at) VALUES(?,?,?,?)"
	if _, err := f.s.db.Exec(ack, ids[1], later.Claim.TurnID, nextID, stamp); err == nil {
		t.Fatal("schema accepted a non-active TaskAttempt")
	}
	coordinationDecideExec(t, f.s, "UPDATE tasks SET active_attempt_id=? WHERE id=?", nextID, p.TaskID)
	if _, err := f.s.AcknowledgeWorkInput(ctx, later, ids[1]); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("canonical pointer alone granted worker inbox rights: %v", err)
	}
	if _, err := f.s.db.Exec(ack, ids[1], worker.Member.Claim.TurnID, nextID, stamp); err == nil {
		t.Fatal("schema accepted an acknowledgement from a mismatched Turn")
	}
	result := SubmitTaskInput{GroupID: p.GroupID, TaskID: p.TaskID, ExpectedRevision: p.TaskRevision, AttemptID: nextID,
		ActorID: p.MemberID, MemberScope: &later, Summary: "继续交付。", IdempotencyKey: "later-result"}
	if _, err := f.s.SubmitTask(ctx, result); !errors.Is(err, ErrConflict) {
		t.Fatalf("later canonical attempt bypassed a new pending change: %v", err)
	}
	coordinationDecideExec(t, f.s, ack, ids[1], later.Claim.TurnID, nextID, stamp)
	if submitted, err := f.s.SubmitTask(ctx, result); err != nil || submitted.ID == "" {
		t.Fatalf("original acknowledgement did not survive same-revision continuation: %+v %v", submitted, err)
	}
	page, err := f.s.ListTaskWorkInputs(ctx, p.GroupID, p.TaskID, ListOptions{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != ids[1] || page.NextCursor == nil {
		t.Fatalf("history lost latest acknowledgement: %+v %v", page, err)
	}
	older, err := f.s.ListTaskWorkInputs(ctx, p.GroupID, p.TaskID, ListOptions{Limit: 1, Before: page.NextCursor})
	if err != nil || len(older.Items) != 1 || !reflect.DeepEqual(older.Items[0], i) {
		t.Fatalf("history lost original acknowledgement: %+v %v", older, err)
	}
	dispatchAssertFK(t, f.s)
}

func TestTaskWorkInputForeignReferenceAndIndependentLegacyWork(t *testing.T) {
	f := chatSetup(t, "user", false)
	worker := chatAuthorityWorker(t, f)
	foreign, err := f.s.CreateTask(ctx, taskInput(f.otherID))
	if err != nil {
		t.Fatal(err)
	}
	s, c := newWorkInputCandidate(t, f, "继续本话题任务 "+worker.Provenance.TaskID+"，备注码 "+foreign.ID+"。")
	if !slices.Equal(c.FollowupTaskIDs, []string{worker.Provenance.TaskID}) {
		t.Fatalf("unexpected work hints: %v", c.FollowupTaskIDs)
	}
	input := domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "继续原任务", Followup: &domain.ChatFollowupAssessment{TaskID: foreign.ID}}
	r, err := f.s.ApplyChatAssessment(ctx, s, c.ID, input, "评估后续。", "foreign")
	chatAssertNotAdmitted(t, r, err, "followup_target_unavailable")
	stopped := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}
	taskWorkTestReceipt(t, f.s, s, stopped, domain.TurnStateNotStarted)

	input.Followup = &domain.ChatFollowupAssessment{TaskID: worker.Provenance.TaskID}
	s, c = newWorkInputCandidate(t, f, "请继续原任务。")
	r, err = f.s.ApplyChatAssessmentDormant(ctx, s, c.ID, input, "评估后续。", "unproven")
	chatAssertNotAdmitted(t, r, err, "unproven_task_reference")
	taskWorkTestReceipt(t, f.s, s, stopped, domain.TurnStateNotStarted)

	s, c = newWorkInputCandidate(t, f, "继续任务 <"+worker.Provenance.TaskID+">，备注码 <"+foreign.ID+">。")
	r, err = f.s.ApplyChatAssessmentDormant(ctx, s, c.ID, input, "评估后续。", "explicit")
	if err != nil || r.Outcome != "continued" || r.TaskID != worker.Provenance.TaskID || r.WorkInputID == "" {
		t.Fatalf("unrelated ID-shaped note prevented an explicit single target: %+v %v", r, err)
	}
	taskWorkTestReceipt(t, f.s, s, stopped, domain.TurnStateNotStarted)

	s, c = newWorkInputCandidate(t, f, "请整理下周议程，参考编号 "+foreign.ID+"。")
	independent := f.input
	independent.Title, independent.Acceptance = "下周议程", "列出会议事项"
	r, err = f.s.ApplyChatAssessment(ctx, s, c.ID, independent, "登记独立文本任务。", "independent")
	if err != nil || r.Outcome != "admitted" || r.TaskID == worker.Provenance.TaskID {
		t.Fatalf("foreign ID affected independent work: %+v %v", r, err)
	}
}

func TestTaskWorkInputRejectsUnboundedOrInvalidSource(t *testing.T) {
	f := chatSetup(t, "user", false)
	chatAuthorityWorker(t, f)
	for _, body := range []string{strings.Repeat("字", 3000), "继续\xff"} {
		s, c := newWorkInputCandidate(t, f, body)
		r, err := f.s.ApplyChatAssessment(ctx, s, c.ID, domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "继续原任务"}, "评估后续。", "bounded")
		chatAssertNotAdmitted(t, r, err, "task_reference_unavailable")
	}
}

func TestTaskWorkInputThroughChatIntake(t *testing.T) {
	f := chatSetup(t, "user", false)
	worker := chatAuthorityWorker(t, f)
	assessor := member(t, f.s, f.groupID).ID
	message := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: f.scope.TopicID, ActorID: "user",
		Content: "这份摘要继续做，请把结论放在开头。", RecipientIDs: []string{assessor}}, []string{assessor})
	scope, candidate := workInputCandidate(t, f, message, 0)
	var input domain.ChatAssessment
	if err := json.Unmarshal([]byte(`{"disposition":"continue_work","reason":"原摘要仍在处理","followup":{"constraint":"结论放在开头"}}`), &input); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.ApplyChatAssessmentDormant(ctx, scope, candidate.ID, input, "记录补充要求。", "followup-intake")
	if err != nil || got.Outcome != "continued" || got.TaskID != worker.Provenance.TaskID || got.DispatchRequestID != "" {
		t.Fatalf("open work did not receive follow-up on its original identity: %+v %v", got, err)
	}
}
