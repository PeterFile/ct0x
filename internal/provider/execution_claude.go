package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const claudeTextProfile = "current_member_tools_text_v1"
const claudeFrameLimit = 256 << 10

type claudeSDKExecutor struct {
	cliPath, nodePath, bridgePath, configPath string
	cliFile, nodeFile, bridgeFile, configFile os.FileInfo
	configDigest                              [32]byte
	names                                     []string
	timeout                                   time.Duration
	used                                      atomic.Bool
}

// NewClaudeSDKExecutor only constructs a CLOSED experimental adapter; it never
// probes/launches Claude. Execute remains not_started until root reviews and
// changes the source-level gate after authorized native isolation acceptance.
// No public option or environment variable can bypass that gate.
func NewClaudeSDKExecutor(selected Provider, options ClaudeSDKOptions, timeout time.Duration) (Executor, error) {
	if timeout < 0 {
		return nil, errClaudeSDKConfiguration
	}
	if _, err := validateClaudeSelection(selected); err != nil {
		return nil, err
	}
	names, err := memberToolProfile(options.ToolNames)
	if err != nil {
		return nil, errClaudeSDKConfiguration
	}
	config, err := readMemberToolConfig(options.ConfigPath)
	if err != nil {
		return nil, errClaudeSDKConfiguration
	}
	e := &claudeSDKExecutor{configPath: options.ConfigPath, configFile: config.file, configDigest: config.digest, names: names, timeout: timeout}
	if e.timeout == 0 {
		e.timeout = 10 * time.Minute
	}
	if e.cliPath, e.cliFile, err = claudeTrustedFile(selected.Path, true); err != nil {
		return nil, err
	}
	if e.nodePath, e.nodeFile, err = claudeTrustedFile(options.NodePath, true); err != nil {
		return nil, err
	}
	if e.bridgePath, e.bridgeFile, err = claudeTrustedFile(options.BridgePath, false); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *claudeSDKExecutor) Execute(context.Context, ExecuteRequest, func(Event) error) ExecutionReceipt {
	return ExecutionReceipt{Outcome: OutcomeNotStarted, ExitCode: -1, Reason: "Claude SDK 入口保持关闭，尚未获准进行真实原生验收"}
}

func claudeUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6], raw[8] = raw[6]&0x0f|0x40, raw[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:]), nil
}

func claudeEnvironment(inherited []string) []string {
	allowed := map[string]bool{"ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true, "ANTHROPIC_BASE_URL": true,
		"CLAUDE_CODE_OAUTH_TOKEN": true, "CLAUDE_CONFIG_DIR": true, "CLAUDE_CODE_USE_BEDROCK": true,
		"CLAUDE_CODE_USE_VERTEX": true, "CLAUDE_CODE_USE_FOUNDRY": true}
	env := make([]string, 0, len(inherited))
	for _, entry := range inherited {
		key, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		blocked := false
		for _, prefix := range []string{"AGENT_WORKSPACE_", "PI_", "CODEX_", "NODE_", "BUN_", "DENO_", "CLAUDE_", "ANTHROPIC_", "LD_", "DYLD_"} {
			blocked = blocked || strings.HasPrefix(key, prefix)
		}
		if key != "ENV" && key != "BASH_ENV" && (!blocked || allowed[key]) {
			env = append(env, entry)
		}
	}
	return env
}

func claudeSameFile(path string, before os.FileInfo, executable bool) bool {
	real, now, err := claudeTrustedFile(path, executable)
	return err == nil && real == path && os.SameFile(before, now) && before.Mode() == now.Mode() && before.Size() == now.Size() && before.ModTime().Equal(now.ModTime())
}

// execute is deliberately package-private. Only controlled child fixtures call
// it while the public Execute and production Node entry remain closed. There is
// no alternate production factory, process-wide hook or environment test mode.
func (e *claudeSDKExecutor) execute(ctx context.Context, request ExecuteRequest, onEvent func(Event) error) ExecutionReceipt {
	r := ExecutionReceipt{Outcome: OutcomeNotStarted, ExitCode: -1, Reason: "Claude SDK 执行参数或当前配置无效"}
	if !e.used.CompareAndSwap(false, true) {
		r.Outcome, r.Reason = OutcomeUnknown, "本轮执行器已使用，执行结果未知，需对账"
		return r
	}
	config, err := readMemberToolConfig(e.configPath)
	if err != nil || !os.SameFile(e.configFile, config.file) || config.digest != e.configDigest || !e.configFile.ModTime().Equal(config.file.ModTime()) ||
		!claudeSameFile(e.cliPath, e.cliFile, true) || !claudeSameFile(e.nodePath, e.nodeFile, true) || !claudeSameFile(e.bridgePath, e.bridgeFile, false) {
		return r
	}
	cwd, cwdErr := filepath.EvalSymlinks(request.CWD)
	cwdInfo, statErr := os.Stat(cwd)
	runtime, runtimeErr := os.Lstat(filepath.Clean(request.RuntimeDir))
	if request.Thinking != "" || !filepath.IsAbs(request.CWD) || cwdErr != nil || statErr != nil || !cwdInfo.IsDir() ||
		!filepath.IsAbs(request.RuntimeDir) || runtimeErr != nil || !runtime.IsDir() || runtime.Mode().Perm()&0077 != 0 ||
		!executionID.MatchString(request.RunID) || strings.TrimSpace(request.Prompt) == "" || strings.HasPrefix(strings.TrimSpace(request.Prompt), "/") || len(request.Prompt) > 1<<20 || !utf8.ValidString(request.Prompt) || strings.ContainsRune(request.Prompt, 0) ||
		strings.TrimSpace(request.Model) == "" || len(request.Model) > 256 || !utf8.ValidString(request.Model) || strings.ContainsAny(request.Model, "\x00\r\n") {
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	if ctx.Err() != nil {
		r.Reason = "执行在启动前已取消或超时"
		return r
	}
	session, sessionErr := claudeUUID()
	input, inputErr := claudeUUID()
	var nonce [16]byte
	_, nonceErr := rand.Read(nonce[:])
	if sessionErr != nil || inputErr != nil || nonceErr != nil {
		return r
	}
	server := "aw_claude_" + hex.EncodeToString(nonce[:])
	tokenEnv := "AGENT_WORKSPACE_CLAUDE_TOKEN_" + strings.ToUpper(hex.EncodeToString(nonce[:]))
	dir := filepath.Join(request.RuntimeDir, request.RunID)
	if err := os.Mkdir(dir, 0700); err != nil {
		if os.IsExist(err) {
			r.Outcome, r.Reason = OutcomeUnknown, "执行标识已存在，历史执行结果未知，需对账后再重试"
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
	var pipes []*os.File
	defer func() {
		for _, file := range pipes {
			_ = file.Close()
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
	cmd := exec.Command(e.nodePath, e.bridgePath)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = cwd, stdinR, stdoutW, stderrW
	cmd.Env = append(claudeEnvironment(os.Environ()), tokenEnv+"="+config.token)
	if cmd.Start() != nil {
		r.Reason = "无法启动 Claude SDK 协议桥，输入未被接收"
		return r
	}
	_ = stdinR.Close()
	_ = stdoutW.Close()
	_ = stderrW.Close()
	r.Started, r.Outcome = true, OutcomeUnknown
	delivery := newExecutionDelivery(ctx, cancel, onEvent)
	output := make(chan claudeRead, 16)
	outJoined, errJoined, waitJoined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() { defer close(outJoined); readClaudeFrames(stdoutR, output) }()
	errDone := make(chan bool, 1)
	go func() {
		defer close(errJoined)
		n, err := io.Copy(io.Discard, stderrR)
		raw, _ := json.Marshal(map[string]any{"kind": "stderr_summary", "bytes": n})
		_, _ = errLog.Write(append(raw, '\n'))
		errDone <- err == nil
	}()
	waitDone := make(chan error, 1)
	go func() { defer close(waitJoined); waitDone <- cmd.Wait() }()
	writes, writeErr, writerJoined := make(chan []byte, 8), make(chan struct{}, 1), make(chan struct{})
	go func() {
		defer close(writerJoined)
		for raw := range writes {
			if n, err := stdinW.Write(raw); err != nil || n != len(raw) {
				writeErr <- struct{}{}
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
	sequence := 0
	send := func(kind string, body any) bool {
		if inputClosed {
			return false
		}
		sequence++
		raw, err := json.Marshal(claudeFrame{V: 1, RequestID: request.RunID, Seq: sequence, Kind: kind, SessionID: session, InputUUID: input, Body: mustClaudeJSON(body)})
		if err != nil || len(raw) > 8<<20 {
			return false
		}
		select {
		case writes <- append(raw, '\n'):
			return true
		default:
			return false
		}
	}
	protocol := claudeProtocol{request: request, cwd: cwd, cliPath: e.cliPath, session: session, input: input, server: server, names: e.names,
		receipt: &r, delivery: delivery, cancel: cancel, send: send, permissions: make(map[string]bool), uses: make(map[string]bool), log: outLog}
	if !send("start", map[string]any{"cli_path": e.cliPath, "cwd": cwd, "model": request.Model, "prompt": request.Prompt,
		"profile_id": claudeTextProfile, "allowed_platform_tools": e.names, "mcp_url": config.endpoint + "/mcp", "mcp_server": server, "token_env_name": tokenEnv}) {
		protocol.fail()
	}
	var exitTimer, drainTimer *time.Timer
	var exitC, drainC <-chan time.Time
	defer func() {
		if exitTimer != nil {
			exitTimer.Stop()
		}
		if drainTimer != nil {
			drainTimer.Stop()
		}
	}()
	beginExit := func(grace time.Duration) {
		if exitTimer == nil {
			exitTimer = time.NewTimer(grace)
			exitC = exitTimer.C
		}
	}
	var waited, outDone, stderrDone, outEOF, errEOF, killed bool
	var waitErr error
	ctxDone := ctx.Done()
	for !waited || !outDone || !stderrDone {
		select {
		case result := <-output:
			if result.done {
				outDone, outEOF, output = true, result.complete, nil
				if !waited {
					closeInput()
					beginExit(3 * time.Second)
				}
			} else if result.damaged {
				protocol.fail()
			} else {
				protocol.receive(result.raw, ctx.Err() != nil)
				if protocol.closed {
					beginExit(5 * time.Second)
				}
			}
		case complete := <-errDone:
			stderrDone, errEOF, errDone = true, complete, nil
		case waitErr = <-waitDone:
			waited, waitDone = true, nil
			closeInput()
			if exitTimer != nil {
				exitTimer.Stop()
				exitC = nil
			}
			if !outDone || !stderrDone {
				drainTimer = time.NewTimer(300 * time.Millisecond)
				drainC = drainTimer.C
			}
		case <-ctxDone:
			ctxDone = nil
			protocol.stopping = true
			_ = send("cancel", map[string]any{})
			beginExit(12 * time.Second)
		case <-exitC:
			exitC = nil
			closeInput()
			if !waited && cmd.Process.Kill() == nil {
				killed = true
			}
		case <-drainC:
			drainC = nil
			_ = stdoutR.Close()
			_ = stderrR.Close()
		case <-writeErr:
			writeErr = nil
			if !inputClosed {
				protocol.fail()
			}
		}
	}
	closeInput()
	<-writerJoined
	<-outJoined
	<-errJoined
	<-waitJoined
	r.DeliveryFailed = delivery.finish()
	r.DirectChildReaped = cmd.ProcessState != nil // This is Node, NOT the CLI.
	if r.DirectChildReaped {
		r.ExitCode = cmd.ProcessState.ExitCode()
	}
	outClose, errClose := stdout.Close(), stderr.Close()
	r.LogsTruncated = outLog.truncated || errLog.truncated
	r.OutputComplete = waitErr == nil && !killed && outEOF && errEOF && !protocol.damaged && protocol.closed && protocol.nativeClean && !outLog.failed && !errLog.failed && outClose == nil && errClose == nil
	r.Reason = "Claude SDK 执行结果未知，需对账后再重试"
	if r.OutputComplete && ctx.Err() == nil && !r.DeliveryFailed && r.Terminal != "" && r.Accepted {
		r.Outcome = r.Terminal
		r.Reason = map[Outcome]string{OutcomeSucceeded: "执行完成", OutcomeFailed: "模型执行失败", OutcomeAborted: "模型执行已中止", OutcomeTruncated: "模型输出被截断"}[r.Outcome]
	}
	if r.DeliveryFailed {
		r.Reason = "执行事件保存失败，交付与执行结果未知"
	}
	return r
}
