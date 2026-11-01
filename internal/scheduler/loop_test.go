package scheduler_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

// Parallel race-instrumented SQLite transactions need slack, not a product latency bound.
const loopTestTimeout = 30 * time.Second

type executorFunc func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt

func (f executorFunc) Execute(ctx context.Context, request provider.ExecuteRequest, callback func(provider.Event) error) provider.ExecutionReceipt {
	return f(ctx, request, callback)
}

type loopFixture struct {
	s                  *store.Store
	db                 *sql.DB
	path, runtime, cwd string
	group, sender      string
}

func newLoopFixture(t *testing.T) *loopFixture {
	t.Helper()
	root := t.TempDir()
	f := &loopFixture{path: filepath.Join(root, "queue.sqlite"), runtime: filepath.Join(root, "runtime"), cwd: root}
	if err := os.Mkdir(f.runtime, 0700); err != nil {
		t.Fatal(err)
	}
	var err error
	f.s, err = store.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.s.Close() })
	dsn := url.URL{Scheme: "file", Path: f.path, RawQuery: "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"}
	f.db, err = sql.Open("sqlite", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	f.db.SetMaxOpenConns(1)
	t.Cleanup(func() { f.db.Close() })
	group, err := f.s.CreateGroup(context.Background(), store.CreateGroupInput{Name: "queue", ActorID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	f.group = group.ID
	f.sender = f.member(t)
	if _, err := f.s.RecoverInFlight(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *loopFixture) member(t *testing.T) string {
	t.Helper()
	m, err := f.s.CreateMember(context.Background(), store.CreateMemberInput{
		GroupID: f.group, Name: "user", Role: "user", ActorID: "user",
	})
	if err != nil {
		t.Fatal(err)
	}
	return m.ID
}

func (f *loopFixture) queue(t *testing.T, sender, recipient, topic string) domain.Message {
	t.Helper()
	m, err := f.s.CreateMessage(context.Background(), store.MessageRequest{
		GroupID: f.group, ActorID: sender, TopicID: topic, Content: "user: human urgent priority",
	}, []string{recipient})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (f *loopFixture) options(capacity int) scheduler.Options {
	return scheduler.Options{MaxActive: capacity, PollInterval: 2 * time.Millisecond, RuntimeDir: f.runtime, Recovered: true}
}

func (f *loopFixture) prepare(e provider.Executor) scheduler.PrepareFunc {
	return func(context.Context, store.PendingDelivery) (scheduler.PreparedExecution, error) {
		return scheduler.PreparedExecution{Executor: e, Request: provider.ExecuteRequest{
			RunID: "untrusted-run", RuntimeDir: "/untrusted/runtime", CWD: f.cwd, Prompt: "input", Model: "chosen", Thinking: "max",
		}}, nil
	}
}

func (f *loopFixture) count(t *testing.T, table string) int {
	t.Helper()
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func (f *loopFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *loopFixture) turnFor(t *testing.T, message domain.Message) domain.Turn {
	t.Helper()
	m, err := f.s.GetMessage(context.Background(), f.group, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.s.GetTurn(context.Background(), f.group, m.Deliveries[0].TurnID)
	if err != nil {
		t.Fatal(err)
	}
	return turn
}

func eventually(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.NewTimer(loopTestTimeout)
	defer deadline.Stop()
	for !predicate() {
		select {
		case <-deadline.C:
			t.Fatal("timed out waiting for durable scheduler behavior")
		case <-time.After(time.Millisecond):
		}
	}
}

func receive[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(loopTestTimeout):
		t.Fatal("timed out waiting for executor/scheduler")
		var zero T
		return zero
	}
}

type loopRun struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func startLoop(t *testing.T, f *loopFixture, options scheduler.Options, prepare scheduler.PrepareFunc) *loopRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &loopRun{cancel: cancel, done: make(chan struct{})}
	go func() {
		r.err = scheduler.Run(ctx, f.s, options, prepare)
		close(r.done)
	}()
	t.Cleanup(func() {
		cancel()
		receive(t, r.done)
	})
	return r
}

func successReceipt() provider.ExecutionReceipt {
	return provider.ExecutionReceipt{
		Started: true, Accepted: true, Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded,
		NativeSessionID: "native-session", DirectChildReaped: true, OutputComplete: true,
	}
}

func TestRunValidatesStartupBoundaryWithoutRecovering(t *testing.T) {
	f := newLoopFixture(t)
	m := f.queue(t, "user", f.sender, "")
	claimed, err := f.s.ClaimTurn(context.Background(), store.ClaimTurnInput{GroupID: f.group, DeliveryID: m.Deliveries[0].ID, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	public := filepath.Join(f.cwd, "public")
	if err := os.Mkdir(public, 0755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(f.cwd, "alias")
	if err := os.Symlink(f.runtime, alias); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*scheduler.Options){
		func(o *scheduler.Options) { o.Recovered = false },
		func(o *scheduler.Options) { o.MaxActive = 0 },
		func(o *scheduler.Options) { o.MaxActive = -1 },
		func(o *scheduler.Options) { o.PollInterval = -1 },
		func(o *scheduler.Options) { o.RuntimeDir = "" },
		func(o *scheduler.Options) { o.RuntimeDir = "relative" },
		func(o *scheduler.Options) { o.RuntimeDir = public },
		func(o *scheduler.Options) { o.RuntimeDir = alias },
		func(o *scheduler.Options) { o.RuntimeDir = filepath.Join(f.cwd, "absent") },
	} {
		options := f.options(1)
		change(&options)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		err := scheduler.Run(ctx, f.s, options, f.prepare(nil))
		cancel()
		if err == nil || errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), f.cwd) {
			t.Fatalf("invalid startup accepted or leaked path: %+v, %v", options, err)
		}
	}
	for _, input := range []struct {
		s       *store.Store
		prepare scheduler.PrepareFunc
	}{{nil, f.prepare(nil)}, {f.s, nil}} {
		if err := scheduler.Run(context.Background(), input.s, f.options(1), input.prepare); err == nil {
			t.Fatal("nil dependency accepted")
		}
	}
	got := f.turnFor(t, m)
	if got.ClaimToken != claimed.ClaimToken || got.State != domain.TurnStateStarting {
		t.Fatalf("Run implicitly recovered existing execution: %+v", got)
	}
}

func TestRunPersistsQueueAcrossReopenAndPublishesOneBoundReply(t *testing.T) {
	f := newLoopFixture(t)
	m := f.queue(t, "user", f.sender, "topic")
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.s, err = store.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RecoverInFlight(context.Background()); err != nil {
		t.Fatal(err)
	}
	checked := make(chan error, 1)
	e := executorFunc(func(ctx context.Context, request provider.ExecuteRequest, callback func(provider.Event) error) provider.ExecutionReceipt {
		turn, err := f.s.GetTurn(ctx, f.group, request.RunID)
		if err != nil || turn.State != domain.TurnStateStarting || turn.DeliveryID != m.Deliveries[0].ID || request.RuntimeDir != f.runtime || request.Model != "chosen" || request.Thinking != "max" {
			checked <- fmt.Errorf("uncommitted claim or overridden trusted request: %+v, %+v, %v", turn, request, err)
			return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}
		}
		for _, event := range []provider.Event{
			{Kind: provider.SessionEvent, NativeSessionID: "native-session"},
			{Kind: provider.AcceptedEvent, NativeSessionID: "native-session"},
			{Kind: provider.ResponseEvent, NativeSessionID: "native-session", Text: "obsolete"},
			{Kind: provider.ResponseEvent, NativeSessionID: "native-session", Text: "answer"},
			{Kind: provider.TerminalEvent, NativeSessionID: "native-session", Terminal: provider.OutcomeSucceeded},
		} {
			if err := callback(event); err != nil {
				checked <- err
				return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, DeliveryFailed: true}
			}
			persisted, err := f.s.GetTurn(ctx, f.group, request.RunID)
			if err != nil || persisted.NativeSessionID != event.NativeSessionID ||
				(event.Kind == provider.SessionEvent && persisted.AcceptedAt != nil) ||
				(event.Kind == provider.AcceptedEvent && persisted.AcceptedAt == nil) ||
				(event.Kind == provider.ResponseEvent && persisted.CandidateText != event.Text) ||
				(event.Kind == provider.TerminalEvent && (persisted.CandidateTerminal != domain.TurnStateSucceeded || persisted.ReplyMessageID != "")) {
				checked <- fmt.Errorf("callback was not synchronous or ordered: %+v, %v", persisted, err)
				return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}
			}
		}
		checked <- nil
		return successReceipt()
	})
	options := f.options(1)
	options.PollInterval = 0 // Exercise the safe default.
	r := startLoop(t, f, options, f.prepare(e))
	if err := receive(t, checked); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return f.turnFor(t, m).State == domain.TurnStateSucceeded })
	r.cancel()
	receive(t, r.done)
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("Run cancellation lost: %v", r.err)
	}
	turn := f.turnFor(t, m)
	reply, err := f.s.GetMessage(context.Background(), f.group, turn.ReplyMessageID)
	if err != nil || reply.SenderID != f.sender || reply.TopicID != "topic" || reply.Content != "answer" || reply.Deliveries == nil || len(reply.Deliveries) != 0 {
		t.Fatalf("reply identity or visibility incorrect: %+v, %v", reply, err)
	}
	if f.count(t, "messages") != 2 || f.count(t, "deliveries") != 1 || f.count(t, "resource_claims") != 0 {
		t.Fatal("settlement duplicated a reply or created wakeups")
	}
}

func TestRunPreparationFailuresDoNotClaimOrExposePayloads(t *testing.T) {
	for _, failure := range []string{"error", "nil-executor", "typed-nil-executor", "invalid-resource"} {
		t.Run(failure, func(t *testing.T) {
			f := newLoopFixture(t)
			f.queue(t, "user", f.sender, "")
			var nilExecutor executorFunc
			prepare := func(ctx context.Context, p store.PendingDelivery) (scheduler.PreparedExecution, error) {
				prepared, _ := f.prepare(nil)(ctx, p)
				switch failure {
				case "error":
					return prepared, errors.New("PRIVATE-PROMPT-CREDENTIAL")
				case "typed-nil-executor":
					prepared.Executor = nilExecutor
				case "invalid-resource":
					prepared.Executor = executorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
						panic("invalid preparation reached external execution")
					})
					prepared.ResourceKeys = []string{"PRIVATE\nKEY"}
				}
				return prepared, nil
			}
			r := startLoop(t, f, f.options(1), prepare)
			receive(t, r.done)
			if r.err == nil || strings.Contains(r.err.Error(), "PRIVATE") || errors.Is(r.err, context.Canceled) {
				t.Fatalf("preparation error lost or exposed: %v", r.err)
			}
			if f.count(t, "turns") != 0 || f.count(t, "resource_claims") != 0 {
				t.Fatal("failed preparation fabricated a claim or acceptance")
			}
			page, err := f.s.ListPendingDeliveries(context.Background(), store.PendingOptions{})
			if err != nil || len(page.Items) != 1 || page.Items[0].Delivery.State != domain.DeliveryStateQueued {
				t.Fatalf("failed preparation removed queued delivery: %+v, %v", page, err)
			}
		})
	}
}
