package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"agentworkspace/internal/httpapi"
)

const defaultExecutionMaxActive = 1

type config struct {
	enableNativeSessionResume bool
	nativePiThinking          string
	enableRunExplanation      bool

	listen, dataDir, webDir, devOrigin, piPath, codexPath       string
	enableExecution, enableMemberTools, enableCoordinationRoles bool
	enableCodexAppServer, enableInvestigationExecution          bool
	piMemberToolsExtension, roleThinking                        string
	executionMaxActive, memberToolsMaxMembers                   int
	piFreshTaskWorkExtension, piFreshInvestigationWorkExtension string
	enableInvestigationOwnerManagement                          bool
}

func parseConfig(args []string, output io.Writer) (config, error) {
	var config config
	flags := flag.NewFlagSet("workspace", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {
		fmt.Fprintf(output, "用法：workspace [选项]\n  --listen 地址    回环 IP 与端口（默认 %s，显式指定时可用端口 0）\n  --data-dir 路径  数据目录（默认用户配置目录下的 agent-workspace）\n  --web-dir 路径   网页构建目录（默认禁用，仅提供 API）\n  --enable-execution  启用执行（默认禁用；未启用原生会话续接时仅支持全新单轮）\n  --execution-max-active 数量  最大并发（默认 1，1..8；须启用执行）\n  --enable-member-tools  启用 Pi 六项基础平台工具（含只读历史搜索与分段读取；默认禁用，须启用执行）\n  --pi-member-tools-extension 路径  显式可信扩展的绝对可读文件路径（须保留相邻依赖）\n  --member-tools-max-members 数量  工具增员容量（默认 4，1..200；仅工具启用时可指定）\n  --enable-coordination-roles  显式启用受信协调角色（默认禁用，须启用执行、成员工具及所选原生工具适配器；Pi 仍须可信扩展）\n  --enable-codex-app-server  显式选择实验性 Codex AppServer 与会话级平台 MCP（默认禁用；须启用执行、成员工具和协调角色；可不配置 Pi 扩展）\n  --role-thinking 级别  仅 Pi 协调角色使用：off、minimal、low、medium、high、xhigh、max（默认空，沿用 Pi CLI 默认；Codex 不传此参数；须启用协调角色）\n  --dev-origin 来源  单个 HTTP 回环开发来源，必须有端口（默认禁用）\n  --pi-path 路径   Pi 绝对可执行文件路径（可选，以发现结果为准）\n  --codex-path 路径  Codex 绝对可执行文件路径（可选，以发现结果为准）\nCLI 目录仅启动时只读探测，证据仅存内存，不验证执行能力。适配器能力仅描述固定实现版本支持，不代表账号、网络实测或工作许可；未知能力不能自动分派。\n执行要求数据目录及祖先可信且目录身份稳定；工作目录不是主机文件读取沙箱。Pi 默认禁用工具，显式开启平台工具须选已验证 Pi 1.4.0 与显式模型；Codex 默认仅原生只读工具，无平台令牌。显式开启 AppServer 时，所选受支持版本可复用同一成员工具与 claim/角色围栏；仅受限只读原生文本配置（无可写目录、approval never），不保证禁用全部原生工具；原生 apply_patch 可能仍注册但写入须被原生安全边界拒绝，view_image 关闭，不承诺文件读取隔离；继承 MCP 禁用，唯一平台 MCP 不提供 resources，不启用文件任务或 Artifact。普通投递的基础工具不授予协调或工作准入权限；显式协调角色按服务端执行身份绑定专用工具。角色就绪查询仅为采样诊断，不保证后续认领或执行；不支持运行中追加输入。\n", httpapi.DefaultAddress)
		fmt.Fprintln(output, "  --enable-run-explanation  允许合格的全新普通 Pi 成员轮次主动发布一条公开运行说明（默认禁用；须启用执行和成员工具、配置精确可信 Pi 扩展且数据库支持；不适用于续接或受信协调角色）")
		fmt.Fprintln(output, "  --enable-investigation-execution  显式启用 Pi 1.4.0 只读调查首轮预览（默认禁用；须使用独立的私有数据目录、明确选定的 Pi 和可信扩展，启用协调角色且不得同时启用 Codex AppServer；限单收件人的本机用户原始消息）")
	}
	flags.StringVar(&config.listen, "listen", httpapi.DefaultAddress, "监听地址")
	flags.StringVar(&config.dataDir, "data-dir", "", "数据目录")
	flags.StringVar(&config.webDir, "web-dir", "", "网页构建目录（默认禁用）")
	flags.BoolVar(&config.enableExecution, "enable-execution", false, "启用执行（默认禁用）")
	flags.IntVar(&config.executionMaxActive, "execution-max-active", defaultExecutionMaxActive, "执行最大并发（1..8）")
	flags.BoolVar(&config.enableMemberTools, "enable-member-tools", false, "启用 Pi 基础成员工具（默认禁用）")
	flags.StringVar(&config.piMemberToolsExtension, "pi-member-tools-extension", "", "可信 Pi 扩展的绝对文件路径")
	flags.BoolVar(&config.enableRunExplanation, "enable-run-explanation", false, "启用普通 Pi 公开运行说明（默认禁用）")
	config.registerPiFreshTaskWorkFlag(flags, output)
	config.registerPiFreshInvestigationWorkFlag(flags, output)
	flags.IntVar(&config.memberToolsMaxMembers, "member-tools-max-members", 4, "成员工具增员容量（1..200）")
	flags.BoolVar(&config.enableCoordinationRoles, "enable-coordination-roles", false, "显式启用受信协调角色（默认禁用）")
	flags.BoolVar(&config.enableCodexAppServer, "enable-codex-app-server", false, "显式选择实验性 Codex AppServer 与会话级平台 MCP（默认禁用）")
	flags.BoolVar(&config.enableInvestigationExecution, "enable-investigation-execution", false, "显式启用 Pi 1.4.0 只读调查首轮预览（默认禁用）")
	flags.StringVar(&config.roleThinking, "role-thinking", "", "协调角色的 Pi thinking 级别（默认空）")
	flags.BoolVar(&config.enableNativeSessionResume, "enable-native-session-resume", false, "实验性原生文本会话续接（默认禁用）")
	flags.StringVar(&config.nativePiThinking, "native-pi-thinking", "", "普通文本及 chat_intake 原生 Pi thinking 级别（须显式启用）")
	for name, destination := range map[string]*string{"pi-path": &config.piPath, "codex-path": &config.codexPath} {
		flags.Func(name, "CLI 绝对路径", func(value string) error {
			if *destination != "" || !filepath.IsAbs(value) {
				return errors.New("CLI 路径只能指定一次且必须是绝对路径")
			}
			*destination = value
			return nil
		})
	}
	devSet := false
	flags.Func("dev-origin", "开发来源", func(value string) error {
		if devSet || value == "" {
			return errors.New("开发来源只能指定一次且不能为空")
		}
		devSet = true
		config.devOrigin = value
		return httpapi.ValidateDevOrigin(value)
	})
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(output, "  --enable-native-session-resume  实验性原生文本会话续接（默认禁用；须启用执行与成员工具。Pi 1.4.0 须配置可信扩展；Codex 0.166.0 须同时启用 --enable-codex-app-server，限普通 chat_intake/v1，不续接协调角色或 Task 执行；不启用文件会话、调查续接或运行中追加）\n  --native-pi-thinking 级别  仅 Pi 普通文本与 chat_intake 原生会话使用：off、minimal、low、medium、high、xhigh、max（须显式启用原生会话续接并配置可信 Pi 扩展；不指定则沿用普通 Pi 执行，不影响 --role-thinking）\n")
			return config, err
		}
		return config, errors.New("命令行参数无效，请使用 --help 查看帮助")
	}
	if flags.NArg() != 0 {
		return config, errors.New("不接受位置参数，请使用 --help 查看帮助")
	}
	set := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { set[f.Name] = true })
	address, err := netip.ParseAddrPort(config.listen)
	if err != nil || !address.Addr().IsLoopback() || address.Addr().Zone() != "" || address.Port() == 0 && !set["listen"] {
		return config, errors.New("监听地址必须是带端口的回环 IP 地址")
	}
	config.listen = address.String()
	if !config.enableExecution && set["execution-max-active"] {
		return config, errors.New("执行禁用时不能指定最大并发")
	}
	if config.executionMaxActive < 1 || config.executionMaxActive > 8 {
		return config, errors.New("执行最大并发必须在 1..8 之间")
	}
	if config.enableCodexAppServer && (!config.enableExecution || !config.enableMemberTools || !config.enableCoordinationRoles) {
		return config, errors.New("启用 Codex AppServer 必须同时启用执行、成员工具和协调角色")
	}
	if err := config.validateInvestigationOption(); err != nil {
		return config, err
	}
	if config.enableInvestigationExecution && !set["data-dir"] {
		return config, errors.New("只读调查须显式指定独立的私有 --data-dir")
	}
	if config.enableCoordinationRoles && (!config.enableExecution || !config.enableMemberTools) {
		return config, errors.New("启用协调角色必须同时启用执行、成员工具并显式指定可信 Pi 扩展")
	}
	if err := config.validatePiFreshTaskWork(); err != nil {
		return config, err
	}
	if !config.enableCoordinationRoles && set["role-thinking"] {
		return config, errors.New("协调角色禁用时不能指定角色 thinking 级别，包括空值")
	}
	switch config.roleThinking {
	case "", "off", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return config, errors.New("角色 thinking 级别无效，请使用 --help 查看允许的精确值")
	}
	if !config.enableMemberTools && (set["pi-member-tools-extension"] || set["member-tools-max-members"]) {
		return config, errors.New("成员工具禁用时不能指定扩展或增员容量")
	}
	if config.enableMemberTools {
		if !config.enableExecution || !set["pi-member-tools-extension"] && !config.enableCodexAppServer {
			return config, errors.New("启用成员工具必须同时启用执行并显式指定可信 Pi 扩展")
		}
		if address.Addr() != netip.MustParseAddr("127.0.0.1") && address.Addr() != netip.IPv6Loopback() {
			return config, errors.New("成员工具监听地址必须是 127.0.0.1 或 [::1]")
		}
		if config.memberToolsMaxMembers < 1 || config.memberToolsMaxMembers > 200 {
			return config, errors.New("成员工具增员容量必须在 1..200 之间")
		}
	}
	if config.enableMemberTools && set["pi-member-tools-extension"] {
		path := config.piMemberToolsExtension
		if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
			return config, errors.New("Pi 成员工具扩展必须是绝对可读普通文件路径")
		}
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0444 == 0 {
			return config, errors.New("Pi 成员工具扩展必须是可读普通文件")
		}
		file, openErr := os.Open(path)
		if openErr != nil {
			return config, errors.New("无法读取 Pi 成员工具扩展")
		}
		if file.Close() != nil {
			return config, errors.New("关闭 Pi 成员工具扩展失败")
		}
	}
	if set["native-pi-thinking"] && config.nativePiThinking == "" {
		return config, errors.New("指定的原生 Pi thinking 级别不能为空")
	}
	if err := config.validateNativeSessionConfig(); err != nil {
		return config, err
	}
	if err := config.validateRunExplanationConfig(); err != nil {
		return config, err
	}
	if !set["data-dir"] {
		root, err := os.UserConfigDir()
		if err != nil {
			return config, fmt.Errorf("无法确定默认数据目录：%w", err)
		}
		config.dataDir = filepath.Join(root, "agent-workspace")
	}
	if strings.TrimSpace(config.dataDir) == "" {
		return config, errors.New("数据目录不能为空")
	}
	config.dataDir, err = filepath.Abs(config.dataDir)
	if err != nil {
		return config, fmt.Errorf("解析数据目录失败：%w", err)
	}
	return config, nil
}

func (c config) validateInvestigationOption() error {
	if c.enableInvestigationOwnerManagement && (!c.enableInvestigationExecution || c.piFreshInvestigationWorkExtension == "") {
		return errors.New("调查负责人管理须同时启用只读调查和独立的调查后续工作扩展")
	}
	if !c.enableInvestigationExecution {
		if c.piFreshInvestigationWorkExtension != "" {
			return errors.New("调查后续工作须显式启用 --enable-investigation-execution")
		}
		return nil
	}
	if c.enableCodexAppServer {
		return errors.New("只读调查不能与 Codex AppServer 同时启用")
	}
	if !c.enableExecution || !c.enableMemberTools || !c.enableCoordinationRoles || !filepath.IsAbs(c.piPath) || c.piMemberToolsExtension == "" {
		return errors.New("只读调查须同时启用执行、成员工具和协调角色，并显式提供可信 Pi 1.4.0 的 --pi-path 和扩展")
	}
	if path := c.piFreshInvestigationWorkExtension; path != "" &&
		(!filepath.IsAbs(path) || strings.ContainsRune(path, 0) || filepath.Clean(path) == filepath.Clean(c.piMemberToolsExtension) ||
			filepath.Clean(path) == filepath.Clean(c.piFreshTaskWorkExtension)) {
		return errors.New("调查后续工作须使用独立的可信扩展绝对路径")
	}
	return nil
}

func (c config) validateInvestigationDataDir() error {
	if c.enableInvestigationExecution {
		if info, err := os.Lstat(c.dataDir); err != nil || info.Mode() != os.ModeDir|0700 {
			return errors.New("只读调查的数据目录必须是独立的非符号链接 0700 私有目录")
		}
	}
	return nil
}

func (c config) validateRunExplanationConfig() error {
	if c.enableRunExplanation && (!c.enableExecution || !c.enableMemberTools || !filepath.IsAbs(c.piMemberToolsExtension)) {
		return errors.New("启用公开运行说明须同时启用执行和成员工具，并显式配置可信 Pi 扩展")
	}
	return nil
}

func (c config) validateNativeSessionConfig() error {
	if c.enableNativeSessionResume && (!c.enableExecution || !c.enableMemberTools || c.piMemberToolsExtension == "" && !c.enableCodexAppServer) {
		return errors.New("启用原生会话续接须同时启用执行与成员工具，并显式配置可信 Pi 扩展或启用 Codex AppServer")
	}
	if c.nativePiThinking != "" && (!c.enableNativeSessionResume || c.piMemberToolsExtension == "") {
		return errors.New("普通原生 Pi thinking 级别须启用原生会话续接并指定可信 Pi 扩展")
	}
	switch c.nativePiThinking {
	case "", "off", "minimal", "low", "medium", "high", "xhigh", "max":
		return nil
	default:
		return errors.New("原生 Pi thinking 级别无效，请使用 --help 查看允许的精确值")
	}
}
