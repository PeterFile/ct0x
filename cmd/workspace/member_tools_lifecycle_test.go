package main

import (
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

	"agentworkspace/internal/domain"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

type memberToolsTestExecutor func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt

func (execute memberToolsTestExecutor) Execute(ctx context.Context, request provider.ExecuteRequest, event func(provider.Event) error) provider.ExecutionReceipt {
	return execute(ctx, request, event)
}

func memberToolsSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("member tool lifecycle signal timed out")
	}
}

// Use the existing serve and B factory seams, without production hooks. The
// independent B drain regression covers an authorized operation lasting >5s.
func TestMemberToolsShutdownJoinsHTTPNativeAndReceiptBeforeOwnershipClose(t *testing.T) {
	for _, order := range []string{"http-first", "native-first"} {
		t.Run(order, func(t *testing.T) {
			dir := t.TempDir()
			lock, err := lockDataDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			db, err := store.Open(filepath.Join(dir, "workspace.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx := context.Background()
			if _, err := db.RecoverInFlight(ctx); err != nil {
				t.Fatal(err)
			}
			group, err := db.CreateGroup(ctx, store.CreateGroupInput{Name: "关闭排空"})
			if err != nil {
				t.Fatal(err)
			}
			member, err := db.CreateMember(ctx, store.CreateMemberInput{GroupID: group.ID, Name: "成员", Role: "职责", CreatorID: "user", OwnerID: "user"})
			if err != nil {
				t.Fatal(err)
			}
			message, err := db.CreateMessage(ctx, store.MessageRequest{GroupID: group.ID, ActorID: "user", Content: "触发", RecipientIDs: []string{member.ID}}, []string{member.ID})
			if err != nil {
				t.Fatal(err)
			}
			root, err := privateExecutionRoot(dir, "member-tools")
			if err != nil {
				t.Fatal(err)
			}
			runtimeDir, err := privateExecutionRoot(dir, "runtime")
			if err != nil {
				t.Fatal(err)
			}
			workspaces, err := privateExecutionRoot(dir, "workspaces")
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			runtime, err := platform.NewMemberToolRuntime(db, platform.MemberToolRuntimeOptions{ConfigRoot: root,
				Endpoint: "http://" + listener.Addr().String() + "/api/v1/member-tools", MaxGroupMembers: 4,
				Operations: []string{"list_members", "read_channel", "send_message", "create_member"}})
			if err != nil {
				t.Fatal(err)
			}
			nativeGate, nativeCancelled, scheduleDrained := make(chan struct{}), make(chan struct{}), make(chan struct{})
			httpGate, httpEntered, httpCancelled := make(chan struct{}), make(chan struct{}), make(chan struct{})
			releaseNative := sync.OnceFunc(func() { close(nativeGate) })
			releaseHTTP := sync.OnceFunc(func() { close(httpGate) })
			configReady := make(chan memberToolsReport, 1)
			d := message.Deliveries[0]
			executor, err := runtime.Wrap(platform.MemberToolRunBinding{GroupID: group.ID, MemberID: member.ID, ConversationID: d.ConversationID,
				DeliveryID: d.ID, ProviderID: "fixture-provider", Model: "fixture-model"}, func(configPath, runID string, _ []string) (provider.Executor, error) {
				return memberToolsTestExecutor(func(ctx context.Context, request provider.ExecuteRequest, event func(provider.Event) error) provider.ExecutionReceipt {
					for _, kind := range []provider.EventKind{provider.SessionEvent, provider.AcceptedEvent} {
						if err := event(provider.Event{Kind: kind, NativeSessionID: runID}); err != nil {
							return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, DeliveryFailed: true}
						}
					}
					configReady <- memberToolsReport{RunID: runID, ConfigPath: configPath}
					<-ctx.Done()
					close(nativeCancelled)
					<-nativeGate
					return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Started: true, Accepted: true, NativeSessionID: runID, Reason: "合成执行取消后结果未知"}
				}), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			routes := httpapi.MemberToolRoutes(runtime.Tools())
			handlerDB := make(chan error, 1)
			for i := range routes {
				if routes[i].Pattern == "/api/v1/member-tools/list_members" {
					original := routes[i].Handler
					routes[i].Handler = func(w http.ResponseWriter, r *http.Request) {
						original(w, r)
						close(httpEntered)
						<-r.Context().Done()
						close(httpCancelled)
						<-httpGate
						_, err := db.GetGroup(context.Background(), group.ID)
						handlerDB <- err
					}
				}
			}
			server, err := httpapi.NewServer(listener.Addr().String(), routes...)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			parent, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			var result error
			go func() {
				err := serve(parent, server, listener, 30*time.Millisecond, func(ctx context.Context) error {
					err := scheduler.Run(ctx, db, scheduler.Options{MaxActive: 1, Recovered: true, RuntimeDir: runtimeDir}, func(context.Context, store.PendingDelivery) (scheduler.PreparedExecution, error) {
						return scheduler.PreparedExecution{Executor: executor, ResourceKeys: []string{"fixture-resource"}, Request: provider.ExecuteRequest{CWD: workspaces, Prompt: "fixture", Model: "fixture-model"}}, nil
					})
					close(scheduleDrained)
					return err
				})
				result = errors.Join(err, db.Close(), lock.Close())
				close(done)
			}()
			t.Cleanup(func() { cancel(); releaseHTTP(); releaseNative(); <-done })
			var report memberToolsReport
			select {
			case report = <-configReady:
			case <-time.After(3 * time.Second):
				t.Fatal("postclaim executor did not start")
			}
			var config memberToolsNativeConfig
			memberToolsReadJSON(t, report.ConfigPath, &config)
			requestDone := make(chan struct{})
			go func() {
				defer close(requestDone)
				request, _ := http.NewRequest("POST", config.Endpoint+"/list_members", strings.NewReader("{}"))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer "+config.Token)
				client := &http.Client{Timeout: 3 * time.Second}
				defer client.CloseIdleConnections()
				if response, err := client.Do(request); err == nil {
					_, _ = io.Copy(io.Discard, response.Body)
					response.Body.Close()
				}
			}()
			memberToolsSignal(t, httpEntered)
			cancel()
			memberToolsSignal(t, nativeCancelled)
			memberToolsSignal(t, httpCancelled) // HTTP grace expired; the handler is still owned.
			assertOwned := func() {
				t.Helper()
				select {
				case <-done:
					t.Fatalf("shutdown abandoned an active owner: %v", result)
				default:
				}
				if _, err := db.GetGroup(ctx, group.ID); err != nil {
					t.Fatal("store closed during active HTTP/native execution")
				}
				if competing, err := lockDataDir(dir); err == nil {
					competing.Close()
					t.Fatal("process lock released during active HTTP/native execution")
				}
			}
			assertOwned()
			if _, err := os.Stat(report.ConfigPath); err != nil {
				t.Fatal("cancellation cleaned config before native Execute drained")
			}
			if order == "http-first" {
				releaseHTTP()
				if err := <-handlerDB; err != nil {
					t.Fatal("last HTTP handler lost its store")
				}
				assertOwned()
				releaseNative()
			} else {
				releaseNative()
				memberToolsSignal(t, scheduleDrained)
				assertOwned()
				turn, err := db.GetTurn(ctx, group.ID, report.RunID)
				if err != nil || turn.State != domain.TurnStateUnknown {
					t.Fatal("scheduler did not persist its unknown receipt before drain")
				}
				releaseHTTP()
				if err := <-handlerDB; err != nil {
					t.Fatal("last HTTP handler lost its store after receipt drain")
				}
			}
			memberToolsSignal(t, done)
			memberToolsSignal(t, requestDone)
			if !errors.Is(result, context.DeadlineExceeded) {
				t.Fatalf("HTTP drain failure leaf was hidden: %v", result)
			}
			if _, err := os.Stat(report.ConfigPath); !os.IsNotExist(err) {
				t.Fatal("joined Execute did not remove config")
			}
			if _, err := runtime.Tools().ListMembers(ctx, config.Token, store.ListOptions{}); !errors.Is(err, store.ErrMemberToolDenied) {
				t.Fatal("joined Execute did not revoke token")
			}
			if _, err := db.GetGroup(ctx, group.ID); err == nil {
				t.Fatal("completed owner did not close its Store")
			}
			reopened, err := lockDataDir(dir)
			if err != nil {
				t.Fatal("completed shutdown retained the process lock")
			}
			reopened.Close()
		})
	}
}

func TestMemberToolsIPv6PortZeroAndPiReadiness(t *testing.T) {
	for _, mode := range []string{"ipv6", "missing-model", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			version := "1.4.0"
			if mode == "unsupported" {
				version = "1.4.1"
			}
			f, dir := newExecutionFixture(t, version), t.TempDir()
			args := memberToolsArgs(t, dir, f.path)
			if mode == "ipv6" {
				args = append(args, "--listen", "[0:0:0:0:0:0:0:1]:0")
			}
			base, stop := startRun(t, args...)
			memberToolsHTTP(t, base+"/api/v1/member-tools", "wrong-token", "list_members", map[string]any{}, "", 403, "forbidden")
			if mode == "ipv6" {
				if !strings.HasPrefix(base, "http://[::1]:") || strings.HasSuffix(base, ":0") {
					t.Fatal("IPv6 ephemeral listener was not canonicalized")
				}
			} else {
				var group domain.Group
				executionJSON(t, "POST", base+"/api/v1/groups", platform.GroupInput{Name: "准备门禁"}, &group)
				var member domain.Member
				input := platform.MemberInput{Name: "无效配置成员", Role: "职责", ProviderID: executionProvider(t, base).ID}
				if mode == "unsupported" {
					input.Model = "fixture-model"
				}
				executionJSON(t, "POST", base+"/api/v1/groups/"+group.ID+"/members", input, &member)
				var message domain.Message
				executionJSON(t, "POST", base+"/api/v1/groups/"+group.ID+"/messages", platform.MessageInput{Content: "不应执行", RecipientIDs: []string{member.ID}}, &message)
				time.Sleep(250 * time.Millisecond) // Observe multiple scheduler polls.
				executionJSON(t, "GET", base+"/api/v1/groups/"+group.ID+"/messages/"+message.ID, nil, &message)
				if message.Deliveries[0].State != domain.DeliveryStateQueued || message.Deliveries[0].TurnID != "" {
					t.Fatal("unready Pi configuration obtained a physical claim")
				}
			}
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(filepath.Join(dir, "member-tools"))
			if err != nil || len(entries) != 0 {
				t.Fatal("unclaimed execution produced credentials")
			}
			if _, err := os.Stat(filepath.Join(f.root, "calls")); !os.IsNotExist(err) {
				t.Fatal("readiness-only composition invoked native execution")
			}
		})
	}
}
