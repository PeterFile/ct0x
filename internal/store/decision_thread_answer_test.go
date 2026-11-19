package store

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func installThreadDecisionAnswerFixture(t *testing.T, s *Store) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(decisionThreadAnswerSchema); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func askThreadDecision(t *testing.T, f chatFixture) domain.DecisionRequest {
	t.Helper()
	input, details := decisionClarificationInput()
	q, err := f.s.ApplyChatDecisionClarification(ctx, f.scope, f.candidate.ID, input, details, input.Question, "ask-thread")
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func threadDecisionFixture(t *testing.T) (chatFixture, domain.DecisionRequest) {
	t.Helper()
	f := chatSetup(t, "user", false)
	installDecisionAnswerFixture(t, f.s)
	q := askThreadDecision(t, f) // Prove that an existing opt-in can be answered after installation.
	installThreadDecisionAnswerFixture(t, f.s)
	if _, err := f.s.ApplyTurnReceipt(ctx, f.scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
		t.Fatal(err)
	}
	return f, q
}

// ThreadDecisionHTTPFixture shares the real, unregistered migrations with the
// external transport test, without a production installer or fixture SQL copy.
func ThreadDecisionHTTPFixture(t *testing.T) (*Store, string, domain.DecisionRequest, string) {
	t.Helper()
	f, q := threadDecisionFixture(t)
	return f.s, f.path, q, f.otherID
}

func TestThreadDecisionAnswerReturnsOnlyToTheAsker(t *testing.T) {
	f := chatSetup(t, "user", false)
	installDecisionAnswerFixture(t, f.s)
	q := askThreadDecision(t, f)
	answer := domain.DecisionAnswerInput{Version: q.Version, OptionIndex: 1}
	if page, err := f.s.ListAnswerableDecisions(ctx, ListOptions{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("advertised a route without Thread reply storage: %+v %v", page, err)
	}
	if _, err := f.s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, answer, "legacy"); !errors.Is(err, ErrDecisionRelayUnsupported) {
		t.Fatalf("uninstalled Thread reply route: %v", err)
	}
	installThreadDecisionAnswerFixture(t, f.s)
	other, err := f.s.GetGroup(ctx, f.otherID)
	if err != nil || q.GroupName != other.Name || q.GroupID == other.ID {
		t.Fatalf("fixture must have distinct groups with the same name: %+v %v", other, err)
	}
	for _, foreign := range []domain.ThreadRef{{GroupID: other.ID, TopicID: q.TopicID}, {GroupID: q.GroupID, TopicID: q.TopicID + " "}} {
		chatLocalMessage(t, f.s, MessageRequest{GroupID: foreign.GroupID, TopicID: foreign.TopicID, ActorID: "user", Content: "另一项请求。"}, nil)
	}
	if page, err := f.s.ListAnswerableDecisions(ctx, ListOptions{Limit: 1}); err != nil || len(page.Items) != 1 || page.Items[0].ID != q.ID || page.NextCursor != nil {
		t.Fatalf("installed Thread reply was not answerable: %+v %v", page, err)
	}
	before := messageSequenceSnapshot(t, f.s)
	for _, wrong := range []domain.ThreadRef{
		{GroupID: f.otherID, TopicID: q.TopicID},
		{GroupID: q.GroupID, TopicID: q.TopicID + " "},
	} {
		if got, err := f.s.AnswerDecisionRequest(ctx, "user", wrong, q.ID, answer, "wrong"); !errors.Is(err, ErrConflict) || got.ID != "" {
			t.Fatalf("foreign Thread %v answered: %+v %v", wrong, got, err)
		}
	}
	if _, err := f.s.AnswerDecisionRequest(ctx, f.scope.MemberID, q.ThreadRef, q.ID, answer, "member"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("a member answered in place of the local user: %v", err)
	}
	messageSequencePreserved(t, f.s, before)
	a, err := f.s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, answer, "global")
	if err != nil || a.ID == "" || a.ThreadRef != q.ThreadRef || a.TaskID != "" || a.Text != q.Choices[0] || a.ReplyMessageID != a.WakeMessageID {
		t.Fatalf("expected one same-Thread reply/wake for the existing clarification: %+v %v", a, err)
	}
	m, err := f.s.GetMessage(ctx, q.GroupID, a.ReplyMessageID)
	if err != nil || m.TopicID != q.TopicID || m.SenderID != "user" || m.SourceKind == "local_user_chat" || m.Content != a.Text || len(m.Deliveries) != 1 ||
		m.Deliveries[0].ID != a.WakeDeliveryID || m.Deliveries[0].RecipientID != f.scope.MemberID || m.Deliveries[0].ConversationID != f.scope.ConversationID {
		t.Fatalf("wrong reply/recipient: %+v %v", m, err)
	}
	if got, err := f.s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, answer, "group"); err != nil || got != a {
		t.Fatalf("pair of answer actions did not coalesce: %+v %v", got, err)
	}
	assertDecisionAnswerView(t, f.s, a, nil)
	if page, err := f.s.ListAnswerableDecisions(ctx, ListOptions{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("answered clarification returned to inbox: %+v %v", page, err)
	}
	for _, table := range []string{"messages", "deliveries", "message_log", "decision_thread_answers"} {
		if n := len(messageSequenceTableRows(t, f.s, table)) - len(before[table]); n != 1 {
			t.Fatalf("answer created %d rows in %s, want 1", n, table)
		}
		delete(before, table)
	}
	delete(before, "mutation_idempotency")   // Two transport keys, same immutable answer.
	messageSequencePreserved(t, f.s, before) // No Task, Mandate, fake assessment or new source command.
	dispatchAssertFK(t, f.s)
}

func TestThreadDecisionWakeAfterReopenAndSecondClaim(t *testing.T) {
	f, q := threadDecisionFixture(t)
	a := saveThreadDecisionForTest(t, f.s, q)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	s := queueHandle(t, f.path, false)
	alias := queueHandle(t, f.path, true)
	assertDecisionAnswerView(t, s, a, nil)
	claimInput := ClaimTurnInput{GroupID: q.GroupID, DeliveryID: a.WakeDeliveryID, MaxActive: 2, ResourceKeys: []string{"thread-answer-resource"}}
	before := messageSequenceSnapshot(t, s)
	if _, err := s.ClaimTurn(ctx, claimInput); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("typed wake reached ordinary claim: %v", err)
	}
	if _, err := s.ResolveCoordinationDelivery(ctx, q.GroupID, a.WakeDeliveryID); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("typed wake reached ordinary preparation: %v", err)
	}
	if _, err := s.RecordLocalMessageTrigger(ctx, a.ThreadRef, a.ReplyMessageID); err == nil {
		t.Fatal("reply was promoted into a fresh trigger")
	}
	if _, err := s.RequestWork(ctx, q.GroupID, domain.RequestWorkRequest{TopicID: q.TopicID, MessageID: a.ReplyMessageID,
		Mode: domain.RoutingDecisionCreateNew, Reason: "not authority", NewTask: &domain.NewTaskRequest{Title: "phantom", Acceptance: "no"}}, "promote"); err == nil {
		t.Fatal("answer granted a Task or Mandate")
	}
	claimInput.ThreadDecisionAnswerID = threadDecisionAnswerPrefix + "wrong"
	if _, err := s.ClaimTurn(ctx, claimInput); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("wrong marker claimed the delivery: %v", err)
	}
	messageSequencePreserved(t, s, before)
	for _, lane := range []string{"", "human", "work", PendingThreadDecisions} {
		page, err := s.ListPendingDeliveries(ctx, PendingOptions{Lane: lane, Limit: 1})
		if err != nil || page.NextCursor != nil {
			t.Fatalf("lane %q: %+v %v", lane, page, err)
		}
		if lane == PendingThreadDecisions {
			if len(page.Items) != 1 || page.Items[0].Delivery.ID != a.WakeDeliveryID {
				t.Fatalf("one wake did not survive restart: %+v", page)
			}
		} else if len(page.Items) != 0 {
			t.Fatalf("lane %q showed a fallback wake: %+v", lane, page)
		}
	}
	wake, err := s.ResolveThreadDecisionWake(ctx, q.GroupID, a.WakeDeliveryID)
	if err != nil || wake.Receipt != a || wake.AskerID != f.scope.MemberID || wake.AskerTurnID != f.scope.Claim.TurnID ||
		wake.ConversationID != f.scope.ConversationID || wake.SourceMessageID != q.SourceMessageID || wake.NoticeMessageID != q.Chat.NoticeMessageID || wake.Question != q.Question {
		t.Fatalf("wrong continuation proof: %+v %v", wake, err)
	}
	claimInput.ThreadDecisionAnswerID = a.ID
	start := make(chan struct{})
	var wg sync.WaitGroup
	var turns [2]domain.Turn
	var failures [2]error
	for i, db := range []*Store{s, alias} {
		wg.Go(func() {
			<-start
			turns[i], failures[i] = db.ClaimTurn(ctx, claimInput)
		})
	}
	close(start)
	wg.Wait()
	winner := 0
	if failures[0] != nil {
		winner = 1
	}
	turn := turns[winner]
	if failures[winner] != nil || turn.ID == "" || turn.MemberID != wake.AskerID || turn.ConversationID != wake.ConversationID ||
		!errors.Is(failures[1-winner], ErrTurnBusy) || turns[1-winner].ID != "" {
		t.Fatalf("expected one bound claimant: %+v; %v", turns, failures)
	}
	claim := TurnClaim{GroupID: q.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	if _, err := s.BindMemberTool(ctx, claim); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("claimed wake could issue generic capabilities: %v", err)
	}
	if _, err := s.CaptureChatCandidate(ctx, MemberToolScope{Claim: claim, MemberID: turn.MemberID, ConversationID: turn.ConversationID, TopicID: q.TopicID}); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("claimed wake became a fresh assessor/intake: %v", err)
	}
	admitted := false
	admit := func() bool { admitted = true; return true }
	wrong := wake
	wrong.Receipt.GroupID = f.otherID
	if err := s.CommitThreadDecisionLaunch(ctx, claim, wrong, admit); !errors.Is(err, ErrMemberToolDenied) || admitted {
		t.Fatalf("foreign plan passed typed launch: %v, admitted=%t", err, admitted)
	}
	if err := s.CommitThreadDecisionLaunch(ctx, claim, wake, admit); err != nil || !admitted {
		t.Fatalf("typed continuation not launchable: %v, admitted=%t", err, admitted)
	}
	queueEvent(t, s, claim, provider.AcceptedEvent, "", "")
	assertDecisionAnswerView(t, s, a, nil)
	if settled, err := s.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeSucceeded)); err != nil || settled.State != domain.TurnStateSucceeded {
		t.Fatalf("settling typed wake: %+v %v", settled, err)
	}
	assertDecisionAnswerView(t, s, a, nil) // Neither acceptance nor success proves handling.
	s = queueHandle(t, f.path, false)
	if got := saveThreadDecisionForTest(t, s, q); got != a {
		t.Fatalf("restart requeued the original answer: %+v", got)
	}
	if _, err := s.ClaimTurn(ctx, claimInput); !errors.Is(err, ErrTurnBusy) {
		t.Fatalf("settlement and reopen allowed a second claim: %v", err)
	}
	if len(messageSequenceTableRows(t, s, "decision_thread_answers")) != 1 || len(messageSequenceTableRows(t, s, "deliveries")) != 2 {
		t.Fatal("answer replay was not one message/wake")
	}
	dispatchAssertFK(t, s)
}

func TestThreadDecisionRecoveryDoesNotRequeueOrAcknowledge(t *testing.T) {
	f, q := threadDecisionFixture(t)
	a := saveThreadDecisionForTest(t, f.s, q)
	wake, err := f.s.ResolveThreadDecisionWake(ctx, q.GroupID, a.WakeDeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	input := ClaimTurnInput{GroupID: q.GroupID, DeliveryID: a.WakeDeliveryID, ThreadDecisionAnswerID: a.ID, MaxActive: 2}
	turn, err := f.s.ClaimTurn(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	s := queueHandle(t, f.path, false)
	if count, err := s.RecoverInFlight(ctx); err != nil || count != 1 {
		t.Fatalf("recovery: %d, %v", count, err)
	}
	if _, err := s.ClaimTurn(ctx, input); !errors.Is(err, ErrTurnBusy) {
		t.Fatalf("unknown recovery requeued the answer: %v", err)
	}
	claim := TurnClaim{GroupID: q.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	admitted := false
	if err := s.CommitThreadDecisionLaunch(ctx, claim, wake, func() bool { admitted = true; return true }); !errors.Is(err, ErrStaleClaim) || admitted {
		t.Fatalf("old claimant launched after recovery: %v, admitted=%t", err, admitted)
	}
	assertDecisionAnswerView(t, s, a, nil)
}

func saveThreadDecisionForTest(t *testing.T, s *Store, q domain.DecisionRequest) domain.DecisionAnswerReceipt {
	t.Helper()
	a, err := s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, domain.DecisionAnswerInput{Version: q.Version, Text: "只看工作日"}, "thread-answer")
	if err != nil || a.ID == "" {
		t.Fatalf("save answer: %+v %v", a, err)
	}
	return a
}

func TestThreadDecisionReplyFences(t *testing.T) {
	for _, change := range []string{"new-user-message", "policy", "changed-source", "withdrawal", "expired", "asker-left", "asker-rejoined"} {
		for _, when := range []string{"before-answer", "before-claim", "before-launch"} {
			if change == "asker-rejoined" && when == "before-answer" {
				continue // No pending wake existed at the time of departure.
			}
			t.Run(change+"/"+when, func(t *testing.T) {
				f, q := threadDecisionFixture(t)
				s := f.s
				var a domain.DecisionAnswerReceipt
				var wake ThreadDecisionWake
				var claim TurnClaim
				if when != "before-answer" {
					a = saveThreadDecisionForTest(t, s, q)
					var err error
					wake, err = s.ResolveThreadDecisionWake(ctx, q.GroupID, a.WakeDeliveryID)
					if err != nil {
						t.Fatal(err)
					}
				}
				input := ClaimTurnInput{GroupID: q.GroupID, DeliveryID: a.WakeDeliveryID, MaxActive: 2, ThreadDecisionAnswerID: a.ID}
				if when == "before-launch" {
					turn, err := s.ClaimTurn(ctx, input)
					if err != nil {
						t.Fatal(err)
					}
					claim = TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
				}
				switch change {
				case "new-user-message":
					chatLocalMessage(t, s, MessageRequest{GroupID: q.GroupID, TopicID: q.TopicID, ActorID: "user", Content: "更新这项要求。"}, nil)
				case "policy":
					policy := f.policy.ChatPolicyRequest
					policy.Enabled = false
					if _, err := s.SetChatPolicy(ctx, q.ThreadRef, policy, f.policy.Revision, "updated"); err != nil {
						t.Fatal(err)
					}
				case "changed-source":
					coordinationDecideExec(t, s, `UPDATE deliveries SET turn_id='' WHERE message_id=?`, q.SourceMessageID)
				case "withdrawal":
					coordinationDecideExec(t, s, `UPDATE decision_requests SET withdrawn_at=created_at WHERE id=?`, q.ID)
				case "expired":
					// Advance the expiry without sleeping; the ordinary schema does not let callers mutate a version.
					coordinationDecideExec(t, s, `DROP TRIGGER decision_request_versions_no_update`)
					coordinationDecideExec(t, s, `UPDATE decision_request_versions SET expires_at=? WHERE request_id=?`, q.UpdatedAt.Add(time.Nanosecond).Format(timestampLayout), q.ID)
				case "asker-left", "asker-rejoined":
					coordinationDecideExec(t, s, `UPDATE members SET status='left' WHERE id=?`, f.scope.MemberID)
					if change == "asker-rejoined" {
						coordinationDecideExec(t, s, `UPDATE members SET status='active' WHERE id=?`, f.scope.MemberID)
					}
				}
				before := messageSequenceSnapshot(t, s)
				if page, err := s.ListAnswerableDecisions(ctx, ListOptions{}); err != nil || len(page.Items) != 0 {
					t.Fatalf("stale version in inbox: %+v %v", page, err)
				}
				answer := domain.DecisionAnswerInput{Version: q.Version, Text: "只看工作日"}
				got, err := s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, answer, "thread-answer")
				if a.ID == "" {
					if !errors.Is(err, ErrConflict) || got.ID != "" {
						t.Fatalf("stale question was still answerable: %+v %v", got, err)
					}
				} else {
					if err != nil || got != a {
						t.Fatalf("stale replay did not return the original receipt: got %+v, err %v, want %+v", got, err, a)
					}
					if got, err := s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, answer, "stale-new-key"); !errors.Is(err, ErrConflict) || got.ID != "" {
						t.Fatalf("stale question was answerable with a new key: %+v %v", got, err)
					}
					if _, err := s.ResolveThreadDecisionWake(ctx, q.GroupID, a.WakeDeliveryID); !errors.Is(err, ErrNotFound) {
						t.Fatalf("stale wake was still ready: %v", err)
					}
					if got, err := s.ClaimTurn(ctx, input); err == nil || got.ID != "" {
						t.Fatalf("stale typed wake claimed: %+v %v", got, err)
					}
					input.ThreadDecisionAnswerID = ""
					if got, err := s.ClaimTurn(ctx, input); err == nil || got.ID != "" {
						t.Fatalf("stale wake fell back to ordinary claim: %+v %v", got, err)
					}
					assertDecisionAnswerView(t, s, a, nil)
				}
				if claim.TurnID != "" {
					admitted := false
					if err := s.CommitThreadDecisionLaunch(ctx, claim, wake, func() bool { admitted = true; return true }); !errors.Is(err, ErrMemberToolDenied) || admitted {
						t.Fatalf("stale claimed wake passed final fence: %v admitted=%t", err, admitted)
					}
				}
				messageSequencePreserved(t, s, before)
			})
		}
	}
}

func TestThreadDecisionReplySaveRollsBackAndCannotBeReplaced(t *testing.T) {
	f, q := threadDecisionFixture(t)
	s := f.s
	coordinationDecideExec(t, s, `CREATE TRIGGER thread_reply_drift AFTER INSERT ON mutation_idempotency
	 WHEN NEW.operation='answer_decision_request' BEGIN
	 UPDATE decision_requests SET withdrawn_at=created_at WHERE id='`+q.ID+`'; END`)
	before := messageSequenceSnapshot(t, s)
	if a, err := s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, domain.DecisionAnswerInput{Version: q.Version, Text: "工作日"}, "drift"); !errors.Is(err, ErrConflict) || a.ID != "" {
		t.Fatalf("write did not roll back when the question changed: %+v %v", a, err)
	}
	messageSequencePreserved(t, s, before)
	coordinationDecideExec(t, s, `DROP TRIGGER thread_reply_drift`)
	a := saveThreadDecisionForTest(t, s, q)
	before = messageSequenceSnapshot(t, s)
	for _, stmt := range []string{
		`UPDATE decision_thread_answers SET option_index=2 WHERE id=?`,
		`DELETE FROM decision_thread_answers WHERE id=?`,
		`INSERT OR REPLACE INTO decision_thread_answers SELECT * FROM decision_thread_answers WHERE id=?`,
	} {
		if _, err := s.db.Exec(stmt, a.ID); err == nil {
			t.Fatalf("mutable answer: %s", stmt)
		}
	}
	if _, err := s.db.Exec(`UPDATE deliveries SET topic_id='another-topic' WHERE id=?`, a.WakeDeliveryID); err == nil || !strings.Contains(err.Error(), "投递目标") {
		t.Fatalf("wake could be moved outside its Thread: %v", err)
	}
	messageSequencePreserved(t, s, before)
}
