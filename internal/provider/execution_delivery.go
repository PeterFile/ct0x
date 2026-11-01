package provider

import (
	"context"
	"sync"
)

// Limits include the in-flight callback: at most 64 events and 1 MiB of string
// payload (identity, text, kind and terminal). Fixed event storage is separately
// bounded by the count; each parsed JSONL frame remains capped at 64 KiB.
const executionDeliveryEventLimit = 64
const executionDeliveryByteLimit = 1 << 20

// The parser owns native receipt fields; the sole consumer owns callback failure.
// The mutex only protects queue accounting and stop/overflow, never callback work.
// Pending events stop permanently on overflow, callback failure or cancellation.
type executionDelivery struct {
	ctx      context.Context
	cancel   context.CancelFunc
	callback func(Event) error
	queue    chan Event
	done     chan struct{}

	mu                  sync.Mutex
	count, bytes        int
	stopped, overflowed bool
	failed              bool // Consumer-only; read after done.
}

func newExecutionDelivery(ctx context.Context, cancel context.CancelFunc, callback func(Event) error) *executionDelivery {
	if callback == nil {
		return nil
	}
	d := &executionDelivery{ctx: ctx, cancel: cancel, callback: callback, queue: make(chan Event, executionDeliveryEventLimit), done: make(chan struct{})}
	go d.deliver()
	return d
}

func executionEventBytes(event Event) int {
	return len(event.Kind) + len(event.NativeSessionID) + len(event.Text) + len(event.Terminal)
}

// enqueue never waits for callback progress. Overflow cancels the invocation but
// leaves parsing/draining active, so observed native terminal evidence survives.
func (d *executionDelivery) enqueue(event Event) {
	if d == nil {
		return
	}
	size := executionEventBytes(event)
	d.mu.Lock()
	if d.stopped || d.ctx.Err() != nil {
		d.mu.Unlock()
		return
	}
	if d.count < executionDeliveryEventLimit && size <= executionDeliveryByteLimit-d.bytes {
		select {
		case d.queue <- event:
			d.count++
			d.bytes += size
			d.mu.Unlock()
			return
		default:
		}
	}
	d.stopped, d.overflowed = true, true
	d.mu.Unlock()
	d.cancel()
}

func (d *executionDelivery) deliver() {
	defer close(d.done)
	for event := range d.queue {
		d.mu.Lock()
		stopped := d.stopped || d.ctx.Err() != nil
		// Passing this check claims the one in-flight callback. An overflow
		// after this point may not interrupt it, but prevents all later claims.
		d.mu.Unlock()
		if stopped {
			return
		}
		err := d.callback(event)
		d.mu.Lock()
		d.count--
		d.bytes -= executionEventBytes(event)
		if err != nil {
			d.stopped = true
		}
		d.mu.Unlock()
		if err != nil {
			d.failed = true
			d.cancel()
			return
		}
	}
}

// finish is called only after Wait joins all pipe readers (or Start fails).
// It must join even a cancelled/failed in-flight callback, never abandon it.
func (d *executionDelivery) finish() bool {
	if d == nil {
		return false
	}
	close(d.queue)
	<-d.done
	return d.failed || d.overflowed
}
