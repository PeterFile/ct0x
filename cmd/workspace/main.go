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
	"strings"
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
	if err := config.validateInvestigationDataDir(); err != nil {
		return err
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
	engine := coordination.New(db)
	artifactRoutes, artifactCloser, err := artifactReadRoutes(config.dataDir, db)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, artifactCloser.Close()) }()
	var workspaceOptions []platform.MemberWorkspaceOptions
	if config.enableExecution && config.enableMemberTools && config.enableCoordinationRoles {
		options, err := workspaceRuntimeOptions(config.dataDir, artifactCloser)
		if err != nil {
			return err
		}
		workspaceOptions = append(workspaceOptions, options)
	}
	var schedule func(context.Context) error
	var memberTools *platform.MemberToolRuntime
	var preparer *platform.Preparer
	executionStatus := "执行已禁用，投递仅排队"
	toolStatus := "平台工具已禁用；Pi 禁用工具，Codex 仅原生只读工具且无平台令牌"
	if config.enableExecution {
		schedule, memberTools, preparer, err = executionRunner(config, db, engine, catalog, "http://"+listener.Addr().String()+"/api/v1/member-tools", output, workspaceOptions...)
		if err != nil {
			return err
		}
		if memberTools != nil {
			defer func() { err = errors.Join(err, memberTools.Close()) }()
		}
		catalog, err = catalog.WithAdapterReports(preparer.AdapterReports())
		if err != nil {
			return err
		}
		executionStatus = fmt.Sprintf("全新单轮执行已启用，最大并发 %d；目录授权与Task执行权限分开，只有获准目录Task及实际验证的完整文件工具适配器可读写和发布版本", config.executionMaxActive)
	}
	if memberTools != nil {
		adapterStatus := "仅已验证 Pi 1.4.0 且显式指定模型时可用六项成员工具（含只读历史搜索与分段读取）；Codex 仅原生只读工具且无平台令牌"
		if config.enableCodexAppServer {
			adapterStatus = "Pi 仍须受支持版本、可信扩展与显式模型；Codex 已显式选择实验性 AppServer 与同一成员工具的会话级 MCP，须版本能力受支持且指定模型；仅受限只读原生文本配置（无可写目录、审批 never），不保证禁用所有原生工具或隔离文件读取，不启用文件任务或 Artifact；能力报告不是账号或网络实测，也不授予工作许可"
		}
		toolStatus = "平台工具已配置：" + adapterStatus + "；不授予 Coordinator、Assignment 或 RequestWork 权限"
		if config.enableCoordinationRoles {
			toolStatus = "平台工具与协调角色已配置：普通投递仍仅六项基础成员工具且不授予角色；启用话题聊天策略后可通过绑定来源的结构化语义评估自动准入与登记文本分派，不开放成员冒充本机用户的 RequestWork/RequestDispatch；受信协调员与工作成员按服务端执行身份分别绑定专用工具；" + adapterStatus + "；分派登记不直接执行，具体成果提交不等于接纳；角色就绪查询仅为采样诊断，不保证后续认领或执行"
		}
	}
	local := platform.NewLocalUser(db)
	coordinations := platform.NewCoordinations(engine)
	coordinatorConfiguration := platform.NewCoordinatorConfiguration(engine)
	routes := append(httpapi.LocalUserRoutes(local), httpapi.MessageRoutes(local)...)
	routes = append(routes, httpapi.TaskRoutes(local)...)
	routes = append(routes, httpapi.TaskLifecycleRoutes(local)...)
	routes = append(routes, httpapi.HistoryRoutes(local)...)
	if preparer != nil {
		routes = append(routes, httpapi.WorkspaceGrantRoutes(preparer)...)
	}
	routes = append(routes, httpapi.CoordinationRoutes(coordinations)...)
	routes = append(routes, httpapi.CoordinatorConfigurationRoutes(coordinatorConfiguration)...)
	routes = append(routes, httpapi.ProviderRoute(catalog))
	routes = append(routes, artifactRoutes...)
	if memberTools != nil {
		routes = append(routes, httpapi.MemberToolRoutes(memberTools.Tools())...)
		routes = append(routes, httpapi.MemberHistoryToolRoutes(memberTools.Tools())...)
		if config.enableInvestigationExecution {
			routes = append(routes, httpapi.MemberInvestigationToolRoutes(memberTools.Tools())...)
			if config.piFreshInvestigationWorkExtension != "" {
				toolStatus += "；只读调查首轮仍限单收件人的真实本机用户原始消息与首次正式分派；已配置选定 Pi 1.4.0 的补充、委派与报告全新轮，须逐轮确认启动，不新增工作授权、不恢复会话或授予文件写入"
			} else {
				toolStatus += "；只读调查仅支持选定 Pi 1.4.0、单收件人的真实本机用户原始消息与首次正式分派的角色；仅按策略读取指定资料，不授予续接或文件工具"
			}
		}
		if config.enableCodexAppServer {
			routes = append(routes, httpapi.MemberMCPRoutes(memberTools.Tools())...)
		}
	}
	if config.enableCoordinationRoles {
		routes = append(routes, httpapi.DispatchRequestRoutes(platform.NewDispatchRequests(engine))...)
		routes = append(routes, httpapi.MemberRoleToolRoutes(memberTools.Tools())...)
		routes = append(routes, httpapi.CoordinationReadinessRoutes(db, preparer)...)
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
	sessionStatus := "不支持恢复或运行中追加输入"
	if config.enableNativeSessionResume {
		// Keep capacity reporting independent of native-session selection.
		executionStatus = strings.TrimPrefix(executionStatus, "全新")
		sessionStatus = "已显式选择实验性 Pi 1.4.0 原生文本会话续接；须通过可信扩展校验并为该用途显式指定 thinking；仅当同一作用域最新持久引用合格时续接，每轮重新发证，续接出错不回退重试；Codex AppServer 仍为全新单轮；不续接目录任务或调查；不支持运行中追加，后续输入仍排队"
	}
	if _, err := fmt.Fprintf(output, "本地 HTTP 进程已启动： http://%s （已提供健康检查、群组与成员创建查询、消息持久化、历史与详情、排队投递记录、任务创建、列表与详情、成果版本只读查询与下载及 CLI 目录；目录仅启动时探测、证据仅存内存；%s；%s；%s；工作目录不是主机读取沙箱，数据目录及祖先必须可信且目录身份稳定；其他功能尚未启用）\n", listener.Addr(), executionStatus, sessionStatus, toolStatus); err != nil {
		return fmt.Errorf("输出启动信息失败：%w", err)
	}
	if config.enableInvestigationExecution {
		// Opt in to B only after the exact adapter, blob owner and routes have
		// been composed. There is still no local-user policy or per-turn grant.
		engine.EnableInvestigationExecution()
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
