package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
)

func TestExecutionCatalogSnapshotPagesToExhaustion(t *testing.T) {
	root := t.TempDir()
	var search provider.Search
	for i := range 205 {
		path := filepath.Join(root, fmt.Sprintf("pi-%03d", i))
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf invoked > '"+filepath.Join(root, "invoked")+"'\n"), 0700); err != nil {
			t.Fatal(err)
		}
		search.Explicit = append(search.Explicit, provider.Location{Name: provider.Pi, Path: path})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Keep discovery metadata, but never invoke any of the catalog sentinels.
	catalog := platform.DiscoverProviders(ctx, search, provider.Runner{})
	items, err := providerSnapshot(catalog)
	if err != nil || len(items) != 205 {
		t.Fatalf("trusted snapshot was truncated at a catalog page: items=%d err=%v", len(items), err)
	}
	seen := make(map[string]bool)
	for _, item := range items {
		if seen[item.ID] {
			t.Fatalf("catalog snapshot repeated %s", item.ID)
		}
		seen[item.ID] = true
	}
	for _, item := range provider.Discover(search) {
		if !seen[item.ID] {
			t.Fatalf("catalog snapshot omitted %s", item.ID)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "invoked")); !os.IsNotExist(err) {
		t.Fatalf("cancelled catalog fixture invoked an executable: %v", err)
	}
}

func TestExecutionShutdownErrorPreservesNonCancellationLeaves(t *testing.T) {
	failure := errors.New("material fixture failure")
	for _, input := range []error{nil, context.Canceled, fmt.Errorf("wrapped: %w", context.Canceled), errors.Join(context.Canceled, fmt.Errorf("nested: %w", context.Canceled))} {
		if err := shutdownError(input); err != nil {
			t.Errorf("pure normal cancellation was not normalized: %v", err)
		}
	}
	for _, input := range []error{failure, errors.Join(failure, context.Canceled), fmt.Errorf("outer: %w", errors.Join(context.Canceled, fmt.Errorf("inner: %w", failure))), context.DeadlineExceeded} {
		want := failure
		if input == context.DeadlineExceeded {
			want = context.DeadlineExceeded
		}
		if err := shutdownError(input); !errors.Is(err, want) {
			t.Errorf("non-cancellation leaf lost: input=%v result=%v", input, err)
		}
	}
}

func TestExecutionHTTPFailureCancelsAndDrainsScheduler(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled, gate, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	failure := errors.New("scheduler drain fixture failure")
	var result error
	go func() {
		result = serve(ctx, &http.Server{Handler: http.NotFoundHandler()}, listener, 50*time.Millisecond, func(ctx context.Context) error {
			<-ctx.Done()
			close(cancelled)
			<-gate
			return errors.Join(failure, ctx.Err())
		})
		close(done)
	}()
	t.Cleanup(func() { cancel(); release(); <-done })
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP failure did not promptly cancel scheduling")
	}
	select {
	case <-done:
		t.Fatalf("HTTP failure returned before scheduler drain: %v", result)
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case <-done:
		if !errors.Is(result, failure) || !errors.Is(result, net.ErrClosed) {
			t.Fatalf("HTTP failure or mixed scheduler drain failure lost: %v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("process did not join scheduler drain")
	}
}

func TestExecutionParentDeadlineIsNotNormalShutdown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := serve(ctx, &http.Server{Handler: http.NotFoundHandler()}, listener, 50*time.Millisecond, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parent deadline was incorrectly normalized as normal shutdown: %v", err)
	}
}

func TestExecutionMixedSchedulerFailureStopsHTTP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	failure := errors.New("real scheduler fixture failure")
	err = serve(ctx, &http.Server{Handler: http.NotFoundHandler()}, listener, 50*time.Millisecond, func(context.Context) error {
		return errors.Join(failure, context.Canceled)
	})
	if !errors.Is(err, failure) || ctx.Err() != nil {
		t.Fatalf("mixed scheduler failure was swallowed or did not stop HTTP: %v parent=%v", err, ctx.Err())
	}
}
