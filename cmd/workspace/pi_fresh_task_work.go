package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func (c *config) registerPiFreshTaskWorkFlag(flags *flag.FlagSet, output io.Writer) {
	flags.Func("pi-fresh-task-work-extension", "Pi 纯文本任务续办扩展（默认禁用）", func(path string) error {
		if c.piFreshTaskWorkExtension != "" || !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
			return errors.New("任务续办扩展只能指定一次且必须是绝对路径")
		}
		c.piFreshTaskWorkExtension = path
		return nil
	})
	usage := flags.Usage
	flags.Usage = func() {
		usage()
		fmt.Fprintln(output, "  --pi-fresh-task-work-extension 路径  Pi 纯文本任务续办（默认禁用；须启用执行、成员工具与协调角色，并指定独立可信扩展文件对）")
	}
}

func (c config) validatePiFreshTaskWork() error {
	if c.piFreshTaskWorkExtension != "" &&
		(!c.enableExecution || !c.enableMemberTools || !c.enableCoordinationRoles || c.piMemberToolsExtension == "" ||
			filepath.Clean(c.piFreshTaskWorkExtension) == filepath.Clean(c.piMemberToolsExtension)) {
		return errors.New("Pi 任务续办须显式启用执行、成员工具和协调角色，并指定独立于普通入口的可信扩展")
	}
	return nil
}

func (c config) piFreshTaskWorkLaunch(db *store.Store, engine *coordination.Engine, providers []provider.Provider) (*platform.FreshTaskWorkLaunch, error) {
	entry := c.piFreshTaskWorkExtension
	if entry == "" {
		return nil, nil
	}
	launch := platform.NewPiFreshTaskWorkLaunch(entry, 0)
	if !engine.UsesStore(db) || !slices.ContainsFunc(providers, launch.Supports) {
		return nil, errors.New("Pi 任务续办需要当前 Store、受支持的 Pi 1.4.0 和精确扩展文件对")
	}
	ordinary, ordinaryErr := os.Stat(c.piMemberToolsExtension)
	typed, typedErr := os.Stat(entry)
	if ordinaryErr != nil || typedErr != nil || os.SameFile(ordinary, typed) {
		return nil, errors.New("普通 Pi 入口与任务续办入口必须是不同的可信文件")
	}
	return launch, nil
}
