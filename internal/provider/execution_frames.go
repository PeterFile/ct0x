package provider

import (
	"bytes"
	"context"
	"sync"
)

// These limits include the frame currently being parsed and any ordered damage
// markers. Raw bytes include CR but exclude LF. The producer separately owns one
// fixed 1 MiB framing buffer; this is not the callback/event delivery budget.
const executionFramesCountLimit = 64
const executionFramesByteLimit = 8 << 20

type executionFrame struct {
	data    []byte
	damaged bool
}

// Only tools mode uses this pipeline. The copier owns the log and framing state;
// the sole consumer owns all JSON decoding and native receipt/parser state.
// consume is serial, must finish its work, and must not retain the frame buffer.
// Enqueue never waits on consume. The mutex covers accounting, not copies/parsing.
// After overflow only the accepted prefix is parsed, followed by a damage marker.
// Cancellation does not abandon that prefix or an in-flight parser.
type executionFrames struct {
	log      *executionLog
	cancel   context.CancelFunc
	consume  func(executionFrame)
	queue    chan executionFrame
	done     chan struct{}
	line     []byte
	dropping bool

	mu         sync.Mutex
	count      int
	bytes      int
	overflowed bool
}

func newExecutionFrames(log *executionLog, cancel context.CancelFunc, consume func(executionFrame)) *executionFrames {
	f := &executionFrames{log: log, cancel: cancel, consume: consume, queue: make(chan executionFrame, executionFramesCountLimit),
		done: make(chan struct{}), line: make([]byte, 0, piMemberToolFrameLimit)}
	go f.parse()
	return f
}

func (f *executionFrames) Write(p []byte) (int, error) {
	n, _ := f.log.Write(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		part := p
		if i >= 0 {
			part = p[:i]
		}
		if !f.dropping && len(f.line)+len(part) > piMemberToolFrameLimit {
			f.dropping = true
			// Order the damage at detection, including an oversized line at EOF.
			f.enqueue(nil, true)
		}
		if !f.dropping {
			f.line = append(f.line, part...)
		}
		if i < 0 {
			break
		}
		if !f.dropping {
			f.enqueue(f.line, false)
		}
		f.line, f.dropping = f.line[:0], false
		p = p[i+1:]
	}
	return n, nil
}

func (f *executionFrames) enqueue(data []byte, damaged bool) {
	f.mu.Lock()
	if f.overflowed {
		f.mu.Unlock()
		return
	}
	if f.count >= executionFramesCountLimit || len(data) > executionFramesByteLimit-f.bytes {
		f.overflowed = true
		f.mu.Unlock()
		f.cancel()
		return
	}
	// Reserve before copying so the producer's copy is already in the budget.
	f.count++
	f.bytes += len(data)
	f.mu.Unlock()
	frame := executionFrame{data: make([]byte, len(data)), damaged: damaged}
	copy(frame.data, data)
	select {
	case f.queue <- frame:
	default:
		// The sole producer's reservation normally guarantees channel capacity.
		// Fail closed without blocking if that invariant is ever violated.
		f.mu.Lock()
		f.count--
		f.bytes -= len(data)
		f.overflowed = true
		f.mu.Unlock()
		f.cancel()
	}
}

func (f *executionFrames) parse() {
	defer close(f.done)
	for frame := range f.queue {
		f.consume(frame)
		size := len(frame.data)
		frame = executionFrame{} // Release ownership before releasing its budget.
		f.mu.Lock()
		f.count--
		f.bytes -= size
		f.mu.Unlock()
	}
	f.mu.Lock()
	overflowed := f.overflowed
	f.mu.Unlock()
	if overflowed {
		// No prefix frames remain. Account for the zero-byte in-flight damage
		// marker too, and apply it after the prefix on the sole consumer.
		f.mu.Lock()
		f.count++
		f.mu.Unlock()
		f.consume(executionFrame{damaged: true})
		f.mu.Lock()
		f.count--
		f.mu.Unlock()
	}
}

// finish follows child Wait (or Start failure), so no producer can still write.
// It joins every parser call before event delivery can close or receipts be read.
func (f *executionFrames) finish() {
	if f == nil {
		return
	}
	if len(f.line) != 0 && !f.dropping {
		f.enqueue(nil, true)
	}
	f.line = nil
	close(f.queue)
	<-f.done
}

func (o *executionOutput) recordFrame(frame executionFrame) {
	if frame.damaged {
		o.damaged, o.last = true, ""
		return
	}
	o.recordPi(bytes.TrimSuffix(frame.data, []byte{'\r'}))
}
