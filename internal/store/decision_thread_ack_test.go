package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

type threadDecisionAcknowledger interface {
	AcknowledgeThreadDecision(context.Context, TurnClaim, string, string) (domain.DecisionAnswerView, error)
}

func requireThreadDecisionAcknowledger(t *testing.T, s *Store) threadDecisionAcknowledger {
	t.Helper()
	api, ok := any(s).(threadDecisionAcknowledger)
	if !ok {
		t.Fatal("Store has no explicit acknowledgement for this saved Thread decision")
	}
	return api
}

func denyThreadDecisionAck(t *testing.T, api threadDecisionAcknowledger, claim TurnClaim, answerID, key string, want error) {
	t.Helper()
	if got, err := api.AcknowledgeThreadDecision(ctx, claim, answerID, key); !errors.Is(err, want) || got != (domain.DecisionAnswerView{}) {
		t.Fatalf("acknowledgement = %+v, %v; want no receipt and %v", got, err, want)
	}
}

func TestThreadDecisionAcknowledgementIsExplicitAndDurable(t *testing.T) {
	f, claim, launch := claimedThreadLaunchFixture(t)
	s, a := f.s, launch.Wake.Receipt
	assertDecisionAnswerView(t, s, a, nil)
	api := requireThreadDecisionAcknowledger(t, s)
	const key = "handle-thread"
	denyThreadDecisionAck(t, api, claim, a.ID, key, ErrMemberToolDenied)
	if err := s.CommitThreadDecisionLaunch(ctx, claim, launch, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	queueEvent(t, s, claim, provider.SessionEvent, "", "")
	denyThreadDecisionAck(t, api, claim, a.ID, key, ErrMemberToolDenied)
	coordinationDecideExec(t, s, `UPDATE deliveries SET state='accepted' WHERE id=?`, a.WakeDeliveryID)
	denyThreadDecisionAck(t, api, claim, a.ID, key, ErrMemberToolDenied) // Transport state alone is not execution acceptance.
	queueEvent(t, s, claim, provider.AcceptedEvent, "", "")
	turn := queueTurn(t, s, claim)
	assertDecisionAnswerView(t, s, a, nil)
	before := messageSequenceSnapshot(t, s)
	denyThreadDecisionAck(t, api, claim, a.ID, "", ErrInvalid)
	denyThreadDecisionAck(t, api, claim, threadDecisionAnswerPrefix+"other", key, ErrMemberToolDenied)
	wrong := claim
	wrong.Token = "other-token"
	denyThreadDecisionAck(t, api, wrong, a.ID, key, ErrStaleClaim)
	cold := queueHandle(t, f.path, true)
	denyThreadDecisionAck(t, requireThreadDecisionAcknowledger(t, cold), claim, a.ID, key, ErrMemberToolDenied)
	messageSequencePreserved(t, s, before)

	// Give both connections time to finish their competing acknowledgements.
	// Either the immediate BEGIN or the deferred write fence may have to wait.
	for _, db := range []*Store{s, cold} {
		coordinationDecideExec(t, db, "PRAGMA busy_timeout=60000")
	}
	cold.EnableThreadDecisionExecution()
	var receipts [2]domain.DecisionAnswerView
	var failures [2]error
	keys := []string{key, "competing-key"}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, db := range []*Store{s, cold} {
		api := requireThreadDecisionAcknowledger(t, db)
		wg.Go(func() {
			<-start
			receipts[i], failures[i] = api.AcknowledgeThreadDecision(ctx, claim, a.ID, keys[i])
		})
	}
	close(start)
	wg.Wait()
	winner := 0
	if failures[0] != nil {
		winner = 1
	}
	want := receipts[winner]
	if failures[winner] != nil || want.Receipt != a || want.Handling == nil || want.Handling.TurnID != claim.TurnID ||
		want.Handling.AttemptID != "" || want.Handling.ConsumedAt.Before(*turn.AcceptedAt) ||
		!errors.Is(failures[1-winner], ErrConflict) || receipts[1-winner] != (domain.DecisionAnswerView{}) {
		t.Fatalf("expected one Turn-only handling receipt, got %+v; errors %v; causes %v / %v",
			receipts, failures, errors.Unwrap(failures[0]), errors.Unwrap(failures[1]))
	}
	keyWon := keys[winner]
	if replay, err := api.AcknowledgeThreadDecision(ctx, claim, a.ID, keyWon); err != nil || !reflect.DeepEqual(replay, want) {
		t.Fatalf("same-key replay changed the receipt: %+v, %v", replay, err)
	}
	if rows := messageSequenceTableRows(t, s, "decision_thread_answer_consumptions"); len(rows) != 1 {
		t.Fatalf("expected one durable acknowledgement, got %d", len(rows))
	}
	delete(before, "decision_thread_answer_consumptions")
	messageSequencePreserved(t, s, before) // No Task, Attempt, grant, or new wake.

	assertHistory := func(db *Store) {
		t.Helper()
		got, err := db.GetDecisionAnswerView(ctx, a.ThreadRef, a.RequestID, a.Version)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("read must return the explicit historical handling: %+v, %v", got, err)
		}
		data, err := json.Marshal(got.Handling)
		var fields map[string]json.RawMessage
		if err != nil || json.Unmarshal(data, &fields) != nil || len(fields) != 2 || fields["turn_id"] == nil || fields["consumed_at"] == nil {
			t.Fatalf("expected a real Turn and timestamp with no attempt on the wire: %s, %v", data, err)
		}
	}
	assertHistory(s)
	if _, err := s.BindMemberTool(ctx, claim); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("typed acknowledgement granted ordinary tools: %v", err)
	}
	if _, err := s.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeSucceeded)); err != nil {
		t.Fatal(err)
	}
	assertHistory(s)
	denyThreadDecisionAck(t, api, claim, a.ID, keyWon, ErrMemberToolDenied)
	coordinationDecideExec(t, s, `UPDATE members SET status='left' WHERE id=?`, turn.MemberID)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cold.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, f.path, false)
	assertHistory(reopened)
	denyThreadDecisionAck(t, requireThreadDecisionAcknowledger(t, reopened), claim, a.ID, keyWon, ErrMemberToolDenied)
	dispatchAssertFK(t, reopened)
}

func acceptedThreadDecisionFixture(t *testing.T) (chatFixture, TurnClaim, domain.DecisionAnswerReceipt) {
	t.Helper()
	f, claim, launch := claimedThreadLaunchFixture(t)
	if err := f.s.CommitThreadDecisionLaunch(ctx, claim, launch, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	queueEvent(t, f.s, claim, provider.AcceptedEvent, "", "")
	return f, claim, launch.Wake.Receipt
}

func TestThreadDecisionAcknowledgementLedgerGuards(t *testing.T) {
	f, claim, launch := claimedThreadLaunchFixture(t)
	s, a := f.s, launch.Wake.Receipt
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM decision_thread_answer_consumptions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("SQLite must install an empty acknowledgement ledger, got %d, %v", count, err)
	}
	const insert = `INSERT INTO decision_thread_answer_consumptions(answer_id,delivery_id,turn_id,call_key,consumed_at) VALUES(?,?,?,?,?)`
	refuseInsert := func(deliveryID, turnID string) {
		t.Helper()
		if _, err := s.db.Exec(insert, a.ID, deliveryID, turnID, "key", time.Now().UTC().Format(timestampLayout)); err == nil {
			t.Fatal("ledger accepted an unproved Turn/delivery")
		}
	}
	refuseInsert(a.WakeDeliveryID, claim.TurnID)
	queueEvent(t, s, claim, provider.AcceptedEvent, "", "")
	refuseInsert(a.WakeDeliveryID, launch.Wake.AskerTurnID)
	refuseInsert(queueTurn(t, s, f.scope.Claim).DeliveryID, claim.TurnID)
	if _, err := s.AcknowledgeThreadDecision(ctx, claim, a.ID, "key"); err != nil {
		t.Fatal(err)
	}
	other := queueDelivery(t, s, a.GroupID, launch.Wake.AskerID, a.TopicID)
	before := messageSequenceSnapshot(t, s)
	for stmt, args := range map[string][]any{
		`UPDATE decision_thread_answer_consumptions SET call_key='other' WHERE answer_id=?`:                                              {a.ID},
		`DELETE FROM decision_thread_answer_consumptions WHERE answer_id=?`:                                                              {a.ID},
		`INSERT OR REPLACE INTO decision_thread_answer_consumptions SELECT * FROM decision_thread_answer_consumptions WHERE answer_id=?`: {a.ID},
		`UPDATE turns SET delivery_id=? WHERE id=?`:                                                                                      {other.ID, claim.TurnID},
	} {
		if _, err := s.db.Exec(stmt, args...); err == nil {
			t.Fatalf("acknowledgement identity was mutable: %s", stmt)
		}
	}
	messageSequencePreserved(t, s, before)
	dispatchAssertFK(t, s)
}

func TestThreadDecisionAcknowledgementDeniesAskingAndOrdinaryTurns(t *testing.T) {
	isolateDecisionFixtureRegistry(t)
	f := chatSetup(t, "user", false)
	s := f.s
	installDecisionAnswerFixture(t, s)
	installThreadDecisionAnswerFixture(t, s)
	s.EnableThreadDecisionExecution()
	q := askThreadDecision(t, f, "ask")
	a := saveThreadDecisionForTest(t, s, q)
	queueEvent(t, s, f.scope.Claim, provider.AcceptedEvent, "", "")
	denyThreadDecisionAck(t, s, f.scope.Claim, a.ID, "asker", ErrMemberToolDenied)
	if _, err := s.ApplyTurnReceipt(ctx, f.scope.Claim, queueReceipt(provider.OutcomeSucceeded)); err != nil {
		t.Fatal(err)
	}
	d := queueDelivery(t, s, q.GroupID, f.scope.MemberID, q.TopicID)
	turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: q.GroupID, DeliveryID: d.ID, MaxActive: 1})
	if err != nil || turn.ConversationID != f.scope.ConversationID {
		t.Fatalf("expected the next ordinary Turn in the same conversation: %+v %v", turn, err)
	}
	claim := TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	queueEvent(t, s, claim, provider.AcceptedEvent, "", "")
	before := messageSequenceSnapshot(t, s)
	denyThreadDecisionAck(t, s, claim, a.ID, "next-turn", ErrMemberToolDenied)
	assertDecisionAnswerView(t, s, a, nil)
	messageSequencePreserved(t, s, before)
}

func TestThreadDecisionAcknowledgementFencesStaleOrSettlingTurn(t *testing.T) {
	for _, test := range []string{"withdrawn", "expired", "newer-source", "departed-rejoined", "canceled", "terminal-event", "recovered", "post-lock", "post-insert"} {
		t.Run(test, func(t *testing.T) {
			f, claim, a := acceptedThreadDecisionFixture(t)
			s, want := f.s, ErrMemberToolDenied
			switch test {
			case "withdrawn":
				coordinationDecideExec(t, s, `UPDATE decision_requests SET withdrawn_at=created_at WHERE id=?`, a.RequestID)
			case "expired":
				coordinationDecideExec(t, s, `DROP TRIGGER decision_request_versions_no_update`)
				coordinationDecideExec(t, s, `UPDATE decision_request_versions SET expires_at=? WHERE request_id=?`, a.CreatedAt.Format(timestampLayout), a.RequestID)
			case "newer-source":
				chatLocalMessage(t, s, MessageRequest{GroupID: a.GroupID, TopicID: a.TopicID, ActorID: "user", Content: "新的决定。"}, nil)
			case "departed-rejoined":
				coordinationDecideExec(t, s, `UPDATE members SET status='left' WHERE id=?`, f.scope.MemberID)
				coordinationDecideExec(t, s, `UPDATE members SET status='active' WHERE id=?`, f.scope.MemberID)
			case "canceled":
				if _, err := s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID); err != nil {
					t.Fatal(err)
				}
			case "terminal-event":
				queueEvent(t, s, claim, provider.TerminalEvent, "", provider.OutcomeSucceeded)
			case "recovered":
				if n, err := s.RecoverInFlight(ctx); err != nil || n != 1 {
					t.Fatalf("recover accepted Turn: %d %v", n, err)
				}
				want = ErrStaleClaim
			case "post-lock":
				coordinationDecideExec(t, s, `CREATE TRIGGER fixture_cancel_during_ack_lock AFTER UPDATE OF claim_token ON turns
				 BEGIN UPDATE turns SET cancel_requested_at=accepted_at WHERE id=NEW.id; END`)
			case "post-insert":
				coordinationDecideExec(t, s, `CREATE TRIGGER fixture_withdraw_during_ack AFTER INSERT ON decision_thread_answer_consumptions
				 BEGIN UPDATE decision_requests SET withdrawn_at=created_at WHERE id=(SELECT request_id FROM decision_thread_answers WHERE id=NEW.answer_id); END`)
			}
			before := messageSequenceSnapshot(t, s)
			denyThreadDecisionAck(t, s, claim, a.ID, "fenced", want)
			assertDecisionAnswerView(t, s, a, nil)
			messageSequencePreserved(t, s, before)
		})
	}
}

func TestThreadDecisionAcknowledgementTransactionUncertainty(t *testing.T) {
	f, claim, a := acceptedThreadDecisionFixture(t)
	var outcome interface {
		CommitOutcomeUnknown() bool
		RollbackConfirmed() bool
	}
	const key = "original-key"
	lost := fmt.Errorf("lost commit response: %w", ErrConflict)
	gateway, observed := coordinationDecideWrapped(t, f.path, "ack-error", lost)
	gateway.EnableThreadDecisionExecution()
	got, err := gateway.AcknowledgeThreadDecision(ctx, claim, a.ID, key)
	if got != (domain.DecisionAnswerView{}) || !errors.Is(err, ErrTaskWorkMutationUnknown) || !errors.Is(err, lost) ||
		!errors.As(err, &outcome) || !outcome.CommitOutcomeUnknown() || outcome.RollbackConfirmed() || len(observed.begins) != 1 {
		t.Fatalf("lost commit reported a determined outcome or retried: %+v %v", got, err)
	}
	want, err := f.s.GetDecisionAnswerView(ctx, a.ThreadRef, a.RequestID, a.Version)
	if err != nil || want.Handling == nil || want.Handling.TurnID != claim.TurnID {
		t.Fatalf("unknown commit must reconcile through history: %+v %v", want, err)
	}
	before := messageSequenceSnapshot(t, f.s)
	if got, err := f.s.AcknowledgeThreadDecision(ctx, claim, a.ID, key); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("original call key lost the durable receipt: %+v %v", got, err)
	}
	rollbackLost := errors.New("lost rollback response")
	gateway, boundary := assignmentBoundaryStore(t, f.path, rollbackLost)
	gateway.EnableThreadDecisionExecution()
	got, err = gateway.AcknowledgeThreadDecision(ctx, claim, a.ID, "different-key")
	if got != (domain.DecisionAnswerView{}) || !errors.Is(err, ErrTaskWorkMutationUnknown) || !errors.Is(err, ErrConflict) ||
		!errors.Is(err, rollbackLost) || !errors.As(err, &outcome) || outcome.CommitOutcomeUnknown() || outcome.RollbackConfirmed() || len(boundary.events) != 1 {
		t.Fatalf("unconfirmed rollback must precede the key conflict: %+v %v", got, err)
	}
	messageSequencePreserved(t, f.s, before)
}

func TestThreadDecisionAcknowledgementOptionalSchema(t *testing.T) {
	t.Run("public-v30", func(t *testing.T) {
		f := taskWorkChatAtPrefix(t, 30)
		f.s.EnableThreadDecisionExecution()
		queueEvent(t, f.s, f.scope.Claim, provider.AcceptedEvent, "", "")
		denyThreadDecisionAck(t, f.s, f.scope.Claim, threadDecisionAnswerPrefix+"not-installed", "ack", ErrMemberToolDenied)
		if page, err := f.s.ListPendingDeliveries(ctx, PendingOptions{Lane: PendingThreadDecisions}); err != nil || len(page.Items) != 0 {
			t.Fatalf("v30 exposed a typed lane: %+v %v", page, err)
		}
		if view, err := f.s.GetDecisionAnswerView(ctx, f.candidate.ThreadRef, "unavailable", 1); view != (domain.DecisionAnswerView{}) || !errors.Is(err, ErrDecisionAnswersUnavailable) {
			t.Fatalf("v30 read shape changed: %+v %v", view, err)
		}
	})
	t.Run("saved-answer-without-ledger", func(t *testing.T) {
		f, claim, a := acceptedThreadDecisionFixture(t)
		coordinationDecideExec(t, f.s, `DROP TABLE decision_thread_answer_consumptions`)
		denyThreadDecisionAck(t, f.s, claim, a.ID, "ack", ErrMemberToolDenied)
		assertDecisionAnswerView(t, f.s, a, nil)
	})
}
