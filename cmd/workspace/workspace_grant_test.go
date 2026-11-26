package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

type workspaceGrantExecutorFunc func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt

func (f workspaceGrantExecutorFunc) Execute(ctx context.Context, request provider.ExecuteRequest, emit func(provider.Event) error) provider.ExecutionReceipt {
	return f(ctx, request, emit)
}

// Exercise the command's real serve/scheduler/preparer/error boundary without
// discovery or subprocesses. Only the already-prepared executor is a test seam.
func TestWorkspaceGrantStaleRootServerSurvivesRestart(t *testing.T) {
	for _, mutation := range []string{"missing", "replaced"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			lock, err := lockDataDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			private, err := privateExecutionRoot(dir, "workspaces")
			if err != nil {
				t.Fatal(err)
			}
			runtime, err := privateExecutionRoot(dir, "runtime")
			if err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(dir, "never-execute")
			invoked := sentinel + ".invoked"
			if err := os.WriteFile(sentinel, []byte("#!/bin/sh\nprintf invoked > '"+invoked+"'\nexit 99\n"), 0700); err != nil {
				t.Fatal(err)
			}
			opts := platform.PrepareOptions{PrivateWorkspaceRoot: private, PrivateDataRoot: dir,
				Providers: []provider.Provider{{ID: "local-fixture", Name: provider.Codex, Path: sentinel, Version: "0.162.1", Installation: provider.Installed}}}
			dbPath := filepath.Join(dir, "workspace.db")
			db, err := store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			group, err := db.CreateGroup(ctx, store.CreateGroupInput{Name: "stale external"})
			if err != nil {
				t.Fatal(err)
			}
			member, err := db.CreateMember(ctx, store.CreateMemberInput{GroupID: group.ID, Name: "reader", Role: "read", ProviderID: "local-fixture"})
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(t.TempDir(), "external")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			p, err := platform.NewPreparer(db, opts)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := p.AuthorizeWorkspaceGrant(ctx, group.ID, member.ID, platform.WorkspaceGrantInput{Root: root, Scope: "read_only"}, "authorize")
			if err != nil {
				t.Fatal(err)
			}
			message, err := db.CreateMessage(ctx, store.MessageRequest{GroupID: group.ID, ActorID: "user", Content: "eligible external delivery"}, []string{member.ID})
			if err != nil {
				t.Fatal(err)
			}
			// Verify eligibility before changing only the external directory.
			if _, err := p.Prepare(ctx, store.PendingDelivery{GroupID: group.ID, Delivery: message.Deliveries[0]}); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(root, root+"-old"); err != nil {
				t.Fatal(err)
			}
			if mutation == "replaced" {
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
			}
			for phase := range 2 {
				if phase != 0 {
					if err := db.Close(); err != nil {
						t.Fatal(err)
					}
					db, err = store.Open(dbPath)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := db.RecoverInFlight(ctx); err != nil {
						t.Fatal(err)
					}
				}
				p, err = platform.NewPreparer(db, opts)
				if err != nil {
					t.Fatal(err)
				}
				base, stop, attempts, executions := startWorkspaceGrantServer(t, db, p, runtime)
				awaitExecution(t, func() bool { return attempts.Load() >= 3 })
				var health map[string]any
				executionJSON(t, "GET", base+"/healthz", nil, &health)
				privateGroup, _, privateMessage := enqueueExecution(t, base, "local-fixture", "")
				awaitExecutionWithin(t, 10*time.Second, func() bool {
					stored, err := db.GetMessage(ctx, privateGroup.ID, privateMessage.ID)
					if err != nil || stored.Deliveries[0].TurnID == "" {
						return false
					}
					turn, err := db.GetTurn(ctx, privateGroup.ID, stored.Deliveries[0].TurnID)
					return err == nil && turn.State == domain.TurnStateNotStarted
				})
				var stored domain.Message
				executionJSON(t, "GET", base+"/api/v1/groups/"+group.ID+"/messages/"+message.ID, nil, &stored)
				if d := stored.Deliveries[0]; d.State != domain.DeliveryStateQueued || d.TurnID != "" {
					t.Fatalf("stale external delivery acquired admission: %+v", d)
				}
				var replay store.WorkspaceGrantReceipt
				executionJSON(t, "GET", base+"/api/v1/groups/"+group.ID+"/members/"+member.ID+"/workspace-grant", nil, &replay)
				if replay != receipt {
					t.Fatal("staleness changed immutable authority")
				}
				if err := stop(); err != nil {
					t.Fatalf("external staleness killed command HTTP/scheduler: %v", err)
				}
				if len(executions) != 1 {
					t.Fatalf("expected only a disjoint private admission: %d", len(executions))
				}
				if request := <-executions; filepath.Dir(request.CWD) != private || request.RunID == "" {
					t.Fatalf("wrong execution admitted: %+v", request)
				}
			}
			if _, err := os.Stat(invoked); !os.IsNotExist(err) {
				t.Fatal("server regression invoked a provider")
			}
		})
	}
}

func startWorkspaceGrantServer(t *testing.T, db *store.Store, p *platform.Preparer, runtime string) (string, func() error, *atomic.Int64, <-chan provider.ExecuteRequest) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	local := platform.NewLocalUser(db)
	routes := append(httpapi.LocalUserRoutes(local), httpapi.MessageRoutes(local)...)
	routes = append(routes, httpapi.WorkspaceGrantRoutes(p)...)
	server, err := httpapi.NewServerWithConfig(httpapi.Config{Address: listener.Addr().String()}, routes...)
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	attempts := new(atomic.Int64)
	executions := make(chan provider.ExecuteRequest, 8)
	executor := workspaceGrantExecutorFunc(func(_ context.Context, request provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
		executions <- request
		return provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}
	})
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, server, listener, time.Second, func(ctx context.Context) error {
			return scheduler.Run(ctx, db, scheduler.Options{MaxActive: 1, Recovered: true, RuntimeDir: runtime, PollInterval: 10 * time.Millisecond}, func(ctx context.Context, pending store.PendingDelivery) (scheduler.PreparedExecution, error) {
				got, err := p.Prepare(ctx, pending)
				attempts.Add(1)
				if err == nil {
					got.Executor = executor
				}
				return got, err
			})
		})
	}()
	var once sync.Once
	var result error
	stop := func() error {
		once.Do(func() { cancel(); result = <-done })
		return result
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	})
	return "http://" + listener.Addr().String(), stop, attempts, executions
}

// Only a temporary local server/database is used; no provider is discovered or run.
func TestWorkspaceGrantCommandComposition(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			dir, root := t.TempDir(), t.TempDir()
			marker := filepath.Join(root, "unchanged")
			if err := os.WriteFile(marker, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"--listen", "127.0.0.1:0", "--data-dir", dir}
			if enabled {
				args = append(args, "--enable-execution")
			}
			url, stop := startRun(t, args...)
			var group domain.Group
			executionJSON(t, "POST", url+"/api/v1/groups", platform.GroupInput{Name: "D1 fixture"}, &group)
			var member domain.Member
			executionJSON(t, "POST", url+"/api/v1/groups/"+group.ID+"/members", platform.MemberInput{Name: "reader", Role: "read", ProviderID: "no-provider"}, &member)
			path := "/api/v1/groups/" + group.ID + "/members/" + member.ID + "/workspace-grant"
			client := &http.Client{Timeout: 5 * time.Second}
			defer client.CloseIdleConnections()
			post := func(root, key string, want int) []byte {
				t.Helper()
				body, _ := json.Marshal(platform.WorkspaceGrantInput{Root: root, Scope: "read_only"})
				r, err := http.NewRequest("POST", url+path, bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Idempotency-Key", key)
				response, err := client.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				data, err := io.ReadAll(response.Body)
				if err != nil || response.StatusCode != want {
					t.Fatalf("grant status=%d want=%d body=%s err=%v", response.StatusCode, want, data, err)
				}
				return data
			}
			if !enabled {
				post(root, "authorize", 404)
				return
			}
			post(dir, "private", 400)
			first := post(root, "authorize", 201)
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			url, stop = startRun(t, args...)
			if replay := post(root, "authorize", 201); !bytes.Equal(first, replay) {
				t.Fatal("command reopen changed durable receipt")
			}
			var receipt store.WorkspaceGrantReceipt
			executionJSON(t, "GET", url+path, nil, &receipt)
			if receipt.Root != root || receipt.MemberID != member.ID || receipt.Scope != "read_only" {
				t.Fatalf("misbound command receipt: %+v", receipt)
			}
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(filepath.Join(dir, "workspace.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			stored, err := db.GetMember(context.Background(), group.ID, member.ID)
			if err != nil || stored.Workspace != root {
				t.Fatalf("command did not select authorized root: %+v, %v", stored, err)
			}
			data, err := os.ReadFile(marker)
			if err != nil || string(data) != "original" {
				t.Fatal("granting modified external files")
			}
		})
	}
}
