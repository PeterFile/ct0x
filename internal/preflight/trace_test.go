package preflight

import (
	"context"
	"errors"
	"io"
	"testing"
)

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestReadStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	reader := Reader(ctx, readerFunc(func(p []byte) (int, error) {
		reads++
		cancel() // Cancel during a material read, before producing bytes.
		return copy(p, "fixture bytes"), nil
	}))
	if _, err := io.Copy(io.Discard, io.LimitReader(reader, 1024)); !errors.Is(err, context.Canceled) || reads != 1 {
		t.Fatal("preflight continued reading after cancellation")
	}
}

func TestExpiredStageDoesNotInspect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	var trace []Observation
	ctx = WithObserver(ctx, func(o Observation) { trace = append(trace, o) })
	err := Run(ctx, "fixture_stage", func() error {
		t.Fatal("expired preflight began inspection")
		return nil
	})
	if err != context.DeadlineExceeded || len(trace) != 2 || trace[0].Status != "begin" || trace[1].Status != "deadline" {
		t.Fatal("missing sanitized deadline evidence")
	}
}
