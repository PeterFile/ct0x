package store

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func installDecisionAnswerFixture(t *testing.T, s *Store) {
	t.Helper()
	installDecisionRequestFixture(t, s)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(decisionAnswerSchema); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s.EnableTaskWorkExecution()
}

func assertDecisionAnswerView(t *testing.T, s *Store, a domain.DecisionAnswerReceipt, handling *domain.TaskWorkInputConsumption) {
	t.Helper()
	got, err := s.GetDecisionAnswerView(ctx, a.ThreadRef, a.RequestID, a.Version)
	want := domain.DecisionAnswerView{Receipt: a, Handling: handling}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("decision answer view = %+v (handling: %+v), %v; want receipt %+v, handling %+v", got, got.Handling, err, a, handling)
	}
}

func TestDecisionAnswerOneWakeAndExplicitOwnerAckAfterReopen(t *testing.T) {
	f := chatSetup(t, "user", false)
	f.input.Title = "周末样本验证"
	s := f.s
	installDecisionAnswerFixture(t, s)
	coordinationDecideExec(t, s, `UPDATE members SET workspace='' WHERE id=?`, f.b)
	owner := chatAuthorityWorker(t, f)
	q, err := s.ProposeTaskDecisionRequest(ctx, owner.Member, decisionTaskInput(), "ask-pay")
	if err != nil {
		t.Fatal(err)
	}
	settle := func(owner CoordinationToolScope) {
		t.Helper()
		p := owner.Provenance
		for _, scope := range []MemberToolScope{taskWorkTestCoordinator(t, s, p.GroupID, p.AssignmentID), owner.Member} {
			taskWorkTestReceipt(t, s, scope, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
		}
	}
	settle(owner)
	other := sameDecisionTaskInOtherGroup(t, f)
	peer := chatAuthorityWorker(t, other)
	otherQ, err := s.ProposeTaskDecisionRequest(ctx, peer.Member, decisionTaskInput(), "ask-pay")
	if err != nil || otherQ.Task == nil || otherQ.Task.TaskTitle != q.Task.TaskTitle || otherQ.Task.TaskID == q.Task.TaskID {
		t.Fatalf("second real same-name task: %+v %v", otherQ, err)
	}
	settle(peer)
	defer func() { dispatchAssertFK(t, s) }()
	before := messageSequenceSnapshot(t, s)
	response := domain.DecisionAnswerInput{Version: q.Version, Text: "这次只看工作日"}
	for _, wrong := range []struct {
		actor string
		ref   domain.ThreadRef
		ver   int64
		want  error
	}{
		{peer.Member.MemberID, q.ThreadRef, q.Version, ErrMemberToolDenied},
		{"user", otherQ.ThreadRef, q.Version, ErrConflict},
		{"user", domain.ThreadRef{GroupID: q.GroupID, TopicID: "foreign-topic"}, q.Version, ErrConflict},
		{"user", q.ThreadRef, q.Version + 1, ErrConflict},
	} {
		bad := response
		bad.Version = wrong.ver
		if a, err := s.AnswerDecisionRequest(ctx, wrong.actor, wrong.ref, q.ID, bad, "invalid"); a.ID != "" || !errors.Is(err, wrong.want) {
			t.Fatalf("wrong scope answered: %+v %v", a, err)
		}
	}
	messageSequencePreserved(t, s, before)

	// Two separate connections represent the global and group actions. Both
	// may reach the command concurrently without sharing a transport key.
	alias := queueHandle(t, f.path, false)
	alias.EnableTaskWorkExecution()
	var receipts [2]domain.DecisionAnswerReceipt
	var failures [2]error
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, db := range []*Store{s, alias} {
		wg.Go(func() {
			<-start
			receipts[i], failures[i] = db.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, response, []string{"global", "group"}[i])
		})
	}
	close(start)
	wg.Wait()
	a := receipts[0]
	if failures[0] != nil || failures[1] != nil || a.ID == "" || a != receipts[1] || a.RequestID != q.ID || a.Version != q.Version || a.Text != response.Text || a.ThreadRef != q.ThreadRef || a.TaskID != q.Task.TaskID {
		t.Fatalf("one exact answer expected: %+v; errors=%v", receipts, failures)
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM decision_answers`:                                  1,
		`SELECT count(*) FROM decision_answer_consumptions`:                      0,
		`SELECT count(*) FROM task_work_deliveries WHERE source_kind='followup'`: 1,
		`SELECT count(*) FROM task_work_inputs`:                                  0,
		`SELECT count(*) FROM chat_followup_routes`:                              0,
	} {
		var n int
		if err := s.db.QueryRow(query).Scan(&n); err != nil || n != want {
			t.Fatalf("%s = %d %v; want %d", query, n, err, want)
		}
	}
	for _, table := range []string{"chat_assessments", "tasks", "mandates", "task_attempts", "turns", "resource_claims", "coordination_assignments"} {
		if got := messageSequenceTableRows(t, s, table); !reflect.DeepEqual(got, before[table]) {
			t.Fatalf("saving a decision changed %s", table)
		}
	}
	message, err := s.GetMessage(ctx, q.GroupID, a.ReplyMessageID)
	if err != nil || message.SenderID != "user" || message.TopicID != q.TopicID || message.Content != response.Text || len(message.Deliveries) != 0 {
		t.Fatalf("wrong original-group user reply: %+v %v", message, err)
	}
	if _, err := s.GetMessage(ctx, otherQ.GroupID, a.ReplyMessageID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reply visible to foreign group: %v", err)
	}
	if p, err := s.ListPendingDecisionRequests(ctx, ListOptions{}); err != nil || len(p.Items) != 1 || p.Items[0].ID != otherQ.ID {
		t.Fatalf("pending isolation: %+v %v", p, err)
	}
	after := messageSequenceSnapshot(t, s)
	assertDecisionAnswerView(t, s, a, nil)
	for _, wrong := range []struct {
		thread  domain.ThreadRef
		id      string
		version int64
	}{
		// The other group has a same-named Task but no saved answer of its own.
		{otherQ.ThreadRef, q.ID, q.Version},
		{otherQ.ThreadRef, otherQ.ID, otherQ.Version},
		{domain.ThreadRef{GroupID: q.GroupID, TopicID: q.TopicID + " "}, q.ID, q.Version},
		{q.ThreadRef, "missing", q.Version},
		{q.ThreadRef, q.ID, q.Version + 1},
	} {
		if got, err := s.GetDecisionAnswerView(ctx, wrong.thread, wrong.id, wrong.version); !errors.Is(err, ErrNotFound) || got != (domain.DecisionAnswerView{}) {
			t.Fatalf("wrong scope %+v returned an answer: %+v %v", wrong, got, err)
		}
	}
	changed := response
	changed.Text = "加入周末"
	if got, err := s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, changed, "another-answer"); got.ID != "" || !errors.Is(err, ErrConflict) {
		t.Fatalf("second decision saved: %+v %v", got, err)
	}
	messageSequencePreserved(t, s, after)
	for _, db := range []*Store{s, alias} {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}

	s = queueHandle(t, f.path, false)
	if got, err := s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, response, "global"); err != nil || got != a {
		t.Fatalf("restart changed the receipt: %+v %v", got, err)
	}
	s.EnableTaskWorkExecution()
	wake, err := s.ResolveTaskWorkDelivery(ctx, q.GroupID, a.WakeDeliveryID)
	if err != nil || wake.Blocked != "" || wake.SourceKind != domain.TaskWorkSourceFollowup || wake.SourceID != a.ID || wake.MemberID != owner.Member.MemberID ||
		wake.MessageID != a.WakeMessageID || wake.TaskID != q.Task.TaskID || wake.AssignmentID != q.Task.AssignmentID || wake.MandateID != q.Task.MandateID {
		t.Fatalf("scheduler did not resolve a canonical wake: %+v %v", wake, err)
	}
	if _, err := s.ResolveTaskWorkDelivery(ctx, otherQ.GroupID, wake.DeliveryID); err == nil {
		t.Fatal("foreign group resolved the wake")
	}
	state, err := s.ResolveCoordinationDelivery(ctx, q.GroupID, wake.DeliveryID)
	if err != nil || state.Blocked != CoordinationReady || state.Provenance.SourceID != a.ID || state.Provenance.Kind != CoordinationRoleWorker {
		t.Fatalf("not a typed scheduler role: %+v %v", state, err)
	}
	const resourceKey = "decision-owner-resource"
	live := taskWorkTestClaim(t, s, wake, resourceKey)
	launch := FreshTaskWorkLaunch{Scope: live, Delivery: wake, RequestID: state.Provenance.RequestID, ProviderID: "pi", Model: "model", ResourceKey: resourceKey}
	if err := s.VerifyFreshTaskWorkStartup(ctx, launch); err != nil {
		t.Fatalf("real unbound startup scope rejected: %v", err)
	}
	attempt, err := s.BindTaskWorkContinuation(ctx, live)
	if err != nil || attempt.TaskID != q.Task.TaskID || attempt.TaskRevision != q.Task.TaskRevision || attempt.ID == owner.AttemptID {
		t.Fatalf("not a continuation of the original Task: %+v %v", attempt, err)
	}
	launch.AttemptID = attempt.ID
	if err := s.VerifyFreshTaskWorkStartup(ctx, launch); err != nil {
		t.Fatalf("canonical Attempt failed the shared startup proof: %v", err)
	}
	badLaunch := launch
	badLaunch.ResourceKey = "not-held"
	if err := s.VerifyFreshTaskWorkStartup(ctx, badLaunch); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("decision bypassed resource proof: %v", err)
	}
	if r, err := s.SubmitTaskWorkResult(ctx, live, AssignmentResultInput{Summary: "尚未确认处理。"}, "premature"); r.ID != "" || !errors.Is(err, ErrConflict) {
		t.Fatalf("submission bypassed explicit handling: %+v %v", r, err)
	}
	i, err := s.GetWorkInputForMember(ctx, live, wake.SourceID)
	if err != nil || i.ID != a.ID || i.ReplyMessageID != a.ReplyMessageID || i.CandidateID != "" || i.Consumption != nil ||
		!strings.Contains(i.Content, q.Question) || !strings.HasSuffix(i.Content, response.Text) || i.AfterConstraintRevision != i.BeforeConstraintRevision {
		t.Fatalf("no exact non-granting WorkInput projection: %+v %v", i, err)
	}
	if history, err := s.ListWorkInputHistoryForMember(ctx, live, ListOptions{}); err != nil || len(history.Items) != 1 || history.Items[0].ID != a.ID {
		t.Fatalf("decision missing from worker history: %+v %v", history, err)
	}
	if inbox, err := s.ListWorkInputsForMember(ctx, live); err != nil || len(inbox) != 1 || inbox[0].ID != a.ID {
		t.Fatalf("decision missing from the bounded worker inbox: %+v %v", inbox, err)
	}
	queueEvent(t, s, live.Claim, provider.AcceptedEvent, "", "")
	assertDecisionAnswerView(t, s, a, nil) // Accepted input with a bound Attempt is not an ACK.
	var handling *domain.TaskWorkInputConsumption
	for _, key := range []string{"ack-first", "ack-second"} {
		got, err := s.AcknowledgeWorkInputWithKey(ctx, live, i.ID, key)
		if err != nil || got.Consumption == nil || got.Consumption.TurnID != live.Claim.TurnID || got.Consumption.AttemptID != attempt.ID {
			t.Fatalf("explicit handling not durable: %+v %v", got, err)
		}
		if handling != nil && *got.Consumption != *handling {
			t.Fatalf("ACK replay changed handling: %+v, want %+v", got.Consumption, handling)
		}
		handling = got.Consumption
	}
	afterAck := messageSequenceSnapshot(t, s)
	assertDecisionAnswerView(t, s, a, handling)
	if got, err := s.GetDecisionAnswer(ctx, q.ThreadRef, q.ID, q.Version); err != nil || got != a {
		t.Fatalf("handling changed saved answer: %+v %v", got, err)
	}
	messageSequencePreserved(t, s, afterAck)
	for _, key := range []string{"global", "retry-after-ack"} {
		if got, err := s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, response, key); err != nil || got != a {
			t.Fatalf("retry after handling changed saved answer: %+v %v", got, err)
		}
	}
	if _, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: q.GroupID, DeliveryID: a.WakeDeliveryID, MaxActive: 4}); !errors.Is(err, ErrTurnBusy) {
		t.Fatalf("saved answer could claim a second continuation: %v", err)
	}
	taskWorkTestReceipt(t, s, live, queueReceipt(provider.OutcomeSucceeded), domain.TurnStateSucceeded)
	assertDecisionAnswerView(t, s, a, handling)
	for query, want := range map[string]int{
		`SELECT count(*) FROM task_work_deliveries WHERE source_kind='followup'`: 1,
		`SELECT count(*) FROM decision_answer_consumptions`:                      1,
		`SELECT count(*) FROM task_work_continuations`:                           1,
		`SELECT count(*) FROM task_work_input_consumptions`:                      0,
	} {
		var n int
		if err := s.db.QueryRow(query).Scan(&n); err != nil || n != want {
			t.Fatalf("%s = %d %v; want %d", query, n, err, want)
		}
	}
}

func TestDecisionAnswerVersionAndRevocationFence(t *testing.T) {
	f := chatSetup(t, "user", false)
	s := f.s
	installDecisionAnswerFixture(t, s)
	owner := chatAuthorityWorker(t, f)
	proposal := decisionTaskInput()
	q, err := s.ProposeTaskDecisionRequest(ctx, owner.Member, proposal, "v1")
	if err != nil {
		t.Fatal(err)
	}
	proposal.ExpectedVersion, proposal.Question = q.Version, "这次选择哪组样本？"
	q2, err := s.ProposeTaskDecisionRequest(ctx, owner.Member, proposal, "v2")
	if err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, s)
	if _, err := s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, domain.DecisionAnswerInput{Version: q.Version, OptionIndex: 1}, "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("old labels could be answered: %v", err)
	}
	messageSequencePreserved(t, s, before)
	a, err := s.AnswerDecisionRequest(ctx, "user", q2.ThreadRef, q2.ID, domain.DecisionAnswerInput{Version: q2.Version, OptionIndex: 1}, "valid")
	if err != nil || a.Text != proposal.Choices[0] || a.OptionIndex != 1 || a.Version != q2.Version {
		t.Fatalf("version-bound label was not resolved: %+v %v", a, err)
	}
	if _, err := s.GetWorkInputForMember(ctx, owner.Member, a.ID); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("pending answer was exposed to the original Turn: %v", err)
	}
	if _, err := s.AcknowledgeWorkInputWithKey(ctx, owner.Member, a.ID, "wrong-ack"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("original Turn consumed the answer before its wake: %v", err)
	}
	proposal.Question, proposal.ExpectedVersion = "能否重开这个问题？", q2.Version
	if _, err := s.ProposeTaskDecisionRequest(ctx, owner.Member, proposal, "reopen"); !errors.Is(err, ErrConflict) {
		t.Fatalf("answered slot was amendable: %v", err)
	}
	for _, scope := range []MemberToolScope{taskWorkTestCoordinator(t, s, q.GroupID, q.Task.AssignmentID), owner.Member} {
		taskWorkTestReceipt(t, s, scope, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	}
	coordinationDecideExec(t, s, `UPDATE decision_requests SET withdrawn_at=? WHERE id=?`, time.Now().UTC().Format(timestampLayout), q.ID)
	if wake, err := s.ResolveTaskWorkDelivery(ctx, q.GroupID, a.WakeDeliveryID); err != nil || wake.Blocked != "decision_answer_stale" {
		t.Fatalf("withdrawn answer remains runnable: %+v %v", wake, err)
	}
	before = messageSequenceSnapshot(t, s)
	if _, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: q.GroupID, DeliveryID: a.WakeDeliveryID, MaxActive: 4}); err == nil {
		t.Fatal("withdrawn answer allowed a claim")
	}
	if _, err := s.AnswerDecisionRequest(ctx, "user", q2.ThreadRef, q2.ID, domain.DecisionAnswerInput{Version: q2.Version, OptionIndex: 1}, "valid"); !errors.Is(err, ErrConflict) {
		t.Fatalf("withdrawn answer was executable through retry: %v", err)
	}
	messageSequencePreserved(t, s, before)
	if got, err := s.GetDecisionAnswer(ctx, q.ThreadRef, q.ID, q2.Version); err != nil || got != a {
		t.Fatalf("revocation lost historical receipt: %+v %v", got, err)
	}
}

func TestDecisionAnswerWithdrawalAfterClaimFencesLaunchAndAck(t *testing.T) {
	f := chatSetup(t, "user", false)
	s := f.s
	installDecisionAnswerFixture(t, s)
	coordinationDecideExec(t, s, `UPDATE members SET workspace='' WHERE id=?`, f.b)
	owner := chatAuthorityWorker(t, f)
	q, err := s.ProposeTaskDecisionRequest(ctx, owner.Member, decisionTaskInput(), "ask")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, domain.DecisionAnswerInput{Version: q.Version, Text: "这次只看工作日"}, "answer")
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []MemberToolScope{taskWorkTestCoordinator(t, s, q.GroupID, q.Task.AssignmentID), owner.Member} {
		taskWorkTestReceipt(t, s, scope, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	}
	wake, err := s.ResolveTaskWorkDelivery(ctx, q.GroupID, a.WakeDeliveryID)
	if err != nil || wake.Blocked != "" {
		t.Fatalf("unexpected scope before withdrawal: %+v %v", wake, err)
	}
	const resource = "answer-launch-fence"
	live := taskWorkTestClaim(t, s, wake, resource)
	attempt, err := s.BindTaskWorkContinuation(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	launch := FreshTaskWorkLaunch{Scope: live, Delivery: wake, RequestID: owner.Provenance.RequestID, AttemptID: attempt.ID,
		ProviderID: "pi", Model: "model", ResourceKey: resource}
	if err := s.VerifyFreshTaskWorkStartup(ctx, launch); err != nil {
		t.Fatal(err)
	}
	coordinationDecideExec(t, s, `UPDATE decision_requests SET withdrawn_at=? WHERE id=?`, time.Now().UTC().Format(timestampLayout), q.ID)
	before := messageSequenceSnapshot(t, s)
	invoked := false
	if err := s.CommitFreshTaskWorkLaunch(ctx, launch, func() bool { invoked = true; return true }); !errors.Is(err, ErrMemberToolDenied) || invoked {
		t.Fatalf("withdrawal bypassed final startup fence: %v invoked=%v", err, invoked)
	}
	if _, err := s.AcknowledgeWorkInputWithKey(ctx, live, a.ID, "ack-after-withdraw"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("withdrawal bypassed handling fence: %v", err)
	}
	messageSequencePreserved(t, s, before)
	queueEvent(t, s, live.Claim, provider.AcceptedEvent, "", "")
	queueEvent(t, s, live.Claim, provider.TerminalEvent, "", provider.OutcomeSucceeded)
	assertDecisionAnswerView(t, s, a, nil)
	taskWorkTestReceipt(t, s, live, queueReceipt(provider.OutcomeSucceeded), domain.TurnStateSucceeded)
	assertDecisionAnswerView(t, s, a, nil) // Settlement and resource release are not ACKs either.
}

func TestDecisionAnswerDoesNotTreatClarificationAsGenericChat(t *testing.T) {
	f := chatSetup(t, "user", false)
	installDecisionAnswerFixture(t, f.s)
	input, details := decisionClarificationInput()
	q, err := f.s.ApplyChatDecisionClarification(ctx, f.scope, f.candidate.ID, input, details, input.Question, "thread-only")
	if err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	if got, err := f.s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, domain.DecisionAnswerInput{Version: q.Version, Text: "只看工作日"}, "answer"); !errors.Is(err, ErrDecisionRelayUnsupported) || got.ID != "" {
		t.Fatalf("unsupported thread answer was routed: %+v %v", got, err)
	}
	if page, err := f.s.ListPendingTaskDecisions(ctx, ListOptions{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("public list projected an unsupported relay: %+v %v", page, err)
	}
	if page, err := f.s.ListPendingDecisionRequests(ctx, ListOptions{}); err != nil || len(page.Items) != 1 || page.Items[0].ID != q.ID {
		t.Fatalf("request kernel lost the unresolved clarification: %+v %v", page, err)
	}
	messageSequencePreserved(t, f.s, before)
}
