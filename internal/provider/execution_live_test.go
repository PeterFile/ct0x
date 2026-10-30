//go:build providerlive

package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProviderLivePi(t *testing.T) {
	providerLiveSmoke(t, Pi, "openai-codex/gpt-6.1-sol", "max", NewPiExecutor)
}

func TestProviderLiveCodex(t *testing.T) {
	providerLiveSmoke(t, Codex, "gpt-6.1-sol", "", NewCodexExecutor)
}

const providerLiveEventLimit = 128

type providerLiveEvent struct {
	Kind            EventKind `json:"kind"`
	NativeSessionID string    `json:"native_session_id"`
	Terminal        Outcome   `json:"terminal,omitempty"`
	ElapsedMS       int64     `json:"elapsed_ms"`
}

func providerLiveSmoke(t *testing.T, provider, model, thinking string, newExecutor func(string, time.Duration) (Executor, error)) {
	t.Helper()
	if os.Getenv("AGENT_WORKSPACE_LIVE_SMOKE") != "1" || os.Getenv("AGENT_WORKSPACE_LIVE_PROVIDER") != provider {
		t.Skip("真实请求未显式启用，或未选择此提供方")
	}
	must := func(err error, message string) {
		t.Helper()
		if err != nil {
			t.Fatal(message)
		}
	}
	executable, root := os.Getenv("AGENT_WORKSPACE_LIVE_EXECUTABLE"), os.Getenv("AGENT_WORKSPACE_LIVE_EVIDENCE_DIR")
	if !filepath.IsAbs(executable) || !filepath.IsAbs(root) {
		t.Fatal("必须显式提供绝对路径的可执行文件和证据根目录")
	}
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatal("证据根目录必须已存在、权限为 0700，且不能是符号链接")
	}
	executor, err := newExecutor(executable, 3*time.Minute)
	must(err, "无法构造执行器")
	var random [16]byte
	_, err = rand.Read(random[:])
	must(err, "无法生成随机运行标识")
	nonce := hex.EncodeToString(random[:])
	runID := provider + "-" + nonce
	dir := filepath.Join(root, runID)
	workspace, runtime := filepath.Join(dir, "workspace"), filepath.Join(dir, "runtime")
	for _, path := range []string{dir, workspace, runtime} {
		must(os.Mkdir(path, 0700), "无法独占创建私有运行目录")
	}
	// Never register cleanup: unknown runs and their raw logs require reconciliation.
	evidencePath := filepath.Join(dir, "evidence.json")
	file, err := os.OpenFile(evidencePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	must(err, "无法独占创建私有证据文件")
	defer file.Close()
	request := ExecuteRequest{
		RunID: runID, CWD: workspace, RuntimeDir: runtime, Model: model, Thinking: thinking,
		Prompt: "Reply with exactly the following nonce as plain text, without quotes or formatting: " + nonce +
			"\nDo not use tools, open files, or run commands.",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	started := time.Now()
	events := make([]providerLiveEvent, 0, providerLiveEventLimit)
	exactMatch, eventsTruncated := false, false
	// This is the only invocation. Pi disables tools; Codex uses a read-only sandbox.
	receipt := executor.Execute(ctx, request, func(event Event) error {
		if len(events) >= providerLiveEventLimit {
			eventsTruncated = true
			return errors.New("真实执行事件超过审计上限")
		}
		if event.Kind == ResponseEvent {
			exactMatch = strings.TrimSpace(event.Text) == nonce
		}
		events = append(events, providerLiveEvent{event.Kind, event.NativeSessionID, event.Terminal, time.Since(started).Milliseconds()})
		return nil
	})
	// Keep the receipt's private raw-log paths, but never persist diagnostic text.
	receipt.Reason = ""
	evidence := struct {
		Provider           string              `json:"provider"`
		Executable         string              `json:"executable"`
		Model              string              `json:"model"`
		Thinking           string              `json:"thinking,omitempty"`
		RunID              string              `json:"run_id"`
		Nonce              string              `json:"nonce"`
		StartedAt          time.Time           `json:"started_at"`
		ElapsedMS          int64               `json:"elapsed_ms"`
		Events             []providerLiveEvent `json:"events"`
		EventsTruncated    bool                `json:"events_truncated"`
		Receipt            ExecutionReceipt    `json:"receipt"`
		ExactResponseMatch bool                `json:"exact_response_match"`
	}{provider, executable, model, thinking, runID, nonce, started.UTC(), time.Since(started).Milliseconds(), events, eventsTruncated, receipt, exactMatch}
	must(json.NewEncoder(file).Encode(evidence), "无法保存脱敏证据；私有运行目录已保留")
	must(file.Sync(), "无法同步脱敏证据；私有运行目录已保留")
	must(file.Close(), "无法关闭脱敏证据；私有运行目录已保留")
	t.Logf("证据：%s；结果=%s；精确匹配=%t", evidencePath, receipt.Outcome, exactMatch)

	if !receipt.Started || !receipt.Accepted || receipt.Outcome != OutcomeSucceeded || receipt.Terminal != OutcomeSucceeded ||
		!receipt.DirectChildReaped || !receipt.OutputComplete || receipt.DeliveryFailed {
		t.Error("执行回执未满足成功条件，请检查私有证据")
	}
	phase, ordered, bound := 0, true, receipt.NativeSessionID != ""
	for _, event := range events {
		bound = bound && event.NativeSessionID == receipt.NativeSessionID
		switch event.Kind {
		case SessionEvent:
			ordered = ordered && phase == 0
			phase = 1
		case AcceptedEvent:
			ordered = ordered && phase == 1
			phase = 2
		case ResponseEvent:
			ordered = ordered && (phase == 2 || phase == 3)
			phase = 3
		case TerminalEvent:
			ordered = ordered && phase == 3 && event.Terminal == OutcomeSucceeded
			phase = 4
		default:
			ordered = false
		}
	}
	if !bound || !ordered || phase != 4 {
		t.Error("回调缺失、顺序错误、会话未绑定，或缺少可信成功终态")
	}
	if !exactMatch {
		t.Error("最终完整响应与随机 nonce 不完全匹配")
	}
}
