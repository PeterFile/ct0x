package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func (c *config) registerPiFreshInvestigationWorkFlag(flags *flag.FlagSet, output io.Writer) {
	flags.Func("pi-fresh-investigation-work-extension", "Pi 只读调查后续工作扩展（默认禁用）", func(path string) error {
		if c.piFreshInvestigationWorkExtension != "" || !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
			return errors.New("调查后续工作扩展只能指定一次且必须是绝对路径")
		}
		c.piFreshInvestigationWorkExtension = path
		return nil
	})
	flags.BoolVar(&c.enableInvestigationOwnerManagement, "enable-investigation-owner-management", false, "Pi 调查绑定负责人管理（默认禁用）")
	flags.BoolVar(&c.enableDiscussionWorkReturn, "enable-discussion-work-return", false, "Pi 正式调查交回原讨论（默认禁用）")
	usage := flags.Usage
	flags.Usage = func() {
		usage()
		fmt.Fprintln(output, "  --pi-fresh-investigation-work-extension 路径  Pi 调查补充、委派和报告新轮（默认禁用；须启用只读调查并指定独立的可信扩展，不新增工作授权）")
		fmt.Fprintln(output, "  --enable-investigation-owner-management  Pi 调查负责人有限委派及续轮整项交回（默认禁用；须同时启用只读调查及其后续扩展；仍由平台核验授权与余额）")
		fmt.Fprintln(output, "  --enable-discussion-work-return  实验性正式调查交回原讨论（默认禁用；须同时启用只读调查及其后续扩展；结构化邀请续轮仅新增整项交回，不授予委派）")
	}
}

func (c config) piFreshInvestigationWorkLaunch(db *store.Store, engine *coordination.Engine, providers []provider.Provider) (*platform.InvestigationTaskWorkLaunch, error) {
	entry := c.piFreshInvestigationWorkExtension
	if entry == "" {
		return nil, nil
	}
	if !engine.UsesStore(db) {
		return nil, errors.New("调查后续工作需要当前 Store 的协调引擎")
	}
	launch := platform.NewPiInvestigationTaskWorkLaunch(entry, 0,
		func(ctx context.Context, plan store.FreshTaskWorkLaunch, admit func() bool) error {
			return db.CommitInvestigationTaskWorkLaunch(ctx, store.InvestigationTaskWorkLaunch(plan), admit)
		})
	launch.EnableDiscussionWorkReturn = c.enableDiscussionWorkReturn
	// Use only the same explicitly selected Pi as first investigation, even
	// if the catalog contains other supported installations.
	pi, supports := filepath.Clean(c.piPath), launch.Supports
	launch.Supports = func(selected provider.Provider, profile provider.PiFreshInvestigationProfile) bool {
		return selected.Path == pi && supports(selected, profile)
	}
	probe := provider.PiFreshInvestigationProfile{Action: domain.ChatActionReadOnlyInvestigation,
		SourceKind: domain.TaskWorkSourceDelegation, Operations: []string{platform.ReportWorkOperation}}
	if !slices.ContainsFunc(providers, func(selected provider.Provider) bool { return launch.Supports(selected, probe) }) {
		return nil, errors.New("调查后续工作需要明确选定的 Pi 1.4.0 和精确扩展文件对")
	}
	typed, err := os.Stat(entry)
	if err != nil {
		return nil, errors.New("调查后续工作扩展不可用")
	}
	for _, path := range []string{c.piMemberToolsExtension, c.piFreshTaskWorkExtension} {
		if path == "" {
			continue
		}
		other, err := os.Stat(path)
		if err != nil || os.SameFile(typed, other) {
			return nil, errors.New("调查后续工作须使用独立于普通入口和纯文本续办入口的可信文件")
		}
	}
	return launch, nil
}
