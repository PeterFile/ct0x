package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/preflight"
)

// ErrTurnCancelRequested is the scheduler cause for a persisted cancellation.
var ErrTurnCancelRequested = errors.New("turn cancellation was requested")

type acpExecutor struct {
	config           ACPConfig
	timeout          time.Duration
	member           memberToolConfig
	openCodeDigest   [32]byte
	openCodeAgentSHA string
	agentFile        os.FileInfo
	agentReal        string
}

type acpLaunch struct {
	args             []string
	env              []string
	server           string
	agent            string
	member           memberToolConfig
	seal             *acpSealedProfile
	verify           func(context.Context) bool
	verifyModelCache func() bool
}

// Lifecycle evidence only: never used by exactTool or permission selection.
// Binding includes the immutable version/path/config, run-only server/tools,
// actual launch and original MemberTools config, not a guessed tool identity.
type acpSealedProfile struct {
	binding   [32]byte
	agentFile os.FileInfo
	agentReal string
}

func acpLaunchBinding(c ACPConfig, cwd string, launch acpLaunch) [32]byte {
	return sha256.Sum256(acpJSON([]any{c, cwd, launch.args, launch.env, launch.server, launch.agent, launch.member.digest}))
}

func acpSeal(c ACPConfig, cwd string, launch acpLaunch) *acpSealedProfile {
	info, err := os.Stat(c.AgentPath)
	real, realErr := filepath.EvalSymlinks(c.AgentPath)
	if err != nil || realErr != nil || launch.verify == nil {
		return nil
	}
	return &acpSealedProfile{binding: acpLaunchBinding(c, cwd, launch), agentFile: info, agentReal: real}
}

func (s *acpSealedProfile) valid(c ACPConfig, cwd string, launch acpLaunch) bool {
	info, err := os.Stat(c.AgentPath)
	real, realErr := filepath.EvalSymlinks(c.AgentPath)
	return s != nil && err == nil && realErr == nil && real == s.agentReal && acpStableInfo(s.agentFile, info) && s.binding == acpLaunchBinding(c, cwd, launch)
}

// NewACPExecutor constructs an experimental, opt-in ACP stable-v1 executor.
// No CLI/auth/model probe runs here. Original HOME and credentials stay with the
// CLI. Each Execute creates one child/session/prompt; no resume is supported.
// Trusted executable/configuration paths and ancestors must remain immutable.
func NewACPExecutor(config ACPConfig, timeout time.Duration) (Executor, error) {
	return NewACPExecutorContext(context.Background(), config, timeout)
}

// NewACPExecutorContext is the opt-in, cancellable constructor for a trusted
// ACP factory. ctx covers inspection only; timeout still configures Execute.
// Use the same caller deadline for both. Nothing starts or outlives the call.
func NewACPExecutorContext(ctx context.Context, config ACPConfig, timeout time.Duration) (Executor, error) {
	var names []string
	if err := preflight.Run(ctx, "acp_constructor_validate", func() error {
		if timeout < 0 {
			return ErrAdapterConfiguration
		}
		if err := validateACPConfig(config); err != nil {
			return err
		}
		var err error
		names, err = acpMemberToolProfile(config)
		return err
	}); err != nil {
		return nil, err
	}
	var member memberToolConfig
	if err := preflight.Run(ctx, "acp_constructor_member_config", func() error {
		var err error
		member, err = readACPMemberToolConfig(ctx, config)
		return err
	}); err != nil {
		return nil, err
	}
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	config = cloneACPConfig(config)
	config.ToolNames = names
	e := acpExecutor{config: config, timeout: timeout, member: member}
	if err := preflight.Run(ctx, "acp_constructor_executable_identity", func() error {
		info, err := os.Stat(config.AgentPath)
		if err != nil {
			return ErrAdapterConfiguration
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		real, err := filepath.EvalSymlinks(config.AgentPath)
		if err != nil {
			return ErrAdapterConfiguration
		}
		e.agentFile, e.agentReal = info, real
		return nil
	}); err != nil {
		return nil, err
	}
	if config.VendorProfile == ACPOpenCode119 && config.OpenCode != nil {
		if err := preflight.Run(ctx, "opencode_constructor_image_hash", func() error {
			if e.agentFile.Size() != acpOpenCodeImageSize || acpOpenCodeExecutableSHA256Context(ctx, e.agentReal, e.agentFile) != acpOpenCodeImageSHA256 {
				return ErrAdapterConfiguration
			}
			return nil
		}); err != nil {
			return nil, err
		}
		e.openCodeAgentSHA = acpOpenCodeImageSHA256
		if err := preflight.Run(ctx, "opencode_constructor_dependencies", func() error {
			var err error
			e.openCodeDigest, err = acpOpenCodeDependenciesContext(ctx, *config.OpenCode)
			return err
		}); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e acpExecutor) Execute(ctx context.Context, request ExecuteRequest, callback func(Event) error) ExecutionReceipt {
	return e.execute(ctx, request, callback, acpLaunch{})
}

func (e acpExecutor) execute(ctx context.Context, request ExecuteRequest, callback func(Event) error, launch acpLaunch) (r ExecutionReceipt) {
	// No child in this invocation does not rule out an earlier or concurrent
	// owner of the same RunID. Only an exclusive mkdir proves pre-start ownership.
	r = ExecutionReceipt{Outcome: OutcomeUnknown, ExitCode: -1, Reason: "尚未取得执行目录，历史执行结果未知，需对账后再处理"}
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	defer func() {
		if r.Outcome == OutcomeNotStarted && ctx.Err() != nil {
			r.Reason = "执行在启动前已取消或超时"
		}
	}()
	runConfig := e.config
	if b := runConfig.freshTaskWork; b != nil && request.RunID != b.RunID {
		return r
	}
	if runConfig.Model != "" {
		if request.Model != "" && request.Model != runConfig.Model {
			return r
		}
	} else if runConfig.VendorProfile == ACPAgy15 {
		if !acpValidModel(request.Model) {
			return r
		}
		runConfig.Model = request.Model
	} else {
		return r
	}
	if request.Thinking != "" || !filepath.IsAbs(request.CWD) || !filepath.IsAbs(request.RuntimeDir) || !utf8.ValidString(request.CWD) || !utf8.ValidString(request.RuntimeDir) ||
		!executionID.MatchString(request.RunID) || strings.TrimSpace(request.Prompt) == "" || len(request.Prompt) > 1<<20 || !utf8.ValidString(request.Prompt) {
		return r
	}
	if !preflight.Check(ctx, "acp_execute_directories", func() bool {
		cwd, err := os.Stat(request.CWD)
		if err != nil || !cwd.IsDir() || ctx.Err() != nil {
			return false
		}
		runtime, err := os.Lstat(filepath.Clean(request.RuntimeDir))
		return err == nil && runtime.IsDir() && runtime.Mode().Perm()&0077 == 0
	}) {
		return r
	}
	if launch.server == "" {
		if !preflight.Check(ctx, "acp_execute_member_config", func() bool {
			member, err := readACPMemberToolConfig(ctx, e.config)
			if err != nil || e.member.file == nil || !os.SameFile(e.member.file, member.file) || member.digest != e.member.digest || !member.file.ModTime().Equal(e.member.file.ModTime()) {
				return false
			}
			launch.member = member
			return true
		}) {
			r.Reason = "成员工具配置无效或已变化，历史执行结果未知，需对账后再处理"
			return r
		}
	}
	if ctx.Err() != nil {
		return r
	}
	dir := filepath.Join(request.RuntimeDir, request.RunID)
	r.Reason = "无法独占创建执行日志目录，历史执行结果未知，需对账后再处理"
	mkdirDone := preflight.Begin(ctx, "acp_run_dir")
	if err := ctx.Err(); err != nil {
		mkdirDone(err)
		return r
	}
	mkdirErr := os.Mkdir(dir, 0700)
	mkdirDone(mkdirErr)
	if mkdirErr != nil {
		if os.IsExist(mkdirErr) {
			r.Reason = "执行标识已存在，历史执行结果未知，需对账后再重试"
		}
		return r
	}
	eCopy := e
	eCopy.config = runConfig
	r.Outcome, r.Reason = OutcomeNotStarted, "无法准备执行日志，执行未启动"
	if launch.server == "" {
		prepared, reason := eCopy.prepare(ctx, request, dir, launch.member)
		if reason != "" {
			r.Reason = reason
			return r
		}
		launch = prepared
	}
	if launch.seal != nil && (!preflight.Check(ctx, "acp_launch_seal", func() bool {
		return launch.seal.valid(runConfig, request.CWD, launch)
	}) || launch.verify == nil || !launch.verify(ctx)) {
		r.Reason = "ACP 受信执行来源已变化，执行未启动"
		return r
	}
	var stdout, stderr *os.File
	defer func() {
		if stdout != nil {
			_ = stdout.Close()
		}
		if stderr != nil {
			_ = stderr.Close()
		}
	}()
	if err := preflight.Run(ctx, "acp_log_files", func() error {
		r.StdoutPath, r.StderrPath = filepath.Join(dir, "stdout.jsonl"), filepath.Join(dir, "stderr.log")
		var err error
		stdout, err = os.OpenFile(r.StdoutPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil || ctx.Err() != nil {
			return err
		}
		stderr, err = os.OpenFile(r.StderrPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		return err
	}); err != nil {
		return r
	}
	outLog, errLog := &executionLog{file: stdout, cancel: cancel}, &executionLog{file: stderr, cancel: cancel}
	var pipes []*os.File
	defer func() {
		for _, pipe := range pipes {
			_ = pipe.Close()
		}
	}()
	pipe := func() (*os.File, *os.File, error) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		read, write, err := os.Pipe()
		if err == nil {
			pipes = append(pipes, read, write)
		}
		return read, write, err
	}
	stdinR, stdinW, err := pipe()
	if err != nil {
		return r
	}
	stdoutR, stdoutW, err := pipe()
	if err != nil {
		return r
	}
	stderrR, stderrW, err := pipe()
	if err != nil {
		return r
	}
	if e.config.VendorProfile == ACPOpenCode119 && launch.seal != nil && !preflight.Check(ctx, "opencode_launch_member_config", func() bool {
		member, err := readACPMemberToolConfig(ctx, e.config)
		return err == nil && acpStableInfo(launch.member.file, member.file) && member.digest == launch.member.digest
	}) {
		r.Reason = "成员工具配置无效或已变化，执行未启动"
		return r
	}
	cmd := exec.CommandContext(ctx, e.config.AgentPath, launch.args...)
	// Use the context gate in Start, not CommandContext's default Kill. Once
	// Start succeeds, ACP owns cancellation, drain, and the real Wait result.
	cmd.Cancel = nil
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = request.CWD, launch.env, stdinR, stdoutW, stderrW
	if e.config.VendorProfile == ACPOpenCode119 {
		// Dir was inspected above. A non-nil, zero SysProcAttr skips the
		// redundant os.StartProcess cwd stat after the context gate on Unix;
		// chdir and exec still have to succeed in Start itself.
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	startDone := preflight.Begin(ctx, "acp_os_start")
	if err := ctx.Err(); err != nil {
		startDone(err)
		return r
	}
	if err := cmd.Start(); err != nil {
		startDone(err)
		r.Reason = "无法启动模型进程，提示词未被接收"
		return r
	}
	r.Started, r.Outcome = true, OutcomeUnknown
	startDone(nil)
	_ = stdinR.Close()
	_ = stdoutW.Close()
	_ = stderrW.Close()
	delivery := newExecutionDelivery(ctx, cancel, callback)
	output := make(chan acpRead, 1)
	stdoutJoined, stderrJoined, waitJoined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() { defer close(stdoutJoined); readACPOutput(stdoutR, output) }()
	stderrDone := make(chan bool, 1)
	go func() {
		defer close(stderrJoined)
		hash := sha256.New()
		n, err := io.Copy(hash, stderrR)
		// Diagnostics and bearer-bearing frames are never raw-logged, even when
		// tokens straddle writes or JSON strings are escaped differently.
		raw := acpJSON(map[string]any{"type": "stderr_summary", "bytes": n, "sha256": hex.EncodeToString(hash.Sum(nil))})
		_, _ = errLog.Write(append(raw, '\n'))
		stderrDone <- err == nil
	}()
	waitDone := make(chan error, 1)
	go func() { defer close(waitJoined); waitDone <- cmd.Wait() }()
	writes := make(chan []byte, 4)
	writerDone, writeError := make(chan struct{}), make(chan struct{}, 1)
	go func() {
		defer close(writerDone)
		for frame := range writes {
			if n, err := stdinW.Write(frame); err != nil || n != len(frame) {
				writeError <- struct{}{}
				return
			}
		}
	}()
	inputClosed := false
	closeInput := func() {
		if !inputClosed {
			inputClosed = true
			_ = stdinW.Close()
			close(writes)
		}
	}
	enqueue := func(m acpMessage) bool {
		if inputClosed {
			return false
		}
		frame := acpJSON(m)
		// JSON escaping may expand the existing 1 MiB prompt, not native output.
		if len(frame) == 0 || len(frame) > 7<<20 {
			return false
		}
		select {
		case writes <- append(frame, '\n'):
			return true
		default:
			return false
		}
	}
	protocol := acpProtocol{ctx: ctx, cancel: cancel, config: runConfig, request: request, launch: launch, receipt: &r, delivery: delivery, send: enqueue, tools: make(map[string]acpTool), requests: make(map[string]bool), promptID: 3}
	protocol.summary = func(kind string, fields map[string]any) {
		fields["type"] = kind
		if r.NativeSessionID != "" {
			hash := sha256.Sum256([]byte(r.NativeSessionID))
			fields["session_sha256"] = hex.EncodeToString(hash[:])
		}
		_, _ = outLog.Write(append(acpJSON(fields), '\n'))
	}
	protocol.initialize()
	var exitTimer, drainTimer, cancelTimer *time.Timer
	defer func() {
		for _, timer := range []*time.Timer{exitTimer, drainTimer, cancelTimer} {
			if timer != nil {
				timer.Stop()
			}
		}
	}()
	var exitC, drainC, cancelC <-chan time.Time
	ctxDone := ctx.Done()
	var waited, outDone, errDone, outEOF, errEOF, killed bool
	var waitErr error
	var cancelQueued bool
	var exitDeadline time.Time
	beginExit := func(grace time.Duration) {
		closeInput()
		deadline := time.Now().Add(grace)
		if !waited && (exitTimer == nil || deadline.Before(exitDeadline)) {
			if exitTimer == nil {
				exitTimer = time.NewTimer(grace)
			} else {
				exitTimer.Reset(grace)
			}
			exitDeadline, exitC = deadline, exitTimer.C
		}
	}
	for !waited || !outDone || !errDone {
		select {
		case read := <-output:
			if read.done {
				outDone, outEOF, output = true, read.eof, nil
				if !waited {
					beginExit(1500 * time.Millisecond)
				}
			} else if read.damaged {
				protocol.fail()
			} else {
				protocol.receive(read.message)
				if protocol.promptReturned {
					beginExit(5 * time.Second)
				}
			}
		case complete := <-stderrDone:
			errDone, errEOF, stderrDone = true, complete, nil
		case waitErr = <-waitDone:
			waited, waitDone = true, nil
			closeInput()
			if exitTimer != nil {
				exitTimer.Stop()
				exitC = nil
			}
			if !outDone || !errDone {
				drainTimer = time.NewTimer(200 * time.Millisecond)
				drainC = drainTimer.C
			}
		case <-ctxDone:
			ctxDone = nil
			cancelQueued = protocol.interrupt()
			cancelTimer = time.NewTimer(250 * time.Millisecond)
			cancelC = cancelTimer.C
		case <-cancelC:
			cancelC = nil
			beginExit(1500 * time.Millisecond)
		case <-exitC:
			exitC = nil
			if !waited && cmd.Process.Kill() == nil {
				killed = true
			}
		case <-drainC:
			drainC = nil
			_ = stdoutR.Close()
			_ = stderrR.Close()
		case <-writeError:
			writeError = nil
			if !inputClosed {
				protocol.fail()
			}
		}
	}
	closeInput()
	<-writerDone
	<-stdoutJoined
	<-stderrJoined
	<-waitJoined
	r.DeliveryFailed = delivery.finish()
	r.DirectChildReaped = cmd.ProcessState != nil
	if r.DirectChildReaped {
		r.ExitCode = cmd.ProcessState.ExitCode()
	}
	outClose, errClose := stdout.Close(), stderr.Close()
	r.LogsTruncated = outLog.truncated || errLog.truncated
	profileStable := true
	if launch.seal != nil {
		member, err := readACPMemberToolConfig(context.Background(), e.config)
		// Preserve the post-Wait source check, even for a canceled invocation.
		profileStable = launch.seal.valid(runConfig, request.CWD, launch) && launch.verify != nil && launch.verify(context.Background()) && err == nil && acpStableInfo(launch.member.file, member.file) && member.digest == launch.member.digest
	}
	if e.config.VendorProfile == ACPOpenCode119 && e.config.OpenCode != nil {
		digest, err := acpOpenCodeDependencies(*e.config.OpenCode)
		profileStable = profileStable && err == nil && digest == e.openCodeDigest
	}
	r.OutputComplete = waitErr == nil && !killed && outEOF && errEOF && !protocol.damaged && protocol.awaiting == 0 && profileStable && !outLog.failed && !errLog.failed && outClose == nil && errClose == nil
	r.Reason = "执行结果未知，需对账后再重试"
	settledCancellation := runConfig.VendorProfile == ACPAgy15 && cancelQueued &&
		context.Cause(ctx) == ErrTurnCancelRequested && r.Terminal == OutcomeAborted
	if r.OutputComplete && (ctx.Err() == nil || settledCancellation) && !r.DeliveryFailed && r.Terminal != "" && protocol.settled() {
		r.Outcome = r.Terminal
		r.Reason = map[Outcome]string{OutcomeSucceeded: "原生交互已结束", OutcomeFailed: "原生交互已拒绝", OutcomeAborted: "原生交互已中止", OutcomeTruncated: "原生交互已达到限制"}[r.Outcome]
	}
	if protocol.damaged {
		r.Reason = "ACP 协议或模型配置校验失败，执行结果未知，需对账"
	}
	if protocol.external || protocol.promptReturned && !protocol.settled() {
		r.Reason = "存在未知或未结束工具，执行结果未知，需对账"
	}
	if r.DeliveryFailed {
		r.Reason = "执行事件保存失败，交付与执行结果未知"
	}
	return r
}
