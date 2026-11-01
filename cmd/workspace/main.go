package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
	"agentworkspace/internal/webassets"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "运行失败：", err)
		os.Exit(1)
	}
}

func runWithSearch(ctx context.Context, args []string, output io.Writer, search provider.Search) (err error) {
	config, err := parseConfig(args, output)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("启动已取消：%w", err)
	}
	if err := os.MkdirAll(config.dataDir, 0700); err != nil {
		return fmt.Errorf("创建数据目录失败：%w", err)
	}
	lock, err := lockDataDir(config.dataDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	db, err := store.Open(filepath.Join(config.dataDir, "workspace.db"))
	if err != nil {
		return fmt.Errorf("打开工作区数据库失败：%w", err)
	}
	// serve joins scheduler and HTTP drains before store and lock closure.
	defer func() { err = errors.Join(err, db.Close()) }()
	if _, err := db.RecoverInFlight(ctx); err != nil {
		return fmt.Errorf("恢复在途轮次失败：%w", err)
	}
	search.Explicit = append([]provider.Location{{Name: provider.Pi, Path: config.piPath}, {Name: provider.Codex, Path: config.codexPath}}, search.Explicit...)
	catalog := platform.DiscoverProviders(ctx, search, provider.Runner{Timeout: 2 * time.Second, OutputLimit: 4096})
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("启动已取消：%w", err)
	}
	// Reserve only: no HTTP admission or scheduler claims before composition succeeds.
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", config.listen)
	if err != nil {
		return fmt.Errorf("监听本地地址失败：%w", err)
	}
	defer listener.Close()
	var schedule func(context.Context) error
	var memberTools *platform.MemberToolRuntime
	executionStatus := "执行已禁用，投递仅排队"
	toolStatus := "平台工具已禁用；Pi 禁用工具，Codex 仅原生只读工具且无平台令牌"
	if config.enableExecution {
		schedule, memberTools, err = executionRunner(config, db, catalog, "http://"+listener.Addr().String()+"/api/v1/member-tools")
		if err != nil {
			return err
		}
		executionStatus = "全新单轮执行已启用，最大并发 1"
	}
	if memberTools != nil {
		toolStatus = "平台工具已配置：仅已验证 Pi 1.4.0 且显式指定模型时可用四项基础成员工具；Codex 仅原生只读工具且无平台令牌；不授予 Coordinator、Assignment 或 RequestWork 权限"
	}
	local := platform.NewLocalUser(db)
	engine := coordination.New(db)
	coordinations := platform.NewCoordinations(engine)
	routes := append(httpapi.LocalUserRoutes(local), httpapi.MessageRoutes(local)...)
	routes = append(routes, httpapi.TaskRoutes(local)...)
	routes = append(routes, httpapi.HistoryRoutes(local)...)
	routes = append(routes, httpapi.CoordinationRoutes(coordinations)...)
	routes = append(routes, httpapi.ProviderRoute(catalog))
	if memberTools != nil {
		routes = append(routes, httpapi.MemberToolRoutes(memberTools.Tools())...)
	}
	if config.webDir != "" {
		web, openErr := webassets.Open(config.webDir)
		if openErr != nil {
			return openErr
		}
		defer func() { err = errors.Join(err, web.Close()) }()
		routes = append(routes,
			httpapi.Route{Method: http.MethodGet, Pattern: "/", Handler: web.ServeHTTP},
			httpapi.Route{Method: http.MethodHead, Pattern: "/", Handler: web.ServeHTTP})
	}
	server, err := httpapi.NewServerWithConfig(httpapi.Config{Address: listener.Addr().String(), DevOrigin: config.devOrigin}, routes...)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "本地 HTTP 进程已启动： http://%s （已提供健康检查、群组与成员创建查询、消息持久化、历史与详情、排队投递记录、任务创建、列表与详情及 CLI 目录；目录仅启动时探测、证据仅存内存；%s；不支持恢复或运行中追加输入；%s；工作目录不是主机读取沙箱，数据目录及祖先必须可信且目录身份稳定；其他功能尚未启用）\n", listener.Addr(), executionStatus, toolStatus); err != nil {
		return fmt.Errorf("输出启动信息失败：%w", err)
	}
	return serve(ctx, server, listener, 5*time.Second, schedule)
}

func serve(parent context.Context, server *http.Server, listener net.Listener, grace time.Duration, schedule func(context.Context) error) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var active sync.WaitGroup
	var admission sync.Mutex
	stopping := false
	handler := server.Handler
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admission.Lock()
		if stopping {
			admission.Unlock()
			httpapi.WriteError(w, http.StatusServiceUnavailable, "shutting_down", "HTTP 进程正在关闭")
			return
		}
		active.Add(1)
		admission.Unlock()
		defer active.Done()
		handler.ServeHTTP(w, r)
	})
	var schedulerDone chan error
	if schedule != nil {
		schedulerDone = make(chan error, 1)
		go func() {
			err := schedule(ctx)
			cancel()
			schedulerDone <- err
		}()
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	var serveErr error
	received := false
	select {
	case <-ctx.Done():
	case serveErr = <-done:
		received = true
	}

	// Stop claims and cancel executors immediately, before waiting for HTTP drain.
	cancel()
	// Serve returns ErrServerClosed before Shutdown has drained active requests.
	// Do not derive this grace period from the already-cancelled process context.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), grace)
	defer shutdownCancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		shutdownErr = fmt.Errorf("HTTP 服务关闭失败：%w", errors.Join(shutdownErr, server.Close()))
	}
	// Close cancels request contexts, but does not wait for handlers. Fence late
	// admissions before Wait so the store stays open through the final handler.
	admission.Lock()
	stopping = true
	admission.Unlock()
	if !received {
		serveErr = <-done
	}
	active.Wait()
	var schedulerErr error
	if schedulerDone != nil {
		schedulerErr = <-schedulerDone // No timeout may release DB/lock before receipts drain.
	}
	if serveErr == http.ErrServerClosed {
		serveErr = nil
	} else if serveErr != nil {
		serveErr = fmt.Errorf("HTTP 服务异常停止：%w", serveErr)
	}
	return shutdownError(errors.Join(serveErr, shutdownErr, schedulerErr, parent.Err()))
}
