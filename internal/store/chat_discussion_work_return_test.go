package store

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func requireWorkReturnSchema(t *testing.T) {
	t.Helper()
	if !slices.Contains(migrations, chatDiscussionWorkReturnSchema) {
		t.Skip("requires the appended work-return migration and producer hook")
	}
}

type workReturnFixture struct {
	chatFixture
	invite       domain.ChatDiscussionReceipt
	intake       MemberToolScope
	admission    domain.ChatAssessmentReceipt
	owner        MemberToolScope
	ownerAttempt string
}

const workReturnIntakeResource = "discussion-intake"

func setupWorkReturnAdmission(t *testing.T) workReturnFixture {
	t.Helper()
	f := chatSetup(t, "user", false)
	s := f.s
	// Make the root, receiving assessor, and B coordinator distinct. The worker
	// is the middle inviter, so answering either the coordinator or last hop
	// instead of the original inviter cannot accidentally pass.
	coordinator, assessor := member(t, s, f.groupID).ID, member(t, s, f.groupID).ID
	if _, err := s.SetCoordinator(ctx, f.groupID, coordinator, 1, "return-coordinator"); err != nil {
		t.Fatal(err)
	}
	investigationPolicy(t, f, domain.ChatInvestigationPolicy{Objective: investigationObjective, AllowTopicMessages: true}, 1)
	s.EnableInvestigationExecution()
	s.EnableTaskWorkExecution()
	chatNextCandidate(t, &f, true)
	settle := func(scope MemberToolScope) {
		t.Helper()
		taskWorkTestReceipt(t, s, scope, queueReceipt(provider.OutcomeSucceeded), domain.TurnStateSucceeded)
	}
	queueEvent(t, s, f.scope.Claim, provider.AcceptedEvent, "", "")
	discussionAssess(t, s, f.scope, f.candidate.ID)
	first := discussionApply(t, s, f.scope, f.candidate.ID, "start", domain.ChatDiscussionAction{
		Kind: domain.ChatDiscussionInvite, RecipientID: f.b, Content: "请核对这份摘要。"})
	settle(f.scope)
	middle, mid := discussionArrive(t, s, f.groupID, first.Event.DeliveryID)
	queueEvent(t, s, middle.Claim, provider.AcceptedEvent, "", "")
	discussionAssess(t, s, middle, mid)
	head := discussionApply(t, s, middle, mid, "forward", domain.ChatDiscussionAction{
		Kind: domain.ChatDiscussionInvite, RecipientID: assessor, Content: "请评估未决问题。"})
	settle(middle)
	turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: head.Event.DeliveryID, MaxActive: 6,
		ResourceKeys: []string{workReturnIntakeResource}})
	if err != nil {
		t.Fatal(err)
	}
	intake, candidate := discussionCapture(t, s, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	queueEvent(t, s, intake.Claim, provider.AcceptedEvent, "", "")
	a, err := s.ApplyChatAssessment(ctx, intake, candidate, investigationProposal(), "已核对现有范围。", "admit-investigation")
	if err != nil || a.Outcome != "admitted" {
		t.Fatalf("structured admission: %+v %v", a, err)
	}
	return workReturnFixture{chatFixture: f, invite: head, intake: intake, admission: a}
}

func (f *workReturnFixture) startWorker(t *testing.T) {
	t.Helper()
	s := f.s
	dispatch, err := s.GetDispatchRequest(ctx, f.groupID, f.admission.DispatchRequestID)
	if err != nil {
		t.Fatal(err)
	}
	r := dispatch.Request
	co := coordinationDecideBindRequest(t, s, r)
	fact := investigationCapabilityFact(t, f.chatFixture, r)
	decision, err := s.DecideDispatchWithCapabilities(ctx, co,
		CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.b, Reason: "分配给受限调查成员"}, "assign", []DispatchWorkerCapabilityFact{fact})
	if err != nil || decision.Assignment == nil {
		t.Fatalf("investigation assignment: %+v %v", decision, err)
	}
	worker := coordinationDecideWorker(t, requestDispatchFixture{s: s, worker: f.b}, r, decision, true)
	taskWorkTestReceipt(t, s, co.Member, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	queueEvent(t, s, worker.Member.Claim, provider.AcceptedEvent, "", "")
	f.owner, f.ownerAttempt = worker.Member, worker.AttemptID
}

func (f workReturnFixture) settleIntake(t *testing.T) {
	t.Helper()
	taskWorkTestReceipt(t, f.s, f.intake, queueReceipt(provider.OutcomeSucceeded), domain.TurnStateSucceeded)
}

func setupWorkReturn(t *testing.T) workReturnFixture {
	t.Helper()
	requireWorkReturnSchema(t)
	f := setupWorkReturnAdmission(t)
	f.settleIntake(t)
	f.startWorker(t)
	return f
}

const workReturnKey = "formal-handback"

var workReturnInput = domain.HandbackWorkInput{Summary: "已核对两份讨论材料；尚需确认第三项约束。"}

func workReturnHandback(t *testing.T, s *Store, owner MemberToolScope) domain.TaskWorkHandback {
	t.Helper()
	h, err := s.HandbackWork(ctx, owner, workReturnInput, workReturnKey)
	if err != nil || h.ID == "" || h.NoticeMessageID == "" {
		t.Fatalf("B handback: %+v %v", h, err)
	}
	return h
}

func readWorkReturn(t *testing.T, s *Store, h domain.TaskWorkHandback) *domain.ChatDiscussionWorkReturnReceipt {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	r, err := chatDiscussionWorkReturnReceiptInTx(ctx, tx, h)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func assertNoWorkReturn(t *testing.T, s *Store, owner MemberToolScope, want error) {
	t.Helper()
	before := messageSequenceSnapshot(t, s)
	if h, err := s.HandbackWork(ctx, owner, workReturnInput, workReturnKey); !errors.Is(err, want) || h != (domain.TaskWorkHandback{}) {
		t.Fatalf("denied handback must not commit its result, notice or wake: %+v %v; want %v", h, err, want)
	}
	messageSequencePreserved(t, s, before)
}

func TestChatDiscussionWorkReturnOrdinaryHandback(t *testing.T) {
	requireWorkReturnSchema(t)
	f, owner, child, _ := workReportOwner(t, 1)
	// An unrelated A path in the very same topic is not a structured origin.
	m := discussionUser(t, f.messageFixture, owner.Member.TopicID, f.a)
	root, c := discussionArrive(t, f.s, f.groupID, m.Deliveries[0].ID)
	discussionAssess(t, f.s, root, c)
	discussionApply(t, f.s, root, c, "unrelated", domain.ChatDiscussionAction{Kind: domain.ChatDiscussionInvite, RecipientID: child.ID, Content: "无关讨论。"})
	counts := workReportWakeCounts(t, f.s)
	h := workReturnHandback(t, f.s, owner.Member)
	if r := readWorkReturn(t, f.s, h); r != nil || workReportWakeCounts(t, f.s) != ([3]int{counts[0] + 1, counts[1], counts[2]}) {
		t.Fatalf("ordinary B result changed A path: %+v", r)
	}
	m, err := f.s.GetMessage(ctx, h.GroupID, h.NoticeMessageID)
	if err != nil || len(m.Deliveries) != 0 || m.SourceID != "work:handback:"+h.ID {
		t.Fatalf("B notice changed: %+v %v", m, err)
	}
}

func TestChatDiscussionWorkReturnPublishesAndReplaysAfterSupplement(t *testing.T) {
	f := setupWorkReturn(t)
	s := f.s
	const constraint = "注明结论来源"
	scope, supplement := newWorkInputCandidate(t, f.chatFixture, "请继续调查，并"+constraint+"。", f.intake.MemberID)
	if supplement.ThreadRef != f.candidate.ThreadRef || supplement.Generation <= f.candidate.Generation || supplement.SourceKind != "local_user_chat" {
		t.Fatalf("expected a newer local-user supplement in the original topic: %+v", supplement)
	}
	cmd := domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "补充原调查的交付约束",
		Followup: &domain.ChatFollowupAssessment{Constraint: constraint}}
	continued, err := s.ApplyChatAssessment(ctx, scope, supplement.ID, cmd, "记录补充。", "supplement")
	if err != nil || continued.Outcome != "continued" || continued.WorkInputID == "" {
		t.Fatalf("supplement was not routed to existing work: %+v %v", continued, err)
	}
	ack, err := s.AcknowledgeWorkInput(ctx, f.owner, continued.WorkInputID)
	if err != nil || ack.ReplyMessageID != supplement.MessageID || ack.Consumption == nil || ack.Consumption.TurnID != f.owner.Claim.TurnID {
		t.Fatalf("live owner did not acknowledge the exact supplement: %+v %v", ack, err)
	}
	discussionEnd(t, s, scope)
	discussionUser(t, f.messageFixture, "other-topic")
	chatLocalMessage(t, s, MessageRequest{GroupID: f.otherID, TopicID: supplement.TopicID, ActorID: "user", Content: "其他群的新命令。"}, nil)
	before := messageSequenceSnapshot(t, s)
	counts := workReportWakeCounts(t, s)
	other := queueHandle(t, f.path, false)
	other.EnableInvestigationExecution()
	other.EnableTaskWorkExecution()
	type result struct {
		h   domain.TaskWorkHandback
		err error
	}
	start, responses := make(chan struct{}), make(chan result, 2)
	for _, handle := range []*Store{s, other} {
		go func() {
			<-start
			h, err := handle.HandbackWork(ctx, f.owner, workReturnInput, workReturnKey)
			responses <- result{h, err}
		}()
	}
	close(start)
	first, second := <-responses, <-responses
	if first.err != nil || second.err != nil || first.h.ID == "" || first.h != second.h {
		t.Fatalf("concurrent handback: %+v / %+v", first, second)
	}
	h := first.h
	if ack.TaskID != h.TaskID || ack.MandateID != h.MandateID || ack.AssignmentID != h.AssignmentID || ack.Consumption.AttemptID != h.AuthorAttemptID {
		t.Fatalf("supplement and handback do not match: %+v / %+v", ack, h)
	}
	saved := readWorkReturn(t, other, h)
	if saved == nil || workReportWakeCounts(t, s) != ([3]int{counts[0] + 2, counts[1] + 1, counts[2]}) {
		t.Fatalf("transaction did not create exactly one ordinary delivery and the separate notice and return: %+v", saved)
	}
	r := *saved
	for _, written := range []string{"messages", "message_log", "deliveries", "chat_discussion_work_returns", "task_work_handbacks", "mutation_idempotency"} {
		delete(before, written)
	}
	messageSequencePreserved(t, s, before) // Includes B authority, Turn receipts and resources.
	if r.ThreadRef != h.ThreadRef || r.OriginMessageID != f.candidate.MessageID || r.InvitationMessageID != f.invite.Message.ID ||
		r.HandbackID != h.ID || r.HandbackNoticeMessageID != h.NoticeMessageID || r.MessageID == h.NoticeMessageID || r.SenderID != h.OwnerMemberID || r.RecipientID != f.a {
		t.Fatalf("wrong return bindings: %+v", r)
	}
	m, err := s.GetMessage(ctx, f.groupID, r.MessageID)
	if err != nil || m.GroupID != r.GroupID || m.TopicID != r.TopicID || m.SourceKind != "unknown" || m.SourceID != "" ||
		!strings.Contains(m.Content, "不代表执行完成、成果接纳或新授权") || !strings.Contains(m.Content, h.NoticeMessageID) || !strings.HasSuffix(m.Content, h.Summary) || len(m.Deliveries) != 1 {
		t.Fatalf("wrong public message: %+v %v", m, err)
	}
	d := m.Deliveries[0]
	if d.State != domain.DeliveryStateQueued || d.TurnID != "" || d.ID != r.DeliveryID || d.RecipientID != f.a || d.ConversationID != r.ConversationID {
		t.Fatalf("wrong queued delivery: %+v", d)
	}
	if got, err := other.ResolveChatDiscussionWorkReturnDelivery(ctx, f.groupID, r.DeliveryID); err != nil || got != r {
		t.Fatalf("typed read after reopen: %+v %v", got, err)
	}
	if got, err := s.ResolveChatDiscussionWorkReturnDelivery(ctx, f.otherID, r.DeliveryID); !errors.Is(err, ErrMemberToolDenied) || got.MessageID != "" {
		t.Fatalf("foreign provenance read: %+v %v", got, err)
	}
	if _, err := s.ResolveChatDiscussionWorkReturnDelivery(ctx, f.groupID, f.invite.Event.DeliveryID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ordinary invitation resolved as work return: %v", err)
	}
	if role, err := s.ResolveCoordinationDelivery(ctx, f.groupID, r.DeliveryID); err != nil ||
		role.Provenance.Kind != CoordinationRoleOrdinary || role.Blocked != CoordinationDiscussionWorkReturnHandlerRequired {
		t.Fatalf("legacy preparation must leave the typed return queued: %+v %v", role, err)
	}
	followup, candidate := discussionArrive(t, s, f.groupID, r.DeliveryID)
	if followup.MemberID != f.a || followup.TopicID != r.TopicID || followup.ConversationID != r.ConversationID {
		t.Fatalf("regular claim bound wrong followup: %+v", followup)
	}
	assessment, err := s.ApplyChatAssessment(ctx, followup, candidate, investigationProposal(), "尝试将回传当作新授权。", "no-new-work")
	chatAssertNotAdmitted(t, assessment, err, "untrusted_source")
	discussionEnd(t, s, followup)
	coordinationDecideExec(t, s, "UPDATE members SET status='left' WHERE id=?", f.a)
	before = messageSequenceSnapshot(t, s)
	if again := workReturnHandback(t, s, f.owner); again != h {
		t.Fatalf("B key replay changed the handback: %+v, want %+v", again, h)
	}
	if again := readWorkReturn(t, s, h); again == nil || *again != r {
		t.Fatalf("A replay followed live status: %+v", again)
	}
	messageSequencePreserved(t, s, before)
	dispatchAssertFK(t, s)
}

func TestChatDiscussionWorkReturnLeft(t *testing.T) {
	f := setupWorkReturn(t)
	coordinationDecideExec(t, f.s, "UPDATE members SET status='left' WHERE id=?", f.a)
	counts := workReportWakeCounts(t, f.s)
	h := workReturnHandback(t, f.s, f.owner)
	r := readWorkReturn(t, f.s, h)
	if r == nil || r.RecipientID != f.a || r.DeliveryID != "" || r.ConversationID != "" {
		t.Fatalf("departure did not publish a targetless receipt: %+v", r)
	}
	m, err := f.s.GetMessage(ctx, f.groupID, r.MessageID)
	if err != nil || len(m.Deliveries) != 0 || m.TopicID != h.TopicID || !strings.HasSuffix(m.Content, h.Summary) ||
		workReportWakeCounts(t, f.s) != ([3]int{counts[0] + 2, counts[1], counts[2]}) {
		t.Fatalf("departure rerouted or omitted the public report: %+v %v", m, err)
	}
	before := messageSequenceSnapshot(t, f.s)
	if again := workReturnHandback(t, f.s, f.owner); again != h {
		t.Fatalf("targetless replay changed: %+v", again)
	}
	messageSequencePreserved(t, f.s, before)
}

func TestChatDiscussionWorkReturnAfterUnrelatedUserMessage(t *testing.T) {
	f := setupWorkReturn(t)
	s := f.s
	chatLocalMessage(t, s, MessageRequest{GroupID: f.groupID, TopicID: f.candidate.TopicID, ActorID: "user", Content: "明天例会几点开始？"}, nil)
	counts := workReportWakeCounts(t, s)
	h := workReturnHandback(t, s, f.owner)
	r := readWorkReturn(t, s, h)
	if r == nil || r.OriginMessageID != f.candidate.MessageID || r.InvitationMessageID != f.invite.Message.ID || r.RecipientID != f.a || r.DeliveryID == "" ||
		workReportWakeCounts(t, s) != ([3]int{counts[0] + 2, counts[1] + 1, counts[2]}) {
		t.Fatalf("unrelated user message blocked or rerouted the exact work return: %+v", r)
	}
	before := messageSequenceSnapshot(t, s)
	if again := workReturnHandback(t, s, f.owner); again != h {
		t.Fatalf("unrelated user message changed replay: %+v", again)
	}
	messageSequencePreserved(t, s, before)
}

func TestChatDiscussionWorkReturnAdvancedHead(t *testing.T) {
	requireWorkReturnSchema(t)
	f := setupWorkReturnAdmission(t)
	s := f.s
	// Model an already stored reply from before A5 closed admitted heads.
	// Use the ordinary publisher, bypassing only the new plan guard.
	const content = "已有普通讨论回应。"
	candidate := f.admission.CandidateID
	e := domain.ChatDiscussionEvent{Kind: domain.ChatDiscussionReply, OriginMessageID: f.invite.Event.OriginMessageID,
		SourceMessageID: f.invite.Message.ID, InvitationMessageID: f.invite.Message.ID, ActorID: f.intake.MemberID, RecipientID: f.invite.Event.ActorID}
	mutation := mutationScope{operation: "create_message", actorID: e.ActorID, groupID: f.groupID, key: "old-head"}
	payload := mutationPayload("chat_discussion/v1", candidate, e.Kind, content, "", "")
	if _, err := workMemberTx(ctx, s, f.intake, "测试历史讨论回应", func(tx *sql.Tx) (domain.ChatDiscussionReceipt, error) {
		return publishChatDiscussionInTx(ctx, tx, f.intake, candidate, content, mutation, payload, 3, e, []string{e.RecipientID})
	}); err != nil {
		t.Fatal(err)
	}
	f.settleIntake(t)
	f.startWorker(t)
	counts := workReportWakeCounts(t, s)
	h := workReturnHandback(t, s, f.owner)
	if r := readWorkReturn(t, s, h); r != nil || workReportWakeCounts(t, s) != ([3]int{counts[0] + 1, counts[1], counts[2]}) {
		t.Fatalf("advanced head still produced a wake: %+v", r)
	}
}

func TestChatDiscussionWorkReturnRequiresLiveBHandback(t *testing.T) {
	for _, name := range []string{"canceled-owner", "disabled-policy"} {
		t.Run(name, func(t *testing.T) {
			f := setupWorkReturn(t)
			s := f.s
			switch name {
			case "canceled-owner":
				if _, err := s.RequestTurnCancel(ctx, f.groupID, f.owner.Claim.TurnID); err != nil {
					t.Fatal(err)
				}
			case "disabled-policy":
				policy, err := s.GetChatPolicy(ctx, f.candidate.ThreadRef)
				if err != nil {
					t.Fatal(err)
				}
				investigationDisablePolicy(t, s, policy)
			}
			assertNoWorkReturn(t, s, f.owner, ErrMemberToolDenied)
		})
	}
}

func TestChatDiscussionWorkReturnRequiresPersistedScope(t *testing.T) {
	f := setupWorkReturn(t)
	h := workReturnHandback(t, f.s, f.owner)
	before := messageSequenceSnapshot(t, f.s)
	for name, change := range map[string]func(*domain.TaskWorkHandback){
		"group":      func(b *domain.TaskWorkHandback) { b.GroupID = f.otherID },
		"topic":      func(b *domain.TaskWorkHandback) { b.TopicID += "-other" },
		"task":       func(b *domain.TaskWorkHandback) { b.TaskID += "-other" },
		"mandate":    func(b *domain.TaskWorkHandback) { b.MandateID += "-other" },
		"assignment": func(b *domain.TaskWorkHandback) { b.AssignmentID += "-other" },
		"notice":     func(b *domain.TaskWorkHandback) { b.NoticeMessageID = f.invite.Message.ID },
		"summary":    func(b *domain.TaskWorkHandback) { b.Summary += "未保存的结果" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := h
			change(&bad)
			r, err := workMemberTx(ctx, f.s, f.owner, "测试讨论工作回传", func(tx *sql.Tx) (*domain.ChatDiscussionWorkReturnReceipt, error) {
				return f.s.chatDiscussionWorkReturnInTx(ctx, tx, f.owner, bad, false)
			})
			if !errors.Is(err, ErrMemberToolDenied) || r != nil {
				t.Fatalf("unbound result published: %+v %v", r, err)
			}
		})
	}
	messageSequencePreserved(t, f.s, before)
}

func TestChatDiscussionWorkReturnRequiresIntakeEvidence(t *testing.T) {
	for _, name := range []string{"root-source", "candidate-turn", "lost-terminal", "noncanonical-terminal"} {
		t.Run(name, func(t *testing.T) {
			f := setupWorkReturn(t)
			s := f.s
			turnID := f.intake.Claim.TurnID
			switch name {
			case "root-source":
				coordinationDecideExec(t, s, `DROP TRIGGER chat_intake_executions_update`)
				coordinationDecideExec(t, s, `UPDATE chat_intake_executions SET source_id=? WHERE candidate_id=?`, f.invite.Message.SourceID, f.candidate.ID)
			case "candidate-turn":
				coordinationDecideExec(t, s, `DROP TRIGGER chat_assessments_no_update`)
				coordinationDecideExec(t, s, `UPDATE chat_assessments SET receipt_json=json_set(receipt_json,'$.turn_id',?) WHERE candidate_id=?`, f.owner.Claim.TurnID, f.admission.CandidateID)
			case "lost-terminal":
				coordinationDecideExec(t, s, `UPDATE turns SET receipt_json='' WHERE id=?`, turnID)
			case "noncanonical-terminal":
				coordinationDecideExec(t, s, `UPDATE turns SET receipt_json=json_set(receipt_json,'$.unbound',1) WHERE id=?`, turnID)
			}
			assertNoWorkReturn(t, s, f.owner, ErrMemberToolDenied)
		})
	}
}

func TestChatDiscussionWorkReturnLegacyReplay(t *testing.T) {
	requireWorkReturnSchema(t)
	all := migrations
	defer func() { migrations = all }()
	migrations = all[:slices.Index(all, chatDiscussionWorkReturnSchema)]
	f := setupWorkReturnAdmission(t)
	f.settleIntake(t)
	f.startWorker(t)
	messageSequenceLedger(t, f.s, 30)
	migrations = all // The existing handle still has the schema it opened, not the global latest version.
	d := queueDelivery(t, f.s, f.otherID, f.foreign, "旧版讨论")
	if got, err := f.s.ResolveChatDiscussionWorkReturnDelivery(ctx, f.otherID, d.ID); got != (domain.ChatDiscussionWorkReturnReceipt{}) || err != ErrNotFound {
		t.Fatalf("legacy v30 delivery must have no typed return: %+v %v", got, err)
	}
	if role, err := f.s.ResolveCoordinationDelivery(ctx, f.otherID, d.ID); err != nil ||
		role.Provenance.Kind != CoordinationRoleOrdinary || role.Blocked != CoordinationReady {
		t.Fatalf("legacy v30 ordinary delivery was blocked: %+v %v", role, err)
	}
	h := workReturnHandback(t, f.s, f.owner)

	s := queueHandle(t, f.path, false)
	s.EnableTaskWorkExecution()
	before := messageSequenceSnapshot(t, s)
	if again := workReturnHandback(t, s, f.owner); again != h || readWorkReturn(t, s, h) != nil {
		t.Fatalf("legacy B replay changed or retroactively woke: %+v", again)
	}
	messageSequencePreserved(t, s, before)
}

func TestChatDiscussionWorkReturnAtomicAndImmutable(t *testing.T) {
	f := setupWorkReturn(t)
	s := f.s
	coordinationDecideExec(t, s, `CREATE TRIGGER ignore_return BEFORE INSERT ON chat_discussion_work_returns BEGIN SELECT RAISE(IGNORE); END`)
	assertNoWorkReturn(t, s, f.owner, ErrConflict)
	coordinationDecideExec(t, s, `DROP TRIGGER ignore_return`)
	h := workReturnHandback(t, s, f.owner)
	r := readWorkReturn(t, s, h)
	if r == nil {
		t.Fatal("missing return")
	}
	for _, statement := range []string{
		`INSERT OR REPLACE INTO chat_discussion_work_returns SELECT * FROM chat_discussion_work_returns`,
		`UPDATE chat_discussion_work_returns SET delivery_id=NULL`,
		`DELETE FROM chat_discussion_work_returns`,
		`INSERT OR REPLACE INTO deliveries SELECT d.* FROM deliveries d JOIN chat_discussion_work_returns r ON r.delivery_id=d.id`,
		`INSERT INTO message_command_sources(id,message_id,kind,payload,idempotency_key)
		 SELECT 'forged-command',message_id,'local_user_chat',x'01','forged-key' FROM chat_discussion_work_returns`,
	} {
		if _, err := s.db.Exec(statement); err == nil {
			t.Fatalf("mutable return or forged authority: %s", statement)
		}
	}
	// With the marker intact, damaged source evidence must not become an
	// ordinary delivery in the runtime's resolver or reproduce a wake on retry.
	coordinationDecideExec(t, s, `DROP TRIGGER message_command_sources_delete`)
	coordinationDecideExec(t, s, `DELETE FROM message_command_sources WHERE message_id=?`, r.OriginMessageID)
	if got, err := s.ResolveChatDiscussionWorkReturnDelivery(ctx, r.GroupID, r.DeliveryID); !errors.Is(err, ErrMemberToolDenied) || got.MessageID != "" {
		t.Fatalf("resolver fell through on lost source: %+v %v", got, err)
	}
	if got, err := s.ResolveCoordinationDelivery(ctx, r.GroupID, r.DeliveryID); got != (CoordinationRoleState{}) ||
		!errors.Is(err, ErrMemberToolDenied) || errors.Is(err, ErrNotFound) {
		t.Errorf("legacy resolver fell through on lost source: %+v %v", got, err)
	}
	assertNoWorkReturn(t, s, f.owner, ErrMemberToolDenied)
}
