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

type config struct{ listen, dataDir, devOrigin string }

func parseConfig(args []string, output io.Writer) (config, error) {
	var config config
	flags := flag.NewFlagSet("workspace", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {
		fmt.Fprintf(output, "用法：workspace [选项]\n  --listen 地址    回环 IP 与端口（默认 %s，显式指定时可用端口 0）\n  --data-dir 路径  数据目录（默认用户配置目录下的 agent-workspace）\n  --dev-origin 来源  单个 HTTP 回环开发来源，必须有端口（默认禁用）\n", httpapi.DefaultAddress)
	}
	flags.StringVar(&config.listen, "listen", httpapi.DefaultAddress, "监听地址")
	flags.StringVar(&config.dataDir, "data-dir", "", "数据目录")
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
