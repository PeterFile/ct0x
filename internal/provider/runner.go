package provider

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type Failure string

const (
	StartFailure     Failure = "not_started"
	ExitFailure      Failure = "exit_failed"
	Cancelled        Failure = "cancelled"
	TimedOut         Failure = "timed_out"
	IncompleteOutput Failure = "incomplete_output"
)

type Runner struct {
	Timeout     time.Duration
	OutputLimit int
}

// Result confirms only the direct child's lifecycle. Output is ephemeral and
// must never be persisted as probe evidence or included in product errors.
// Exited means Wait reaped the child, including after cancellation.
type Result struct {
	Started   bool    `json:"started"`
	Exited    bool    `json:"exited"`
	ExitCode  int     `json:"exit_code"`
	Failure   Failure `json:"failure"`
	Truncated bool    `json:"truncated"`
	Stdout    string  `json:"-"`
	Stderr    string  `json:"-"`
}

// Run executes an absolute path directly, never a shell. Timeout defaults to
// two seconds (maximum five); each output stream defaults to 4 KiB (max 64 KiB).
// WaitDelay bounds inherited pipes; descendants are not claimed to be stopped.
func (r Runner) Run(ctx context.Context, path string, args, env []string) Result {
	result := Result{ExitCode: -1}
	if !filepath.IsAbs(path) {
		result.Failure = StartFailure
		return result
	}
	timeout, limit := r.Timeout, r.OutputLimit
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	if timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	if limit <= 0 {
		limit = 4096
	}
	if limit > 65536 {
		limit = 65536
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout, stderr := &limitedOutput{limit: limit}, &limitedOutput{limit: limit}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 50 * time.Millisecond
	err := cmd.Start()
	if err == nil {
		result.Started = true
		err = cmd.Wait()
		result.Exited = cmd.ProcessState != nil
		if result.Exited {
			result.ExitCode = cmd.ProcessState.ExitCode()
		}
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		result.Failure = TimedOut
	case ctx.Err() != nil:
		result.Failure = Cancelled
	case !result.Started:
		result.Failure = StartFailure
	case errors.Is(err, exec.ErrWaitDelay):
		result.Failure = IncompleteOutput
	case err != nil:
		result.Failure = ExitFailure
	}
	result.Stdout, result.Stderr = string(stdout.data), string(stderr.data)
	result.Truncated = stdout.truncated || stderr.truncated
	return result
}

type limitedOutput struct {
	data      []byte
	limit     int
	truncated bool
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := b.limit - len(b.data); len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	b.data = append(b.data, p...)
	return n, nil
}
