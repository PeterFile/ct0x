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

type config struct {
	listen, dataDir, webDir, devOrigin, piPath, codexPath string
	enableExecution, enableMemberTools                    bool
	piMemberToolsExtension                                string
	memberToolsMaxMembers                                 int
}

func parseConfig(args []string, output io.Writer) (config, error) {
	var config config
	flags := flag.NewFlagSet("workspace", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {
		fmt.Fprintf(output, "用法：workspace [选项]\n  --listen 地址    回环 IP 与端口（默认 %s，显式指定时可用端口 0）\n  --data-dir 路径  数据目录（默认用户配置目录下的 agent-workspace）\n  --web-dir 路径   网页构建目录（默认禁用，仅提供 API）\n  --enable-execution  启用全新单轮执行（默认禁用，最大并发 1；不支持恢复）\n  --enable-member-tools  启用 Pi 六项基础平台工具（含只读历史搜索与分段读取；默认禁用，须启用执行）\n  --pi-member-tools-extension 路径  显式可信扩展的绝对可读文件路径（须保留相邻依赖）\n  --member-tools-max-members 数量  工具增员容量（默认 4，1..200；仅工具启用时可指定）\n  --dev-origin 来源  单个 HTTP 回环开发来源，必须有端口（默认禁用）\n  --pi-path 路径   Pi 绝对可执行文件路径（可选，以发现结果为准）\n  --codex-path 路径  Codex 绝对可执行文件路径（可选，以发现结果为准）\nCLI 目录仅启动时只读探测，证据仅存内存，不验证执行能力。\n执行要求数据目录及祖先可信且目录身份稳定；工作目录不是主机文件读取沙箱。Pi 默认禁用工具，显式开启平台工具须选已验证 Pi 1.4.0 与显式模型；Codex 仅原生只读工具，无平台令牌。基础工具不授予协调或工作准入权限；不支持运行中追加输入。\n", httpapi.DefaultAddress)
	}
	flags.StringVar(&config.listen, "listen", httpapi.DefaultAddress, "监听地址")
	flags.StringVar(&config.dataDir, "data-dir", "", "数据目录")
	flags.StringVar(&config.webDir, "web-dir", "", "网页构建目录（默认禁用）")
	flags.BoolVar(&config.enableExecution, "enable-execution", false, "启用全新单轮执行（默认禁用）")
	flags.BoolVar(&config.enableMemberTools, "enable-member-tools", false, "启用 Pi 基础成员工具（默认禁用）")
	flags.StringVar(&config.piMemberToolsExtension, "pi-member-tools-extension", "", "可信 Pi 扩展的绝对文件路径")
	flags.IntVar(&config.memberToolsMaxMembers, "member-tools-max-members", 4, "成员工具增员容量（1..200）")
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
	if !config.enableMemberTools && (set["pi-member-tools-extension"] || set["member-tools-max-members"]) {
		return config, errors.New("成员工具禁用时不能指定扩展或增员容量")
	}
	if config.enableMemberTools {
		if !config.enableExecution || !set["pi-member-tools-extension"] {
			return config, errors.New("启用成员工具必须同时启用执行并显式指定可信 Pi 扩展")
		}
		if address.Addr() != netip.MustParseAddr("127.0.0.1") && address.Addr() != netip.IPv6Loopback() {
			return config, errors.New("成员工具监听地址必须是 127.0.0.1 或 [::1]")
		}
		if config.memberToolsMaxMembers < 1 || config.memberToolsMaxMembers > 200 {
			return config, errors.New("成员工具增员容量必须在 1..200 之间")
		}
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
