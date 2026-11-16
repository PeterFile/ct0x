package coordination

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

// This is a hang guard for real SQLite contention, not a pass deadline.
const pumpSchedulerWatchdog = 30 * time.Second

type pumpSchedulerExecutor func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt

func (f pumpSchedulerExecutor) Execute(ctx context.Context, request provider.ExecuteRequest, callback func(provider.Event) error) provider.ExecutionReceipt {
	return f(ctx, request, callback)
}

type pumpSchedulerErrors struct {
	mu       sync.Mutex
	items    []error
	overflow int
}

func (r *pumpSchedulerErrors) record(err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Preserve entire joined trees, including mixed cancellation and SQL errors.
	if len(r.items) < 8 {
		r.items = append(r.items, err)
	} else {
		r.overflow++
	}
}

func (r *pumpSchedulerErrors) checked(t *testing.T) []error {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.overflow != 0 {
		t.Fatalf("bounded error recorder overflowed: %d", r.overflow)
	}
	return append([]error(nil), r.items...)
}

type pumpSchedulerReport struct {
	ctx       context.Context
	err       error
	elapsed   time.Duration
	cutoff    time.Time
	after     *store.DispatchDueCursor
	exhausted bool
}

func pumpSchedulerPass(p *intentPass, recorder *pumpSchedulerErrors, ctx context.Context) pumpSchedulerReport {
	started := time.Now()
	err := p.run(ctx)
	recorder.record(err)
	report := pumpSchedulerReport{ctx: ctx, err: err, elapsed: time.Since(started), cutoff: p.cutoff,
		exhausted: p.dispatchExhausted && p.assignmentExhausted}
	if p.dispatchAfter != nil {
		cursor := *p.dispatchAfter
		report.after = &cursor
	}
	return report
}

func pumpSchedulerReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(pumpSchedulerWatchdog):
		t.Fatal("scheduler composition did not reach the observation gate")
		var zero T
		return zero
	}
}

func pumpSchedulerWait(ctx context.Context, gate <-chan struct{}) {
	select {
	case <-gate:
	case <-ctx.Done():
	}
}

type pumpSchedulerRun struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func pumpSchedulerStart(t *testing.T, f intentFixture, options scheduler.Options, prepare scheduler.PrepareFunc) *pumpSchedulerRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &pumpSchedulerRun{cancel: cancel, done: make(chan struct{})}
	go func() {
		r.err = scheduler.Run(ctx, f.s, options, prepare)
		close(r.done)
	}()
	t.Cleanup(func() {
		cancel()
		pumpSchedulerReceive(t, r.done)
	})
	return r
}

func pumpSchedulerOptions(t *testing.T, f intentFixture, capacity int) scheduler.Options {
	t.Helper()
	runtime := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(runtime, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RecoverInFlight(context.Background()); err != nil {
		t.Fatal(err)
	}
	return scheduler.Options{Recovered: true, MaxActive: capacity, PollInterval: 20 * time.Millisecond, RuntimeDir: runtime}
}

func pumpSchedulerPrepare(e provider.Executor) scheduler.PrepareFunc {
	return func(_ context.Context, pending store.PendingDelivery) (scheduler.PreparedExecution, error) {
		return scheduler.PreparedExecution{Executor: e, Request: provider.ExecuteRequest{Prompt: "trusted test input"},
			ResourceKeys: []string{"pump-scheduler:" + pending.Delivery.RecipientID}}, nil
	}
}

func pumpSchedulerQueue(t *testing.T, f intentFixture, recipient string) domain.Message {
	t.Helper()
	m, err := f.s.CreateMessage(context.Background(), store.MessageRequest{
		GroupID: f.thread.GroupID, TopicID: f.thread.TopicID, ActorID: "user", Content: "普通消息",
	}, []string{recipient})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func pumpSchedulerCount(t *testing.T, f intentFixture, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func pumpSchedulerSuccess() provider.ExecutionReceipt {
	return provider.ExecutionReceipt{Started: true, Accepted: true, Outcome: provider.OutcomeSucceeded,
		Terminal: provider.OutcomeSucceeded, NativeSessionID: "pump-native", DirectChildReaped: true, OutputComplete: true}
}

func pumpSchedulerBeginEvidence(t *testing.T, report pumpSchedulerReport) {
	t.Helper()
	var evidence interface {
		Phase() string
		RollbackConfirmed() bool
		CommitOutcomeUnknown() bool
		Unwrap() error
	}
	if !errors.As(report.err, &evidence) {
		t.Fatalf("missing real transaction evidence, elapsed=%s err=%v", report.elapsed, report.err)
	}
	// Inspect the actual transaction cause, without discarding a mixed outer join.
	code, classified := store.ClassifyDispatchTransient(evidence.Unwrap())
	if !errors.As(report.err, &evidence) || evidence.Phase() != "begin" || evidence.CommitOutcomeUnknown() ||
		!classified || code != store.DispatchTransientSQLiteBusy {
		t.Fatalf("expected real BEGIN BUSY evidence, elapsed=%s err=%v", report.elapsed, report.err)
	}
	t.Logf("real SQLite contention: phase=%s cause=%v elapsed=%s known-no-transaction=%t commit-unknown=%t",
		evidence.Phase(), report.err, report.elapsed, evidence.RollbackConfirmed(), evidence.CommitOutcomeUnknown())
}

func TestIntentPumpSchedulerMaterializesEmptyQueueAtCapacity(t *testing.T) {
	f := intentConfigured(t)
	first := f.request(t)
	if n := pumpSchedulerCount(t, f, "SELECT count(*) FROM deliveries"); n != 0 {
		t.Fatalf("intent fixture already had physical deliveries: %d", n)
	}
	p, err := newIntentPass(New(f.s))
	if err != nil {
		t.Fatal(err)
	}
	var recorder pumpSchedulerErrors
	reports := make(chan pumpSchedulerReport, 3)
	secondEntered, secondGate, thirdEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	options := pumpSchedulerOptions(t, f, 1)
	cycle := 0
	options.IntentPass = func(ctx context.Context) {
		cycle++
		if cycle == 2 {
			close(secondEntered)
			pumpSchedulerWait(ctx, secondGate)
		}
		if cycle == 3 {
			close(thirdEntered)
			<-ctx.Done()
		}
		reports <- pumpSchedulerPass(p, &recorder, ctx)
	}
	started := make(chan provider.ExecuteRequest, 2)
	callbacks := make(chan error, 1)
	e := pumpSchedulerExecutor(func(ctx context.Context, request provider.ExecuteRequest, callback func(provider.Event) error) provider.ExecutionReceipt {
		started <- request
		<-ctx.Done()
		callbacks <- callback(provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "pump-native"})
		return provider.ExecutionReceipt{Started: true, Accepted: true, NativeSessionID: "pump-native", Outcome: provider.OutcomeUnknown}
	})
	prepareContexts := make(chan context.Context, 2)
	r := pumpSchedulerStart(t, f, options, func(ctx context.Context, pending store.PendingDelivery) (scheduler.PreparedExecution, error) {
		prepareContexts <- ctx
		return pumpSchedulerPrepare(e)(ctx, pending)
	})
	initial := pumpSchedulerReceive(t, reports)
	call := pumpSchedulerReceive(t, started)
	if initial.err != nil || pumpSchedulerReceive(t, prepareContexts) != initial.ctx {
		t.Fatalf("initial core pass failed or used a different run context: %v", initial.err)
	}
	view, err := f.s.GetDispatchRequest(context.Background(), first.GroupID, first.ID)
	turn, turnErr := f.s.GetTurn(context.Background(), first.GroupID, call.RunID)
	if err != nil || turnErr != nil || view.State.Delivery == nil || turn.DeliveryID != view.State.Delivery.DeliveryID {
		t.Fatalf("empty queue was not materialized before scheduling: %+v %+v %v %v", view, turn, err, turnErr)
	}
	pumpSchedulerReceive(t, secondEntered)
	second := f.request(t)
	close(secondGate)
	if report := pumpSchedulerReceive(t, reports); report.err != nil {
		t.Fatal(report.err)
	}
	pumpSchedulerReceive(t, thirdEntered) // The full-capacity schedule has returned to another cycle.
	secondView, err := f.s.GetDispatchRequest(context.Background(), second.GroupID, second.ID)
	if err != nil || secondView.State.Delivery == nil || pumpSchedulerCount(t, f, "SELECT count(*) FROM turns") != 1 ||
		pumpSchedulerCount(t, f, "SELECT count(*) FROM deliveries WHERE state='queued' AND turn_id=''") != 1 {
		t.Fatalf("M=1 blocked the hook or allowed a second physical claim: %+v %v", secondView, err)
	}
	select {
	case extra := <-started:
		t.Fatalf("second active execution escaped capacity: %+v", extra)
	default:
	}
	r.cancel()
	pumpSchedulerReceive(t, r.done)
	if err := pumpSchedulerReceive(t, callbacks); err != nil {
		t.Fatalf("shutdown callback lost its independent write context: %v", err)
	}
	turn, err = f.s.GetTurn(context.Background(), first.GroupID, call.RunID)
	if err != nil || turn.State != domain.TurnStateUnknown || turn.AcceptedAt == nil || turn.ClaimToken == "" ||
		pumpSchedulerCount(t, f, "SELECT count(*) FROM resource_claims") != 1 || !errors.Is(r.err, context.Canceled) {
		t.Fatalf("unknown drain lost facts or released occupancy: %+v err=%v run=%v", turn, err, r.err)
	}
	for _, err := range recorder.checked(t) {
		if !errors.Is(err, context.Canceled) || err.Error() != context.Canceled.Error() {
			t.Fatalf("unexpected recorded core error: %v", err)
		}
	}
}

func TestIntentPumpSchedulerPersistentFailurePrefixReaches51(t *testing.T) {
	f := intentConfigured(t)
	for range 51 {
		f.request(t)
	}
	ordinary := pumpSchedulerQueue(t, f, f.worker)
	at := time.Now().UTC().Add(time.Second)
	prefix, err := f.s.ListDueDispatchRequests(context.Background(), at, store.DispatchDueOptions{})
	if err != nil || len(prefix.Items) != 50 || prefix.NextCursor == nil {
		t.Fatalf("prefix setup: %+v %v", prefix, err)
	}
	tail, err := f.s.ListDueDispatchRequests(context.Background(), at, store.DispatchDueOptions{After: prefix.NextCursor})
	if err != nil || len(tail.Items) != 1 {
		t.Fatalf("tail setup: %+v %v", tail, err)
	}
	intentExec(t, f.db, "CREATE TABLE pump_bad_prefix(id TEXT PRIMARY KEY)")
	for _, item := range prefix.Items {
		intentExec(t, f.db, "INSERT INTO pump_bad_prefix VALUES(?)", item.RequestID)
	}
	intentExec(t, f.db, `CREATE TRIGGER pump_prefix_fault BEFORE UPDATE ON coordination_dispatch_state
	 WHEN EXISTS(SELECT 1 FROM pump_bad_prefix WHERE id=NEW.request_id)
	 BEGIN SELECT RAISE(ABORT,'persistent scheduler intent fault'); END`)
	p := intentCore(t, f.s, at)
	var recorder pumpSchedulerErrors
	reports := make(chan pumpSchedulerReport, 3)
	secondEntered, secondGate, thirdEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	options := pumpSchedulerOptions(t, f, 1)
	options.PollInterval = time.Hour // Only the real finished event can start the next cycle.
	cycle := 0
	options.IntentPass = func(ctx context.Context) {
		cycle++
		if cycle == 2 {
			close(secondEntered)
			pumpSchedulerWait(ctx, secondGate)
		}
		if cycle == 3 {
			close(thirdEntered)
			<-ctx.Done()
		}
		reports <- pumpSchedulerPass(p, &recorder, ctx)
	}
	started := make(chan provider.ExecuteRequest, 2)
	e := pumpSchedulerExecutor(func(_ context.Context, request provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
		started <- request
		return pumpSchedulerSuccess()
	})
	r := pumpSchedulerStart(t, f, options, pumpSchedulerPrepare(e))
	firstReport := pumpSchedulerReceive(t, reports)
	ordinaryCall := pumpSchedulerReceive(t, started)
	pumpSchedulerReceive(t, secondEntered)
	ordinaryTurn, err := f.s.GetTurn(context.Background(), f.thread.GroupID, ordinaryCall.RunID)
	if err != nil || ordinaryTurn.DeliveryID != ordinary.Deliveries[0].ID || ordinaryTurn.State != domain.TurnStateSucceeded ||
		firstReport.err == nil || strings.Count(firstReport.err.Error(), "persistent scheduler intent fault") != 49 ||
		firstReport.after == nil || firstReport.after.RequestID != prefix.Items[48].RequestID {
		t.Fatalf("core errors blocked ordinary scheduling or lost the bounded prefix: turn=%+v err=%v pass=%+v", ordinaryTurn, err, firstReport)
	}
	close(secondGate)
	secondReport := pumpSchedulerReceive(t, reports)
	lastCall := pumpSchedulerReceive(t, started)
	pumpSchedulerReceive(t, thirdEntered)
	view, err := f.s.GetDispatchRequest(context.Background(), f.thread.GroupID, tail.Items[0].RequestID)
	lastTurn, turnErr := f.s.GetTurn(context.Background(), f.thread.GroupID, lastCall.RunID)
	if err != nil || turnErr != nil || view.State.Delivery == nil || view.State.Delivery.MaterializedFromGeneration != 1 ||
		lastTurn.DeliveryID != view.State.Delivery.DeliveryID || lastTurn.State != domain.TurnStateSucceeded ||
		secondReport.err == nil || strings.Count(secondReport.err.Error(), "persistent scheduler intent fault") != 1 ||
		secondReport.cutoff != firstReport.cutoff || secondReport.after == nil || secondReport.after.RequestID != tail.Items[0].RequestID {
		t.Fatalf("scheduler did not resume the same core through item 51: view=%+v turn=%+v errors=%v/%v report=%+v", view, lastTurn, err, turnErr, secondReport)
	}
	rows := engineLocalRows(t, f.db, `SELECT generation,status,attempt_count,last_error_code FROM coordination_dispatch_state
	 WHERE request_id IN (SELECT id FROM pump_bad_prefix)`)
	if len(rows) != 50 {
		t.Fatalf("lost failed prefix: %d", len(rows))
	}
	for _, row := range rows {
		if !reflect.DeepEqual(row, []any{int64(1), "pending", int64(0), ""}) {
			t.Fatalf("persistent failure invented defer or partial writes: %v", row)
		}
	}
	r.cancel()
	pumpSchedulerReceive(t, r.done)
	recorded := recorder.checked(t)
	if len(recorded) != 3 || recorded[0] != firstReport.err || recorded[1] != secondReport.err || !errors.Is(recorded[2], context.Canceled) ||
		!errors.Is(r.err, context.Canceled) {
		t.Fatalf("adapter lost a bounded error tree or made it scheduler-fatal: %v run=%v", recorded, r.err)
	}
	t.Logf("real scheduler completion cycles: first=49 failures, next=1 failure; item51=%s receipt=%s turn=%s", tail.Items[0].RequestID, view.State.Delivery.DeliveryID, lastTurn.ID)
}

func TestIntentPumpSchedulerCancellationPreservesMixedErrorsAndDrain(t *testing.T) {
	f := intentConfigured(t)
	ordinary := pumpSchedulerQueue(t, f, f.worker)
	p, err := newIntentPass(New(f.s))
	if err != nil {
		t.Fatal(err)
	}
	var recorder pumpSchedulerErrors
	reports := make(chan pumpSchedulerReport, 2)
	secondEntered, secondGate, receiptGate := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseReceipt sync.Once
	release := func() { releaseReceipt.Do(func() { close(receiptGate) }) }
	options := pumpSchedulerOptions(t, f, 2)
	cycle := 0
	options.IntentPass = func(ctx context.Context) {
		cycle++
		if cycle == 2 {
			close(secondEntered)
			pumpSchedulerWait(ctx, secondGate)
		}
		reports <- pumpSchedulerPass(p, &recorder, ctx)
	}
	started := make(chan provider.ExecuteRequest, 2)
	callbacks := make(chan error, 2)
	e := pumpSchedulerExecutor(func(ctx context.Context, request provider.ExecuteRequest, callback func(provider.Event) error) provider.ExecutionReceipt {
		started <- request
		<-ctx.Done()
		callbacks <- callback(provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "pump-native"})
		<-receiptGate
		return provider.ExecutionReceipt{Started: true, Accepted: true, NativeSessionID: "pump-native", Outcome: provider.OutcomeUnknown}
	})
	r := pumpSchedulerStart(t, f, options, pumpSchedulerPrepare(e))
	t.Cleanup(release) // Unblock the executor before Run cleanup on a failed assertion.
	if report := pumpSchedulerReceive(t, reports); report.err != nil {
		t.Fatal(report.err)
	}
	call := pumpSchedulerReceive(t, started)
	pumpSchedulerReceive(t, secondEntered)
	first, second := f.request(t), f.request(t)
	pumpSchedulerQueue(t, f, f.coord) // Capacity exists, but the post-hook fence must prevent this claim.
	intentExec(t, f.db, `CREATE TABLE pump_cancel_bad(id TEXT PRIMARY KEY)`)
	intentExec(t, f.db, "INSERT INTO pump_cancel_bad VALUES(?)", first.ID)
	intentExec(t, f.db, `CREATE TRIGGER pump_cancel_fault BEFORE UPDATE ON coordination_dispatch_state
	 WHEN EXISTS(SELECT 1 FROM pump_cancel_bad WHERE id=NEW.request_id)
	 BEGIN SELECT RAISE(ABORT,'mixed scheduler intent fault'); END`)
	cancelledTurn, err := f.s.RequestTurnCancel(context.Background(), f.thread.GroupID, call.RunID)
	if err != nil || cancelledTurn.DeliveryID != ordinary.Deliveries[0].ID || cancelledTurn.State != domain.TurnStateCancelRequested {
		t.Fatalf("durable cancellation setup: %+v %v", cancelledTurn, err)
	}
	at := time.Now().Add(time.Second)
	clockCalls := 0
	p.now = func() time.Time {
		clockCalls++
		if clockCalls == 3 { // New cutoff, failed item, then cancellation before the next materialization.
			r.cancel()
		}
		return at
	}
	close(secondGate)
	report := pumpSchedulerReceive(t, reports)
	if !errors.Is(report.err, context.Canceled) || !strings.Contains(report.err.Error(), "mixed scheduler intent fault") {
		t.Fatalf("mixed join tree was swallowed or cancellation did extra work: %v", report.err)
	}
	if err := pumpSchedulerReceive(t, callbacks); err != nil {
		t.Fatalf("cancelled hook prevented independent event persistence: %v", err)
	}
	turn, err := f.s.GetTurn(context.Background(), f.thread.GroupID, call.RunID)
	if err != nil || turn.State != domain.TurnStateCancelRequested || turn.AcceptedAt == nil || turn.ClaimToken != cancelledTurn.ClaimToken ||
		pumpSchedulerCount(t, f, "SELECT count(*) FROM turns") != 1 || pumpSchedulerCount(t, f, "SELECT count(*) FROM resource_claims") != 1 {
		t.Fatalf("hook inferred settlement from cancellation: %+v %v", turn, err)
	}
	select {
	case <-r.done:
		t.Fatalf("Run returned before the real receipt drained: %v", r.err)
	default:
	}
	for _, request := range []domain.DispatchRequest{first, second} {
		view, err := f.s.GetDispatchRequest(context.Background(), request.GroupID, request.ID)
		if err != nil || view.State.Generation != 1 || view.State.AttemptCount != 0 || view.State.Delivery != nil || view.State.Status != domain.DispatchStatusPending {
			t.Fatalf("cancelled/faulted materialization wrote or deferred: %+v %v", view, err)
		}
	}
	release()
	pumpSchedulerReceive(t, r.done)
	turn, err = f.s.GetTurn(context.Background(), f.thread.GroupID, call.RunID)
	recorded := recorder.checked(t)
	if err != nil || turn.State != domain.TurnStateUnknown || turn.AcceptedAt == nil || turn.ClaimToken != cancelledTurn.ClaimToken ||
		pumpSchedulerCount(t, f, "SELECT count(*) FROM resource_claims") != 1 ||
		pumpSchedulerCount(t, f, "SELECT count(*) FROM turns WHERE receipt_json<>''") != 1 ||
		len(recorded) != 1 || recorded[0] != report.err || !errors.Is(r.err, context.Canceled) {
		t.Fatalf("drain lost receipt, mixed error, or unknown occupancy: %+v err=%v recorded=%v run=%v", turn, err, recorded, r.err)
	}
}

func TestIntentPumpSchedulerBeginBusyRecovery(t *testing.T) {
	f := intentConfigured(t)
	request := f.request(t) // Exactly one candidate means one BEGIN wait, not 50 waits.
	p, err := newIntentPass(New(f.s))
	if err != nil {
		t.Fatal(err)
	}
	var recorder pumpSchedulerErrors
	reports := make(chan pumpSchedulerReport, 3)
	unlocked := make(chan struct{})
	options := pumpSchedulerOptions(t, f, 1)
	cycle := 0
	options.IntentPass = func(ctx context.Context) {
		cycle++
		if cycle == 3 {
			<-ctx.Done()
		}
		reports <- pumpSchedulerPass(p, &recorder, ctx)
		if cycle == 1 {
			pumpSchedulerWait(ctx, unlocked)
		}
	}
	before := engineCoordinatorSnapshot(t, f.db)
	writer, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.Exec("UPDATE groups SET name=name WHERE id=?", f.thread.GroupID); err != nil {
		t.Fatal(err)
	}
	started := make(chan provider.ExecuteRequest, 1)
	e := pumpSchedulerExecutor(func(_ context.Context, request provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
		started <- request
		return pumpSchedulerSuccess()
	})
	r := pumpSchedulerStart(t, f, options, pumpSchedulerPrepare(e))
	blocked := pumpSchedulerReceive(t, reports)
	pumpSchedulerBeginEvidence(t, blocked)
	if blocked.after == nil || *blocked.after != (store.DispatchDueCursor{RequestID: request.ID, NextAttemptAt: request.CreatedAt}) ||
		!blocked.exhausted || !reflect.DeepEqual(before, engineCoordinatorSnapshot(t, f.db)) {
		t.Fatalf("BEGIN failure deferred, partially wrote, or lost original cursor: %+v", blocked)
	}
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	close(unlocked)
	recovered := pumpSchedulerReceive(t, reports)
	call := pumpSchedulerReceive(t, started)
	view, err := f.s.GetDispatchRequest(context.Background(), request.GroupID, request.ID)
	if recovered.err != nil || err != nil || view.State.Delivery == nil || view.State.Delivery.MaterializedFromGeneration != 1 ||
		view.State.AttemptCount != 0 || view.State.Generation != 2 {
		t.Fatalf("later scheduler cycle did not recover pending original generation: %+v pass=%v read=%v", view, recovered.err, err)
	}
	r.cancel()
	pumpSchedulerReceive(t, r.done)
	turn, err := f.s.GetTurn(context.Background(), request.GroupID, call.RunID)
	if err != nil || turn.State != domain.TurnStateSucceeded || !errors.Is(r.err, context.Canceled) {
		t.Fatalf("recovered work lost its independent receipt: %+v %v run=%v", turn, err, r.err)
	}
	recorded := recorder.checked(t)
	if len(recorded) < 1 || recorded[0] != blocked.err {
		t.Fatalf("BEGIN error was not visible: %v", recorded)
	}
	for _, err := range recorded[1:] {
		if err.Error() != context.Canceled.Error() {
			t.Fatalf("unexpected later core error: %v", err)
		}
	}
}

func TestIntentPumpSchedulerBeginBusyActiveReceiptDrain(t *testing.T) {
	f := intentConfigured(t)
	ordinary := pumpSchedulerQueue(t, f, f.worker)
	p, err := newIntentPass(New(f.s))
	if err != nil {
		t.Fatal(err)
	}
	var recorder pumpSchedulerErrors
	reports := make(chan pumpSchedulerReport, 3)
	secondEntered, ready, beginning, unlocked := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	options := pumpSchedulerOptions(t, f, 1)
	cycle := 0
	options.IntentPass = func(ctx context.Context) {
		cycle++
		if cycle == 2 {
			close(secondEntered)
			pumpSchedulerWait(ctx, ready)
		}
		reports <- pumpSchedulerPass(p, &recorder, ctx)
		if cycle == 2 {
			pumpSchedulerWait(ctx, unlocked)
		}
		if cycle == 3 {
			<-ctx.Done()
		}
	}
	type callbackObservation struct {
		err     error
		started time.Time
		elapsed time.Duration
	}
	started := make(chan provider.ExecuteRequest, 1)
	emit := make(chan struct{})
	var emitOnce sync.Once
	releaseExecutor := func() { emitOnce.Do(func() { close(emit) }) }
	callbacks := make(chan callbackObservation, 1)
	returning := make(chan time.Time, 1)
	e := pumpSchedulerExecutor(func(ctx context.Context, request provider.ExecuteRequest, callback func(provider.Event) error) provider.ExecutionReceipt {
		started <- request
		pumpSchedulerWait(ctx, emit)
		at := time.Now()
		err := callback(provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "pump-native"})
		callbacks <- callbackObservation{err: err, started: at, elapsed: time.Since(at)}
		returning <- time.Now()
		return provider.ExecutionReceipt{Started: true, Accepted: true, NativeSessionID: "pump-native", Outcome: provider.OutcomeUnknown, DeliveryFailed: err != nil}
	})
	r := pumpSchedulerStart(t, f, options, pumpSchedulerPrepare(e))
	t.Cleanup(releaseExecutor)
	if initial := pumpSchedulerReceive(t, reports); initial.err != nil {
		t.Fatal(initial.err)
	}
	call := pumpSchedulerReceive(t, started)
	pumpSchedulerReceive(t, secondEntered)
	request := f.request(t) // One due candidate beside the already active executor.
	at := time.Now().Add(time.Second)
	clockCalls := 0
	p.now = func() time.Time {
		clockCalls++
		if clockCalls == 2 {
			close(beginning)
		}
		return at
	}
	writer, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.Exec("UPDATE groups SET name=name WHERE id=?", f.thread.GroupID); err != nil {
		t.Fatal(err)
	}
	close(ready)
	pumpSchedulerReceive(t, beginning)
	// Arrange an actual callback during the default immediate-BEGIN wait.
	// The gate is not a product deadline or a promise of immediate SQL cancellation.
	time.Sleep(100 * time.Millisecond)
	releaseExecutor()
	blocked := pumpSchedulerReceive(t, reports)
	// Release the external writer once the synchronous pass yields, not early to
	// manufacture a successful callback/receipt. Keep every observed failure.
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	observation := pumpSchedulerReceive(t, callbacks)
	returnedAt := pumpSchedulerReceive(t, returning)
	pumpSchedulerBeginEvidence(t, blocked)
	if blocked.after == nil || blocked.after.RequestID != request.ID || blocked.after.NextAttemptAt != request.CreatedAt {
		t.Fatalf("contended pass lost its original candidate key: %+v", blocked)
	}
	view, err := f.s.GetDispatchRequest(context.Background(), request.GroupID, request.ID)
	if err != nil || view.State.Generation != 1 || view.State.AttemptCount != 0 || view.State.Delivery != nil {
		t.Fatalf("contended BEGIN invented defer or partial materialization: %+v %v", view, err)
	}
	close(unlocked)
	select {
	case <-r.done:
		// Record the exact inherited path before asking the owner to resolve it.
	case recovered := <-reports:
		if recovered.err != nil {
			t.Fatalf("later cycle failed after releasing writer: %v", recovered.err)
		}
		view, err = f.s.GetDispatchRequest(context.Background(), request.GroupID, request.ID)
		if err != nil || view.State.Delivery == nil {
			t.Fatalf("later scheduler cycle did not materialize: %+v %v", view, err)
		}
		r.cancel()
		pumpSchedulerReceive(t, r.done)
	case <-time.After(pumpSchedulerWatchdog):
		t.Fatal("unbuffered completion/receipt drain did not settle after writer release")
	}
	turn, err := f.s.GetTurn(context.Background(), f.thread.GroupID, call.RunID)
	receipts := pumpSchedulerCount(t, f, "SELECT count(*) FROM turns WHERE receipt_json<>''")
	claims := pumpSchedulerCount(t, f, "SELECT count(*) FROM resource_claims")
	recorded := recorder.checked(t)
	t.Logf("same-Store contention: pass=%s callback-start=%s callback=%s callback-error=%v executor-return=%s run=%v turn=%+v read=%v receipt-rows=%d resource-claims=%d core-errors=%v ordinary=%s",
		blocked.elapsed, observation.started.Format(time.RFC3339Nano), observation.elapsed, observation.err,
		returnedAt.Format(time.RFC3339Nano), r.err, turn, err, receipts, claims, recorded, ordinary.ID)
	if observation.err != nil || r.err == nil || r.err.Error() != context.Canceled.Error() {
		t.Fatalf("inherited event/receipt persistence failure prevents nonfatal hook recovery: callback=%v run=%v receipt-rows=%d claims=%d", observation.err, r.err, receipts, claims)
	}
	if err != nil || turn.State != domain.TurnStateUnknown || turn.ClaimToken == "" || turn.AcceptedAt == nil || receipts != 1 || claims != 1 {
		t.Fatalf("contention drain erased unknown execution evidence: %+v read=%v receipts=%d claims=%d", turn, err, receipts, claims)
	}
}
