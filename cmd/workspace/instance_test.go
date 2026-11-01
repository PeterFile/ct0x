//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestInstanceProcessHelper(t *testing.T) {
	if os.Getenv("AGENT_WORKSPACE_F1_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg != "--" {
			continue
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			cancel()
		}()
		err := runWithSearch(ctx, os.Args[i+1:], os.Stdout, provider.Search{})
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(3)
}

type instanceOutput struct{ bytes.Buffer }

func (output *instanceOutput) Write(body []byte) (int, error) {
	n := len(body)
	_, _ = output.Buffer.Write(body[:min(len(body), 4096-output.Len())])
	return n, nil
}

type instanceProcess struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	ready  chan string
	done   chan struct{}
	stderr instanceOutput
	err    error
}

func launchInstance(t *testing.T, dir, address string, args ...string) *instanceProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	p := &instanceProcess{ready: make(chan string, 1), done: make(chan struct{})}
	args = append([]string{"-test.run=^TestInstanceProcessHelper$", "--", "--data-dir", dir, "--listen", address}, args...)
	p.cmd = exec.CommandContext(ctx, executable, args...)
	p.cmd.Env = append(os.Environ(), "AGENT_WORKSPACE_F1_HELPER=1")
	p.cmd.Stderr = &p.stderr
	p.cmd.WaitDelay = time.Second
	p.input, err = p.cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := p.cmd.StdoutPipe()
	if err != nil {
		cancel()
		p.input.Close()
		t.Fatal(err)
	}
	if err := p.cmd.Start(); err != nil {
		cancel()
		p.input.Close()
		t.Fatal(err)
	}
	go func() {
		scanner := bufio.NewScanner(output)
		if scanner.Scan() {
			p.ready <- scanner.Text()
		}
		_, _ = io.Copy(io.Discard, output)
	}()
	go func() {
		p.err = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		p.input.Close()
		cancel()
		<-p.done
	})
	return p
}

func (p *instanceProcess) url(t *testing.T) string {
	t.Helper()
	select {
	case line := <-p.ready:
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, "http://") {
				return field
			}
		}
		t.Fatalf("helper did not report startup URL: %s", line)
	case <-p.done:
		t.Fatalf("helper exited before startup: %v %s", p.err, &p.stderr)
	case <-time.After(10 * time.Second):
		t.Fatal("helper startup timed out")
	}
	return ""
}

func (p *instanceProcess) rejected(t *testing.T) {
	t.Helper()
	select {
	case line := <-p.ready:
		t.Fatalf("second process accepted the same data directory: %s", line)
	case <-p.done:
		if p.err == nil || !strings.Contains(p.stderr.String(), "数据目录已被其他本地实例使用") {
			t.Fatalf("expected prompt ownership rejection: %v %s", p.err, &p.stderr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ownership acquisition blocked instead of failing promptly")
	}
}

func (p *instanceProcess) stop(t *testing.T, crash bool) {
	t.Helper()
	if crash {
		if err := p.cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
	} else if err := p.input.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
		if !crash && p.err != nil {
			t.Fatalf("normal shutdown failed: %v %s", p.err, &p.stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("helper did not stop")
	}
}

func otherListenAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}

func TestInstanceOwnershipAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, instanceLockName)
	contents := []byte("999999\nstale ownership text is not a lock\n")
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	owner := launchInstance(t, dir, "127.0.0.1:0")
	t.Logf("owner: %s", owner.url(t))
	second := launchInstance(t, dir, otherListenAddress(t))
	second.rejected(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	launchInstance(t, alias, "127.0.0.1:0").rejected(t)
	other := launchInstance(t, t.TempDir(), "127.0.0.1:0")
	t.Logf("independent data directory: %s", other.url(t))
	other.stop(t, false)
	owner.stop(t, false)
	for _, crash := range []bool{true, false} {
		restarted := launchInstance(t, dir, "127.0.0.1:0")
		t.Logf("reacquired after release: %s; kill=%t", restarted.url(t), crash)
		restarted.stop(t, crash)
		current, err := os.Stat(path)
		body, readErr := os.ReadFile(path)
		if err != nil || readErr != nil || !os.SameFile(original, current) || !bytes.Equal(body, contents) {
			t.Fatalf("lock file was replaced, removed or truncated: %v %v", err, readErr)
		}
	}
}

func TestInstanceStartupRecoveryIsFencedByOwnership(t *testing.T) {
	dir := t.TempDir()
	fixture := newExecutionFixture(t, "1.4.0")
	selected := provider.Discover(provider.Search{Explicit: []provider.Location{{Name: provider.Pi, Path: fixture.path}}})[0]
	ctx := context.Background()
	db, err := store.Open(filepath.Join(dir, "workspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	group, err := db.CreateGroup(ctx, store.CreateGroupInput{Name: "recovery", ActorID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	var deliveries []domain.Delivery
	for range 2 {
		member, err := db.CreateMember(ctx, store.CreateMemberInput{GroupID: group.ID, Name: "member", Role: "role", ActorID: "user", ProviderID: selected.ID})
		if err != nil {
			t.Fatal(err)
		}
		message, err := db.CreateMessage(ctx, store.MessageRequest{GroupID: group.ID, ActorID: "user", Content: "input"}, []string{member.ID})
		if err != nil {
			t.Fatal(err)
		}
		deliveries = append(deliveries, message.Deliveries[0])
	}
	initial, err := db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: group.ID, DeliveryID: deliveries[0].ID, MaxActive: 1, ResourceKeys: []string{"fixture-workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	owner := launchInstance(t, dir, "127.0.0.1:0")
	url := owner.url(t)
	recovered, err := db.GetTurn(ctx, group.ID, initial.ID)
	if err != nil || recovered.State != domain.TurnStateUnknown || recovered.ClaimToken == initial.ClaimToken {
		t.Fatalf("owned startup did not fence the starting turn: %+v %v", recovered, err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	for range 3 {
		response, err := client.Get(url + "/api/v1/groups")
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 {
			t.Fatal("owned HTTP server was not ready")
		}
	}
	launchInstance(t, dir, otherListenAddress(t)).rejected(t)
	unchanged, err := db.GetTurn(ctx, group.ID, initial.ID)
	if err != nil || unchanged.ClaimToken != recovered.ClaimToken || unchanged.State != domain.TurnStateUnknown || unchanged.UpdatedAt != recovered.UpdatedAt {
		t.Fatal("HTTP access or a failed second owner refenced the turn")
	}
	owner.stop(t, false)
	restarted := launchInstance(t, dir, "127.0.0.1:0", "--enable-execution", "--pi-path", fixture.path)
	restarted.url(t)
	again, err := db.GetTurn(ctx, group.ID, initial.ID)
	if err != nil || again.State != domain.TurnStateUnknown || again.ClaimToken == recovered.ClaimToken {
		t.Fatal("new owned startup did not keep unknown work fenced")
	}
	time.Sleep(250 * time.Millisecond)
	stable, err := db.GetTurn(ctx, group.ID, initial.ID)
	if err != nil || stable.ClaimToken != again.ClaimToken || stable.UpdatedAt != again.UpdatedAt {
		t.Fatalf("scheduler repeated startup recovery: %+v %v", stable, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "calls")); !os.IsNotExist(err) {
		t.Fatalf("enabled restart blindly re-executed unknown work or bypassed capacity: %v", err)
	}
	for _, input := range []store.ClaimTurnInput{
		{GroupID: group.ID, DeliveryID: deliveries[1].ID, MaxActive: 1},
		{GroupID: group.ID, DeliveryID: deliveries[1].ID, MaxActive: 2, ResourceKeys: []string{"fixture-workspace"}},
	} {
		if _, err := db.ClaimTurn(ctx, input); !errors.Is(err, store.ErrTurnBusy) {
			t.Fatalf("unknown work released capacity or resources: %v", err)
		}
	}
	restarted.stop(t, false)
}

func TestInstanceRejectsUnsafeFilesBeforeOpeningDatabase(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, instanceLockName)
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(target, path)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			p := launchInstance(t, dir, "127.0.0.1:0")
			select {
			case line := <-p.ready:
				t.Fatalf("unsafe lock file accepted: %s", line)
			case <-p.done:
				if p.err == nil || !chinese(p.stderr.String()) {
					t.Fatalf("unsafe lock must fail in Chinese: %v %s", p.err, &p.stderr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("unsafe lock file blocked startup")
			}
			if _, err := os.Stat(filepath.Join(dir, "workspace.db")); !os.IsNotExist(err) {
				t.Fatal("unsafe lock file allowed database initialization")
			}
			if body, err := os.ReadFile(target); err != nil || string(body) != "unchanged" {
				t.Fatal("lock-file target was modified")
			}
		})
	}
}

func TestExecutionSignalDrainsReceiptsAndHTTPBeforeOwnershipRelease(t *testing.T) {
	f, dir := newExecutionFixture(t, "1.4.0"), t.TempDir()
	if err := os.WriteFile(filepath.Join(f.root, "hold"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	owner := launchInstance(t, dir, "127.0.0.1:0", "--enable-execution", "--pi-path", f.path)
	url := owner.url(t)
	_, _, message := enqueueExecution(t, url, executionProvider(t, url).ID, "")
	observer, err := sql.Open("sqlite", filepath.Join(dir, "workspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	var turnID string
	awaitExecution(t, func() bool {
		var state string
		err := observer.QueryRow("SELECT turn_id,state FROM deliveries WHERE id=?", message.Deliveries[0].ID).Scan(&turnID, &state)
		return err == nil && state == "accepted"
	})
	tx, err := observer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE groups SET name=name WHERE id=?", message.GroupID); err != nil {
		t.Fatal(err)
	}
	connection, err := net.DialTimeout("tcp", strings.TrimPrefix(url, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	body := `{"name":"drain 后提交"}`
	if _, err := fmt.Fprintf(connection, "POST /api/v1/groups HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nExpect: 100-continue\r\n\r\n", strings.TrimPrefix(url, "http://"), len(body)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil || response.StatusCode != http.StatusContinue {
		t.Fatalf("HTTP handler did not enter its body read: %v %v", response, err)
	}
	response.Body.Close()
	if err := owner.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-owner.done:
		t.Fatalf("signal released ownership before receipt and HTTP drain: %v %s", owner.err, &owner.stderr)
	case <-time.After(20 * time.Millisecond):
	}
	assertLocked := func() {
		t.Helper()
		if lock, err := lockDataDir(dir); err == nil {
			lock.Close()
			t.Fatal("process released its data-dir lock during drain")
		}
	}
	assertLocked()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var receipt provider.ExecutionReceipt
	awaitExecution(t, func() bool {
		var raw string
		err := observer.QueryRow("SELECT receipt_json FROM turns WHERE id=?", turnID).Scan(&raw)
		return err == nil && raw != "" && json.Unmarshal([]byte(raw), &receipt) == nil
	})
	if !receipt.Started || !receipt.Accepted || !receipt.DirectChildReaped || receipt.Outcome != provider.OutcomeUnknown {
		t.Fatalf("signal did not cancel, reap and persist the real fixture execution: %+v", receipt)
	}
	select {
	case <-owner.done:
		t.Fatalf("HTTP handler was not drained after the execution receipt: %v", owner.err)
	default:
	}
	assertLocked()
	if _, err := io.WriteString(connection, body); err != nil {
		t.Fatal(err)
	}
	response, err = http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("DB closed before active HTTP handler committed: %d", response.StatusCode)
	}
	select {
	case <-owner.done:
		if owner.err != nil {
			t.Fatalf("normal signal shutdown failed: %v %s", owner.err, &owner.stderr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("process did not finish after both drains")
	}
	lock, err := lockDataDir(dir)
	if err != nil {
		t.Fatalf("fully drained process retained ownership: %v", err)
	}
	defer lock.Close()
	var state string
	var committed int
	if err := observer.QueryRow("SELECT state FROM turns WHERE id=?", turnID).Scan(&state); err != nil || state != "unknown" {
		t.Fatalf("cancelled unknown outcome was lost: state=%s err=%v", state, err)
	}
	if err := observer.QueryRow("SELECT count(*) FROM groups WHERE name=?", "drain 后提交").Scan(&committed); err != nil || committed != 1 {
		t.Fatalf("HTTP drain lost its commit: count=%d err=%v", committed, err)
	}
	t.Logf("SIGTERM: receipt reaped=%t outcome=%s; HTTP committed=%d; ownership retained until both drains", receipt.DirectChildReaped, receipt.Outcome, committed)
}

func TestInstanceDescriptorIsCloseOnExec(t *testing.T) {
	lock, err := lockDataDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, lock.(*instanceLock).file.Fd(), syscall.F_GETFD, 0)
	if errno != 0 || flags&syscall.FD_CLOEXEC == 0 {
		t.Fatalf("child CLIs could inherit the ownership descriptor: %v", errno)
	}
}
