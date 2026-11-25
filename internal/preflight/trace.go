// Package preflight provides opt-in, in-process timing for local startup checks.
package preflight

import (
	"context"
	"errors"
	"io"
	"time"
)

// Observation contains only a fixed call-site stage label, a status, and a
// monotonic elapsed duration. It is diagnostic, never execution acceptance.
type Observation struct {
	Stage   string
	Status  string
	Elapsed time.Duration
}

type observerKey struct{}

// WithObserver installs a trusted, synchronous observer. It must not block or
// do filesystem/network I/O. No diagnostics are emitted when it is absent.
func WithObserver(ctx context.Context, observe func(Observation)) context.Context {
	return context.WithValue(ctx, observerKey{}, observe)
}

// Begin records the start of one stage. Pass only a constant stage label. The
// returned function maps errors to fixed statuses without exposing their text.
func Begin(ctx context.Context, stage string) func(error) {
	observe, _ := ctx.Value(observerKey{}).(func(Observation))
	if observe == nil {
		return func(error) {}
	}
	start := time.Now()
	observe(Observation{Stage: stage, Status: "begin"})
	return func(err error) {
		status := "ok"
		switch {
		case errors.Is(err, context.Canceled):
			status = "canceled"
		case errors.Is(err, context.DeadlineExceeded):
			status = "deadline"
		case err != nil:
			status = "rejected"
		}
		observe(Observation{Stage: stage, Status: status, Elapsed: time.Since(start)})
	}
}

// Run checks cancellation on both sides of a synchronous inspection. The
// inspection must check ctx itself between any longer units of work.
func Run(ctx context.Context, stage string, inspect func() error) error {
	end := Begin(ctx, stage)
	err := ctx.Err()
	if err == nil {
		err = inspect()
	}
	if canceled := ctx.Err(); canceled != nil {
		err = canceled
	}
	end(err)
	if canceled := ctx.Err(); canceled != nil {
		return canceled
	}
	return err
}

var errRejected = errors.New("preflight rejected")

func Check(ctx context.Context, stage string, inspect func() bool) bool {
	return Run(ctx, stage, func() error {
		if !inspect() {
			return errRejected
		}
		return nil
	}) == nil
}

// Reader checks cancellation at read boundaries. A regular-file syscall may
// not be interruptible; no worker keeps reading after return.
func Reader(ctx context.Context, source io.Reader) io.Reader {
	return reader{ctx, source}
}

type reader struct {
	ctx    context.Context
	source io.Reader
}

func (r reader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.source.Read(p)
	if canceled := r.ctx.Err(); canceled != nil {
		return n, canceled
	}
	return n, err
}
