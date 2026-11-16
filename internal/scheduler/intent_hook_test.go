package scheduler_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func TestSchedulerIntentHookOuterCycleAndNil(t *testing.T) {
	for _, hooked := range []bool{true, false} {
		name := "nil"
		if hooked {
			name = "hook"
		}
		t.Run(name, func(t *testing.T) {
			f := newLoopFixture(t)
			ready := map[string]bool{}
			for _, lane := range []struct {
				sender string
				count  int
			}{{"user", 112}, {f.sender, 48}} {
				for i := 1; i <= lane.count; i++ {
					recipient := f.sender
					if i%16 == 0 {
						recipient = f.member(t)
					}
					m := f.queue(t, lane.sender, recipient, "")
					ready[m.Deliveries[0].ID] = i%16 == 0
				}
			}
			started := make(chan provider.ExecuteRequest, 16)
			e := executorFunc(func(ctx context.Context, request provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
				started <- request
				<-ctx.Done()
				return provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeUnknown}
			})
			options := f.options(9)
			options.PollInterval = time.Hour
			var hookCalls, humans, work int
			var hookCtx context.Context
			var differentContext bool
			if hooked {
				options.IntentPass = func(ctx context.Context) {
					hookCalls++
					hookCtx = ctx
				}
			}
			prepare := func(ctx context.Context, pending store.PendingDelivery) (scheduler.PreparedExecution, error) {
				if hooked && ctx != hookCtx {
					differentContext = true
				}
				if pending.SenderID == "user" {
					humans++
				} else {
					work++
				}
				if !ready[pending.Delivery.ID] {
					return scheduler.PreparedExecution{}, scheduler.ErrNotReady
				}
				return f.prepare(e)(ctx, pending)
			}
			r := startLoop(t, f, options, prepare)
			for range 8 {
				receive(t, started)
			}
			r.cancel()
			receive(t, r.done)
			wantCalls := 0
			if hooked {
				wantCalls = 1
			}
			if hookCalls != wantCalls || differentContext || humans != 96 || work != 32 || f.count(t, "turns") != 8 {
				t.Fatalf("outer cycle changed: hooks=%d want=%d same-context=%t human=%d work=%d turns=%d",
					hookCalls, wantCalls, !differentContext, humans, work, f.count(t, "turns"))
			}
			if !errors.Is(r.err, context.Canceled) {
				t.Fatalf("drain lost cancellation: %v", r.err)
			}
		})
	}
}

func TestSchedulerIntentHookCompletionStartsAnotherCycle(t *testing.T) {
	f := newLoopFixture(t)
	f.queue(t, "user", f.sender, "")
	f.queue(t, "user", f.member(t), "")
	started := make(chan invocation, 2)
	options := f.options(1)
	options.PollInterval = time.Hour
	var calls atomic.Int64
	options.IntentPass = func(context.Context) { calls.Add(1) }
	r := startLoop(t, f, options, f.prepare(controlledExecutor(started)))
	first := receive(t, started)
	if got := calls.Load(); got != 1 {
		t.Fatalf("initial cycle hooks=%d, want 1", got)
	}
	first.complete <- successReceipt()
	receive(t, started)
	if got := calls.Load(); got != 2 {
		t.Fatalf("finished event did not start exactly one new pass: %d", got)
	}
	r.cancel()
	receive(t, r.done)
}

func TestSchedulerIntentHookContextFences(t *testing.T) {
	t.Run("pre-cancel", func(t *testing.T) {
		f := newLoopFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var calls int
		options := f.options(1)
		options.IntentPass = func(context.Context) { calls++ }
		err := scheduler.Run(ctx, f.s, options, f.prepare(nil))
		if !errors.Is(err, context.Canceled) || calls != 0 || f.count(t, "turns") != 0 {
			t.Fatalf("pre-cancelled run did work: hooks=%d err=%v", calls, err)
		}
	})
	t.Run("within-hook", func(t *testing.T) {
		f := newLoopFixture(t)
		f.queue(t, "user", f.sender, "")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var calls, prepares int
		options := f.options(1)
		options.IntentPass = func(runCtx context.Context) {
			calls++
			if runCtx.Err() != nil {
				t.Error("hook entered with a cancelled run context")
			}
			cancel()
		}
		err := scheduler.Run(ctx, f.s, options, func(context.Context, store.PendingDelivery) (scheduler.PreparedExecution, error) {
			prepares++
			return scheduler.PreparedExecution{}, scheduler.ErrNotReady
		})
		if calls != 1 || prepares != 0 || f.count(t, "turns") != 0 || !errors.Is(err, context.Canceled) {
			t.Fatalf("hook cancellation fence missing: hooks=%d prepares=%d err=%v", calls, prepares, err)
		}
	})
	t.Run("cancel-during-poll-before-hook", func(t *testing.T) {
		f := newLoopFixture(t)
		f.queue(t, "user", f.sender, "")
		started := make(chan provider.ExecuteRequest, 1)
		e := executorFunc(func(ctx context.Context, request provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
			started <- request
			<-ctx.Done()
			return provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeUnknown}
		})
		options := f.options(1)
		options.PollInterval = 200 * time.Millisecond
		var calls atomic.Int64
		options.IntentPass = func(context.Context) { calls.Add(1) }
		r := startLoop(t, f, options, f.prepare(e))
		receive(t, started)
		conn, err := f.db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
			t.Fatal(err)
		}
		defer conn.ExecContext(context.Background(), "ROLLBACK")
		before := calls.Load()
		// The active-turn poll cannot read through this real exclusive lock.
		// This wait observes ticker opportunities, not a cancellation latency SLA.
		time.Sleep(450 * time.Millisecond)
		if got := calls.Load(); got != before {
			t.Fatalf("hook ran through a blocked cancellation poll: before=%d after=%d", before, got)
		}
		r.cancel()
		if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
			t.Fatal(err)
		}
		receive(t, r.done)
		if got := calls.Load(); got != before || !errors.Is(r.err, context.Canceled) {
			t.Fatalf("cancelled poll opened another hook: before=%d after=%d err=%v", before, got, r.err)
		}
	})
}
