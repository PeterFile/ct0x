package scheduler_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func TestRunClaimsStructuralThreeHumanToOneWork(t *testing.T) {
	f := newLoopFixture(t)
	var humans, work []string
	for range 4 {
		work = append(work, f.queue(t, f.sender, f.sender, "work").ID)
	}
	for range 16 {
		humans = append(humans, f.queue(t, "user", f.sender, "human").ID)
	}
	claimed := make(chan string, 32)
	e := executorFunc(func(ctx context.Context, request provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
		turn, err := f.s.GetTurn(ctx, f.group, request.RunID)
		if err != nil {
			return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}
		}
		claimed <- turn.DeliveryID
		return successReceipt()
	})
	// Record message IDs from committed deliveries rather than preparation attempts.
	deliveryToMessage := map[string]string{}
	page, err := f.s.ListPendingDeliveries(context.Background(), store.PendingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range page.Items {
		deliveryToMessage[p.Delivery.ID] = p.Delivery.MessageID
	}
	r := startLoop(t, f, f.options(1), f.prepare(e))
	var got, want []string
	for i := range 4 {
		want = append(want, humans[i*3:i*3+3]...)
		want = append(want, work[i])
	}
	for range len(want) {
		got = append(got, deliveryToMessage[receive(t, claimed)])
	}
	r.cancel()
	receive(t, r.done)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("priority was inferred from role/text, or work starved: got %v, want %v", got, want)
	}
}

func TestRunFallbackPreservesOwedLaneOpportunity(t *testing.T) {
	for _, owed := range []string{"human", "work"} {
		t.Run(owed, func(t *testing.T) {
			f := newLoopFixture(t)
			for range 6 {
				f.queue(t, "user", f.sender, "human")
			}
			for range 3 {
				f.queue(t, f.sender, f.sender, "work")
			}
			page, err := f.s.ListPendingDeliveries(context.Background(), store.PendingOptions{})
			if err != nil {
				t.Fatal(err)
			}
			senders := map[string]string{}
			for _, p := range page.Items {
				senders[p.Delivery.ID] = p.SenderID
			}
			var blocked atomic.Bool
			blocked.Store(owed == "work")
			var humanCount int
			order := make(chan string, 16)
			e := executorFunc(func(ctx context.Context, request provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
				turn, err := f.s.GetTurn(ctx, f.group, request.RunID)
				if err != nil {
					return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}
				}
				lane := "work"
				if senders[turn.DeliveryID] == "user" {
					lane = "human"
					humanCount++
				}
				if owed == "work" && humanCount == 4 || owed == "human" && lane == "work" {
					blocked.Store(false)
				} else if owed == "human" && humanCount == 2 {
					blocked.Store(true)
				}
				order <- lane
				return successReceipt()
			})
			prepare := func(ctx context.Context, p store.PendingDelivery) (scheduler.PreparedExecution, error) {
				if blocked.Load() && (p.SenderID == "user") == (owed == "human") {
					return scheduler.PreparedExecution{}, fmt.Errorf("pending: %w", scheduler.ErrNotReady)
				}
				return f.prepare(e)(ctx, p)
			}
			r := startLoop(t, f, f.options(1), prepare)
			want := []string{"human", "human", "work", "human", "work"}
			if owed == "work" {
				want = []string{"human", "human", "human", "human", "work", "human"}
			}
			got := make([]string, len(want))
			for i := range got {
				got[i] = receive(t, order)
			}
			r.cancel()
			receive(t, r.done)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("fallback erased owed %s opportunity: got %v, want %v", owed, got, want)
			}
		})
	}
}

func TestRunBoundedCursorsPassBusyOrUnreadyPagesInBothLanes(t *testing.T) {
	for _, lane := range []string{"human", "work"} {
		for _, conflict := range []string{"resource", "unready"} {
			t.Run(lane+"/"+conflict, func(t *testing.T) {
				f := newLoopFixture(t)
				owner, blocked, free := f.member(t), f.member(t), f.member(t)
				m := f.queue(t, "user", owner, "owner")
				held, err := f.s.ClaimTurn(context.Background(), store.ClaimTurnInput{
					GroupID: f.group, DeliveryID: m.Deliveries[0].ID, MaxActive: 2, ResourceKeys: []string{"shared"},
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.s.ApplyTurnReceipt(context.Background(), store.TurnClaim{GroupID: f.group, TurnID: held.ID, Token: held.ClaimToken}, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}); err != nil {
					t.Fatal(err)
				}
				sender := "user"
				if lane == "work" {
					sender = f.sender
				}
				wantProgress := make([]string, 0, 206)
				for i := range 205 { // More than even the store's maximum page size.
					queued := f.queue(t, sender, blocked, fmt.Sprint(i))
					wantProgress = append(wantProgress, queued.Deliveries[0].ID)
				}
				last := f.queue(t, sender, free, "free")
				wantProgress = append(wantProgress, last.Deliveries[0].ID)
				progress := make(chan string, len(wantProgress))
				started := make(chan provider.ExecuteRequest, 2)
				e := executorFunc(func(ctx context.Context, request provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
					started <- request
					<-ctx.Done()
					return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}
				})
				var visits atomic.Int64
				var lastVisited atomic.Value
				t.Cleanup(func() {
					if t.Failed() {
						t.Logf("preparation visits before timeout: %d; last=%v", visits.Load(), lastVisited.Load())
					}
				})
				prepare := func(ctx context.Context, p store.PendingDelivery) (scheduler.PreparedExecution, error) {
					lastVisited.Store(p.Delivery.ID)
					if visits.Add(1) <= int64(len(wantProgress)) {
						progress <- p.Delivery.ID
					}
					if p.Delivery.RecipientID == blocked && conflict == "unready" {
						return scheduler.PreparedExecution{}, scheduler.ErrNotReady
					}
					prepared, err := f.prepare(e)(ctx, p)
					if p.Delivery.RecipientID == blocked {
						prepared.ResourceKeys = []string{"shared"}
					}
					return prepared, err
				}
				scanStarted := time.Now()
				r := startLoop(t, f, f.options(2), prepare)
				originalDeadline := time.NewTimer(5 * time.Second)
				defer originalDeadline.Stop()
				deadline := time.NewTimer(loopTestTimeout)
				defer deadline.Stop()
				var request provider.ExecuteRequest
			awaitStart:
				for {
					select {
					case request = <-started:
						break awaitStart
					case <-originalDeadline.C:
						t.Logf("progress at original 5s deadline: preparations=%d; last=%v", visits.Load(), lastVisited.Load())
					case <-deadline.C:
						t.Fatalf("independent execution did not start: preparations=%d; last=%v", visits.Load(), lastVisited.Load())
					}
				}
				for i, want := range wantProgress {
					if got := receive(t, progress); got != want {
						t.Fatalf("cursor failed FIFO progress at %d: got %s, want %s", i, got, want)
					}
				}
				turn, err := f.s.GetTurn(context.Background(), f.group, request.RunID)
				if err != nil || turn.DeliveryID != last.Deliveries[0].ID || visits.Load() < 206 || f.count(t, "turns") != 2 {
					t.Fatalf("blocked page hid independent delivery: %+v, visits=%d, %v", turn, visits.Load(), err)
				}
				t.Logf("cursor progress: %d distinct FIFO deliveries, last=%s; preparations=%d; independent turn=%s; scan elapsed=%s",
					len(wantProgress), last.Deliveries[0].ID, visits.Load(), turn.ID, time.Since(scanStarted))
				r.cancel()
				receive(t, r.done)
				if !errors.Is(r.err, context.Canceled) {
					t.Fatalf("unexpected scheduler failure: %v", r.err)
				}
			})
		}
	}
}

func TestRunScanBudgetWaitsForPollAndWrapsExhaustedLane(t *testing.T) {
	f := newLoopFixture(t)
	for range 205 {
		f.queue(t, "user", f.sender, "")
	}
	var visits atomic.Int64
	prepare := func(context.Context, store.PendingDelivery) (scheduler.PreparedExecution, error) {
		visits.Add(1)
		return scheduler.PreparedExecution{}, scheduler.ErrNotReady
	}
	options := f.options(1)
	options.PollInterval = time.Hour
	r := startLoop(t, f, options, prepare)
	eventually(t, func() bool { return visits.Load() > 0 })
	time.Sleep(30 * time.Millisecond)
	if count := visits.Load(); count > 128 {
		t.Fatalf("one cycle scanned an unbounded backlog: %d", count)
	}
	r.cancel()
	receive(t, r.done)

	// A short exhausted lane must wrap and revisit a newly-ready queued head.
	f = newLoopFixture(t)
	m := f.queue(t, "user", f.sender, "")
	var ready atomic.Bool
	started := make(chan struct{}, 1)
	e := executorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
		started <- struct{}{}
		return successReceipt()
	})
	var scans atomic.Int64
	r = startLoop(t, f, f.options(1), func(ctx context.Context, p store.PendingDelivery) (scheduler.PreparedExecution, error) {
		scans.Add(1)
		if !ready.Load() {
			return scheduler.PreparedExecution{}, scheduler.ErrNotReady
		}
		return f.prepare(e)(ctx, p)
	})
	eventually(t, func() bool { return scans.Load() >= 2 })
	ready.Store(true)
	receive(t, started)
	eventually(t, func() bool { return f.turnFor(t, m).State == domain.TurnStateSucceeded })
}
