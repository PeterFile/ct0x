package provider

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type codexAppServerExecutor struct{ executionCommand }

// NewCodexAppServerExecutor is an experimental, opt-in fresh-thread/text-turn
// adapter for Codex 0.166. It preserves the original CLI HOME and authentication.
// It requires trusted, stable local configuration: double config/read and named
// MCP disabling do not provide atomic config isolation, ABA protection or a
// directory-read sandbox. No tools, resume or dynamic tool registration are offered.
// A nonpositive timeout defaults to ten minutes; caller deadlines also apply.
func NewCodexAppServerExecutor(path string, timeout time.Duration) (Executor, error) {
	command, err := newExecutionCommand(path, timeout)
	if err != nil {
		return nil, err
	}
	return codexAppServerExecutor{command}, nil
}

type codexAppServerRead struct {
	message  codexRPCMessage
	raw      []byte
	damaged  bool
	done     bool
	complete bool
}

// All raw responses (especially secret-bearing config/read), unknown methods and
// malformed frames are omitted. Only selected native lifecycle notifications are
// raw-logged; configuration gets a separate count/provenance-digest summary.
func readCodexAppServerOutput(source io.Reader, output chan<- codexAppServerRead) {
	reader := newCodexRPCReader(source)
	for {
		message, err := reader.Next()
		if err != nil {
			if err == io.EOF {
				output <- codexAppServerRead{done: true, complete: true}
			} else {
				output <- codexAppServerRead{damaged: true}
				_, drainErr := io.Copy(io.Discard, reader.input)
				output <- codexAppServerRead{done: true, complete: drainErr == nil}
			}
			return
		}
		result := codexAppServerRead{message: message}
		if message.Method != nil && message.ID == nil {
			switch *message.Method {
			case "thread/started", "turn/started", "item/started", "item/completed", "turn/completed":
				result.raw = append(append([]byte(nil), reader.frame...), '\n')
			}
		}
		output <- result
	}
}

func codexAppServerArguments() []string {
	args := []string{}
	for _, feature := range codexAppServerDisabledFeatures {
		args = append(args, "-c", "features."+feature+"=false")
	}
	args = append(args, "-c", "agents.enabled=false", "-c", `web_search="disabled"`, "-c", "notify=[]",
		"-c", `approval_policy="never"`, "-c", `sandbox_mode="read-only"`, "app-server", "--listen", "stdio://")
	return args
}

func (e codexAppServerExecutor) Execute(ctx context.Context, request ExecuteRequest, onEvent func(Event) error) ExecutionReceipt {
	r := ExecutionReceipt{Outcome: OutcomeNotStarted, ExitCode: -1, Reason: "执行参数无效"}
	if request.Thinking != "" {
		r.Reason = "Codex 暂不支持 thinking 参数"
		return r
	}
	cwd, cwdErr := os.Stat(request.CWD)
	runtime, runtimeErr := os.Lstat(filepath.Clean(request.RuntimeDir))
	if !filepath.IsAbs(request.CWD) || !filepath.IsAbs(request.RuntimeDir) ||
		cwdErr != nil || !cwd.IsDir() || runtimeErr != nil || !runtime.IsDir() || runtime.Mode().Perm()&0077 != 0 ||
		!executionID.MatchString(request.RunID) || strings.TrimSpace(request.Prompt) == "" || len(request.Prompt) > 1<<20 ||
		len(request.Model) > 256 || strings.ContainsAny(request.Model, "\x00\r\n") || !utf8.ValidString(request.Prompt) || !utf8.ValidString(request.Model) {
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
	// Explicit pipes keep cmd.Wait independent of parsing and callbacks. Only the
	// owner below closes read ends, after real Wait plus a bounded EOF grace.
	var pipes []*os.File
	defer func() {
		for _, pipe := range pipes {
			_ = pipe.Close()
		}
	}()
	pipe := func() (*os.File, *os.File, error) {
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
	cmd := exec.Command(e.path, codexAppServerArguments()...)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = request.CWD, stdinR, stdoutW, stderrW
	if cmd.Start() != nil {
		r.Reason = "无法启动模型进程，提示词未被接收"
		return r
	}
	_ = stdinR.Close()
	_ = stdoutW.Close()
	_ = stderrW.Close()
	r.Started, r.Outcome = true, OutcomeUnknown
	delivery := newExecutionDelivery(ctx, cancel, onEvent)
	output := make(chan codexAppServerRead, 1)
	stdoutJoined, stderrJoined, waitJoined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stdoutJoined)
		readCodexAppServerOutput(stdoutR, output)
	}()
	stderrDone := make(chan bool, 1)
	go func() {
		defer close(stderrJoined)
		_, err := io.Copy(errLog, stderrR)
		stderrDone <- err == nil
	}()
	waitDone := make(chan error, 1)
	go func() {
		defer close(waitJoined)
		waitDone <- cmd.Wait()
	}()
	// Stdin writes never run on the protocol/drain owner. A nonreading child
	// cannot prevent cancellation; closing the descriptor joins a blocked writer.
	writes := make(chan []byte, 4)
	writerDone := make(chan struct{})
	writeError := make(chan struct{}, 1)
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
	enqueue := func(message codexRPCMessage) bool {
		if inputClosed {
			return false
		}
		// Input has the existing 1 MiB prompt limit, not the smaller encoded
		// response-frame budget: JSON escaping may expand a literal prompt.
		frame, err := json.Marshal(message)
		if err != nil {
			return false
		}
		select {
		case writes <- append(frame, '\n'):
			return true
		default:
			return false
		}
	}
	protocol := codexAppServerProtocol{
		ctx: ctx, cancel: cancel, request: request, receipt: &r, delivery: delivery,
		items: make(map[string]codexAppServerItem), requests: make(map[string]bool),
		send: func(id int, method string, params any) bool {
			message := codexRPCMessage{Method: &method}
			if id != 0 {
				message.ID = json.RawMessage(strconv.Itoa(id))
			}
			if params != nil {
				message.Params = mustCodexAppServerJSON(params)
			}
			return enqueue(message)
		},
		reply: func(request codexRPCMessage, result any) bool {
			message := codexRPCMessage{ID: request.ID}
			if result == nil {
				message.Error = mustCodexAppServerJSON(map[string]any{"code": -32601, "message": "受限执行器不支持该请求"})
			} else {
				message.Result = mustCodexAppServerJSON(result)
			}
			return enqueue(message)
		},
		summary: func(snapshot codexAppServerConfigSnapshot) {
			raw := mustCodexAppServerJSON(map[string]any{"type": "config_summary", "mcp_count": len(snapshot.names), "origins_sha256": hex.EncodeToString(snapshot.origins[:])})
			_, _ = outLog.Write(append(raw, '\n'))
		},
	}
	protocol.next(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "agent_workspace", "title": "Agent Workspace", "version": "0.1.0"}})
	var exitTimer, drainTimer, interruptTimer *time.Timer
	defer func() {
		for _, timer := range []*time.Timer{exitTimer, drainTimer, interruptTimer} {
			if timer != nil {
				timer.Stop()
			}
		}
	}()
	var exitC, drainC, interruptC <-chan time.Time
	ctxDone := ctx.Done()
	var waitErr error
	var exitDeadline time.Time
	var waited, outDone, errDone, outEOF, errEOF, killed, stopping bool
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
		case result := <-output:
			if result.done {
				outDone, outEOF, output = true, result.complete, nil
				if !waited && r.Terminal == "" {
					beginExit(1500 * time.Millisecond)
				}
			} else if result.damaged {
				protocol.fail()
			} else {
				if len(result.raw) != 0 {
					_, _ = outLog.Write(result.raw)
				}
				protocol.receive(result.message)
				if stopping && !inputClosed {
					protocol.interrupt()
				}
				if r.Terminal != "" {
					// Native stdio shutdown can take over a second even without a
					// turn. Do not substitute a kill for a successful normal exit.
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
			ctxDone, stopping = nil, true
			protocol.interrupt()
			interruptTimer = time.NewTimer(250 * time.Millisecond)
			interruptC = interruptTimer.C
		case <-interruptC:
			interruptC = nil
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
	// No parser/reader/writer can enqueue now. Finish joins even an in-flight
	// callback whose persistence work outlives the native process.
	r.DeliveryFailed = delivery.finish()
	r.DirectChildReaped = cmd.ProcessState != nil
	if r.DirectChildReaped {
		r.ExitCode = cmd.ProcessState.ExitCode()
	}
	outClose, errClose := stdout.Close(), stderr.Close()
	r.LogsTruncated = outLog.truncated || errLog.truncated
	r.OutputComplete = waitErr == nil && !killed && outEOF && errEOF && !protocol.damaged && protocol.awaiting == 0 && !outLog.failed && !errLog.failed && outClose == nil && errClose == nil
	r.Reason = "执行结果未知，需对账后再重试"
	if r.OutputComplete && ctx.Err() == nil && !r.DeliveryFailed && r.Terminal != "" && protocol.settled() {
		r.Outcome = r.Terminal
		r.Reason = map[Outcome]string{OutcomeSucceeded: "执行完成", OutcomeFailed: "模型执行失败", OutcomeAborted: "模型执行已中止"}[r.Outcome]
	}
	if protocol.damaged {
		r.Reason = "原生协议或配置校验失败，执行结果未知，需对账后再重试"
	}
	if protocol.external || !protocol.settled() && !protocol.damaged && r.Terminal != "" {
		r.Reason = "检测到外部、未完成工作或缺少完整答复，执行结果未知，需对账后再重试"
	}
	if r.DeliveryFailed {
		r.Reason = "执行事件保存失败，交付与执行结果未知"
	}
	return r
}
