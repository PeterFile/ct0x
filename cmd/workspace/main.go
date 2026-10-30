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

	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
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
	db, err := store.Open(filepath.Join(config.dataDir, "workspace.db"))
	if err != nil {
		return fmt.Errorf("打开工作区数据库失败：%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", config.listen)
	if err != nil {
		return fmt.Errorf("监听本地地址失败：%w", err)
	}
	defer listener.Close()
	search.Explicit = append([]provider.Location{{Name: provider.Pi, Path: config.piPath}, {Name: provider.Codex, Path: config.codexPath}}, search.Explicit...)
	catalog := platform.DiscoverProviders(ctx, search, provider.Runner{Timeout: 2 * time.Second, OutputLimit: 4096})
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("启动已取消：%w", err)
	}
	routes := append(httpapi.LocalUserRoutes(platform.NewLocalUser(db)), httpapi.ProviderRoute(catalog))
	server, err := httpapi.NewServerWithConfig(httpapi.Config{Address: listener.Addr().String(), DevOrigin: config.devOrigin}, routes...)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "本地 HTTP 进程已启动： http://%s （已提供健康检查、群组与成员创建查询及 CLI 目录；目录仅启动时探测、证据仅存内存，其他功能尚未启用）\n", listener.Addr()); err != nil {
		return fmt.Errorf("输出启动信息失败：%w", err)
	}
	return serve(ctx, server, listener, 5*time.Second)
}

func serve(ctx context.Context, server *http.Server, listener net.Listener, grace time.Duration) error {
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
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	var serveErr error
	received := false
	select {
	case <-ctx.Done():
	case serveErr = <-done:
		received = true
	}

	// Serve returns ErrServerClosed before Shutdown has drained active requests.
	// Do not derive this grace period from the already-cancelled process context.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
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
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	} else if serveErr != nil {
		serveErr = fmt.Errorf("HTTP 服务异常停止：%w", serveErr)
	}
	return errors.Join(serveErr, shutdownErr)
}
