package scheduler_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

type invocation struct {
	request  provider.ExecuteRequest
	complete chan provider.ExecutionReceipt
}

func controlledExecutor(started chan<- invocation) provider.Executor {
	return executorFunc(func(ctx context.Context, request provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
		call := invocation{request: request, complete: make(chan provider.ExecutionReceipt, 1)}
		started <- call
		select {
		case receipt := <-call.complete:
			return receipt
		case <-ctx.Done():
			return provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeUnknown}
		}
	})
}

func TestRunParallelMembersRespectCapacityConversationAndMemberLocks(t *testing.T) {
	f := newLoopFixture(t)
	a, b, c, d := f.member(t), f.member(t), f.member(t), f.member(t)
	first := f.queue(t, "user", a, "one")
	second := f.queue(t, "user", a, "one")
	third := f.queue(t, "user", a, "two")
	f.queue(t, "user", b, "one")
	f.queue(t, "user", c, "one")
	last := f.queue(t, "user", d, "one")
	started := make(chan invocation, 8)
	r := startLoop(t, f, f.options(3), f.prepare(controlledExecutor(started)))
	var aCall invocation
	members := map[string]bool{}
	for range 3 {
		call := receive(t, started)
		turn, err := f.s.GetTurn(context.Background(), f.group, call.request.RunID)
		if err != nil || members[turn.MemberID] {
			t.Fatalf("same member overlapped or claim missing: %+v, %v", turn, err)
		}
		members[turn.MemberID] = true
		if turn.MemberID == a {
			aCall = call
		}
	}
	if !members[a] || !members[b] || !members[c] {
		t.Fatalf("independent members failed to run concurrently: %v", members)
	}
	select {
	case call := <-started:
		t.Fatalf("exceeded local capacity: %+v", call.request)
	case <-time.After(20 * time.Millisecond):
	}
	for _, message := range []domain.Message{last, second, third} {
		aCall.complete <- successReceipt()
		aCall = receive(t, started)
		turn, err := f.s.GetTurn(context.Background(), f.group, aCall.request.RunID)
		if err != nil || turn.DeliveryID != message.Deliveries[0].ID {
			t.Fatalf("member/conversation exclusivity or FIFO broken: %+v, %v", turn, err)
		}
		if f.turnFor(t, first).State != domain.TurnStateSucceeded {
			t.Fatal("later execution started before earlier receipt settled")
		}
	}
	r.cancel()
	receive(t, r.done)
}

func TestRunUnknownRetainsCapacityAndResourcesAcrossRecoveryWithoutReplay(t *testing.T) {
	f := newLoopFixture(t)
	first := f.queue(t, "user", f.sender, "")
	other := f.member(t)
	waiting := f.queue(t, "user", other, "")
	var executions atomic.Int64
	e := executorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
		executions.Add(1)
		return provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeUnknown, DirectChildReaped: true, ExitCode: 0}
	})
	withResource := func(ctx context.Context, p store.PendingDelivery) (scheduler.PreparedExecution, error) {
		prepared, err := f.prepare(e)(ctx, p)
		prepared.ResourceKeys = []string{"shared"}
		return prepared, err
	}
	r := startLoop(t, f, f.options(1), withResource)
	eventually(t, func() bool { return f.count(t, "turns") == 1 })
	eventually(t, func() bool { return f.turnFor(t, first).State == domain.TurnStateUnknown })
	time.Sleep(20 * time.Millisecond)
	r.cancel()
	receive(t, r.done)
	if executions.Load() != 1 || f.count(t, "resource_claims") != 1 {
		t.Fatal("returned unknown released capacity/resources or was retried")
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.s, err = store.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if recovered, err := f.s.RecoverInFlight(context.Background()); err != nil || recovered != 1 {
		t.Fatalf("explicit recovery: %d, %v", recovered, err)
	}
	before := f.turnFor(t, first)
	r = startLoop(t, f, f.options(1), withResource)
	time.Sleep(25 * time.Millisecond)
	r.cancel()
	receive(t, r.done)
	if executions.Load() != 1 || f.turnFor(t, first).ClaimToken != before.ClaimToken {
		t.Fatal("Run replayed unknown or called RecoverInFlight itself")
	}
	for _, input := range []store.ClaimTurnInput{
		{GroupID: f.group, DeliveryID: waiting.Deliveries[0].ID, MaxActive: 1},
		{GroupID: f.group, DeliveryID: waiting.Deliveries[0].ID, MaxActive: 10, ResourceKeys: []string{"shared"}},
	} {
		if _, err := f.s.ClaimTurn(context.Background(), input); !errors.Is(err, store.ErrTurnBusy) {
			t.Fatalf("unknown occupancy released after local handle/restart: %v", err)
		}
	}
}

func TestRunDoesNotRetryProvenNotStartedDelivery(t *testing.T) {
	f := newLoopFixture(t)
	m := f.queue(t, "user", f.sender, "")
	var calls atomic.Int64
	e := executorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
		calls.Add(1)
		return provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}
	})
	r := startLoop(t, f, f.options(1), f.prepare(e))
	eventually(t, func() bool { return f.count(t, "turns") == 1 })
	eventually(t, func() bool { return f.turnFor(t, m).State == domain.TurnStateNotStarted })
	time.Sleep(20 * time.Millisecond)
	r.cancel()
	receive(t, r.done)
	if calls.Load() != 1 || f.count(t, "turns") != 1 {
		t.Fatal("not_started delivery was automatically retried")
	}
}
