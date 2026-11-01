//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
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
		ctx, cancel := context.WithCancel(context.Background())
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

func launchInstance(t *testing.T, dir, address string) *instanceProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	p := &instanceProcess{ready: make(chan string, 1), done: make(chan struct{})}
	p.cmd = exec.CommandContext(ctx, executable, "-test.run=^TestInstanceProcessHelper$", "--", "--data-dir", dir, "--listen", address)
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
		member, err := db.CreateMember(ctx, store.CreateMemberInput{GroupID: group.ID, Name: "member", Role: "role", ActorID: "user"})
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
	restarted := launchInstance(t, dir, "127.0.0.1:0")
	restarted.url(t)
	again, err := db.GetTurn(ctx, group.ID, initial.ID)
	if err != nil || again.State != domain.TurnStateUnknown || again.ClaimToken == recovered.ClaimToken {
		t.Fatal("new owned startup did not keep unknown work fenced")
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
