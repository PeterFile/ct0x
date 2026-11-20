package coordination

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

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
	mu    sync.Mutex
	items []error
	limit int
}

func (r *pumpSchedulerErrors) record(err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Preserve entire joined trees, including mixed cancellation and SQL errors.
	limit := r.limit
	if limit == 0 {
		limit = 8
	}
	if len(r.items) == limit {
		panic(fmt.Sprintf("bounded intent error recorder overflow: limit=%d next-full-tree=%v", limit, err))
	}
	r.items = append(r.items, err)
}

func (r *pumpSchedulerErrors) checked(t *testing.T) []error {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
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

// This test-only seam exposes Stats, never an operable DB or configurable pool.
// A Store layout/type change must fail rather than inspect any other memory.
func pumpSchedulerStoreStats(t *testing.T, s *store.Store) func() sql.DBStats {
	t.Helper()
	if s == nil {
		t.Fatal("Store Stats requires a nonnil Store")
	}
	field := reflect.ValueOf(s).Elem().FieldByName("db")
	if !field.IsValid() || field.Type() != reflect.TypeOf((*sql.DB)(nil)) || !field.CanAddr() || field.IsNil() {
		t.Fatal("Store Stats requires the fixed addressable nonnil db field of exact type *sql.DB")
	}
	db := *(**sql.DB)(unsafe.Pointer(field.UnsafeAddr()))
	return db.Stats
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
	t.Logf("materialization BEGIN evidence: phase=%s cause=%v elapsed=%s known-no-transaction=%t commit-unknown=%t parent=%v",
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

// This pre-Materialize oracle accepts only the local ctx.Err -> errors.Join
// shape, not deadline wrappers in general.
func pumpSchedulerOnlyDeadline(err error) bool {
	if err == context.DeadlineExceeded {
		return true
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok || reflect.TypeOf(err) != reflect.TypeOf(errors.Join(context.DeadlineExceeded)) {
		return false
	}
	children := joined.Unwrap()
	if len(children) == 0 {
		return false
	}
	for _, child := range children {
		if !pumpSchedulerOnlyDeadline(child) {
			return false
		}
	}
	return true
}

// This unapproved node is only a fixture for the pre-Materialize oracle.
type pumpSchedulerOracleMulti []error

func (e pumpSchedulerOracleMulti) Error() string {
	return "unapproved oracle multi-error"
}

func (e pumpSchedulerOracleMulti) Unwrap() []error {
	return e
}

func TestIntentPumpSchedulerPredeadlineOracle(t *testing.T) {
	deadline := context.DeadlineExceeded
	diskIO := errors.New("disk I/O error")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"raw", deadline, true},
		{"joined", errors.Join(deadline, deadline), true},
		{"nested", errors.Join(errors.Join(deadline, errors.Join(deadline)), deadline), true},
		{"nil", nil, false},
		{"empty-join", errors.Join(nil, nil), false},
		{"disk-io", diskIO, false},
		{"canceled", context.Canceled, false},
		{"deadline-text-not-sentinel", errors.New(deadline.Error()), false},
		{"mixed-disk-io", errors.Join(deadline, diskIO), false},
		{"reviewer-nested-mixed", errors.Join(errors.Join(deadline, diskIO), deadline), false},
		{"wrapped-deadline", fmt.Errorf("wrapped: %w", deadline), false},
		{"joined-wrapped-deadline", errors.Join(deadline, fmt.Errorf("wrapped: %w", errors.Join(deadline, deadline))), false},
		{"mixed-canceled", errors.Join(deadline, context.Canceled), false},
		{"transaction-error", intentTestEvidence{cause: deadline, phase: "begin", rollback: true}, false},
		{"candidate-error", fmt.Errorf("物化分派请求 original（世代 1）失败：%w", deadline), false},
		{"query-error", fmt.Errorf("扫描到期分派请求失败：%w", deadline), false},
		{"reviewer-nested-multi-query", errors.Join(errors.Join(deadline, fmt.Errorf("扫描到期分派请求失败：%w / %w", deadline, deadline)), deadline), false},
		{"nested-multi-wrapper", errors.Join(deadline, fmt.Errorf("unapproved multi wrapper: %w / %w", errors.Join(deadline, deadline), fmt.Errorf("nested multi wrapper: %w / %w", deadline, deadline))), false},
		{"fake-multi-nil-child", pumpSchedulerOracleMulti{deadline, nil}, false},
		{"fake-multi-all-deadline", pumpSchedulerOracleMulti{deadline, deadline}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pumpSchedulerOnlyDeadline(tc.err); got != tc.want {
				t.Fatalf("predeadline leaf oracle=%t, want %t: %v", got, tc.want, tc.err)
			}
		})
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
	items := append(append([]store.DispatchDueCandidate(nil), prefix.Items...), tail.Items...)
	positions := make(map[string]int, len(items))
	for i, item := range items {
		positions[item.RequestID] = i
	}
	// Capacity bounds diagnostics, not throughput. A whole-exercise watchdog
	// also bounds no-progress segments; every allowed tree is retained.
	maxSegments := 2*len(items) + 8
	exercise, finish := context.WithTimeout(context.Background(), 2*time.Minute)
	defer finish()
	before := engineCoordinatorSnapshot(t, f.db)
	p := intentCore(t, f.s, at)
	recorder := pumpSchedulerErrors{limit: maxSegments}
	type segment struct {
		pumpSchedulerReport
		sweep      int
		before     int
		boundaries []int
		empty      int
		induced    bool
		held       *sql.Tx
		heldIndex  int
		setupErr   error
	}
	reports := make(chan segment, 1)
	entered := make(chan context.Context, 1)
	proceed, advance, tailPreparing := make(chan struct{}), make(chan struct{}), make(chan struct{})
	forced := make(map[int]bool, 7)
	options := pumpSchedulerOptions(t, f, 1)
	sweep, segments := 0, 0
	preexpiryGateUsed := false
	options.IntentPass = func(ctx context.Context) {
		select {
		case <-tailPreparing:
			<-ctx.Done()
			return
		default:
		}
		entered <- ctx
		pumpSchedulerWait(ctx, proceed)
		if ctx.Err() != nil {
			return
		}
		segments++
		if segments > maxSegments {
			reports <- segment{setupErr: fmt.Errorf("segment diagnostic bound exceeded: %d", maxSegments)}
			<-ctx.Done()
			return
		}
		reset := p.cutoff.IsZero() || p.dispatchExhausted && p.assignmentExhausted
		if reset {
			sweep++
		}
		current := segment{sweep: sweep, before: -1, heldIndex: -1}
		if !reset && p.dispatchAfter != nil {
			current.before = positions[p.dispatchAfter.RequestID]
		}
		dispatchEmpty, assignmentEmpty := p.dispatchExhausted, p.assignmentExhausted
		if reset {
			dispatchEmpty, assignmentEmpty = false, false
		}
		cutoff := at.Add(time.Duration(sweep-1) * time.Second)
		needCutoff := reset
		p.now = func() time.Time {
			if needCutoff {
				needCutoff = false
				return cutoff
			}
			i := 0
			if p.dispatchAfter != nil {
				i = positions[p.dispatchAfter.RequestID] + 1
			}
			if i >= len(items) {
				current.setupErr = fmt.Errorf("unexpected candidate boundary after original cursor: %+v", p.dispatchAfter)
				return cutoff
			}
			current.boundaries = append(current.boundaries, i)
			if sweep == 1 && len(forced) < 7 && !forced[i] && current.held == nil {
				// The due query is closed. A separate real writer now prevents
				// this candidate's BEGIN until after the production pass returns.
				writer, err := f.db.BeginTx(exercise, nil)
				if err != nil {
					current.setupErr = err
					return cutoff
				}
				t.Cleanup(func() { writer.Rollback() })
				current.held, current.heldIndex = writer, i
				_, current.setupErr = writer.ExecContext(exercise, "UPDATE groups SET name=name WHERE id=?", f.thread.GroupID)
				if current.setupErr == nil && !preexpiryGateUsed {
					preexpiryGateUsed, current.induced = true, true
					// Deliberately park with the writer beyond run's fixed 1s
					// child. This is test scheduling, not an SQL latency bound.
					hold := 2 * time.Second
					t.Logf("intentional pre-Materialize gate: candidate=%s original-key=%+v sweep=%d cutoff=%s hold=%s", items[i].RequestID, items[i], sweep, cutoff, hold)
					gate := time.NewTimer(hold)
					select {
					case <-gate.C:
					case <-exercise.Done():
						current.setupErr = fmt.Errorf("pre-Materialize gate watchdog: %w", exercise.Err())
					}
					gate.Stop()
				}
			}
			return cutoff
		}
		current.pumpSchedulerReport = pumpSchedulerPass(p, &recorder, ctx)
		if !dispatchEmpty && p.dispatchExhausted {
			current.empty++
		}
		if !assignmentEmpty && p.assignmentExhausted {
			current.empty++
		}
		reports <- current
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
	var ordinaryCall, lastCall provider.ExecuteRequest
	var view domain.DispatchView
	var expectedErrors []error
	lastBySweep := make(map[int]int)
	cutoffBySweep := make(map[int]time.Time)
	seenBySweep := make(map[int]map[int]bool)
	firstSweepSegments, sqlFaults, materializeDeadlines, noAdvanceDeadlines := 0, 0, 0, 0
	preexpiry := struct {
		count, index, sweep int
		cutoff              time.Time
		uncounted, retried  bool
	}{index: -1}
	for lastCall.RunID == "" {
		select {
		case call := <-started:
			if ordinaryCall.RunID == "" {
				ordinaryCall = call
			} else {
				lastCall = call
			}
			continue
		case <-entered:
			if len(expectedErrors) > 0 && ordinaryCall.RunID == "" {
				ordinaryCall = pumpSchedulerReceive(t, started)
			}
			if ordinaryCall.RunID != "" {
				// Finish ordinary receipt persistence before injecting another
				// external writer. The scheduler is gated before this pass.
				poll := time.NewTicker(time.Millisecond)
				for {
					turn, err := f.s.GetTurn(exercise, f.thread.GroupID, ordinaryCall.RunID)
					if err != nil {
						poll.Stop()
						t.Fatal(err)
					}
					if turn.State == domain.TurnStateSucceeded {
						break
					}
					select {
					case <-poll.C:
					case <-exercise.Done():
						poll.Stop()
						t.Fatalf("ordinary receipt did not settle: turn=%+v errors=%v", turn, expectedErrors)
					}
				}
				poll.Stop()
			}
		case <-exercise.Done():
			t.Fatalf("prefix exercise watchdog: cursors=%v forced=%v errors=%v", lastBySweep, forced, expectedErrors)
		case <-r.done:
			t.Fatalf("scheduler stopped before item51: run=%v cursors=%v errors=%v", r.err, lastBySweep, expectedErrors)
		}
		pumpSchedulerAdvance(t, r, proceed)
		report := pumpSchedulerReceive(t, reports)
		if report.setupErr != nil {
			t.Fatalf("controlled prefix setup failed: %v report=%+v", report.setupErr, report)
		}
		if report.err != nil {
			expectedErrors = append(expectedErrors, report.err)
		}
		if report.ctx.Err() != nil {
			t.Fatalf("budget cancelled the parent: %v", report.err)
		}
		if _, exists := lastBySweep[report.sweep]; !exists {
			lastBySweep[report.sweep] = -1
			cutoffBySweep[report.sweep] = report.cutoff
			seenBySweep[report.sweep] = make(map[int]bool, len(items))
		}
		if report.before != lastBySweep[report.sweep] || report.cutoff != cutoffBySweep[report.sweep] {
			t.Fatalf("sweep rewound or widened cutoff: last=%v report=%+v", lastBySweep, report)
		}
		after := -1
		if report.after != nil {
			var exists bool
			after, exists = positions[report.after.RequestID]
			if !exists || *report.after != (store.DispatchDueCursor{RequestID: items[after].RequestID, NextAttemptAt: items[after].NextAttemptAt}) {
				t.Fatalf("not an exact original candidate key: %+v", report.after)
			}
		}
		byCandidate := make(map[int]error)
		queryFailures := 0
		var observations []error
		if joined, ok := report.err.(interface{ Unwrap() []error }); ok {
			observations = joined.Unwrap()
		} else if report.err != nil {
			observations = []error{report.err}
		}
		for _, observed := range observations {
			candidate := -1
			for i, item := range items {
				if strings.HasPrefix(observed.Error(), fmt.Sprintf("物化分派请求 %s（世代 1）失败：", item.RequestID)) {
					candidate = i
					break
				}
			}
			if candidate < 0 {
				if !errors.Is(observed, context.DeadlineExceeded) {
					t.Fatalf("unexpected non-candidate error tree: %v", observed)
				}
				if strings.HasPrefix(observed.Error(), "扫描到期") {
					queryFailures++
				}
				continue // The full joined tree remains in recorder and stdout.
			}
			if byCandidate[candidate] != nil || candidate <= report.before || candidate > after {
				t.Fatalf("error identity does not match original cursor progression: candidate=%s report=%+v tree=%v", items[candidate].RequestID, report, observed)
			}
			byCandidate[candidate] = observed
			var evidence interface {
				Phase() string
				RollbackConfirmed() bool
				CommitOutcomeUnknown() bool
				Unwrap() error
			}
			if !errors.As(observed, &evidence) {
				t.Fatalf("candidate error lost transaction evidence: %v", observed)
			}
			if strings.Contains(evidence.Unwrap().Error(), "persistent scheduler intent fault") {
				if evidence.Phase() != "receipt" || evidence.CommitOutcomeUnknown() {
					t.Fatalf("trigger error has wrong transaction phase: %v", observed)
				}
				sqlFaults++
			} else if errors.Is(observed, context.DeadlineExceeded) {
				materializeDeadlines++
			} else {
				t.Fatalf("unexpected candidate SQL/transaction tree: %v", observed)
			}
		}
		charged := len(report.boundaries) + report.empty + queryFailures
		if charged > 50 || queryFailures > 1 || after < report.before || after-report.before > len(report.boundaries) || len(report.boundaries)-(after-report.before) > 1 {
			t.Fatalf("invalid <=50 observation accounting: charged=%d after=%d report=%+v", charged, after, report)
		}
		for n, candidate := range report.boundaries {
			if candidate != report.before+1+n {
				t.Fatalf("due boundary rescanned or skipped an original key: %+v", report)
			}
		}
		for i := report.before + 1; i <= after; i++ {
			if seenBySweep[report.sweep][i] || i < len(prefix.Items) && byCandidate[i] == nil {
				t.Fatalf("advanced prefix key was rescanned or lost its error identity: item=%s report=%+v", items[i].RequestID, report)
			}
			seenBySweep[report.sweep][i] = true
		}
		if after == report.before && errors.Is(report.err, context.DeadlineExceeded) {
			noAdvanceDeadlines++
		}
		if report.sweep == 1 {
			firstSweepSegments++
		}
		if report.held != nil {
			forcedBefore := len(forced)
			if after == report.before {
				// The writer uses the independent exercise context. Holding it
				// does not prove the child crossed the Materialize ctx fence.
				var transaction interface{ Phase() string }
				if !pumpSchedulerOnlyDeadline(report.err) || len(byCandidate) != 0 || errors.As(report.err, &transaction) ||
					queryFailures != 0 || len(report.boundaries) != 1 || report.boundaries[0] != report.heldIndex || report.heldIndex != report.before+1 {
					t.Fatalf("held pre-Materialize expiry lost its unchanged cursor or no-candidate tree: %+v", report)
				}
				if forced[report.heldIndex] {
					t.Fatalf("unadvanced pre-Materialize expiry counted as a forced BEGIN: %+v forced=%v", report, forced)
				}
				if report.induced {
					preexpiry.count++
					if preexpiry.count != 1 {
						t.Fatal("pre-Materialize gate was induced more than once")
					}
					preexpiry.index, preexpiry.sweep, preexpiry.cutoff = report.heldIndex, report.sweep, report.cutoff
					preexpiry.uncounted = !forced[report.heldIndex]
				}
				t.Logf("held pre-Materialize deadline: induced=%t original-key=%+v unchanged-cursor=%+v sweep=%d cutoff=%s forced=%d/%d parent=%v full-tree=%v", report.induced, items[report.heldIndex], report.after, report.sweep, report.cutoff, forcedBefore, len(forced), report.ctx.Err(), report.err)
			} else {
				if report.induced {
					t.Fatalf("intentional pre-Materialize gate unexpectedly entered Materialize: %+v", report)
				}
				// Only real typed BEGIN evidence and exact key advancement may
				// count; expiry alone proves no transaction/resource release.
				pumpSchedulerBeginEvidence(t, report.pumpSchedulerReport)
				if after != report.heldIndex || after != report.before+1 || byCandidate[after] == nil ||
					strings.Contains(byCandidate[after].Error(), "persistent scheduler intent fault") {
					t.Fatalf("held BEGIN did not advance exactly its original identity without trigger: %+v", report)
				}
				forced[after] = true
				if after == preexpiry.index {
					if preexpiry.count != 1 || !preexpiry.uncounted || preexpiry.retried || report.sweep != preexpiry.sweep || report.cutoff != preexpiry.cutoff {
						t.Fatalf("induced pre-Materialize key was not retried in the same sweep/cutoff: proof=%+v report=%+v", preexpiry, report)
					}
					preexpiry.retried = true
					t.Logf("induced pre-Materialize same-key BEGIN retry: original-key=%+v cursor=%+v sweep=%d cutoff=%s forced=%d full-tree=%v", items[after], report.after, report.sweep, report.cutoff, len(forced), report.err)
				}
			}
			// Both dispositions retain the real writer until the pass returns.
			if err := report.held.Rollback(); err != nil {
				t.Fatal(err)
			}
		}
		lastBySweep[report.sweep] = after
		t.Logf("real segment=%d sweep=%d cutoff=%s before=%d cursor=%+v charged=%d boundaries=%v empty=%d full-tree=%v", segments, report.sweep, report.cutoff, report.before, report.after, charged, report.boundaries, report.empty, report.err)
		view, err = f.s.GetDispatchRequest(exercise, f.thread.GroupID, tail.Items[0].RequestID)
		if err != nil {
			t.Fatal(err)
		}
		pumpSchedulerAdvance(t, r, advance)
	}
	if preexpiry.count != 1 || !preexpiry.uncounted || !preexpiry.retried || !forced[preexpiry.index] {
		t.Fatalf("missing induced uncounted pre-Materialize expiry and same-original-key BEGIN retry: proof=%+v forced=%v", preexpiry, forced)
	}
	if firstSweepSegments <= 6 || len(forced) != 7 || materializeDeadlines < 7 || len(seenBySweep[1]) < len(prefix.Items) ||
		view.State.Status != domain.DispatchStatusEnqueued || view.State.Delivery == nil || view.State.Delivery.MaterializedFromGeneration != 1 || view.State.Generation != 2 || view.State.AttemptCount != 0 {
		t.Fatalf("missing >6 real segments, distinct no-trigger deadlines, or original-generation item51 receipt: segments=%d forced=%v seen=%v view=%+v", firstSweepSegments, forced, seenBySweep[1], view)
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
	after := engineCoordinatorSnapshot(t, f.db)
	for _, table := range []string{"messages", "message_log", "deliveries"} {
		if len(after[table]) != len(before[table])+1 {
			t.Fatalf("prefix failure partially persisted %s: before=%d after=%d", table, len(before[table]), len(after[table]))
		}
	}
	t.Logf("real scheduler progression: first-sweep-segments=%d forced-original-identities=%v SQL-faults=%d materialize-deadlines=%d no-advance-deadlines=%d item51=%s receipt=%s ordinary-turn=%s tail-turn=%s induced-predeadline=%+v", firstSweepSegments, forced, sqlFaults, materializeDeadlines, noAdvanceDeadlines, tail.Items[0].RequestID, view.State.Delivery.DeliveryID, ordinaryTurn.ID, lastTurn.ID, preexpiry)
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
	// Let the next intent scan finish and park the scheduler at its callback gate.
	if report := pumpSchedulerReceive(t, reports); report.err != nil {
		t.Fatalf("post-start intent pass failed: %v", report.err)
	}
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
	for _, callbackFirst := range []bool{false, true} {
		name := "pass-first"
		if callbackFirst {
			name = "callback-first-rejected"
		}
		t.Run(name, func(t *testing.T) {
			f := intentConfigured(t)
			stats := pumpSchedulerStoreStats(t, f.s)
			ordinary := pumpSchedulerQueue(t, f, f.worker)
			p, err := newIntentPass(New(f.s))
			if err != nil {
				t.Fatal(err)
			}
			var recorder pumpSchedulerErrors
			reports := make(chan pumpSchedulerReport, 8)
			secondEntered, ready, materialize, advance := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			beforeMaterialize := make(chan sql.DBStats, 1)
			passReturned := make(chan struct{})
			options := pumpSchedulerOptions(t, f, 1)
			cycle := 0
			options.IntentPass = func(ctx context.Context) {
				cycle++
				if cycle == 2 {
					close(secondEntered)
					pumpSchedulerWait(ctx, ready)
				}
				report := pumpSchedulerPass(p, &recorder, ctx)
				if cycle == 2 {
					close(passReturned)
				}
				reports <- report
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
			callbacks := make(chan callbackObservation, 1)
			e := pumpSchedulerExecutor(func(ctx context.Context, request provider.ExecuteRequest, callback func(provider.Event) error) provider.ExecutionReceipt {
				executionContexts <- ctx
				started <- request
				pumpSchedulerWait(ctx, emit)
				at := time.Now()
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
					// ListDue has returned and closed its rows. No same-Store user
					// can run here except the deliberately gated executor and pass.
					beforeMaterialize <- stats()
					pumpSchedulerWait(initial.ctx, materialize)
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
			idle := pumpSchedulerReceive(t, beforeMaterialize)
			if idle.MaxOpenConnections != 1 || idle.OpenConnections != 1 || idle.InUse != 0 || idle.Idle != 1 {
				t.Fatalf("due query did not return the single connection before Materialize: %+v", idle)
			}
			awaitStats := func(label string, accept func(sql.DBStats) bool) sql.DBStats {
				t.Helper()
				poll := time.NewTicker(time.Millisecond)
				defer poll.Stop()
				guard := time.NewTimer(pumpSchedulerWatchdog)
				defer guard.Stop()
				for {
					select {
					case <-passReturned:
						t.Fatalf("pass returned before physical %s: stats=%+v", label, stats())
					default:
					}
					current := stats()
					if accept(current) {
						select {
						case <-passReturned:
							t.Fatalf("pass returned during physical %s: stats=%+v", label, current)
						default:
							return current
						}
					}
					select {
					case <-poll.C:
					case <-guard.C:
						t.Fatalf("physical %s was not observed: stats=%+v", label, current)
					case <-r.done:
						t.Fatalf("scheduler stopped before physical %s: %v", label, r.err)
					}
				}
			}
			passOwns := func(snapshot sql.DBStats) bool {
				select {
				case <-materialize:
				default:
					return false
				}
				select {
				case <-emit:
					return false // Callback was enabled before attribution could be established.
				case <-passReturned:
					return false
				default:
				}
				return snapshot.MaxOpenConnections == 1 && snapshot.InUse == 1
			}
			if callbackFirst {
				// The pass is still physically gated before Materialize. An InUse=1
				// transition can only be the callback, not a pre-call timestamp.
				releaseExecutor()
				owner := awaitStats("callback-first ownership", func(s sql.DBStats) bool { return s.InUse == 1 })
				if passOwns(owner) || owner.WaitCount != idle.WaitCount {
					t.Fatalf("callback-owned connection was falsely attributed to the gated pass: idle=%+v owner=%+v", idle, owner)
				}
				close(materialize)
				waiting := awaitStats("pass pool wait behind callback", func(s sql.DBStats) bool { return s.InUse == 1 && s.WaitCount > owner.WaitCount })
				if passOwns(waiting) {
					t.Fatal("callback-first ordering was accepted by the positive handshake")
				}
				t.Logf("negative ordering rejected: candidate=%s idle=%+v callback-owner=%+v pass-wait=%+v", request.ID, idle, owner, waiting)
			} else {
				close(materialize)
				owner := awaitStats("pass-only ownership", func(s sql.DBStats) bool { return s.InUse == 1 })
				if !passOwns(owner) {
					t.Fatalf("pass ownership was not attributable before enabling callback: %+v", owner)
				}
				releaseExecutor()
				waiting := awaitStats("callback pool wait behind pass", func(s sql.DBStats) bool { return s.InUse == 1 && s.WaitCount > owner.WaitCount })
				t.Logf("positive physical handshake: candidate=%s idle=%+v pass-owner=%+v callback-wait=%+v", request.ID, idle, owner, waiting)
			}
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
			if observation.err != nil || err != nil || turn.State != domain.TurnStateRunning || turn.AcceptedAt == nil ||
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
		})
	}
}
