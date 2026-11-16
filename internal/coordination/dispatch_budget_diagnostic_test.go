package coordination

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// Inspect each concrete node, not evidence borrowed from one of its descendants.
func dispatchBudgetLogError(t *testing.T, path string, err error) {
	t.Helper()
	if err == nil {
		t.Logf("error[%s]=nil", path)
		return
	}
	var phase, unknown, rollback any = "unsupported", "unsupported", "unsupported"
	if e, ok := err.(interface{ Phase() string }); ok {
		phase = e.Phase()
	}
	if e, ok := err.(interface{ CommitOutcomeUnknown() bool }); ok {
		unknown = e.CommitOutcomeUnknown()
	}
	if e, ok := err.(interface{ RollbackConfirmed() bool }); ok {
		rollback = e.RollbackConfirmed()
	}
	t.Logf("error[%s] type=%T text=%q Phase=%v CommitOutcomeUnknown=%v RollbackConfirmed=%v deadline=%t canceled=%t",
		path, err, err.Error(), phase, unknown, rollback, errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled))
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		for i, child := range e.Unwrap() {
			dispatchBudgetLogError(t, fmt.Sprintf("%s/%d", path, i), child)
		}
	case interface{ Unwrap() error }:
		dispatchBudgetLogError(t, path+"/unwrap", e.Unwrap())
	}
}

type dispatchBudgetObservation struct {
	counts        [5]int64
	assignments   int64
	ordinary      domain.Message
	ordinaryTurns int64
	tail          domain.DispatchView
}

// Reads are outside the measured call and cannot acknowledge its rollback.
func dispatchBudgetRead(t *testing.T, f intentFixture, ordinary domain.Message, items []store.DispatchDueCandidate, faults int, label string) dispatchBudgetObservation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pumpSchedulerWatchdog)
	defer cancel()
	var out dispatchBudgetObservation
	err := f.db.QueryRowContext(ctx, `SELECT
	 (SELECT count(*) FROM messages), (SELECT count(*) FROM message_log),
	 (SELECT count(*) FROM deliveries), (SELECT count(*) FROM turns),
	 (SELECT count(*) FROM resource_claims), (SELECT count(*) FROM coordination_assignments),
	 (SELECT count(*) FROM turns WHERE delivery_id=?)`, ordinary.Deliveries[0].ID).Scan(
		&out.counts[0], &out.counts[1], &out.counts[2], &out.counts[3], &out.counts[4], &out.assignments, &out.ordinaryTurns)
	if err != nil {
		dispatchBudgetLogError(t, label+"/counts", err)
		t.Fatal("bounded diagnostic counts unavailable")
	}
	out.ordinary, err = f.s.GetMessage(ctx, f.thread.GroupID, ordinary.ID)
	if err != nil {
		dispatchBudgetLogError(t, label+"/ordinary", err)
		t.Fatal("bounded ordinary readback unavailable")
	}
	t.Logf("observation %s counts[messages,message_log,deliveries,turns,resource_claims]=%v assignments=%d ordinary-message=%s deliveries=%+v ordinary-turn-count=%d",
		label, out.counts, out.assignments, out.ordinary.ID, out.ordinary.Deliveries, out.ordinaryTurns)
	for i := len(items) - 1 - faults; i < len(items); i++ {
		item := items[i]
		view, err := f.s.GetDispatchRequest(ctx, f.thread.GroupID, item.RequestID)
		if err != nil {
			dispatchBudgetLogError(t, fmt.Sprintf("%s/readback/%d", label, i), err)
			t.Fatalf("bounded Dispatch readback unavailable: index=%d key=%s", i, item.RequestID)
		}
		t.Logf("observation %s index=%d original=%+v request=%+v state=%+v receipt=%+v",
			label, i, item, view.Request, view.State, view.State.Delivery)
		if i == len(items)-1 {
			out.tail = view
			continue
		}
		s := view.State
		unchanged := s.Status == domain.DispatchStatusPending && s.Generation == 1 && s.AttemptCount == 0 &&
			s.LastErrorCode == "" && s.HeldReason == "" && s.Delivery == nil && s.NextAttemptAt.Equal(item.NextAttemptAt)
		t.Logf("observation %s fault-index=%d pending-gen1-attempt0-no-error-no-receipt-original-key=%t (not rollback evidence)", label, i, unchanged)
		if !unchanged {
			t.Errorf("fault state changed: index=%d state=%+v receipt=%+v", i, s, s.Delivery)
		}
	}
	return out
}

// This isolates a resumed pass's budget; it is not scheduler/release acceptance.
func TestDispatchBudgetDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget time.Duration
		faults int
	}{
		{"direct1s", time.Second, 0},
		{"direct10s", 10 * time.Second, 0},
		{"passAfterOneFault", pumpSchedulerWatchdog, 1},
		{"passAfterTwoFaults", pumpSchedulerWatchdog, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := intentConfigured(t)
			for range 51 {
				f.request(t)
			}
			ordinary := pumpSchedulerQueue(t, f, f.worker)
			if len(ordinary.Deliveries) != 1 {
				t.Fatalf("ordinary fixture deliveries: %+v", ordinary.Deliveries)
			}
			at := time.Now().UTC().Add(time.Second)
			prefix, err := f.s.ListDueDispatchRequests(context.Background(), at, store.DispatchDueOptions{})
			if err != nil || len(prefix.Items) != 50 || prefix.NextCursor == nil {
				t.Fatalf("prefix setup: %+v %v", prefix, err)
			}
			tailPage, err := f.s.ListDueDispatchRequests(context.Background(), at, store.DispatchDueOptions{After: prefix.NextCursor})
			if err != nil || len(tailPage.Items) != 1 || tailPage.NextCursor != nil {
				t.Fatalf("tail setup: %+v %v", tailPage, err)
			}
			intentExec(t, f.db, "CREATE TABLE pump_bad_prefix(id TEXT PRIMARY KEY)")
			for _, item := range prefix.Items {
				intentExec(t, f.db, "INSERT INTO pump_bad_prefix VALUES(?)", item.RequestID)
			}
			intentExec(t, f.db, `CREATE TRIGGER pump_prefix_fault BEFORE UPDATE ON coordination_dispatch_state
			 WHEN EXISTS(SELECT 1 FROM pump_bad_prefix WHERE id=NEW.request_id)
			 BEGIN SELECT RAISE(ABORT,'persistent scheduler intent fault'); END`)
			items := append(append([]store.DispatchDueCandidate(nil), prefix.Items...), tailPage.Items...)
			positions := make(map[string]int, len(items))
			t.Logf("observation-only fresh fixture: cutoff=%s group=%s coordinator=%s worker=%s ordinary=%s; ordered51 uses zero-based original indices, not CI IDs/run IDs",
				at.Format(time.RFC3339Nano), f.thread.GroupID, f.coord, f.worker, ordinary.ID)
			for i, item := range items {
				if _, exists := positions[item.RequestID]; exists || item.Generation != 1 {
					t.Fatalf("invalid original candidate: index=%d item=%+v", i, item)
				}
				positions[item.RequestID] = i
				t.Logf("ordered51 index=%d original=%+v", i, item)
			}
			tail := items[50]
			t.Logf("this fixture's original-tail-index=50 tailkey=%s original-generation=%d next-attempt=%s",
				tail.RequestID, tail.Generation, tail.NextAttemptAt.Format(time.RFC3339Nano))
			originalIndex := func(cursor *store.DispatchDueCursor) int {
				if cursor == nil {
					t.Fatal("resumed Dispatch cursor is nil")
				}
				i, ok := positions[cursor.RequestID]
				if !ok || *cursor != (store.DispatchDueCursor{NextAttemptAt: items[i].NextAttemptAt, RequestID: items[i].RequestID}) {
					t.Fatalf("cursor is not an exact original key: %+v", cursor)
				}
				return i
			}
			if originalIndex(prefix.NextCursor) != 49 {
				t.Fatal("prefix cursor is not original index 49")
			}
			inspectFaults := max(1, tc.faults)
			before := dispatchBudgetRead(t, f, ordinary, items, inspectFaults, "before")
			if before.assignments != 0 || before.tail.State.Generation != tail.Generation ||
				before.tail.State.Status != domain.DispatchStatusPending || before.tail.State.Delivery != nil {
				t.Fatal("fixture is not an untouched Dispatch-only pending sweep")
			}

			var result domain.DispatchMaterialization
			var callErr error
			if tc.faults == 0 {
				ctx, cancel := context.WithTimeout(context.Background(), tc.budget)
				started := time.Now()
				result, callErr = f.s.MaterializeDispatchRequest(ctx, tail.RequestID, tail.Generation, time.Now())
				elapsed, returned, callerErr := time.Since(started), time.Now(), ctx.Err()
				cancel()
				t.Logf("observation direct budget=%s call-start=%s call-return=%s elapsed=%s caller-ctx.Err=%v result=%+v returned-receipt=%+v",
					tc.budget, started.Format(time.RFC3339Nano), returned.Format(time.RFC3339Nano), elapsed, callerErr, result, result.Delivery)
			} else {
				p := intentCore(t, f.s, at)
				beforeIndex := 49 - tc.faults
				key := items[beforeIndex]
				// Resume only the legal original sweep state, without replaying earlier work.
				p.cutoff = at
				p.dispatchAfter = &store.DispatchDueCursor{NextAttemptAt: key.NextAttemptAt, RequestID: key.RequestID}
				p.assignmentExhausted, p.dispatchExhausted, p.next = true, false, intentDispatch
				type boundary struct {
					elapsed time.Duration
					before  store.DispatchDueCursor
					index   int
				}
				boundaries := make([]boundary, 0, tc.faults+1)
				var runStarted time.Time
				p.now = func() time.Time {
					elapsed := time.Since(runStarted)
					i := originalIndex(p.dispatchAfter) + 1
					if i >= len(items) || i != beforeIndex+1+len(boundaries) {
						t.Fatalf("illegal derived candidate boundary: index=%d cursor=%+v", i, p.dispatchAfter)
					}
					// No logging, Store reads, waits, or deadline changes in this hook.
					boundaries = append(boundaries, boundary{elapsed: elapsed, before: *p.dispatchAfter, index: i})
					return at
				}
				t.Logf("observation pass seed: before-index=%d cursor=%+v cutoff=%s dispatch-exhausted=%t assignment-exhausted=%t next=%d; one run, production child=1s outer-watchdog=%s",
					originalIndex(p.dispatchAfter), p.dispatchAfter, p.cutoff.Format(time.RFC3339Nano), p.dispatchExhausted, p.assignmentExhausted, p.next, tc.budget)
				ctx, cancel := context.WithTimeout(context.Background(), tc.budget)
				runStarted = time.Now()
				callErr = p.run(ctx)
				elapsed, returned, callerErr := time.Since(runStarted), time.Now(), ctx.Err()
				cancel()
				t.Logf("observation pass run-start=%s run-return=%s elapsed=%s outer-caller-ctx.Err=%v (not child ctx.Err) final-index=%d cursor=%+v cutoff=%s dispatch-exhausted=%t assignment-exhausted=%t next=%d",
					runStarted.Format(time.RFC3339Nano), returned.Format(time.RFC3339Nano), elapsed, callerErr, originalIndex(p.dispatchAfter), p.dispatchAfter,
					p.cutoff.Format(time.RFC3339Nano), p.dispatchExhausted, p.assignmentExhausted, p.next)
				for _, b := range boundaries {
					t.Logf("observation boundary time-since-run-start=%s previous-original-key=%+v derived-index=%d original=%+v tail=%t",
						b.elapsed, b.before, b.index, items[b.index], b.index == 50)
				}
				t.Log("boundary timing bounds elapsed time before materialization; it does not expose the private child's exact deadline. p.run exposes no materialization receipt; its aggregate error/nil is not tail progress.")
			}
			dispatchBudgetLogError(t, "call", callErr)
			after := dispatchBudgetRead(t, f, ordinary, items, inspectFaults, "after")
			ordinarySame := reflect.DeepEqual(before.ordinary, after.ordinary)
			turnsSame := before.ordinaryTurns == after.ordinaryTurns
			t.Logf("observation ordinary-message/delivery-unchanged=%t ordinary-turn-count-unchanged=%t persisted-tail-receipt=%t; readback is not rollback confirmation; not a release PASS",
				ordinarySame, turnsSame, after.tail.State.Delivery != nil)
			if !ordinarySame || !turnsSame {
				t.Error("diagnostic altered the ordinary message/delivery/turn relationship")
			}
			if receipt := after.tail.State.Delivery; receipt != nil && receipt.MaterializedFromGeneration != tail.Generation {
				t.Errorf("persisted receipt is not for the exact original tail generation: original=%+v receipt=%+v", tail, receipt)
			}
			if tc.faults == 0 && callErr == nil {
				s := after.tail.State
				want := domain.DispatchMaterialization{Disposition: string(s.Status), Generation: s.Generation, Delivery: s.Delivery, HeldReason: s.HeldReason}
				if !reflect.DeepEqual(result, want) {
					t.Errorf("returned materialization differs from independent persisted tail state: returned=%+v receipt=%+v persisted=%+v receipt=%+v", result, result.Delivery, want, want.Delivery)
				}
			}
		})
	}
}
