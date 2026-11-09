package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"modernc.org/sqlite"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func assignmentPending(t *testing.T) (requestDispatchFixture, domain.DispatchRequest, domain.DecisionReceipt, CoordinationToolScope) {
	t.Helper()
	f, r, scope := coordinationDecideScope(t)
	return f, r, coordinationDecideAssign(t, f, scope, "assignment"), scope
}

// Every positive fixture uses the real admission/request/claim/decision path, with all guards enabled.
func assignmentNew(t *testing.T, s *Store, groupID, coordinator, worker, topic, objective, acceptance, description string) (domain.DispatchRequest, domain.DecisionReceipt, CoordinationToolScope) {
	t.Helper()
	msg, err := s.CreateMessage(ctx, MessageRequest{GroupID: groupID, TopicID: topic, ActorID: "user", Content: "授权来源"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := s.RequestWork(ctx, groupID, domain.RequestWorkRequest{TopicID: topic, MessageID: msg.ID, Mode: domain.RoutingDecisionCreateNew, Reason: "明确授权", NewTask: &domain.NewTaskRequest{Title: objective, Acceptance: acceptance, Description: description}}, "admit-"+msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.RequestDispatch(ctx, groupID, admitted.Task.ID, 1, 1, "保存调度", "request-"+msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	scope := coordinationDecideBindRequest(t, s, r)
	result, err := s.DecideDispatch(ctx, scope, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: worker, Reason: "保存决定"}, "decide-"+msg.ID)
	if err != nil {
		t.Fatalf("real decision (%s): %v", coordinator, err)
	}
	return r, result, scope
}

type assignmentQuery struct {
	query string
	args  []any
}
type assignmentRecorder struct {
	s         *Store
	queries   []assignmentQuery
	beforeCAS func(driver.Conn)
	casRows   []int64
}
type assignmentDriver struct {
	base     driver.Driver
	recorder *assignmentRecorder
}
type assignmentConn struct {
	driver.Conn
	recorder *assignmentRecorder
}

func (d *assignmentDriver) Open(name string) (driver.Conn, error) {
	c, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &assignmentConn{c, d.recorder}, nil
}
func (c *assignmentConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
}
func (c *assignmentConn) record(q string, args []driver.NamedValue) {
	values := make([]any, len(args))
	for i, a := range args {
		values[i] = a.Value
		if a.Name != "" {
			values[i] = sql.Named(a.Name, a.Value)
		}
	}
	c.recorder.queries = append(c.recorder.queries, assignmentQuery{q, values})
}
func (c *assignmentConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.record(q, args)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}
func (c *assignmentConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.record(q, args)
	cas := strings.Contains(q, "UPDATE coordination_assignment_dispatch") && strings.Contains(q, "materialized_from_generation")
	if cas && c.recorder.beforeCAS != nil {
		hook := c.recorder.beforeCAS
		c.recorder.beforeCAS = nil
		hook(c.Conn)
	}
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
	if cas && err == nil {
		n, e := result.RowsAffected()
		if e == nil {
			c.recorder.casRows = append(c.recorder.casRows, n)
		}
	}
	return result, err
}
func assignmentQueryStore(t *testing.T, path string) *assignmentRecorder {
	t.Helper()
	r := &assignmentRecorder{}
	name := fmt.Sprintf("assignment-recorder-%p", r)
	sql.Register(name, &assignmentDriver{&sqlite.Driver{}, r})
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"}
	db, err := sql.Open(name, dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	r.s = &Store{db: db}
	t.Cleanup(func() { r.s.Close() })
	return r
}

func assignmentBody(r domain.DispatchRequest, a domain.Assignment, objective, acceptance string) string {
	return fmt.Sprintf("平台根据已保存的协调决定生成以下工作分配，并非模型逐字发言。\nAssignment: %s\nDispatchRequest: %s\nTask: %s\nMandate: %s\n目标：\n%s\n验收条件：\n%s\n描述及原因见上述引用。", a.ID, r.ID, r.TaskID, r.MandateID, objective, acceptance)
}

func assignmentAssertReceipt(t *testing.T, s *Store, r domain.DispatchRequest, a domain.Assignment, got domain.AssignmentMaterialization, from int64, at time.Time) {
	t.Helper()
	if got.Disposition != "enqueued" || got.Generation != from+1 || got.HeldReason != "" || got.Delivery == nil {
		t.Fatalf("not an atomic successful materialization: %+v", got)
	}
	d := got.Delivery
	if d.MessageID == "" || d.DeliveryID == "" || d.ConversationID == "" || d.WorkerMemberID != a.WorkerMemberID || d.MaterializedFromGeneration != from || d.CreatedAt != at.UTC() {
		t.Fatalf("inexact receipt: %+v", d)
	}
	view, err := s.GetAssignment(ctx, r.GroupID, a.ID)
	if err != nil || !reflect.DeepEqual(view.Dispatch.Delivery, d) || view.Dispatch.Status != domain.AssignmentDispatchStatusEnqueued || view.Dispatch.Generation != from+1 || view.Dispatch.UpdatedAt != at.UTC() {
		t.Fatalf("uncommitted receipt: %+v, %v", view, err)
	}
	m, err := s.GetMessage(ctx, r.GroupID, d.MessageID)
	if err != nil || m.GroupID != r.GroupID || m.TopicID != r.TopicID || m.SenderID != r.CoordinatorMemberID || len(m.Deliveries) != 1 {
		t.Fatalf("not exact singleton author/group/topic: %+v, %v", m, err)
	}
	delivery := m.Deliveries[0]
	if delivery.ID != d.DeliveryID || delivery.MessageID != d.MessageID || delivery.RecipientID != a.WorkerMemberID || delivery.ConversationID != d.ConversationID {
		t.Fatalf("receipt substituted delivery: %+v", delivery)
	}
}

func TestMaterializeAssignmentConcurrentReceiptReplayAndReopen(t *testing.T) {
	f, r, decision, _ := assignmentPending(t)
	a := *decision.Assignment
	// Public keys that resemble internal work cannot hijack the empty-key primitive.
	for _, key := range []string{a.ID, "assignment_materialize:" + a.ID} {
		if _, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: r.GroupID, TopicID: r.TopicID, ActorID: f.coord, Content: "旧消息", IdempotencyKey: key}, nil); err != nil {
			t.Fatal(err)
		}
	}
	second := queueHandle(t, f.path, false)
	// This auxiliary test explicitly configures a 60s SQLite wait budget.
	// It does not establish behavior under the production default 5s budget.
	for _, handle := range []*Store{f.s, second} {
		coordinationDecideExec(t, handle, "PRAGMA busy_timeout=60000")
	}
	before := coordinationDecideSnapshot(t, f.s)
	type result struct {
		v   domain.AssignmentMaterialization
		err error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, s := range []*Store{f.s, second} {
		go func(s *Store) {
			<-start
			v, err := s.MaterializeAssignment(ctx, a.ID, 1, materializeAt)
			results <- result{v, err}
		}(s)
	}
	close(start)
	x, y := <-results, <-results
	if x.err != nil || y.err != nil || !reflect.DeepEqual(x.v, y.v) {
		t.Fatalf("two handles failed/diverged: x=%+v err=%v cause=%v / y=%+v err=%v cause=%v", x.v, x.err, errors.Unwrap(x.err), y.v, y.err, errors.Unwrap(y.err))
	}
	assignmentAssertReceipt(t, second, r, a, x.v, 1, materializeAt)
	after := coordinationDecideSnapshot(t, second)
	for table, rows := range before {
		switch table {
		case "messages", "message_log", "deliveries", "conversations":
			if len(after[table]) != len(rows)+1 {
				t.Errorf("%s must gain one row", table)
			}
		case "coordination_assignment_dispatch", "sqlite_sequence":
		default:
			if !reflect.DeepEqual(rows, after[table]) {
				t.Errorf("changed unrelated %s", table)
			}
		}
	}
	m, err := second.GetMessage(ctx, r.GroupID, x.v.Delivery.MessageID)
	if err != nil || m.Content != assignmentBody(r, a, f.admission.Mandate.Scope.Objective, f.admission.Mandate.Scope.Acceptance) {
		t.Fatalf("wrong platform body: %q, %v", m.Content, err)
	}
	for _, gen := range []int64{2, 3} {
		v, err := second.MaterializeAssignment(ctx, a.ID, gen, materializeAt)
		requestDispatchZero(t, v, err, ErrConflict)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, f.path, false)
	v, err := reopened.MaterializeAssignment(ctx, a.ID, 1, materializeAt.Add(time.Hour))
	if err != nil || !reflect.DeepEqual(v, x.v) {
		t.Fatalf("reopen replay: %+v, %v", v, err)
	}
	coordinationDecidePreserved(t, reopened, after)
	dispatchAssertFK(t, reopened)
}

type assignmentTimedEvent struct {
	name string
	at   time.Time
}
type assignmentTimeline struct {
	mu     sync.Mutex
	events []assignmentTimedEvent
}

func (l *assignmentTimeline) record(name string) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	at := time.Now()
	l.events = append(l.events, assignmentTimedEvent{name, at})
	return at
}
func (l *assignmentTimeline) first(name string) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.events {
		if e.name == name {
			return e.at
		}
	}
	return time.Time{}
}
func (l *assignmentTimeline) log(t *testing.T) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.events {
		t.Logf("SQLite observation event=%s utc=%s", e.name, e.at.UTC().Format(timestampLayout))
	}
}
func (l *assignmentTimeline) ordered(t *testing.T, names ...string) {
	t.Helper()
	var previous time.Time
	for _, name := range names {
		at := l.first(name)
		if at.IsZero() || !previous.IsZero() && at.Before(previous) {
			t.Fatalf("missing/out-of-order SQLite event %s", name)
		}
		previous = at
	}
}

type assignmentCommitGate struct {
	entered, release  chan struct{}
	blocked, released sync.Once
}

func assignmentNewCommitGate() *assignmentCommitGate {
	return &assignmentCommitGate{entered: make(chan struct{}), release: make(chan struct{})}
}
func (g *assignmentCommitGate) block() { g.blocked.Do(func() { close(g.entered); <-g.release }) }
func (g *assignmentCommitGate) open()  { g.released.Do(func() { close(g.release) }) }

// This observer is pinned to modernc v1.61.0's real busy callback. Context.Done
// observes its stack; it never installs a busy handler or controls retries.
type assignmentBusyContext struct {
	context.Context
	timeline *assignmentTimeline
	label    string
	observed chan struct{}
	once     sync.Once
}

func (c *assignmentBusyContext) Done() <-chan struct{} {
	var pcs [8]uintptr
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs[:])])
	for {
		frame, more := frames.Next()
		if frame.Function == "modernc.org/sqlite.busyHandler" {
			c.once.Do(func() { c.timeline.record(c.label + ".busy_handler"); close(c.observed) })
			break
		}
		if !more {
			break
		}
	}
	return c.Context.Done()
}
func assignmentWatchBusy(t *testing.T, l *assignmentTimeline, label string) *assignmentBusyContext {
	t.Helper()
	base, cancel := context.WithTimeout(ctx, 90*time.Second)
	t.Cleanup(cancel)
	return &assignmentBusyContext{Context: base, timeline: l, label: label, observed: make(chan struct{})}
}
func assignmentWait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(100 * time.Second):
		t.Fatal("timed out waiting for explicit SQLite synchronization")
	}
	var zero T
	return zero
}

func assignmentAssertDefaultHandle(t *testing.T, s *Store) {
	t.Helper()
	var timeout, foreignKeys int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if timeout != 5000 || foreignKeys != 1 || s.db.Stats().MaxOpenConnections != 1 {
		t.Fatalf("not the Store.Open defaults: timeout=%d foreign_keys=%d pool=%+v", timeout, foreignKeys, s.db.Stats())
	}
	t.Logf("real Store.Open handle=%p busy_timeout=%d foreign_keys=%d max_connections=1", s, timeout, foreignKeys)
}
func assignmentDefaultHandle(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	assignmentAssertDefaultHandle(t, s)
	return s
}
func assignmentObserveOpen(t *testing.T, s *Store, l *assignmentTimeline, label string, onCommit func()) {
	t.Helper()
	assignmentAssertDefaultHandle(t, s)
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var source sync.Once
	err = conn.Raw(func(raw any) error {
		hooks, ok := raw.(sqlite.HookRegisterer)
		if !ok {
			return errors.New("installed SQLite driver lacks transaction hooks")
		}
		auth, ok := raw.(sqlite.AuthorizerRegisterer)
		if !ok {
			return errors.New("installed SQLite driver lacks an authorizer")
		}
		hooks.RegisterCommitHook(func() int32 {
			l.record(label + ".commit_hook")
			if onCommit != nil {
				onCommit()
			}
			l.record(label + ".commit_hook_return")
			return 0
		})
		hooks.RegisterRollbackHook(func() { l.record(label + ".rollback_hook") })
		return auth.RegisterAuthorizer(func(action sqlite.AuthorizerActionCode, arg1, arg2, database, trigger string) sqlite.AuthorizerReturnCode {
			if action == sqlite.AuthTransaction {
				l.record(label + "." + strings.ToLower(arg1) + "_prepare")
			}
			if action == sqlite.AuthRead && arg1 == "coordination_assignments" {
				source.Do(func() { l.record(label + ".first_source_read") })
			}
			return sqlite.AuthorizerOK
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

type assignmentCallResult struct {
	value domain.AssignmentMaterialization
	err   error
}

func assignmentStartMaterialize(t *testing.T, s *Store, c context.Context, id string, l *assignmentTimeline, label string) <-chan assignmentCallResult {
	t.Helper()
	result := make(chan assignmentCallResult, 1)
	done := make(chan struct{})
	t.Cleanup(func() { assignmentWait(t, done) })
	go func() {
		defer close(done)
		l.record(label + ".call_start")
		value, err := s.MaterializeAssignment(c, id, 1, materializeAt)
		l.record(label + ".call_return")
		result <- assignmentCallResult{value, err}
	}()
	return result
}
func assignmentRequireKnownBusy(t *testing.T, result assignmentCallResult) {
	t.Helper()
	requestDispatchZero(t, result.value, result.err, nil)
	dispatchTransientRequire(t, result.err, 5, DispatchTransientSQLiteBusy)
	var failure *assignmentTransactionError
	if !errors.As(result.err, &failure) || failure.Phase() != "begin" || !failure.RollbackConfirmed() || failure.CommitOutcomeUnknown() {
		t.Fatalf("BUSY without known no-transaction evidence: %v", result.err)
	}
	t.Logf("real SQLite BUSY phase=%s rollback_confirmed=%t commit_unknown=%t cause=%v", failure.Phase(), failure.RollbackConfirmed(), failure.CommitOutcomeUnknown(), errors.Unwrap(result.err))
}
func assignmentAssertOneMaterialization(t *testing.T, s *Store, before map[string][][]any) {
	t.Helper()
	after := coordinationDecideSnapshot(t, s)
	for table, rows := range before {
		switch table {
		case "messages", "message_log", "deliveries", "conversations":
			if len(after[table]) != len(rows)+1 {
				t.Fatalf("%s did not gain exactly one row", table)
			}
		case "coordination_assignment_dispatch", "sqlite_sequence":
		default:
			if !reflect.DeepEqual(rows, after[table]) {
				t.Fatalf("materialization changed %s", table)
			}
		}
	}
}

func TestMaterializeAssignmentDefaultBusyTimelyRelease(t *testing.T) {
	f, r, d, _ := assignmentPending(t)
	second := assignmentDefaultHandle(t, f.path)
	before := coordinationDecideSnapshot(t, f.s)
	l := &assignmentTimeline{}
	defer l.log(t)
	gate := assignmentNewCommitGate()
	defer gate.open()
	assignmentObserveOpen(t, f.s, l, "winner", gate.block)
	assignmentObserveOpen(t, second, l, "contender", nil)
	winner := assignmentStartMaterialize(t, f.s, ctx, d.Assignment.ID, l, "winner")
	assignmentWait(t, gate.entered)
	watch := assignmentWatchBusy(t, l, "contender")
	contender := assignmentStartMaterialize(t, second, watch, d.Assignment.ID, l, "contender")
	assignmentWait(t, watch.observed)
	l.record("control.release")
	gate.open()
	x, y := assignmentWait(t, winner), assignmentWait(t, contender)
	if x.err != nil || y.err != nil || !reflect.DeepEqual(x.value, y.value) {
		t.Fatalf("default-5s competing calls must both succeed identically: %+v %v / %+v %v", x.value, x.err, y.value, y.err)
	}
	l.ordered(t, "winner.begin_prepare", "winner.first_source_read", "winner.commit_hook", "contender.begin_prepare", "contender.busy_handler", "control.release", "contender.first_source_read", "contender.call_return")
	l.ordered(t, "control.release", "winner.commit_hook_return", "winner.call_return")
	if l.first("winner.call_return").Sub(l.first("contender.busy_handler")) >= 5*time.Second {
		t.Fatal("winner did not finish releasing the lock within the default busy budget")
	}
	// BEGIN acquisition is bracketed by prepare and first source read; release
	// is bracketed by commit-hook return and the successful method return.
	t.Logf("actual_busy_to_release_signal=%s actual_busy_to_winner_return=%s", l.first("control.release").Sub(l.first("contender.busy_handler")), l.first("winner.call_return").Sub(l.first("contender.busy_handler")))
	assignmentAssertReceipt(t, second, r, *d.Assignment, x.value, 1, materializeAt)
	assignmentAssertOneMaterialization(t, second, before)
	dispatchAssertFK(t, second)
}

func TestMaterializeAssignmentDefaultBusyReconcile(t *testing.T) {
	f, r, d, _ := assignmentPending(t)
	second := assignmentDefaultHandle(t, f.path)
	observer := assignmentDefaultHandle(t, f.path)
	before := coordinationDecideSnapshot(t, observer)
	l := &assignmentTimeline{}
	defer l.log(t)
	gate := assignmentNewCommitGate()
	defer gate.open()
	assignmentObserveOpen(t, f.s, l, "winner", gate.block)
	assignmentObserveOpen(t, second, l, "contender", nil)
	winner := assignmentStartMaterialize(t, f.s, ctx, d.Assignment.ID, l, "winner")
	assignmentWait(t, gate.entered)
	watch := assignmentWatchBusy(t, l, "contender")
	contender := assignmentStartMaterialize(t, second, watch, d.Assignment.ID, l, "contender")
	assignmentWait(t, watch.observed)
	lost := assignmentWait(t, contender)
	assignmentRequireKnownBusy(t, lost)
	if !l.first("contender.first_source_read").IsZero() {
		t.Fatal("failed BEGIN reached source SQL")
	}
	// The winner's commit hook can already hold an EXCLUSIVE lock. Do not
	// attempt an observer read here; verify exact persistence after its release.
	l.record("control.release")
	gate.open()
	won := assignmentWait(t, winner)
	if won.err != nil {
		t.Fatalf("winner failed: %v, cause=%v", won.err, errors.Unwrap(won.err))
	}
	l.ordered(t, "winner.begin_prepare", "winner.first_source_read", "winner.commit_hook", "contender.begin_prepare", "contender.busy_handler", "contender.call_return", "control.release", "winner.commit_hook_return", "winner.call_return")
	t.Logf("controlled_busy_callback_to_error=%s held_commit_gate=%s", l.first("contender.call_return").Sub(l.first("contender.busy_handler")), l.first("control.release").Sub(l.first("winner.commit_hook")))
	assignmentAssertReceipt(t, observer, r, *d.Assignment, won.value, 1, materializeAt)
	assignmentAssertOneMaterialization(t, observer, before)
	for _, s := range []*Store{f.s, second, observer} {
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	reopened := assignmentDefaultHandle(t, f.path)
	frozen := coordinationDecideSnapshot(t, reopened)
	replay, err := reopened.MaterializeAssignment(ctx, d.Assignment.ID, 1, materializeAt.Add(time.Hour))
	if err != nil || !reflect.DeepEqual(replay, won.value) {
		t.Fatalf("original-generation reconciliation changed winner receipt: %+v %v", replay, err)
	}
	for _, gen := range []int64{1, 2} {
		if err := reopened.DeferAssignment(ctx, d.Assignment.ID, gen, materializeAt, DispatchTransientSQLiteBusy); !errors.Is(err, ErrConflict) {
			t.Fatalf("late BUSY overwrote success: %v", err)
		}
	}
	coordinationDecidePreserved(t, reopened, frozen)
	dispatchAssertFK(t, reopened)
}

func TestMaterializeAssignmentDefaultCommitBusyUnknown(t *testing.T) {
	f, r, d, _ := assignmentPending(t)
	reader := assignmentDefaultHandle(t, f.path)
	before := coordinationDecideSnapshot(t, f.s)
	l := &assignmentTimeline{}
	defer l.log(t)
	assignmentObserveOpen(t, f.s, l, "writer", nil)
	l.record("reader.begin_call")
	tx, err := reader.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var generation int64
	if err := tx.QueryRow("SELECT generation FROM coordination_assignment_dispatch WHERE assignment_id=?", d.Assignment.ID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	l.record("reader.shared_lock_observed")
	result := assignmentStartMaterialize(t, f.s, ctx, d.Assignment.ID, l, "writer")
	failed := assignmentWait(t, result)
	requestDispatchZero(t, failed.value, failed.err, nil)
	dispatchTransientRequire(t, failed.err, 5, DispatchTransientSQLiteBusy)
	var failure *assignmentTransactionError
	if !errors.As(failed.err, &failure) || failure.Phase() != "commit" || !failure.CommitOutcomeUnknown() || failure.RollbackConfirmed() {
		t.Fatalf("real Commit BUSY was treated as known rollback: %v", failed.err)
	}
	t.Logf("real SQLite COMMIT BUSY phase=%s rollback_confirmed=%t commit_unknown=%t cause=%v", failure.Phase(), failure.RollbackConfirmed(), failure.CommitOutcomeUnknown(), errors.Unwrap(failed.err))
	// This driver's observed cleanup rollback is specific evidence for this case,
	// not a reason to classify every Commit error as a known rollback.
	l.ordered(t, "reader.shared_lock_observed", "writer.begin_prepare", "writer.first_source_read", "writer.commit_prepare", "writer.rollback_hook", "writer.call_return")
	coordinationDecidePreserved(t, f.s, before)
	l.record("reader.release_call")
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	l.record("reader.release_return")
	for _, s := range []*Store{f.s, reader} {
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	reopened := assignmentDefaultHandle(t, f.path)
	coordinationDecidePreserved(t, reopened, before)
	view, err := reopened.GetAssignment(ctx, r.GroupID, d.Assignment.ID)
	if err != nil || view.Dispatch.Generation != 1 || view.Dispatch.AttemptCount != 0 || view.Dispatch.Delivery != nil || view.Dispatch.Status != domain.AssignmentDispatchStatusPending {
		t.Fatalf("unknown commit automatically deferred/retried: %+v, %v", view, err)
	}
	// Only this explicit original-generation reconciliation may now materialize.
	reconciled, err := reopened.MaterializeAssignment(ctx, d.Assignment.ID, 1, materializeAt)
	if err != nil {
		t.Fatal(err)
	}
	assignmentAssertReceipt(t, reopened, r, *d.Assignment, reconciled, 1, materializeAt)
	assignmentAssertOneMaterialization(t, reopened, before)
	dispatchAssertFK(t, reopened)
}

func TestMaterializeAssignmentSelectedWorkerAndAssignees(t *testing.T) {
	for _, left := range []bool{false, true} {
		t.Run(fmt.Sprintf("selected-left=%t", left), func(t *testing.T) {
			s, _, thread, input := requestWorkFixture(t)
			coord, worker, other := member(t, s, thread.GroupID).ID, member(t, s, thread.GroupID).ID, member(t, s, thread.GroupID).ID
			if _, err := s.SetCoordinator(ctx, thread.GroupID, coord, 0, "config"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetDispatchPolicy(ctx, thread.GroupID, []string{worker, other}, 0, "config"); err != nil {
				t.Fatal(err)
			}
			parent, err := s.CreateTask(ctx, taskInput(thread.GroupID))
			if err != nil {
				t.Fatal(err)
			}
			input.NewTask.AssigneeID = worker
			input.NewTask.DependencyIDs = []string{parent.ID}
			admitted, err := s.RequestWork(ctx, thread.GroupID, input, "admit")
			if err != nil {
				t.Fatal(err)
			}
			r, err := s.RequestDispatch(ctx, thread.GroupID, admitted.Task.ID, 1, 1, "安排", "dispatch")
			if err != nil {
				t.Fatal(err)
			}
			scope := coordinationDecideBindRequest(t, s, r)
			receipt, err := s.DecideDispatch(ctx, scope, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: worker, Reason: "指定成员"}, "decide")
			if err != nil {
				t.Fatal(err)
			}
			if left {
				coordinationDecideExec(t, s, "UPDATE members SET status='left' WHERE id=?", worker)
			}
			got, err := s.MaterializeAssignment(ctx, receipt.Assignment.ID, 1, materializeAt)
			if err != nil {
				t.Fatal(err)
			}
			if left {
				if got.Disposition != "held" || got.HeldReason != domain.AssignmentHeldWorkerInactive || got.Delivery != nil {
					t.Fatalf("arbitrary other worker substituted for selected member: %+v", got)
				}
			} else {
				assignmentAssertReceipt(t, s, r, *receipt.Assignment, got, 1, materializeAt)
				// Unaccepted dependencies gate future activation, not intent persistence.
				task, err := s.GetTask(ctx, thread.GroupID, admitted.Task.ID)
				if err != nil || task.ActiveAttemptID != "" {
					t.Fatalf("materialize activated an Attempt: %+v %v", task, err)
				}
			}
		})
	}
}

func TestMaterializeAssignmentInputAndCurrentEligibility(t *testing.T) {
	f, _, d, _ := assignmentPending(t)
	a := d.Assignment
	before := coordinationDecideSnapshot(t, f.s)
	for _, id := range []string{"", " padded", "id\n", "id\x00", "\xff", strings.Repeat("x", 4097)} {
		v, err := f.s.MaterializeAssignment(ctx, id, 1, materializeAt)
		requestDispatchZero(t, v, err, ErrInvalid)
	}
	for _, gen := range []int64{0, -1, math.MinInt64} {
		v, err := f.s.MaterializeAssignment(ctx, a.ID, gen, materializeAt)
		requestDispatchZero(t, v, err, ErrInvalid)
	}
	for _, at := range []time.Time{time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)} {
		v, err := f.s.MaterializeAssignment(ctx, a.ID, 1, at)
		requestDispatchZero(t, v, err, ErrInvalid)
	}
	v, err := f.s.MaterializeAssignment(ctx, "missing", 1, materializeAt)
	requestDispatchZero(t, v, err, ErrNotFound)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	v, err = f.s.MaterializeAssignment(cancelled, a.ID, 1, materializeAt)
	requestDispatchZero(t, v, err, context.Canceled)
	coordinationDecidePreserved(t, f.s, before)
	for _, change := range []string{"task-revision", "active-attempt", "submitted", "accepted", "authority", "missing-current", "binding", "policy", "coordinator-left", "worker-left", "task-assignee"} {
		t.Run(change, func(t *testing.T) {
			f, _, d, _ := assignmentPending(t)
			want := materializeChangeEligibility(t, f, change)
			if change == "worker-left" {
				want = domain.AssignmentHeldWorkerInactive
			}
			if change == "task-assignee" {
				want = domain.DispatchHeldTaskStale
			}
			before := coordinationDecideSnapshot(t, f.s)
			v, err := f.s.MaterializeAssignment(ctx, d.Assignment.ID, 1, materializeAt)
			if err != nil || v.Disposition != "held" || v.HeldReason != want || v.Generation != 2 || v.Delivery != nil {
				t.Fatalf("wrong exact held classification: %+v, %v, want=%s", v, err, want)
			}
			view, err := f.s.GetAssignment(ctx, f.groupID, d.Assignment.ID)
			if err != nil || view.Dispatch.AttemptCount != 0 || view.Dispatch.NextAttemptAt != d.Assignment.CreatedAt || view.Dispatch.LastErrorCode != "" || view.Dispatch.UpdatedAt != materializeAt {
				t.Fatalf("held damaged metadata: %+v, %v", view, err)
			}
			delete(before, "coordination_assignment_dispatch")
			coordinationDecidePreservedSubset(t, f.s, before)
			frozen := coordinationDecideSnapshot(t, f.s)
			for _, gen := range []int64{1, 2} {
				v, err := f.s.MaterializeAssignment(ctx, d.Assignment.ID, gen, materializeAt)
				requestDispatchZero(t, v, err, ErrConflict)
			}
			coordinationDecidePreserved(t, f.s, frozen)
		})
	}
}

func coordinationDecidePreservedSubset(t *testing.T, s *Store, before map[string][][]any) {
	t.Helper()
	after := coordinationDecideSnapshot(t, s)
	for table, rows := range before {
		if !reflect.DeepEqual(rows, after[table]) {
			t.Fatalf("changed unrelated %s", table)
		}
	}
}

func TestMaterializeAssignmentPayload(t *testing.T) {
	dummy := domain.DispatchRequest{ID: strings.Repeat("x", 26), TaskID: strings.Repeat("x", 26), MandateID: strings.Repeat("x", 26)}
	dummyA := domain.Assignment{ID: strings.Repeat("x", 26)}
	budget := 32768 - len(assignmentBody(dummy, dummyA, "", "验收"))
	for _, tc := range []struct {
		name, objective, acceptance, held string
		counterfactual                    bool
	}{
		{"exact", strings.Repeat("x", budget), "验收", "", false},
		{"multibyte", strings.Repeat("目", budget/3) + strings.Repeat("x", budget%3), "验收", "", false},
		{"one-over", strings.Repeat("x", budget+1), "验收", domain.DispatchHeldPayloadTooLarge, false},
		{"nul", "目标\x00无损", "验收\x00无损", "", false},
		{"invalid-objective", "目标\xff", "验收", domain.DispatchHeldPayloadNotEncodable, true},
		{"invalid-acceptance", "目标", "验收\xff", domain.DispatchHeldPayloadNotEncodable, true},
		{"huge-objective", strings.Repeat("目", 100000), "验收", domain.DispatchHeldPayloadTooLarge, true},
		{"huge-acceptance", "目标", strings.Repeat("验", 100000), domain.DispatchHeldPayloadTooLarge, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, path := openTestStore(t)
			g, c, w := dispatchConfiguredGroup(t, s)
			objective, acceptance := tc.objective, tc.acceptance
			if tc.counterfactual {
				objective, acceptance = "合法目标", "合法验收"
			}
			r, d, scope := assignmentNew(t, s, g, c, w, " \x00\xfftopic ", objective, acceptance, strings.Repeat("无关描述", 100000))
			assignmentEndCoordinator(t, s, scope)
			if tc.counterfactual {
				// Negative corruption only: N2 would correctly refuse such text before a real decision.
				coordinationDecideExec(t, s, "DROP TRIGGER mandates_no_update")
				coordinationDecideExec(t, s, "UPDATE mandates SET objective=?,acceptance=? WHERE id=?", tc.objective, tc.acceptance, r.MandateID)
			}
			recorded := assignmentQueryStore(t, path)
			v, err := recorded.s.MaterializeAssignment(ctx, d.Assignment.ID, 1, materializeAt.In(time.FixedZone("clock", 3600)))
			if err != nil {
				t.Fatal(err)
			}
			for _, q := range recorded.queries {
				if strings.Contains(q.query, "description") || strings.Contains(q.query, "c.reason") || strings.Contains(q.query, "r.reason") {
					t.Fatalf("loaded unrelated unbounded body: %s", q.query)
				}
			}
			if tc.held != "" {
				if v.Disposition != "held" || v.HeldReason != tc.held || v.Delivery != nil {
					t.Fatalf("payload not held: %+v", v)
				}
				return
			}
			assignmentAssertReceipt(t, s, r, *d.Assignment, v, 1, materializeAt)
			m, err := s.GetMessage(ctx, r.GroupID, v.Delivery.MessageID)
			if err != nil || m.Content != assignmentBody(r, *d.Assignment, tc.objective, tc.acceptance) || !utf8.ValidString(m.Content) || len(m.Content) > 32768 {
				t.Fatalf("lossy/invalid body length=%d: %v", len(m.Content), err)
			}
			if (tc.name == "exact" || tc.name == "multibyte") && len(m.Content) != 32768 {
				t.Fatalf("wrong exact boundary: %d", len(m.Content))
			}
		})
	}
}

func TestMaterializeAssignmentWriteFailuresRollback(t *testing.T) {
	for _, point := range []string{"messages", "message_log", "conversations", "deliveries", "coordination_assignment_dispatch", "held"} {
		for _, mode := range []string{"abort", "ignore", "commit"} {
			t.Run(point+"/"+mode, func(t *testing.T) {
				f, _, d, _ := assignmentPending(t)
				observer := queueHandle(t, f.path, false)
				table, op := point, "INSERT"
				if point == "held" {
					table = "coordination_assignment_dispatch"
					materializeChangeEligibility(t, f, "worker-left")
				}
				if table == "coordination_assignment_dispatch" {
					op = "UPDATE"
				}
				timing, body := "AFTER", "SELECT RAISE(ABORT,'PRIVATE WRITE FAULT');"
				if mode == "ignore" {
					timing, body = "BEFORE", "SELECT RAISE(IGNORE);"
				}
				if mode == "commit" {
					coordinationDecideExec(t, f.s, "CREATE TABLE assignment_commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)")
					body = "INSERT INTO assignment_commit_guard VALUES('missing');"
				}
				coordinationDecideExec(t, f.s, "CREATE TRIGGER assignment_fault "+timing+" "+op+" ON "+table+" BEGIN "+body+" END")
				before := coordinationDecideSnapshot(t, observer)
				v, err := f.s.MaterializeAssignment(ctx, d.Assignment.ID, 1, materializeAt)
				requestDispatchZero(t, v, err, nil)
				if mode != "ignore" {
					var cause *sqlite.Error
					if !errors.As(err, &cause) {
						t.Fatalf("SQLite cause lost: %v", err)
					}
				}
				var failure *assignmentTransactionError
				if !errors.As(err, &failure) || failure.CommitOutcomeUnknown() != (mode == "commit") || mode != "commit" && !failure.RollbackConfirmed() {
					t.Fatalf("incorrect phase/rollback evidence: %v", err)
				}
				if mode == "commit" && (failure.Phase() != "commit" || !strings.Contains(err.Error(), "结果未确认")) {
					t.Fatalf("Commit is always unknown: %v", err)
				}
				if strings.Contains(err.Error(), "PRIVATE") {
					t.Fatal("unsafe SQL error disclosure")
				}
				coordinationDecidePreserved(t, observer, before)
				coordinationDecideExec(t, f.s, "DROP TRIGGER assignment_fault")
				retry, err := observer.MaterializeAssignment(ctx, d.Assignment.ID, 1, materializeAt)
				if err != nil || retry.Generation != 2 {
					t.Fatalf("rollback burned generation: %+v, %v", retry, err)
				}
				dispatchAssertFK(t, observer)
			})
		}
	}
}

func TestMaterializeAssignmentCoordinatorLifecycleAndHistoricalReplay(t *testing.T) {
	for _, state := range []string{"terminal", "unknown", "cancel_requested"} {
		t.Run(state, func(t *testing.T) {
			f, r, d, scope := assignmentPending(t)
			if state == "terminal" {
				assignmentEndCoordinator(t, f.s, scope)
			} else {
				queueEvent(t, f.s, scope.Member.Claim, provider.AcceptedEvent, "", "")
				if state == "unknown" {
					receipt := queueReceipt(provider.OutcomeUnknown)
					receipt.OutputComplete = false
					if _, err := f.s.ApplyTurnReceipt(ctx, scope.Member.Claim, receipt); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := f.s.RequestTurnCancel(ctx, r.GroupID, scope.Member.Claim.TurnID); err != nil {
						t.Fatal(err)
					}
				}
			}
			prior := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.MaterializeAssignment(ctx, d.Assignment.ID, 1, materializeAt)
			if err != nil {
				t.Fatalf("historical coordinator identity incorrectly requires live scope: %v", err)
			}
			assignmentAssertReceipt(t, f.s, r, *d.Assignment, got, 1, materializeAt)
			after := coordinationDecideSnapshot(t, f.s)
			for _, table := range []string{"turns", "resource_claims", "task_attempts"} {
				if !reflect.DeepEqual(prior[table], after[table]) {
					t.Fatalf("materialize changed %s", table)
				}
			}
			turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.GroupID, DeliveryID: got.Delivery.DeliveryID, MaxActive: 8, ResourceKeys: []string{"worker-execution"}})
			if err != nil {
				t.Fatal(err)
			}
			claim := TurnClaim{GroupID: r.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
			queueEvent(t, f.s, claim, provider.AcceptedEvent, "", "")
			if state == "unknown" {
				receipt := queueReceipt(provider.OutcomeUnknown)
				receipt.OutputComplete = false
				if _, err := f.s.ApplyTurnReceipt(ctx, claim, receipt); err != nil {
					t.Fatal(err)
				}
			} else if _, err := f.s.RequestTurnCancel(ctx, r.GroupID, turn.ID); err != nil {
				t.Fatal(err)
			}
			coordinationDecideExec(t, f.s, "UPDATE deliveries SET error='runtime diagnostic' WHERE id=?", got.Delivery.DeliveryID)
			for _, change := range []string{"task-revision", "authority", "binding", "policy", "coordinator-left", "worker-left"} {
				materializeChangeEligibility(t, f, change)
			}
			frozen := coordinationDecideSnapshot(t, f.s)
			if err := f.s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := queueHandle(t, f.path, false)
			replay, err := reopened.MaterializeAssignment(ctx, d.Assignment.ID, 1, materializeAt.Add(time.Hour))
			if err != nil || !reflect.DeepEqual(replay, got) {
				t.Fatalf("receipt revalidated current/queued shape: %+v, %v", replay, err)
			}
			coordinationDecidePreserved(t, reopened, frozen)
			due, err := reopened.ListDueAssignments(ctx, time.Now().Add(24*time.Hour), AssignmentDueOptions{})
			if err != nil || len(due.Items) != 0 {
				t.Fatalf("enqueued reentered due: %+v, %v", due, err)
			}
		})
	}
}

func TestMaterializeAssignmentDamagedSourceIsNotHeld(t *testing.T) {
	for _, damage := range []string{"decision-worker", "coordinator-turn", "dual-role", "routing", "binding-history", "missing-table"} {
		t.Run(damage, func(t *testing.T) {
			f, _, d, scope := assignmentPending(t)
			// Counterfactual corruption tests only; never used to establish positive capability.
			switch damage {
			case "decision-worker":
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_decisions_no_update")
				coordinationDecideExec(t, f.s, "UPDATE coordination_decisions SET worker_member_id=? WHERE id=?", f.coord, d.Decision.ID)
			case "coordinator-turn":
				assignmentEndCoordinator(t, f.s, scope)
				other := queueDelivery(t, f.s, f.groupID, f.coord, "ordinary")
				turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: other.ID, MaxActive: 8})
				if err != nil {
					t.Fatal(err)
				}
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_decisions_no_update")
				coordinationDecideExec(t, f.s, "UPDATE coordination_decisions SET coordinator_turn_id=? WHERE id=?", turn.ID, d.Decision.ID)
			case "dual-role":
				coordinationDecideExec(t, f.s, "PRAGMA ignore_check_constraints=ON; DROP TRIGGER coordination_assignment_dispatch_update; DROP TRIGGER coordination_assignment_receipt_source")
				coordinationDecideExec(t, f.s, "UPDATE coordination_assignment_dispatch SET delivery_id=? WHERE assignment_id=?", scope.Provenance.DeliveryID, d.Assignment.ID)
			case "routing":
				coordinationDecideExec(t, f.s, "DROP TRIGGER routing_decisions_no_update; DROP TRIGGER mandates_no_update")
				tx, err := f.s.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if _, err := tx.Exec("UPDATE routing_decisions SET task_revision_at_admission=2 WHERE id=?", f.admission.Decision.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec("UPDATE mandates SET task_revision_at_admission=2 WHERE id=?", f.admission.Mandate.ID); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			case "binding-history":
				coordinationDecideExec(t, f.s, "DROP TRIGGER group_coordinator_binding_revisions_no_update")
				coordinationDecideExec(t, f.s, "UPDATE group_coordinator_binding_revisions SET member_id=? WHERE group_id=?", f.worker, f.groupID)
			case "missing-table":
				coordinationDecideExec(t, f.s, "ALTER TABLE task_coordination RENAME TO missing_task_coordination")
			}
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.MaterializeAssignment(ctx, d.Assignment.ID, 1, materializeAt)
			requestDispatchZero(t, got, err, nil)
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}

func assignmentLateHistoryFixture(t *testing.T, state string) (requestDispatchFixture, domain.DispatchRequest, domain.DecisionReceipt, domain.Turn) {
	t.Helper()
	s, path, thread, input := requestWorkFixture(t)
	coord, worker, historyWorker := member(t, s, thread.GroupID).ID, member(t, s, thread.GroupID).ID, member(t, s, thread.GroupID).ID
	if _, err := s.SetCoordinator(ctx, thread.GroupID, coord, 0, "config"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDispatchPolicy(ctx, thread.GroupID, []string{worker}, 0, "config"); err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, taskInput(thread.GroupID))
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(task))
	if err != nil {
		t.Fatal(err)
	}
	submission, err := s.SubmitTask(ctx, taskSubmissionInput(human))
	if err != nil {
		t.Fatal(err)
	}
	review := taskReviewInput(submission)
	review.Outcome = domain.ReviewOutcomeRejected
	if _, err := s.ReviewTask(ctx, review); err != nil {
		t.Fatal(err)
	}
	task, err = s.GetTask(ctx, thread.GroupID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldDelivery := queueDelivery(t, s, thread.GroupID, historyWorker, "old-worker")
	oldTurn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: thread.GroupID, DeliveryID: oldDelivery.ID, MaxActive: 8, ResourceKeys: []string{"old-history-claim"}})
	if err != nil {
		t.Fatal(err)
	}
	claim := TurnClaim{GroupID: thread.GroupID, TurnID: oldTurn.ID, Token: oldTurn.ClaimToken}
	queueEvent(t, s, claim, provider.AcceptedEvent, "", "")
	if state == "unknown" {
		receipt := queueReceipt(provider.OutcomeUnknown)
		receipt.OutputComplete = false
		if _, err := s.ApplyTurnReceipt(ctx, claim, receipt); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := s.RequestTurnCancel(ctx, thread.GroupID, oldTurn.ID); err != nil {
			t.Fatal(err)
		}
	}
	admitted, err := s.RequestWork(ctx, thread.GroupID, requestWorkLink(input, task), "admit-history")
	if err != nil {
		t.Fatal(err)
	}
	f := requestDispatchFixture{s: s, path: path, groupID: thread.GroupID, coord: coord, worker: worker, task: task, admission: admitted}
	r, err := s.RequestDispatch(ctx, f.groupID, task.ID, 2, 1, "安排当前版本", "dispatch-history")
	if err != nil {
		t.Fatal(err)
	}
	scope := coordinationDecideBindRequest(t, s, r)
	// The older Turn has no Attempt relationship yet, so the real Decide is eligible.
	d := coordinationDecideAssign(t, f, scope, "decide-history")
	return f, r, d, oldTurn
}

func TestMaterializeAssignmentHistoricalNonterminalUnbounded(t *testing.T) {
	for _, state := range []string{"unknown", "cancel_requested"} {
		t.Run(state, func(t *testing.T) {
			f, r, d, oldTurn := assignmentLateHistoryFixture(t, state)
			historyWorker := oldTurn.MemberID
			// A guard-checked historical append models a late-discovered Attempt, not N3 activation.
			// Temporarily expose its revision within one transaction; no FK/trigger is disabled.
			tx, err := f.s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for _, q := range []string{fmt.Sprintf("UPDATE tasks SET revision=1 WHERE id='%s'", r.TaskID), fmt.Sprintf("INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,turn_id,created_at) VALUES('early-history','%s','%s',1,'%s','%s','2000-01-01T00:00:00.000000000Z')", r.TaskID, r.GroupID, historyWorker, oldTurn.ID), fmt.Sprintf("UPDATE tasks SET revision=2 WHERE id='%s'", r.TaskID)} {
				if _, err := tx.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			terminalMember := member(t, f.s, r.GroupID).ID
			terminalDelivery := queueDelivery(t, f.s, r.GroupID, terminalMember, "new-terminal")
			terminal, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.GroupID, DeliveryID: terminalDelivery.ID, MaxActive: 8})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.ApplyTurnReceipt(ctx, TurnClaim{GroupID: r.GroupID, TurnID: terminal.ID, Token: terminal.ClaimToken}, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
				t.Fatal(err)
			}
			coordinationDecideExec(t, f.s, `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<4096)
   INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,turn_id,created_at)
   SELECT printf('new-terminal-%06d',i),?,?,2,?,?,'2030-01-01T00:00:00.000000000Z' FROM n`, r.TaskID, r.GroupID, terminalMember, terminal.ID)
			recorder := assignmentQueryStore(t, f.path)
			before := coordinationDecideSnapshot(t, f.s)
			started := time.Now()
			got, err := recorder.s.MaterializeAssignment(ctx, d.Assignment.ID, 1, materializeAt)
			if err != nil || got.HeldReason != domain.DispatchHeldTaskStale || got.Disposition != "held" || got.Delivery != nil {
				t.Fatalf("early other-worker/revision %s hidden by 4096 terminal rows: %+v, %v", state, got, err)
			}
			delete(before, "coordination_assignment_dispatch")
			coordinationDecidePreservedSubset(t, f.s, before)
			var count int
			if err := f.s.db.QueryRow("SELECT count(*) FROM task_attempts WHERE group_id=? AND task_id=?", r.GroupID, r.TaskID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			t.Logf("state=%s history_rows=%d old_revision=1 captured_revision=2 different_worker=true elapsed=%s (no hard work bound)", state, count, time.Since(started))
			found := false
			for _, q := range recorder.queries {
				if strings.Contains(q.query, "starting','running','cancel_requested','unknown") && strings.HasPrefix(q.query, "SELECT") {
					plan := resolutionPlan(t, f.s, q.query, q.args...)
					if !strings.Contains(plan, "task_attempts_group_task_created_id") || strings.Contains(strings.ToUpper(q.query), "LIMIT") {
						t.Fatal("history fence is truncated or lost Task index")
					}
					found = true
					break
				}
			}
			if !found {
				t.Fatal("actual history query not captured")
			}
			dispatchAssertFK(t, f.s)
		})
	}
}

func assignmentMutation(f requestDispatchFixture, change string) string {
	rTask, g := f.task.ID, f.groupID
	switch change {
	case "task":
		return fmt.Sprintf("UPDATE tasks SET revision=revision+1 WHERE id='%s';", rTask)
	case "assignee":
		return fmt.Sprintf("UPDATE tasks SET assignee_id='%s' WHERE id='%s';", f.coord, rTask)
	case "authority":
		return fmt.Sprintf("UPDATE task_coordination SET authority_generation=authority_generation+1 WHERE task_id='%s';", rTask)
	case "mandate":
		return fmt.Sprintf("DELETE FROM task_coordination WHERE task_id='%s';", rTask)
	case "binding":
		return fmt.Sprintf("INSERT INTO group_coordinator_binding_revisions SELECT group_id,2,member_id FROM group_coordinator_binding_revisions WHERE group_id='%s' AND revision=1; UPDATE group_coordinator_bindings SET revision=2 WHERE group_id='%s';", g, g)
	case "policy":
		return fmt.Sprintf("INSERT INTO group_dispatch_policy_revisions SELECT group_id,2,allowed_worker_ids FROM group_dispatch_policy_revisions WHERE group_id='%s' AND revision=1; UPDATE group_dispatch_policies SET revision=2 WHERE group_id='%s';", g, g)
	case "coordinator":
		return fmt.Sprintf("UPDATE members SET status='left' WHERE id='%s';", f.coord)
	case "worker":
		return fmt.Sprintf("UPDATE members SET status='left' WHERE id='%s';", f.worker)
	}
	panic("unknown mutation")
}

func assignmentPreparedMutation(t *testing.T, f requestDispatchFixture, change string) string {
	t.Helper()
	switch change {
	case "active", "submission":
		mutation := fmt.Sprintf("INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,created_at) VALUES('write-attempt','%s','%s',1,'user','2026-11-09T00:00:00.000000000Z'); UPDATE tasks SET active_attempt_id='write-attempt' WHERE id='%s';", f.task.ID, f.groupID, f.task.ID)
		if change == "submission" {
			mutation += fmt.Sprintf("INSERT INTO task_submissions(id,task_id,group_id,task_revision,attempt_id,summary,submitted_by,created_at) VALUES('write-submission','%s','%s',1,'write-attempt','提交','user','2026-11-09T00:00:00.000000000Z'); UPDATE tasks SET status='submitted',current_submission_id='write-submission' WHERE id='%s';", f.task.ID, f.groupID, f.task.ID)
		}
		return mutation
	case "current-mandate":
		other := coordinationAdmissionRows(f.task, coordinationAdmissionSource(t, f.s, domain.ThreadRef{GroupID: f.groupID, TopicID: f.admission.Mandate.TopicID}))
		other.current = nil
		if err := coordinationAdmissionCommit(f.s, other); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("UPDATE task_coordination SET current_mandate_id='%s' WHERE task_id='%s';", other.mandate[0], f.task.ID)
	case "history":
		worker := member(t, f.s, f.groupID).ID
		delivery := queueDelivery(t, f.s, f.groupID, worker, "write-history")
		turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: delivery.ID, MaxActive: 8, ResourceKeys: []string{"write-history-resource"}})
		if err != nil {
			t.Fatal(err)
		}
		claim := TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken}
		queueEvent(t, f.s, claim, provider.AcceptedEvent, "", "")
		receipt := queueReceipt(provider.OutcomeUnknown)
		receipt.OutputComplete = false
		if _, err := f.s.ApplyTurnReceipt(ctx, claim, receipt); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,turn_id,created_at) VALUES('write-history','%s','%s',1,'%s','%s','2000-01-01T00:00:00.000000000Z');", f.task.ID, f.groupID, worker, turn.ID)
	default:
		return assignmentMutation(f, change)
	}
}

func TestMaterializeAssignmentWritePointEligibilityAndFinalSQL(t *testing.T) {
	changes := []string{"task", "active", "submission", "assignee", "authority", "mandate", "current-mandate", "binding", "policy", "coordinator", "worker", "history"}
	for _, point := range []string{"messages", "message_log", "conversations", "deliveries", "coordination_assignment_dispatch", "sql-cas"} {
		for _, change := range changes {
			t.Run(point+"/"+change, func(t *testing.T) {
				f, _, d, _ := assignmentPending(t)
				observer := queueHandle(t, f.path, false)
				mutation := assignmentPreparedMutation(t, f, change)
				recorder := assignmentQueryStore(t, f.path)
				if point == "sql-cas" {
					recorder.beforeCAS = func(conn driver.Conn) {
						if _, err := conn.(driver.ExecerContext).ExecContext(ctx, mutation, nil); err != nil {
							t.Fatal(err)
						}
					}
				} else {
					op := "INSERT"
					if point == "coordination_assignment_dispatch" {
						op = "UPDATE"
					}
					coordinationDecideExec(t, f.s, "CREATE TRIGGER assignment_drift AFTER "+op+" ON "+point+" BEGIN "+mutation+" END")
				}
				before := coordinationDecideSnapshot(t, observer)
				got, err := recorder.s.MaterializeAssignment(ctx, d.Assignment.ID, 1, materializeAt)
				requestDispatchZero(t, got, err, ErrConflict)
				coordinationDecidePreserved(t, observer, before)
				if point == "sql-cas" && !reflect.DeepEqual(recorder.casRows, []int64{0}) {
					t.Fatalf("final SQL did not independently refuse drift: %v", recorder.casRows)
				}
			})
		}
	}
}

func TestMaterializeAssignmentSQLCapturedFence(t *testing.T) {
	for _, change := range []string{"mandate-revision", "mandate-assignee", "allowlist", "binding-member", "decision-worker"} {
		t.Run(change, func(t *testing.T) {
			f, r, d, _ := assignmentPending(t)
			// Counterfactual immutable-source drift at the exact final SQL boundary.
			var mutation string
			switch change {
			case "mandate-revision":
				coordinationDecideExec(t, f.s, "DROP TRIGGER mandates_no_update; DROP TRIGGER routing_decisions_no_update")
				mutation = fmt.Sprintf("UPDATE mandates SET task_revision_at_admission=2 WHERE id='%s'; UPDATE routing_decisions SET task_revision_at_admission=2 WHERE id='%s';", r.MandateID, f.admission.Decision.ID)
			case "mandate-assignee":
				coordinationDecideExec(t, f.s, "DROP TRIGGER mandates_no_update")
				mutation = fmt.Sprintf("UPDATE mandates SET assignee_id='%s' WHERE id='%s';", f.coord, r.MandateID)
			case "allowlist":
				coordinationDecideExec(t, f.s, "DROP TRIGGER group_dispatch_policy_revisions_no_update")
				mutation = fmt.Sprintf("UPDATE group_dispatch_policy_revisions SET allowed_worker_ids='[]' WHERE group_id='%s';", f.groupID)
			case "binding-member":
				coordinationDecideExec(t, f.s, "DROP TRIGGER group_coordinator_binding_revisions_no_update")
				mutation = fmt.Sprintf("UPDATE group_coordinator_binding_revisions SET member_id='%s' WHERE group_id='%s';", f.worker, f.groupID)
			case "decision-worker":
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_decisions_no_update")
				mutation = fmt.Sprintf("UPDATE coordination_decisions SET worker_member_id='%s' WHERE id='%s';", f.coord, d.Decision.ID)
			}
			recorder := assignmentQueryStore(t, f.path)
			recorder.beforeCAS = func(conn driver.Conn) {
				if _, err := conn.(driver.ExecerContext).ExecContext(ctx, mutation, nil); err != nil {
					t.Fatal(err)
				}
			}
			before := coordinationDecideSnapshot(t, f.s)
			got, err := recorder.s.MaterializeAssignment(ctx, d.Assignment.ID, 1, materializeAt)
			requestDispatchZero(t, got, err, ErrConflict)
			if !reflect.DeepEqual(recorder.casRows, []int64{0}) {
				t.Fatalf("captured source was not in final SQL: %v", recorder.casRows)
			}
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}

func TestMaterializeAssignmentLostCAS(t *testing.T) {
	f, _, d, _ := assignmentPending(t)
	coordinationDecideExec(t, f.s, `PRAGMA recursive_triggers=OFF; CREATE TRIGGER assignment_lost_cas BEFORE UPDATE ON coordination_assignment_dispatch BEGIN
 UPDATE coordination_assignment_dispatch SET generation=generation+1,attempt_count=attempt_count+1,last_error_code='sqlite_busy',
 updated_at='2026-11-09T00:00:00.000000000Z',next_attempt_at='2026-11-09T00:00:02.000000000Z' WHERE assignment_id=OLD.assignment_id;
 SELECT RAISE(IGNORE); END`)
	before := coordinationDecideSnapshot(t, f.s)
	got, err := f.s.MaterializeAssignment(ctx, d.Assignment.ID, 1, materializeAt)
	requestDispatchZero(t, got, err, ErrConflict)
	coordinationDecidePreserved(t, f.s, before)
}

func TestMaterializeAssignmentFinalRuntimeShape(t *testing.T) {
	for _, shape := range []string{"body", "failed", "error"} {
		t.Run(shape, func(t *testing.T) {
			f, _, d, _ := assignmentPending(t)
			if shape == "body" {
				coordinationDecideExec(t, f.s, `PRAGMA recursive_triggers=OFF; CREATE TRIGGER assignment_shape BEFORE INSERT ON messages BEGIN
    INSERT INTO messages(id,group_id,topic_id,sender_id,content,created_at) VALUES(NEW.id,NEW.group_id,NEW.topic_id,NEW.sender_id,'substituted',NEW.created_at); SELECT RAISE(IGNORE); END`)
			} else {
				mutation := "UPDATE deliveries SET error='dirty' WHERE id=NEW.delivery_id;"
				if shape == "failed" {
					mutation = "UPDATE deliveries SET state='failed' WHERE id=NEW.delivery_id;"
				}
				coordinationDecideExec(t, f.s, "CREATE TRIGGER assignment_shape AFTER UPDATE ON coordination_assignment_dispatch BEGIN "+mutation+" END")
			}
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.MaterializeAssignment(ctx, d.Assignment.ID, 1, materializeAt)
			requestDispatchZero(t, got, err, ErrConflict)
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}

func TestMaterializeAssignmentCommitUnknown(t *testing.T) {
	for _, mode := range []string{"refuse", "ack-error"} {
		t.Run(mode, func(t *testing.T) {
			f, r, d, _ := assignmentPending(t)
			second := queueHandle(t, f.path, false)
			coordinationDecideExec(t, second, "PRAGMA busy_timeout=0")
			lock, err := f.s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, busy := second.db.BeginTx(ctx, nil)
			if busy == nil {
				t.Fatal("expected real SQLite busy")
			}
			if err := lock.Rollback(); err != nil {
				t.Fatal(err)
			}
			wrapped, _ := coordinationDecideWrapped(t, f.path, mode, busy)
			before := coordinationDecideSnapshot(t, second)
			got, err := wrapped.MaterializeAssignment(ctx, d.Assignment.ID, 1, materializeAt)
			requestDispatchZero(t, got, err, busy)
			var failure *assignmentTransactionError
			if !errors.As(err, &failure) || failure.Phase() != "commit" || !failure.CommitOutcomeUnknown() || failure.RollbackConfirmed() || !strings.Contains(err.Error(), "结果未确认") {
				t.Fatalf("Commit error treated as known rollback: %v", err)
			}
			if _, ok := ClassifyDispatchTransient(err); !ok {
				t.Fatal("raw SQLite classifier should still identify the cause, not its outcome")
			}
			if err := wrapped.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := queueHandle(t, f.path, false)
			view, readErr := reopened.GetAssignment(ctx, r.GroupID, d.Assignment.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode == "refuse" {
				coordinationDecidePreserved(t, reopened, before)
				if view.Dispatch.Delivery != nil {
					t.Fatal("before-commit refusal committed")
				}
			} else {
				if view.Dispatch.Delivery == nil || view.Dispatch.Generation != 2 || view.Dispatch.AttemptCount != 0 {
					t.Fatalf("postcommit ack lost actual state: %+v", view)
				}
			}
			replay, err := reopened.MaterializeAssignment(ctx, d.Assignment.ID, 1, materializeAt)
			if err != nil {
				t.Fatalf("original generation reconciliation: %v", err)
			}
			assignmentAssertReceipt(t, reopened, r, *d.Assignment, replay, 1, materializeAt)
			if mode == "ack-error" && !reflect.DeepEqual(replay.Delivery, view.Dispatch.Delivery) {
				t.Fatal("reconciliation allocated new IDs")
			}
		})
	}
}

func assignmentEndCoordinator(t *testing.T, s *Store, scope CoordinationToolScope) {
	t.Helper()
	if _, err := s.ApplyTurnReceipt(ctx, scope.Member.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
		t.Fatal(fmt.Errorf("end fixture coordinator: %w", err))
	}
}
