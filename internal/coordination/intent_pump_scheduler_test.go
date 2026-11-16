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
	returned  time.Time
	cutoff    time.Time
	after     *store.DispatchDueCursor
	exhausted bool
}

func pumpSchedulerPass(p *intentPass, recorder *pumpSchedulerErrors, ctx context.Context) pumpSchedulerReport {
	started := time.Now()
	err := p.run(ctx)
	recorder.record(err)
	report := pumpSchedulerReport{ctx: ctx, err: err, elapsed: time.Since(started), returned: time.Now(), cutoff: p.cutoff,
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

func pumpSchedulerAdvance(t *testing.T, r *pumpSchedulerRun, gate chan<- struct{}) {
	t.Helper()
	select {
	case gate <- struct{}{}:
	case <-r.done:
		t.Fatalf("scheduler stopped before the next real cycle: %v", r.err)
	case <-time.After(pumpSchedulerWatchdog):
		t.Fatal("scheduler did not leave its observation gate")
	}
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
	// BEGIN observes the child's actual deadline, not the healthy run parent.
	// The Store's begin evidence means no transaction, not an observed rollback.
	if evidence.Phase() != "begin" || !evidence.RollbackConfirmed() || evidence.CommitOutcomeUnknown() ||
		!errors.Is(evidence.Unwrap(), context.DeadlineExceeded) || !errors.Is(report.err, context.DeadlineExceeded) || report.ctx.Err() != nil {
		t.Fatalf("expected real child-deadline BEGIN/no-transaction evidence, parent=%v elapsed=%s err=%v cause=%v",
			report.ctx.Err(), report.elapsed, report.err, evidence.Unwrap())
	}
	t.Logf("real SQLite contention: phase=%s cause=%v elapsed=%s known-no-transaction=%t commit-unknown=%t parent=%v",
		evidence.Phase(), evidence.Unwrap(), report.elapsed, evidence.RollbackConfirmed(), evidence.CommitOutcomeUnknown(), report.ctx.Err())
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
	reports := make(chan pumpSchedulerReport, 8)
	advance, tailPreparing := make(chan struct{}), make(chan struct{})
	options := pumpSchedulerOptions(t, f, 1)
	options.IntentPass = func(ctx context.Context) {
		select {
		case <-tailPreparing:
			<-ctx.Done() // Stop observation only after item 51 can actually launch.
			return
		default:
		}
		reports <- pumpSchedulerPass(p, &recorder, ctx)
		pumpSchedulerWait(ctx, advance)
	}
	started := make(chan provider.ExecuteRequest, 2)
	e := pumpSchedulerExecutor(func(_ context.Context, request provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
		started <- request
		return pumpSchedulerSuccess()
	})
	r := pumpSchedulerStart(t, f, options, func(ctx context.Context, pending store.PendingDelivery) (scheduler.PreparedExecution, error) {
		if pending.Delivery.ID != ordinary.Deliveries[0].ID {
			close(tailPreparing)
		}
		return pumpSchedulerPrepare(e)(ctx, pending)
	})
	var ordinaryCall provider.ExecuteRequest
	var view domain.DispatchView
	var expectedErrors []error
	var perCycle []int
	seen := make(map[string]int)
	total := 0
	for cycle := 0; cycle < 6; cycle++ {
		report := pumpSchedulerReceive(t, reports)
		text := ""
		if report.err != nil {
			expectedErrors = append(expectedErrors, report.err)
			text = report.err.Error()
		}
		count := strings.Count(text, "persistent scheduler intent fault")
		perCycle = append(perCycle, count)
		total += count
		if count > 50 || cycle == 0 && count > 49 || total > 50 || report.cutoff != at || report.ctx.Err() != nil ||
			report.err != nil && count == 0 && !errors.Is(report.err, context.DeadlineExceeded) {
			t.Fatalf("lost observation cap, cutoff, or error tree: counts=%v report=%+v", perCycle, report)
		}
		for _, item := range prefix.Items {
			seen[item.RequestID] += strings.Count(text, item.RequestID)
			if seen[item.RequestID] > 1 {
				t.Fatalf("rescanned failed prefix %s: counts=%v", item.RequestID, perCycle)
			}
		}
		view, err = f.s.GetDispatchRequest(context.Background(), f.thread.GroupID, tail.Items[0].RequestID)
		if err != nil {
			t.Fatal(err)
		}
		if view.State.Delivery != nil {
			if report.after == nil || *report.after != (store.DispatchDueCursor{RequestID: tail.Items[0].RequestID, NextAttemptAt: tail.Items[0].NextAttemptAt}) {
				t.Fatalf("item51 lost original cursor: %+v", report)
			}
			break
		}
		if total > 0 && (report.after == nil || *report.after != (store.DispatchDueCursor{RequestID: prefix.Items[total-1].RequestID, NextAttemptAt: prefix.Items[total-1].NextAttemptAt})) {
			t.Fatalf("failed prefix did not advance its exact original key: counts=%v report=%+v", perCycle, report)
		}
		pumpSchedulerAdvance(t, r, advance)
		if cycle == 0 {
			ordinaryCall = pumpSchedulerReceive(t, started)
		}
	}
	if total != 50 || view.State.Delivery == nil || view.State.Delivery.MaterializedFromGeneration != 1 || view.State.AttemptCount != 0 {
		t.Fatalf("real scheduler cycles did not persist item51 after exactly 50 failures: counts=%v view=%+v", perCycle, view)
	}
	for _, item := range prefix.Items {
		if seen[item.RequestID] != 1 {
			t.Fatalf("failed prefix identity missing: %s count=%d", item.RequestID, seen[item.RequestID])
		}
	}
	var lastCall provider.ExecuteRequest
	for cycle := 0; cycle < 4 && lastCall.RunID == ""; cycle++ {
		pumpSchedulerAdvance(t, r, advance)
		select {
		case lastCall = <-started:
		case report := <-reports:
			// A ticker may precede consuming the ordinary completion. Preserve
			// genuine later-sweep errors separately from the measured first 50.
			if report.err != nil {
				expectedErrors = append(expectedErrors, report.err)
				count := strings.Count(report.err.Error(), "persistent scheduler intent fault")
				if count > 50 || count == 0 && !errors.Is(report.err, context.DeadlineExceeded) {
					t.Fatalf("unexpected post-materialization tree: %v", report.err)
				}
				t.Logf("post-materialization real cycle: failures=%d cursor=%+v cutoff=%s err=%v", count, report.after, report.cutoff, report.err)
			}
			if report.ctx.Err() != nil || report.cutoff != at {
				t.Fatalf("later real cycle lost the parent/cutoff: %+v", report)
			}
		case <-r.done:
			t.Fatalf("scheduler stopped before tail launch: %v", r.err)
		case <-time.After(pumpSchedulerWatchdog):
			t.Fatal("real scheduler did not launch item51 after materializing it")
		}
	}
	if lastCall.RunID == "" {
		t.Fatal("bounded real scheduler cycles did not launch item51")
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
	ordinaryTurn, err := f.s.GetTurn(context.Background(), f.thread.GroupID, ordinaryCall.RunID)
	lastTurn, turnErr := f.s.GetTurn(context.Background(), f.thread.GroupID, lastCall.RunID)
	recorded := recorder.checked(t)
	if err != nil || turnErr != nil || ordinaryTurn.DeliveryID != ordinary.Deliveries[0].ID || ordinaryTurn.State != domain.TurnStateSucceeded ||
		lastTurn.DeliveryID != view.State.Delivery.DeliveryID || lastTurn.State != domain.TurnStateSucceeded ||
		!reflect.DeepEqual(recorded, expectedErrors) || r.err == nil || r.err.Error() != context.Canceled.Error() {
		t.Fatalf("core errors blocked ordinary/tail scheduling or lost bounded trees: ordinary=%+v last=%+v errors=%v/%v recorded=%v run=%v", ordinaryTurn, lastTurn, err, turnErr, recorded, r.err)
	}
	t.Logf("real scheduler cycles: failures=%v total=%d cutoff=%s item51=%s receipt=%s turn=%s", perCycle, total, at, tail.Items[0].RequestID, view.State.Delivery.DeliveryID, lastTurn.ID)
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
	request := f.request(t) // Exactly one candidate with the original Store defaults.
	p, err := newIntentPass(New(f.s))
	if err != nil {
		t.Fatal(err)
	}
	var recorder pumpSchedulerErrors
	reports := make(chan pumpSchedulerReport, 8)
	advance := make(chan struct{})
	options := pumpSchedulerOptions(t, f, 1)
	options.IntentPass = func(ctx context.Context) {
		reports <- pumpSchedulerPass(p, &recorder, ctx)
		pumpSchedulerWait(ctx, advance)
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
		!reflect.DeepEqual(before, engineCoordinatorSnapshot(t, f.db)) {
		t.Fatalf("BEGIN failure deferred, partially wrote, or lost original cursor: %+v", blocked)
	}
	// The writer remains held through pass return and all no-partial-state checks.
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	var view domain.DispatchView
	for cycle := 0; cycle < 6; cycle++ {
		pumpSchedulerAdvance(t, r, advance)
		recovered := pumpSchedulerReceive(t, reports)
		view, err = f.s.GetDispatchRequest(context.Background(), request.GroupID, request.ID)
		if recovered.err != nil || recovered.ctx != blocked.ctx || recovered.ctx.Err() != nil || err != nil {
			t.Fatalf("real recovery cycle failed: report=%+v read=%v", recovered, err)
		}
		if view.State.Delivery != nil {
			break
		}
		if recovered.cutoff != blocked.cutoff || !recovered.exhausted {
			t.Fatalf("old sweep lost cutoff or exhaustion probes: %+v", recovered)
		}
	}
	if view.State.Delivery == nil || view.State.Delivery.MaterializedFromGeneration != 1 || view.State.AttemptCount != 0 || view.State.Generation != 2 {
		t.Fatalf("bounded genuine scheduler cycles did not recover original generation: %+v", view)
	}
	pumpSchedulerAdvance(t, r, advance)
	call := pumpSchedulerReceive(t, started)
	r.cancel()
	pumpSchedulerReceive(t, r.done)
	turn, err := f.s.GetTurn(context.Background(), request.GroupID, call.RunID)
	if err != nil || turn.State != domain.TurnStateSucceeded || r.err == nil || r.err.Error() != context.Canceled.Error() {
		t.Fatalf("recovered work lost its independent receipt: %+v %v run=%v", turn, err, r.err)
	}
	recorded := recorder.checked(t)
	if len(recorded) != 1 || recorded[0] != blocked.err {
		t.Fatalf("BEGIN error was lost or another core error appeared: %v", recorded)
	}
	t.Logf("original key=%+v exhausted-at-budget=%t original-generation-receipt=%s turn=%s", blocked.after, blocked.exhausted, view.State.Delivery.DeliveryID, turn.ID)
}

func TestIntentPumpSchedulerBeginBusyActiveReceiptDrain(t *testing.T) {
	f := intentConfigured(t)
	ordinary := pumpSchedulerQueue(t, f, f.worker)
	p, err := newIntentPass(New(f.s))
	if err != nil {
		t.Fatal(err)
	}
	var recorder pumpSchedulerErrors
	reports := make(chan pumpSchedulerReport, 8)
	secondEntered, ready, beginning, advance := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	options := pumpSchedulerOptions(t, f, 1)
	cycle := 0
	options.IntentPass = func(ctx context.Context) {
		cycle++
		if cycle == 2 {
			close(secondEntered)
			pumpSchedulerWait(ctx, ready)
		}
		reports <- pumpSchedulerPass(p, &recorder, ctx)
		if cycle >= 2 {
			pumpSchedulerWait(ctx, advance)
		}
	}
	type callbackObservation struct {
		err     error
		started time.Time
		elapsed time.Duration
	}
	started := make(chan provider.ExecuteRequest, 1)
	executionContexts := make(chan context.Context, 1)
	emit, receiptGate := make(chan struct{}), make(chan struct{})
	var emitOnce, receiptOnce sync.Once
	releaseExecutor := func() { emitOnce.Do(func() { close(emit) }) }
	releaseReceipt := func() { receiptOnce.Do(func() { close(receiptGate) }) }
	callbackEntered := make(chan struct{})
	callbacks := make(chan callbackObservation, 1)
	e := pumpSchedulerExecutor(func(ctx context.Context, request provider.ExecuteRequest, callback func(provider.Event) error) provider.ExecutionReceipt {
		executionContexts <- ctx
		started <- request
		pumpSchedulerWait(ctx, emit)
		at := time.Now()
		close(callbackEntered)
		err := callback(provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "pump-native"})
		callbacks <- callbackObservation{err: err, started: at, elapsed: time.Since(at)}
		<-receiptGate // Acceptance must be inspected before receipt can backfill it.
		return provider.ExecutionReceipt{Started: true, Accepted: true, NativeSessionID: "pump-native", Outcome: provider.OutcomeUnknown, DeliveryFailed: err != nil}
	})
	r := pumpSchedulerStart(t, f, options, pumpSchedulerPrepare(e))
	t.Cleanup(releaseExecutor)
	t.Cleanup(releaseReceipt)
	initial := pumpSchedulerReceive(t, reports)
	if initial.err != nil {
		t.Fatal(initial.err)
	}
	call := pumpSchedulerReceive(t, started)
	executionCtx := pumpSchedulerReceive(t, executionContexts)
	pumpSchedulerReceive(t, secondEntered)
	originalTurn, err := f.s.GetTurn(context.Background(), f.thread.GroupID, call.RunID)
	if err != nil || originalTurn.DeliveryID != ordinary.Deliveries[0].ID || originalTurn.ClaimToken == "" {
		t.Fatalf("ordinary active claim missing: %+v %v", originalTurn, err)
	}
	request := f.request(t) // One due candidate beside the already active executor.
	before := engineCoordinatorSnapshot(t, f.db)
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
	// Observe a real OS wait, then enter the callback while BEGIN is contended.
	// This is not a cancellation-latency or hard SQL wall-time assertion.
	time.Sleep(100 * time.Millisecond)
	releaseExecutor()
	pumpSchedulerReceive(t, callbackEntered)
	blocked := pumpSchedulerReceive(t, reports)
	pumpSchedulerBeginEvidence(t, blocked)
	if blocked.ctx != initial.ctx || executionCtx.Err() != nil || blocked.after == nil ||
		*blocked.after != (store.DispatchDueCursor{RequestID: request.ID, NextAttemptAt: request.CreatedAt}) ||
		!reflect.DeepEqual(before, engineCoordinatorSnapshot(t, f.db)) {
		t.Fatalf("contended pass changed run/executor context, original key, or persistent state: %+v executor=%v", blocked, executionCtx.Err())
	}
	// Release only after the pass has returned while the writer was still held.
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	observation := pumpSchedulerReceive(t, callbacks)
	turn, err := f.s.GetTurn(context.Background(), f.thread.GroupID, call.RunID)
	if observation.err != nil || !observation.started.Before(blocked.returned) || err != nil || turn.State != domain.TurnStateRunning || turn.AcceptedAt == nil ||
		turn.NativeSessionID != "pump-native" || turn.ClaimToken != originalTurn.ClaimToken ||
		pumpSchedulerCount(t, f, "SELECT count(*) FROM turns WHERE receipt_json<>''") != 0 ||
		pumpSchedulerCount(t, f, "SELECT count(*) FROM resource_claims") != 1 || blocked.ctx.Err() != nil || executionCtx.Err() != nil {
		t.Fatalf("event failed or acceptance was not independently persisted before receipt: callback=%v turn=%+v read=%v parent=%v executor=%v", observation.err, turn, err, blocked.ctx.Err(), executionCtx.Err())
	}
	acceptedAt := *turn.AcceptedAt
	t.Logf("event-before-receipt: turn=%s acceptedAt=%s same-token=true receipt-rows=0 claims=1 parent=%v executor=%v", turn.ID, acceptedAt, blocked.ctx.Err(), executionCtx.Err())
	view, err := f.s.GetDispatchRequest(context.Background(), request.GroupID, request.ID)
	if err != nil || view.State.Generation != 1 || view.State.AttemptCount != 0 || view.State.Delivery != nil || view.State.Status != domain.DispatchStatusPending {
		t.Fatalf("contended BEGIN invented defer or partial materialization: %+v %v", view, err)
	}
	releaseReceipt()
	for recoveryCycle := 0; recoveryCycle < 6; recoveryCycle++ {
		pumpSchedulerAdvance(t, r, advance)
		recovered := pumpSchedulerReceive(t, reports)
		if recovered.err != nil || recovered.ctx != blocked.ctx || recovered.ctx.Err() != nil {
			t.Fatalf("later real scheduler cycle failed: %+v", recovered)
		}
		view, err = f.s.GetDispatchRequest(context.Background(), request.GroupID, request.ID)
		turn, turnErr := f.s.GetTurn(context.Background(), f.thread.GroupID, call.RunID)
		if err != nil || turnErr != nil {
			t.Fatalf("recovery evidence read failed: %v / %v", err, turnErr)
		}
		if view.State.Delivery != nil && turn.State == domain.TurnStateUnknown && pumpSchedulerCount(t, f, "SELECT count(*) FROM turns WHERE receipt_json<>''") == 1 {
			break
		}
	}
	if view.State.Delivery == nil || view.State.Delivery.MaterializedFromGeneration != 1 || view.State.Generation != 2 || view.State.AttemptCount != 0 {
		t.Fatalf("bounded genuine scheduler cycles did not materialize original generation: %+v", view)
	}
	turn, err = f.s.GetTurn(context.Background(), f.thread.GroupID, call.RunID)
	if blocked.ctx.Err() != nil || err != nil || turn.State != domain.TurnStateUnknown ||
		pumpSchedulerCount(t, f, "SELECT count(*) FROM turns WHERE receipt_json<>''") != 1 ||
		pumpSchedulerCount(t, f, "SELECT count(*) FROM resource_claims") != 1 {
		t.Fatalf("unknown receipt/occupancy did not settle under the healthy parent: parent=%v turn=%+v read=%v", blocked.ctx.Err(), turn, err)
	}
	r.cancel()
	pumpSchedulerReceive(t, r.done)
	turn, err = f.s.GetTurn(context.Background(), f.thread.GroupID, call.RunID)
	receipts := pumpSchedulerCount(t, f, "SELECT count(*) FROM turns WHERE receipt_json<>''")
	claims := pumpSchedulerCount(t, f, "SELECT count(*) FROM resource_claims")
	recorded := recorder.checked(t)
	t.Logf("same-Store contention: pass=%s callback-start=%s callback=%s callback-error=%v run=%v turn=%s read=%v receipt-rows=%d resource-claims=%d core-errors=%v original-generation-receipt=%s",
		blocked.elapsed, observation.started.Format(time.RFC3339Nano), observation.elapsed, observation.err,
		r.err, turn.ID, err, receipts, claims, recorded, view.State.Delivery.DeliveryID)
	if observation.err != nil || r.err == nil || r.err.Error() != context.Canceled.Error() || len(recorded) != 1 || recorded[0] != blocked.err {
		t.Fatalf("event/receipt failure or lost hook error: callback=%v run=%v recorded=%v receipts=%d claims=%d", observation.err, r.err, recorded, receipts, claims)
	}
	if err != nil || turn.State != domain.TurnStateUnknown || turn.ClaimToken != originalTurn.ClaimToken || turn.AcceptedAt == nil || !turn.AcceptedAt.Equal(acceptedAt) || receipts != 1 || claims != 1 {
		t.Fatalf("contention drain erased unknown execution evidence: %+v read=%v receipts=%d claims=%d", turn, err, receipts, claims)
	}
}
