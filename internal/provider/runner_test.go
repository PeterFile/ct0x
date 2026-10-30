package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestRunnerUsesLiteralArgumentsAndReportsExit(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "unexpected")
	arg := "$(touch '" + marker + "')"
	path := executable(t, filepath.Join(root, "literal ; executable"), "printf '%s' \"$1\"; printf error >&2; exit 7")
	got := (Runner{}).Run(context.Background(), path, []string{arg}, nil)
	if !got.Started || !got.Exited || got.ExitCode != 7 || got.Failure != ExitFailure || got.Stdout != arg || got.Stderr != "error" {
		t.Fatalf("unexpected exit receipt: %+v", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("arguments were interpreted by a shell")
	}
	missing := (Runner{}).Run(context.Background(), filepath.Join(root, "missing"), nil, nil)
	if missing.Started || missing.Exited || missing.ExitCode != -1 || missing.Failure != StartFailure {
		t.Fatalf("missing executable was not classified: %+v", missing)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := (Runner{}).Run(ctx, path, nil, nil); got.Started || got.Exited || got.Failure != Cancelled {
		t.Fatalf("pre-start cancellation: %+v", got)
	}
}

func TestRunnerBoundsBothOutputStreamsWithoutBlocking(t *testing.T) {
	path := executable(t, filepath.Join(t.TempDir(), "output"), "i=0; while [ \"$i\" -lt 1000 ]; do printf 0123456789; printf abcdefghij >&2; i=$((i+1)); done")
	got := (Runner{OutputLimit: 32}).Run(context.Background(), path, nil, nil)
	if !got.Started || !got.Exited || got.ExitCode != 0 || got.Failure != "" || !got.Truncated || len(got.Stdout) != 32 || len(got.Stderr) != 32 {
		t.Fatalf("output must be drained but retained within bounds: %+v", got)
	}
}

func TestRunnerReapsDirectChildAndBoundsInheritedPipes(t *testing.T) {
	for _, mode := range []Failure{TimedOut, Cancelled, IncompleteOutput} {
		t.Run(string(mode), func(t *testing.T) {
			end := "wait"
			if mode == IncompleteOutput {
				end = "exit 0"
			}
			path := executable(t, filepath.Join(t.TempDir(), "descendant"), "sleep 5 &\nprintf '%s %s\\n' \"$$\" \"$!\"\n"+end)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == Cancelled {
				timer := time.AfterFunc(75*time.Millisecond, cancel)
				defer timer.Stop()
			}
			start := time.Now()
			got := (Runner{Timeout: 150 * time.Millisecond}).Run(ctx, path, nil, nil)
			var parent, child int
			if n, _ := fmt.Sscanf(got.Stdout, "%d %d", &parent, &child); n != 2 {
				t.Fatalf("missing subprocess PIDs: %+v", got)
			}
			process, _ := os.FindProcess(child)
			defer process.Kill()
			if time.Since(start) > time.Second || !got.Started || !got.Exited || got.Failure != mode {
				t.Fatalf("runner did not return a prompt terminal receipt: %+v", got)
			}
			if _, err := syscall.Wait4(parent, nil, syscall.WNOHANG, nil); err != syscall.ECHILD {
				t.Fatalf("direct child was not reaped by Wait: %v", err)
			}
		})
	}
}
