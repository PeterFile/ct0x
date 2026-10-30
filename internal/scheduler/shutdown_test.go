package scheduler_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func releaseGate(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return gate, release
}

func assertDraining(t *testing.T, r *loopRun) {
	t.Helper()
	select {
	case <-r.done:
		t.Fatalf("Run returned while execution/receipt persistence was still active: %v", r.err)
	case <-time.After(20 * time.Millisecond):
	}
}

func resourcePrepare(f *loopFixture, e provider.Executor) scheduler.PrepareFunc {
	return func(ctx context.Context, p store.PendingDelivery) (scheduler.PreparedExecution, error) {
		prepared, err := f.prepare(e)(ctx, p)
		prepared.ResourceKeys = []string{"resource:" + p.Delivery.RecipientID}
		return prepared, err
	}
}

func TestRunObservesDurableCancelWithoutReleasingBeforeReceipt(t *testing.T) {
	f := newLoopFixture(t)
	m := f.queue(t, "user", f.sender, "")
	started, cancelled := make(chan provider.ExecuteRequest, 1), make(chan struct{}, 1)
	gate, release := releaseGate(t)
	e := executorFunc(func(ctx context.Context, request provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
		started <- request
		<-ctx.Done()
		cancelled <- struct{}{}
		<-gate
		return provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeUnknown, DirectChildReaped: true}
	})
	r := startLoop(t, f, f.options(1), resourcePrepare(f, e))
	t.Cleanup(release) // Release the controlled child before startLoop cleanup on failure.
	request := receive(t, started)
	if _, err := f.s.RequestTurnCancel(context.Background(), f.group, request.RunID); err != nil {
		t.Fatal(err)
	}
	receive(t, cancelled)
	if f.turnFor(t, m).State != domain.TurnStateCancelRequested || f.count(t, "resource_claims") != 1 {
		t.Fatal("durable cancellation released claims without a receipt")
	}
	assertDraining(t, r)
	release()
	eventually(t, func() bool { return f.turnFor(t, m).State == domain.TurnStateUnknown })
	if f.count(t, "resource_claims") != 1 {
		t.Fatal("unknown cancellation receipt released resources")
	}
}

func TestRunCancellationDrainsEveryChildAndWaitsForReceiptPersistence(t *testing.T) {
	f := newLoopFixture(t)
	a, b := f.member(t), f.member(t)
	messages := []domain.Message{f.queue(t, "user", a, ""), f.queue(t, "user", b, "")}
	firstGate, releaseFirst := releaseGate(t)
	secondGate, releaseSecond := releaseGate(t)
	started := make(chan provider.ExecuteRequest, 2)
	callbacks := make(chan error, 2)
	returning := make(chan string, 2)
	e := executorFunc(func(ctx context.Context, request provider.ExecuteRequest, callback func(provider.Event) error) provider.ExecutionReceipt {
		var member string
		_ = f.db.QueryRow("SELECT member_id FROM turns WHERE id=?", request.RunID).Scan(&member)
		started <- request
		<-ctx.Done()
		// Shutdown callbacks still persist observed facts with bounded independent contexts.
		err := callback(provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "native-session"})
		callbacks <- err
		if member == a {
			<-firstGate
		} else {
			<-secondGate
		}
		returning <- request.RunID
		return provider.ExecutionReceipt{Started: true, Accepted: true, NativeSessionID: "native-session", Outcome: provider.OutcomeUnknown}
	})
	r := startLoop(t, f, f.options(2), resourcePrepare(f, e))
	t.Cleanup(releaseFirst)
	t.Cleanup(releaseSecond)
	receive(t, started)
	receive(t, started)
	r.cancel()
	for range 2 {
		if err := receive(t, callbacks); err != nil {
			t.Fatalf("cancelled Run prevented event persistence: %v", err)
		}
	}
	if _, err := f.s.CreateMessage(context.Background(), store.MessageRequest{GroupID: f.group, ActorID: "user", Content: "usable during drain"}, []string{}); err != nil {
		t.Fatalf("DB unavailable during drain: %v", err)
	}
	assertDraining(t, r)
	releaseFirst()
	receive(t, returning)
	eventually(t, func() bool { return f.turnFor(t, messages[0]).State == domain.TurnStateUnknown })
	assertDraining(t, r)

	// Hold a real SQLite writer lock after Execute returns to isolate receipt draining.
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE groups SET name=name WHERE id=?", f.group); err != nil {
		t.Fatal(err)
	}
	releaseSecond()
	receive(t, returning)
	assertDraining(t, r)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	receive(t, r.done)
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("context cancellation not preserved after drain: %v", r.err)
	}
	for _, m := range messages {
		if turn := f.turnFor(t, m); turn.State != domain.TurnStateUnknown || turn.AcceptedAt == nil {
			t.Fatalf("shutdown failed to persist final evidence: %+v", turn)
		}
	}
	if f.count(t, "resource_claims") != 2 {
		t.Fatal("drain released unknown resources")
	}
}

func TestRunFatalPreparationCancelsAndDrainsExistingExecution(t *testing.T) {
	f := newLoopFixture(t)
	first := f.queue(t, "user", f.sender, "")
	second := f.queue(t, "user", f.member(t), "")
	started, cancelled := make(chan struct{}), make(chan struct{}, 1)
	gate, release := releaseGate(t)
	e := executorFunc(func(ctx context.Context, _ provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
		close(started)
		<-ctx.Done()
		cancelled <- struct{}{}
		<-gate
		return provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeUnknown}
	})
	prepare := func(ctx context.Context, p store.PendingDelivery) (scheduler.PreparedExecution, error) {
		if p.Delivery.MessageID == second.ID {
			<-started
			return scheduler.PreparedExecution{}, errors.New("PRIVATE-CONFIG-PROMPT")
		}
		return resourcePrepare(f, e)(ctx, p)
	}
	r := startLoop(t, f, f.options(2), prepare)
	t.Cleanup(release)
	receive(t, cancelled)
	assertDraining(t, r)
	if f.count(t, "turns") != 1 || f.count(t, "resource_claims") != 1 {
		t.Fatal("fatal preparation claimed additional work or released a live child")
	}
	release()
	receive(t, r.done)
	if r.err == nil || strings.Contains(r.err.Error(), "PRIVATE") || f.turnFor(t, first).State != domain.TurnStateUnknown {
		t.Fatalf("preparation error lost, leaked, or prevented cleanup: %v", r.err)
	}
}

func TestRunCallbackFailurePropagatesAndPersistsUnknownWithoutReleasing(t *testing.T) {
	f := newLoopFixture(t)
	m := f.queue(t, "user", f.sender, "")
	f.exec(t, `CREATE TRIGGER fail_accept BEFORE UPDATE ON deliveries WHEN NEW.state='accepted'
	 BEGIN SELECT RAISE(ABORT,'PRIVATE-CALLBACK-SQL'); END`)
	failed := make(chan error, 1)
	gate, release := releaseGate(t)
	e := executorFunc(func(ctx context.Context, _ provider.ExecuteRequest, callback func(provider.Event) error) provider.ExecutionReceipt {
		if err := callback(provider.Event{Kind: provider.SessionEvent, NativeSessionID: "native-session"}); err != nil {
			failed <- err
			return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, DeliveryFailed: true}
		}
		err := callback(provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "native-session"})
		failed <- err
		<-gate
		r := successReceipt()
		r.Outcome, r.DeliveryFailed = provider.OutcomeUnknown, true
		return r
	})
	r := startLoop(t, f, f.options(1), resourcePrepare(f, e))
	t.Cleanup(release)
	if err := receive(t, failed); err == nil || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("callback error swallowed or exposed: %v", err)
	}
	if f.turnFor(t, m).AcceptedAt != nil {
		t.Fatal("failed acceptance partially persisted")
	}
	f.exec(t, "DROP TRIGGER fail_accept")
	release()
	receive(t, r.done)
	if r.err == nil || strings.Contains(r.err.Error(), "PRIVATE") {
		t.Fatalf("material callback persistence error was lost or leaked: %v", r.err)
	}
	turn := f.turnFor(t, m)
	if turn.State != domain.TurnStateUnknown || turn.AcceptedAt == nil || turn.ReplyMessageID != "" || f.count(t, "resource_claims") != 1 {
		t.Fatalf("failed callback was treated as successful settlement: %+v", turn)
	}
}

func TestRunReceiptFailureReportsSafeErrorAndLeavesDurableClaimIntact(t *testing.T) {
	f := newLoopFixture(t)
	m := f.queue(t, "user", f.sender, "")
	f.exec(t, `CREATE TRIGGER fail_receipt BEFORE UPDATE ON turns WHEN NEW.receipt_json<>''
	 BEGIN SELECT RAISE(ABORT,'PRIVATE-RECEIPT-SQL'); END`)
	e := executorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
		return successReceipt()
	})
	r := startLoop(t, f, f.options(1), resourcePrepare(f, e))
	receive(t, r.done)
	turn := f.turnFor(t, m)
	if r.err == nil || strings.Contains(r.err.Error(), "PRIVATE") || turn.State != domain.TurnStateStarting || turn.AcceptedAt != nil || f.count(t, "resource_claims") != 1 {
		t.Fatalf("failed receipt was hidden or partially settled: %+v, %v", turn, r.err)
	}
	f.exec(t, "DROP TRIGGER fail_receipt")
	if recovered, err := f.s.RecoverInFlight(context.Background()); err != nil || recovered != 1 {
		t.Fatalf("failed receipt did not leave a recoverable durable claim: %d, %v", recovered, err)
	}
}

func TestRunStoreFailureDrainsAndPreservesPersistenceErrorWithCancellation(t *testing.T) {
	f := newLoopFixture(t)
	m := f.queue(t, "user", f.sender, "")
	started, cancelled := make(chan struct{}, 1), make(chan struct{}, 1)
	gate, release := releaseGate(t)
	e := executorFunc(func(ctx context.Context, _ provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
		started <- struct{}{}
		<-ctx.Done()
		cancelled <- struct{}{}
		<-gate
		return provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeUnknown}
	})
	r := startLoop(t, f, f.options(1), resourcePrepare(f, e))
	t.Cleanup(release)
	receive(t, started)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	receive(t, cancelled)
	assertDraining(t, r)
	r.cancel()
	release()
	receive(t, r.done)
	if r.err == nil || !errors.Is(r.err, context.Canceled) || r.err.Error() == context.Canceled.Error() || strings.Contains(r.err.Error(), "sql:") {
		t.Fatalf("store/persistence error was lost, leaked, or replaced by cancellation: %v", r.err)
	}
	var err error
	f.s, err = store.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if turn := f.turnFor(t, m); turn.State != domain.TurnStateStarting || f.count(t, "resource_claims") != 1 {
		t.Fatalf("failed cleanup released unresolved execution: %+v", turn)
	}
}

func TestRunPreservesDeadlineCancellation(t *testing.T) {
	f := newLoopFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := scheduler.Run(ctx, f.s, f.options(1), f.prepare(nil))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline lost: %v", err)
	}
}
