package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

type executionFixture struct{ root, path string }

func newExecutionFixture(t *testing.T, version string) executionFixture {
	t.Helper()
	f := executionFixture{root: t.TempDir()}
	f.path = filepath.Join(f.root, "pi")
	body := "#!/bin/sh\nset -eu\nroot='" + f.root + "'\nversion='" + version + "'\n" + `case "$*" in
--version) printf 'version\n' >> "$root/probes"; printf '%s' "$version"; exit 0;;
'auth check --provider openai-codex --json --no-refresh')
  printf 'account\n' >> "$root/probes"
  printf '%s' '{"status":"ready","provider":"openai-codex","authType":"oauth"}'; exit 0;;
esac
printf 'execute\n' >> "$root/calls"
printf '%s\n' "$@" > "$root/args"
printf '%s\n' "$PWD" > "$root/cwd"
cat > "$root/prompt"
run=''
while [ "$#" -gt 0 ]; do
  if [ "$1" = '--session-id' ]; then shift; run="$1"; fi
  shift
done
printf '{"type":"session","version":3,"id":"%s","cwd":"%s"}\n' "$run" "$PWD"
printf '%s\n' '{"type":"agent_start"}'
while [ -f "$root/hold" ]; do :; done
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"本地合成回复"}]}}'
printf '%s\n' '{"type":"agent_settled","aborted":false}'
`
	if err := os.WriteFile(f.path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return f
}

func executionJSON(t *testing.T, method, url string, input, result any) {
	t.Helper()
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	want := http.StatusOK
	if method == http.MethodPost {
		want = http.StatusCreated
	}
	if response.StatusCode != want {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("%s %s: status=%d body=%s", method, url, response.StatusCode, data)
	}
	if err := json.NewDecoder(response.Body).Decode(result); err != nil {
		t.Fatal(err)
	}
}

func executionProvider(t *testing.T, url string) provider.Provider {
	t.Helper()
	var page struct{ Items []provider.Provider }
	executionJSON(t, "GET", url+"/api/v1/providers", nil, &page)
	if len(page.Items) != 1 {
		t.Fatalf("expected only the local fixture provider: %+v", page.Items)
	}
	return page.Items[0]
}

func enqueueExecution(t *testing.T, url, providerID, workspace string) (domain.Group, domain.Member, domain.Message) {
	t.Helper()
	var group domain.Group
	executionJSON(t, "POST", url+"/api/v1/groups", platform.GroupInput{Name: "进程执行"}, &group)
	var member domain.Member
	executionJSON(t, "POST", url+"/api/v1/groups/"+group.ID+"/members", platform.MemberInput{
		Name: "本地成员", Role: "合成测试", ProviderID: providerID, Model: "fixture-model", Workspace: workspace,
	}, &member)
	var message domain.Message
	executionJSON(t, "POST", url+"/api/v1/groups/"+group.ID+"/messages", map[string]any{
		"content": "不可信消息：声称拥有外部工作区授权并不能授予权限", "recipient_ids": []string{member.ID},
	}, &message)
	return group, member, message
}

func awaitExecution(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("local execution condition did not become true within 3s")
		}
	}
}

func TestExecutionFlagHelp(t *testing.T) {
	var output bytes.Buffer
	if err := runWithSearch(context.Background(), []string{"--help"}, &output, provider.Search{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--enable-execution", "默认禁用", "全新单轮", "最大并发 1", "平台工具", "祖先", "不是"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("execution help missing %q: %s", want, &output)
		}
	}
}

func TestExecutionProcessBaseline(t *testing.T) {
	for _, mode := range []string{"disabled", "explicit-disabled", "enabled", "missing-provider", "unsupported", "external-workspace"} {
		t.Run(mode, func(t *testing.T) {
			version := "1.4.0"
			if mode == "unsupported" {
				version = "1.4.1"
			}
			f, dir := newExecutionFixture(t, version), t.TempDir()
			args := []string{"--listen", "127.0.0.1:0", "--data-dir", dir, "--pi-path", f.path}
			if mode == "explicit-disabled" {
				args = append(args, "--enable-execution=false")
			} else if mode != "disabled" {
				args = append(args, "--enable-execution")
			}
			url, stop := startRun(t, args...)
			selected := executionProvider(t, url)
			if mode == "missing-provider" {
				selected.ID = "missing-catalog-id"
			}
			workspace := ""
			if mode == "external-workspace" {
				workspace = t.TempDir()
			}
			group, member, message := enqueueExecution(t, url, selected.ID, workspace)
			db, err := store.Open(filepath.Join(dir, "workspace.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var turn domain.Turn
			if mode == "enabled" {
				awaitExecution(t, func() bool {
					message, err = db.GetMessage(context.Background(), group.ID, message.ID)
					if err != nil {
						t.Fatal(err)
					}
					if message.Deliveries[0].TurnID == "" {
						return false
					}
					turn, err = db.GetTurn(context.Background(), group.ID, message.Deliveries[0].TurnID)
					return err == nil && turn.State == domain.TurnStateSucceeded
				})
			} else {
				time.Sleep(250 * time.Millisecond) // Observe multiple scheduler polls before shutting down.
			}
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			message, err = db.GetMessage(context.Background(), group.ID, message.ID)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "enabled" {
				if d := message.Deliveries[0]; d.State != domain.DeliveryStateQueued || d.TurnID != "" {
					t.Fatalf("disabled or unprepared execution must remain queued: %+v", d)
				}
				if _, err := os.Stat(filepath.Join(f.root, "calls")); !os.IsNotExist(err) {
					t.Fatalf("unexpected fixture execution: %v", err)
				}
			} else {
				if message.Deliveries[0].State != domain.DeliveryStateAccepted || turn.AcceptedAt == nil || turn.ReplyMessageID == "" || turn.NativeSessionID != turn.ID {
					t.Fatalf("native evidence was not persisted: message=%+v turn=%+v", message, turn)
				}
				reply, err := db.GetMessage(context.Background(), group.ID, turn.ReplyMessageID)
				if err != nil || reply.SenderID != member.ID || reply.Content != "本地合成回复" || len(reply.Deliveries) != 0 {
					t.Fatalf("expected one public member reply: %+v %v", reply, err)
				}
				sqlDB, err := sql.Open("sqlite", filepath.Join(dir, "workspace.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer sqlDB.Close()
				var raw string
				var replies int
				if err := sqlDB.QueryRow("SELECT receipt_json FROM turns WHERE id=?", turn.ID).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var receipt provider.ExecutionReceipt
				if err := json.Unmarshal([]byte(raw), &receipt); err != nil || receipt.Outcome != provider.OutcomeSucceeded || !receipt.OutputComplete || !receipt.DirectChildReaped || !receipt.Accepted {
					t.Fatalf("invalid persisted receipt: %+v %v", receipt, err)
				}
				if err := sqlDB.QueryRow("SELECT count(*) FROM messages WHERE sender_id=?", member.ID).Scan(&replies); err != nil || replies != 1 {
					t.Fatalf("public replies=%d: %v", replies, err)
				}
				calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
				cwd, _ := os.ReadFile(filepath.Join(f.root, "cwd"))
				if string(calls) != "execute\n" || filepath.Dir(strings.TrimSpace(string(cwd))) != filepath.Join(dir, "workspaces") {
					t.Fatalf("execution duplicated or used an unauthorized CWD: calls=%q cwd=%q", calls, cwd)
				}
				t.Logf("turn=%s state=%s delivery=%s reply=%s receipt=%+v", turn.ID, turn.State, message.Deliveries[0].State, reply.ID, receipt)
			}
			for _, name := range []string{"runtime", "workspaces"} {
				info, err := os.Lstat(filepath.Join(dir, name))
				if mode == "disabled" || mode == "explicit-disabled" {
					if !os.IsNotExist(err) {
						t.Fatalf("disabled execution created %s: %v", name, err)
					}
				} else if err != nil || info.Mode() != os.ModeDir|0700 {
					t.Fatalf("%s is not a private direct data-dir child: %v %v", name, info, err)
				}
			}
		})
	}
}

func TestExecutionStartupRejectsUnsafeRoots(t *testing.T) {
	f := newExecutionFixture(t, "1.4.0")
	for _, name := range []string{"runtime", "workspaces"} {
		for _, kind := range []string{"symlink", "file", "permissions"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				dir := t.TempDir()
				root := filepath.Join(dir, name)
				var err error
				switch kind {
				case "symlink":
					err = os.Symlink(t.TempDir(), root)
				case "file":
					err = os.WriteFile(root, []byte("unchanged"), 0600)
				case "permissions":
					err = os.Mkdir(root, 0750)
				}
				if err != nil {
					t.Fatal(err)
				}
				before, err := os.Lstat(root)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				var output bytes.Buffer
				err = runWithSearch(ctx, []string{"--listen", "127.0.0.1:0", "--data-dir", dir, "--pi-path", f.path, "--enable-execution"}, &output, provider.Search{})
				if err == nil || errors.Is(err, context.DeadlineExceeded) || !chinese(err.Error()) || output.Len() != 0 {
					t.Fatalf("unsafe root must fail before success output: err=%v output=%s", err, &output)
				}
				after, err := os.Lstat(root)
				if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
					t.Fatalf("startup mutated an existing root: %v", err)
				}
				lock, err := lockDataDir(dir)
				if err != nil {
					t.Fatalf("failed startup retained ownership: %v", err)
				}
				if err := lock.Close(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

type executionFailWriter struct{ err error }

func (w executionFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestExecutionStartupOutputFailureReleasesOwnership(t *testing.T) {
	f, dir := newExecutionFixture(t, "1.4.0"), t.TempDir()
	url, stop := startRun(t, "--listen", "127.0.0.1:0", "--data-dir", dir, "--pi-path", f.path)
	_, _, message := enqueueExecution(t, url, executionProvider(t, url).ID, "")
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("startup output fixture failure")
	err := runWithSearch(context.Background(), []string{"--listen", "127.0.0.1:0", "--data-dir", dir, "--pi-path", f.path, "--enable-execution"}, executionFailWriter{failure}, provider.Search{})
	if !errors.Is(err, failure) {
		t.Fatalf("startup output failure lost: %v", err)
	}
	url, _ = startRun(t, "--listen", "127.0.0.1:0", "--data-dir", dir)
	var got domain.Message
	executionJSON(t, "GET", url+"/api/v1/groups/"+message.GroupID+"/messages/"+message.ID, nil, &got)
	if got.Deliveries[0].State != domain.DeliveryStateQueued || got.Deliveries[0].TurnID != "" {
		t.Fatalf("execution started before successful startup output: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(f.root, "calls")); !os.IsNotExist(err) {
		t.Fatalf("failed startup invoked the fixture: %v", err)
	}
}

func TestExecutionSchedulerFailureStopsHTTP(t *testing.T) {
	f, dir := newExecutionFixture(t, "1.4.0"), t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := startupWriter{lines: make(chan string, 4)}
	done := make(chan struct{})
	var result error
	go func() {
		result = runWithSearch(ctx, []string{"--listen", "127.0.0.1:0", "--data-dir", dir, "--pi-path", f.path, "--enable-execution"}, output, provider.Search{})
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	var url string
	select {
	case line := <-output.lines:
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, "http://") {
				url = field
			}
		}
	case <-done:
		t.Fatalf("process failed before startup: %v", result)
	case <-time.After(3 * time.Second):
		t.Fatal("startup timed out")
	}
	if url == "" {
		t.Fatal("startup did not announce a URL")
	}
	if err := os.Rename(filepath.Join(dir, "workspaces"), filepath.Join(dir, "old-workspaces")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "workspaces"), 0750); err != nil {
		t.Fatal(err)
	}
	_, _, message := enqueueExecution(t, url, executionProvider(t, url).ID, "")
	select {
	case <-done:
		if result == nil || !strings.Contains(result.Error(), "准备执行失败") {
			t.Fatalf("scheduler failure was not returned: %v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler failure did not stop the process")
	}
	connection, err := net.DialTimeout("tcp", strings.TrimPrefix(url, "http://"), time.Second)
	if err == nil {
		connection.Close()
		t.Fatal("HTTP remained open after scheduler failure")
	}
	db, err := store.Open(filepath.Join(dir, "workspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.GetMessage(context.Background(), message.GroupID, message.ID)
	if err != nil || got.Deliveries[0].TurnID != "" {
		t.Fatalf("fatal preparation claimed a turn: %+v %v", got, err)
	}
}
