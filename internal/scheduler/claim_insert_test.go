package scheduler_test

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"

	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func TestRunIgnoredTurnInsertDoesNotExecute(t *testing.T) {
	f := newLoopFixture(t)
	message := f.queue(t, "user", f.sender, "claim-insert")
	f.exec(t, `CREATE TRIGGER scheduler_claim_insert_ignore BEFORE INSERT ON turns
	 BEGIN SELECT RAISE(IGNORE); END`)
	var executions atomic.Int32
	executor := executorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
		executions.Add(1)
		return provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}
	})
	prepared := make(chan store.PendingDelivery, 1)
	prepare := func(ctx context.Context, pending store.PendingDelivery) (scheduler.PreparedExecution, error) {
		prepared <- pending
		return f.prepare(executor)(ctx, pending)
	}
	run := startLoop(t, f, f.options(1), prepare)
	pending := receive(t, prepared)
	if pending.GroupID != f.group || !reflect.DeepEqual(pending.Delivery, message.Deliveries[0]) {
		t.Fatalf("scheduler did not reach the legal delivery: %+v", pending)
	}
	// Run drains every launched executor before returning; this is not a timing assertion.
	receive(t, run.done)
	t.Logf("prepare observed the legal delivery; Run returned %v; Execute calls=%d", run.err, executions.Load())
	if run.err == nil || run.err.Error() != "读写调度状态失败，已停止调度，请核对持久化状态" {
		t.Errorf("ignored claim did not stop Run with a safe persistence error: %v", run.err)
	}
	if got := executions.Load(); got != 0 {
		t.Errorf("ignored Turn INSERT reached Execute %d times", got)
	}
	if f.count(t, "turns") != 0 || f.count(t, "resource_claims") != 0 {
		t.Error("ignored claim left execution or resource rows")
	}
	got, err := f.s.GetMessage(context.Background(), f.group, message.ID)
	if err != nil || !reflect.DeepEqual(got, message) {
		t.Errorf("failed scheduler claim changed the queued delivery: %+v, %v", got, err)
	}
}
