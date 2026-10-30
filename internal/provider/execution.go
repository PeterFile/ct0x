package provider

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Outcome string
type EventKind string

const (
	OutcomeNotStarted Outcome   = "not_started"
	OutcomeUnknown    Outcome   = "unknown"
	OutcomeSucceeded  Outcome   = "succeeded"
	OutcomeFailed     Outcome   = "failed"
	OutcomeAborted    Outcome   = "aborted"
	OutcomeTruncated  Outcome   = "truncated"
	SessionEvent      EventKind = "session"
	AcceptedEvent     EventKind = "accepted"
	ResponseEvent     EventKind = "response"
	TerminalEvent     EventKind = "terminal"
)

// ExecuteRequest starts a fresh run, never resumes one. RuntimeDir must be an
// existing private platform-owned directory with trusted ancestors; RunID reserves
// a new child directory exclusively. CWD is explicit and grants no sandbox.
// Model is optional and uses the CLI's model selector. Prompt travels over stdin;
// Pi trims its surrounding whitespace. Credentials remain with the original CLI.
// RunID is 1-64 ASCII letters/digits/underscores/hyphens, with alphanumeric ends;
// prompts are at most 1 MiB. Raw logs retain at most 1 MiB per stream.
type ExecuteRequest struct {
	RunID      string `json:"run_id"`
	CWD        string `json:"cwd"`
	RuntimeDir string `json:"runtime_dir"`
	Prompt     string `json:"prompt"`
	Model      string `json:"model"`
}

// Response events are completed assistant text snapshots, replacing earlier
// snapshots (including failed retry responses), not streaming deltas or thoughts.
// Terminal is native evidence, not permission to release external resources.
// JSONL frames are capped at 64 KiB; oversized/invalid output makes the outcome
// unknown while draining continues. Response text is bounded by that frame size.
type Event struct {
	Kind            EventKind `json:"kind"`
	NativeSessionID string    `json:"native_session_id"`
	Text            string    `json:"text,omitempty"`
	Terminal        Outcome   `json:"terminal,omitempty"`
}

// Started and Accepted observe only this invocation. With OutcomeUnknown, false
// values do not disprove earlier execution of the same RunID; history is not loaded.
// Only Outcome summarizes the whole execution/delivery. Terminal preserves
// observed native evidence even if delivery, cancellation or output later fails.
// Reaping the direct child never proves descendants or external tools stopped.
// OutputComplete verifies clean child/output completion, not descendant shutdown.
type ExecutionReceipt struct {
	Started           bool    `json:"started"`
	Accepted          bool    `json:"accepted"`
	Outcome           Outcome `json:"outcome"`
	Terminal          Outcome `json:"terminal,omitempty"`
	DeliveryFailed    bool    `json:"delivery_failed"`
	NativeSessionID   string  `json:"native_session_id"`
	DirectChildReaped bool    `json:"direct_child_reaped"`
	OutputComplete    bool    `json:"output_complete"`
	ExitCode          int     `json:"exit_code"`
	StdoutPath        string  `json:"stdout_path"`
	StderrPath        string  `json:"stderr_path"`
	LogsTruncated     bool    `json:"logs_truncated"`
	Reason            string  `json:"reason"`
}

// Callbacks run serially as records arrive, must return promptly, and must not
// retain mutable shared state unsafely. An error cancels this invocation, disables
// later callbacks and makes delivery/outcome unknown; Execute never retries.
// No callbacks run after Execute returns. A nil callback discards delivery.
type Executor interface {
	Execute(ctx context.Context, request ExecuteRequest, onEvent func(Event) error) ExecutionReceipt
}

type piExecutor struct {
	path    string
	timeout time.Duration
}

// NewPiExecutor binds one executable. This adapter supports only isolated plain
// text turns; it does not advertise tools, resume, steering or compaction APIs.
// A nonpositive timeout defaults to ten minutes; the caller's deadline also applies.
func NewPiExecutor(path string, timeout time.Duration) (Executor, error) {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return nil, errors.New("可执行文件必须使用绝对路径")
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	return piExecutor{path: path, timeout: timeout}, nil
}

var executionID = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_-]{0,62}[A-Za-z0-9])?$`)

func (e piExecutor) Execute(ctx context.Context, request ExecuteRequest, onEvent func(Event) error) ExecutionReceipt {
	r := ExecutionReceipt{Outcome: OutcomeNotStarted, ExitCode: -1, Reason: "执行参数无效"}
	cwd, cwdErr := os.Stat(request.CWD)
	runtime, runtimeErr := os.Lstat(filepath.Clean(request.RuntimeDir))
	if !filepath.IsAbs(request.CWD) || !filepath.IsAbs(request.RuntimeDir) ||
		cwdErr != nil || !cwd.IsDir() || runtimeErr != nil || !runtime.IsDir() || runtime.Mode().Perm()&0077 != 0 ||
		!executionID.MatchString(request.RunID) || strings.TrimSpace(request.Prompt) == "" || len(request.Prompt) > 1<<20 ||
		len(request.Model) > 256 || strings.ContainsAny(request.Model, "\x00\r\n") {
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	if ctx.Err() != nil {
		r.Reason = "执行在启动前已取消或超时"
		return r
	}
	dir := filepath.Join(request.RuntimeDir, request.RunID)
	r.Reason = "无法独占创建执行日志目录"
	if err := os.Mkdir(dir, 0700); err != nil {
		if os.IsExist(err) {
			r.Outcome = OutcomeUnknown
			r.Reason = "执行标识已存在，历史执行结果未知，需对账后再重试"
		}
		return r
	}
	r.StdoutPath, r.StderrPath = filepath.Join(dir, "stdout.jsonl"), filepath.Join(dir, "stderr.log")
	stdout, err := os.OpenFile(r.StdoutPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return r
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(r.StderrPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return r
	}
	defer stderr.Close()
	outLog, errLog := &executionLog{file: stdout, cancel: cancel}, &executionLog{file: stderr, cancel: cancel}
	out := &piOutput{log: outLog, receipt: &r, callback: onEvent, cancel: cancel, runID: request.RunID}
	args := []string{"--offline", "--mode", "json", "-p", "--no-extensions", "--no-mcp", "--no-tools", "--no-skills", "--no-prompt-templates", "--no-context-files", "--no-approve", "--session-dir", dir, "--session-id", request.RunID}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	cmd := exec.CommandContext(ctx, e.path, args...)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = request.CWD, strings.NewReader(request.Prompt), out, errLog
	// WaitDelay bounds inherited pipes, not descendants' lifetimes. Wait joins
	// the copying goroutines before receipt inspection or callback state returns.
	cmd.WaitDelay = 100 * time.Millisecond
	if cmd.Start() != nil {
		r.Reason = "无法启动模型进程，提示词未被接收"
		return r
	}
	r.Started, r.Outcome = true, OutcomeUnknown
	err = cmd.Wait()
	r.DirectChildReaped = cmd.ProcessState != nil
	if r.DirectChildReaped {
		r.ExitCode = cmd.ProcessState.ExitCode()
	}
	outClose, errClose := stdout.Close(), stderr.Close()
	r.LogsTruncated = outLog.truncated || errLog.truncated
	r.OutputComplete = err == nil && !out.damaged && len(out.line) == 0 && !out.dropping && !outLog.failed && !errLog.failed && outClose == nil && errClose == nil
	r.Reason = "执行结果未知，需对账后再重试"
	if r.OutputComplete && ctx.Err() == nil && !r.DeliveryFailed && r.Terminal != "" {
		r.Outcome = r.Terminal
		r.Reason = map[Outcome]string{OutcomeSucceeded: "执行完成", OutcomeFailed: "模型执行失败", OutcomeAborted: "模型执行已中止", OutcomeTruncated: "模型输出被截断"}[r.Outcome]
	}
	if r.DeliveryFailed {
		r.Reason = "执行事件保存失败，交付与执行结果未知"
	}
	return r
}
