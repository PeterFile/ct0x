package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/store"
)

func chinese(text string) bool {
	return strings.ContainsFunc(text, func(r rune) bool { return unicode.Is(unicode.Han, r) })
}

func TestConfigDefaultsAndHelp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	root, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	config, err := parseConfig(nil, io.Discard)
	if err != nil || config.listen != httpapi.DefaultAddress || config.dataDir != filepath.Join(root, "agent-workspace") || config.devOrigin != "" {
		t.Fatalf("unsafe or non-durable defaults: %+v %v", config, err)
	}
	for _, flag := range []string{"-h", "--help"} {
		var output bytes.Buffer
		if err := run(context.Background(), []string{flag}, &output); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"用法", "--listen", "127.0.0.1:7310", "--data-dir", "--dev-origin", "禁用"} {
			if !strings.Contains(output.String(), want) {
				t.Errorf("help missing %q: %s", want, &output)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "agent-workspace")); !os.IsNotExist(err) {
		t.Fatalf("config/help created runtime state: %v", err)
	}
}

func TestInvalidConfigurationCreatesNoRuntimeState(t *testing.T) {
	for _, args := range [][]string{
		{"--listen", "localhost:7310"}, {"--listen", "attacker.example:7310"}, {"--listen", "0.0.0.0:7310"},
		{"--listen", "[::]:7310"}, {"--listen", "192.168.1.1:7310"}, {"--listen", "[::1%lo0]:7310"},
		{"--listen", "127.0.0.1"}, {"--listen", "127.0.0.1:65536"}, {"--listen", ""},
		{"--unknown"}, {"--listen"}, {"unexpected"}, {"--data-dir", ""},
		{"--dev-origin", "http://localhost:5173"}, {"--dev-origin", "https://127.0.0.1:5173"},
		{"--dev-origin", ""}, {"--dev-origin", "http://127.0.0.1:5173", "--dev-origin", "http://127.0.0.1:5173"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "must-not-exist")
			var output bytes.Buffer
			err := run(context.Background(), append([]string{"--data-dir", dir}, args...), &output)
			if err == nil || !chinese(err.Error()) {
				t.Fatalf("invalid config must fail in Chinese: %v", err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("invalid config created runtime state: %v", err)
			}
			if strings.Contains(output.String(), "http://") {
				t.Fatalf("failure printed a startup URL: %s", &output)
			}
		})
	}
}

func TestStartupFailures(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(base, "corrupt")
	if err := os.Mkdir(corrupt, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corrupt, "workspace.db"), []byte("not SQLite"), 0600); err != nil {
		t.Fatal(err)
	}
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	for _, tt := range []struct{ name, dir, listen string }{
		{"data dir is file", file, "127.0.0.1:0"},
		{"invalid parent", filepath.Join(file, "child"), "127.0.0.1:0"},
		{"store open failure", corrupt, "127.0.0.1:0"},
		{"listen failure", filepath.Join(base, "occupied"), occupied.Addr().String()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := run(context.Background(), []string{"--listen", tt.listen, "--data-dir", tt.dir}, &output); err == nil || !chinese(err.Error()) {
				t.Fatalf("expected startup error, got %v", err)
			}
			if output.Len() != 0 {
				t.Fatalf("failed startup printed success: %s", &output)
			}
		})
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	unused := filepath.Join(base, "cancelled")
	if err := run(cancelled, []string{"--listen", "127.0.0.1:0", "--data-dir", unused}, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled startup must stop: %v", err)
	}
	if _, err := os.Stat(unused); !os.IsNotExist(err) {
		t.Fatalf("cancelled startup created runtime state: %v", err)
	}
}

type startupWriter struct{ lines chan string }

func (w startupWriter) Write(body []byte) (int, error) {
	w.lines <- string(body)
	return len(body), nil
}

func startRun(t *testing.T, args ...string) (string, func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	output := startupWriter{lines: make(chan string, 4)}
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, output) }()
	var once sync.Once
	var result error
	stop := func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-done:
			case <-time.After(10 * time.Second):
				result = errors.New("run did not stop")
			}
		})
		return result
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	})
	select {
	case line := <-output.lines:
		if !chinese(line) || !strings.Contains(line, "仅") || !strings.Contains(line, "健康检查") {
			t.Fatalf("startup must describe the health-only boundary: %s", line)
		}
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, "http://") {
				return field, stop
			}
		}
		t.Fatalf("startup did not report actual URL: %s", line)
	case err := <-done:
		once.Do(func() { cancel(); result = err })
		t.Fatalf("run exited before startup: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("startup timed out")
	}
	return "", stop
}

func TestRunLifecycleAndPersistence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runtime ?#_pragma=foreign_keys(0)")
	url, stop := startRun(t, "--listen", "127.0.0.1:0", "--data-dir", dir, "--dev-origin", "http://127.0.0.1:5173")
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("data directory must be private: %v %v", info, err)
	}
	path := filepath.Join(dir, "workspace.db")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("server started without opening SQLite: %v", err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	group, err := db.CreateGroup(context.Background(), store.CreateGroupInput{Name: "重启后仍存在"})
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("seed persistent state: %v %v", err, closeErr)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	for _, tt := range []struct {
		path string
		code int
	}{
		{"/healthz", http.StatusOK}, {"/api/v1/groups", http.StatusNotFound}, {"/api/v1/groups/group/members", http.StatusNotFound},
	} {
		response, err := client.Get(url + tt.path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != tt.code || strings.Contains(string(body), dir) {
			t.Fatalf("unexpected health-only HTTP response: %d %s %v", response.StatusCode, body, err)
		}
	}
	r, _ := http.NewRequest(http.MethodGet, url+"/healthz", nil)
	r.Host = "attacker.example"
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("ephemeral listener lost Host guard: %d", response.StatusCode)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	address := strings.TrimPrefix(url, "http://")
	rebound, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("shutdown did not release listener: %v", err)
	}
	rebound.Close()
	_, stopAgain := startRun(t, "--listen", address, "--data-dir", dir)
	if err := stopAgain(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.GetGroup(context.Background(), group.ID)
	if err != nil || got != group {
		t.Fatalf("restart lost persistent data: %+v %v", got, err)
	}
}

func TestServeDrainsBeforeClosingStore(t *testing.T) {
	for _, mode := range []string{"graceful", "forced", "external shutdown"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "workspace.db")
			db, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			entered, release, forced := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			wrote := make(chan error, 1)
			server, err := httpapi.NewServer(listener.Addr().String(), httpapi.Route{Method: http.MethodGet, Pattern: "/slow", Handler: func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				if mode == "forced" {
					<-r.Context().Done()
					close(forced)
				}
				<-release
				_, err := db.CreateGroup(context.Background(), store.CreateGroupInput{Name: "在途请求"})
				wrote <- err
				httpapi.WriteJSON(w, http.StatusOK, struct{}{})
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			shuttingDown := make(chan struct{})
			var shutdownOnce sync.Once
			server.RegisterOnShutdown(func() { shutdownOnce.Do(func() { close(shuttingDown) }) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			grace := time.Second
			if mode == "forced" {
				grace = 50 * time.Millisecond
			}
			done := make(chan error, 1)
			go func() {
				err := serve(ctx, server, listener, grace)
				done <- errors.Join(err, db.Close())
			}()
			requestDone := make(chan error, 1)
			go func() {
				client := &http.Client{Timeout: 3 * time.Second}
				defer client.CloseIdleConnections()
				response, err := client.Get("http://" + listener.Addr().String() + "/slow")
				if err == nil {
					_, err = io.Copy(io.Discard, response.Body)
					response.Body.Close()
				}
				requestDone <- err
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("serve returned before accepting requests: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("request never entered")
			}
			if mode == "external shutdown" {
				go server.Shutdown(context.Background())
			} else {
				cancel()
			}
			select {
			case <-shuttingDown:
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown never started")
			}
			if mode == "forced" {
				select {
				case <-forced:
				case <-time.After(5 * time.Second):
					t.Fatal("grace expiry did not close active connections")
				}
			}
			select {
			case err := <-done:
				t.Fatalf("Serve returning ErrServerClosed must not release the store before handlers drain: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			if _, err := db.ListGroups(context.Background(), store.ListOptions{}); err != nil {
				t.Fatalf("store closed while request is active: %v", err)
			}
			unblock()
			select {
			case err := <-done:
				if mode == "forced" && !errors.Is(err, context.DeadlineExceeded) || mode != "forced" && err != nil {
					t.Fatalf("unexpected shutdown result: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown failed to finish")
			}
			if err := <-wrote; err != nil {
				t.Fatalf("in-flight request could not commit before store close: %v", err)
			}
			if err := <-requestDone; mode != "forced" && err != nil {
				t.Fatal(err)
			}
			reopened, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			page, err := reopened.ListGroups(context.Background(), store.ListOptions{})
			if err != nil || len(page.Items) != 1 || page.Items[0].Name != "在途请求" {
				t.Fatalf("drain lost committed request: %+v %v", page, err)
			}
		})
	}
}
