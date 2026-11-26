package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/coordination"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

// Share the existing read API's blob owner without enabling workspace tools
// or provisioning a workspace staging directory for ordinary Pi.
func executionArtifactOptions(config config, owner io.Closer) ([]platform.MemberWorkspaceOptions, error) {
	if !config.enableExecution || !config.enableMemberTools {
		return nil, nil
	}
	if config.enableCoordinationRoles {
		options, err := workspaceRuntimeOptions(config.dataDir, owner)
		if err != nil {
			return nil, err
		}
		return []platform.MemberWorkspaceOptions{options}, nil
	}
	if config.enablePiProjectExperience {
		blobs, ok := owner.(*artifactfs.Store)
		if !ok || blobs == nil {
			return nil, errors.New("成果文件库所有者无效")
		}
		return []platform.MemberWorkspaceOptions{{Blobs: blobs}}, nil
	}
	return nil, nil
}

func executionRunner(config config, db *store.Store, engine *coordination.Engine, catalog *platform.ProviderCatalog, endpoint string, output io.Writer, workspace ...platform.MemberWorkspaceOptions) (func(context.Context) error, *platform.MemberToolRuntime, *platform.Preparer, error) {
	if err := config.validatePiFreshTaskWork(); err != nil {
		return nil, nil, nil, err
	}
	if config.enableCodexAppServer && (!config.enableExecution || !config.enableMemberTools || !config.enableCoordinationRoles) {
		return nil, nil, nil, errors.New("启用 Codex AppServer 必须同时启用执行、成员工具和协调角色")
	}
	if err := config.validateThreadDecisionHandoff(); err != nil {
		return nil, nil, nil, err
	}
	if err := config.validateInvestigationOption(); err != nil {
		return nil, nil, nil, err
	}
	if err := config.validateNativeSessionConfig(); err != nil {
		return nil, nil, nil, err
	}
	if err := config.validateRunExplanationConfig(); err != nil {
		return nil, nil, nil, err
	}
	if err := config.validatePiProjectExperience(); err != nil {
		return nil, nil, nil, err
	}
	if err := config.validateInvestigationDataDir(); err != nil {
		return nil, nil, nil, err
	}
	runtime, err := privateExecutionRoot(config.dataDir, "runtime")
	if err != nil {
		return nil, nil, nil, err
	}
	workspaces, err := privateExecutionRoot(config.dataDir, "workspaces")
	if err != nil {
		return nil, nil, nil, err
	}
	providers, err := providerSnapshot(catalog)
	if err != nil {
		return nil, nil, nil, err
	}
	freshTaskWork, err := config.piFreshTaskWorkLaunch(db, engine, providers)
	if err != nil {
		return nil, nil, nil, err
	}
	investigationTaskWork, err := config.piFreshInvestigationWorkLaunch(db, engine, providers)
	if err != nil {
		return nil, nil, nil, err
	}
	maxActive := config.executionMaxActive
	if maxActive == 0 { // Direct config literals may leave the CLI default unset.
		maxActive = defaultExecutionMaxActive
	}
	schedulerOptions := scheduler.Options{MaxActive: maxActive, Recovered: true, RuntimeDir: runtime}
	// Only explicit local-user receipts authorize external directories. Protect
	// the entire private data root, not just per-conversation workspaces.
	options := platform.PrepareOptions{Providers: providers, PrivateWorkspaceRoot: workspaces, PrivateDataRoot: config.dataDir}
	if config.enableInvestigationExecution || config.enablePiProjectExperience {
		// Same immutable blob owner, independent authority for each profile.
		if len(workspace) == 0 || workspace[0].Blobs == nil {
			return nil, nil, nil, errors.New("缺少成果读取服务")
		}
		artifacts, err := platform.NewArtifacts(db, workspace[0].Blobs)
		if err != nil {
			return nil, nil, nil, err
		}
		grant, err := platform.NewMemberArtifactGrant(artifacts)
		if err != nil {
			return nil, nil, nil, err
		}
		if config.enablePiProjectExperience {
			options.PiProjectExperienceArtifacts = grant // A borrower, not a per-turn grant.
		}
		if config.enableInvestigationExecution {
			if _, err := privateExecutionRoot(workspaces, platform.InvestigationCWDName); err != nil {
				return nil, nil, nil, err
			}
			options.Investigation = &platform.InvestigationPreparationOptions{PiPath: config.piPath, Artifacts: grant}
			if investigationTaskWork != nil {
				investigationTaskWork.Artifacts = grant
			}
		}
	}
	var tools *platform.MemberToolRuntime
	if config.enableMemberTools {
		root, err := privateExecutionRoot(config.dataDir, "member-tools")
		if err != nil {
			return nil, nil, nil, err
		}
		toolOptions := platform.MemberToolRuntimeOptions{
			ConfigRoot: root, Endpoint: endpoint, MaxGroupMembers: config.memberToolsMaxMembers,
			Operations: []string{"list_members", "read_channel", "send_message", "create_member", "search_history", "read_history"},
		}
		if config.enableRunExplanation {
			if !engine.UsesStore(db) {
				return nil, nil, nil, errors.New("公开运行说明须使用同一数据库")
			}
			toolOptions.RunExplanation = engine
		}
		if config.enableNativeSessionResume || config.enableRunExplanation {
			toolOptions.NativeExecutionRoot = schedulerOptions.RuntimeDir
		}
		if config.enableCoordinationRoles {
			toolOptions.RoleMaxActive, toolOptions.RoleExecutionRoot = schedulerOptions.MaxActive, schedulerOptions.RuntimeDir
			toolOptions.ChatIntake = engine
			options.RoleThinking = config.roleThinking
		}
		toolOptions.FreshTaskWork = freshTaskWork
		toolOptions.EnablePiTaskDecisionProposals = config.enablePiTaskDecisionProposals
		switch {
		case config.enableInvestigationOwnerManagement:
			tools, err = platform.NewMemberToolRuntimeWithInvestigationOwnerManagement(db, toolOptions, engine,
				investigationTaskWork.DelegateSupport(providers), investigationTaskWork)
		case investigationTaskWork != nil:
			tools, err = platform.NewMemberToolRuntimeWithTaskWorkReports(db, toolOptions, engine, investigationTaskWork)
		default:
			tools, err = platform.NewMemberToolRuntime(db, toolOptions)
		}
		if err != nil {
			return nil, nil, nil, err
		}
		if config.enableCoordinationRoles && len(workspace) > 0 {
			if err := tools.EnableWorkspace(workspace[0]); err != nil {
				return nil, nil, nil, err
			}
		}
		options.MemberTools, options.PiMemberToolsExtension = tools, config.piMemberToolsExtension
		options.EnablePiProjectExperience = config.enablePiProjectExperience
		options.EnableCodexAppServer = config.enableCodexAppServer
		// Installing the qualified new bridge opts the trusted Pi intake into relay.
		options.EnablePiDiscussionRelay = config.enableCoordinationRoles
		options.EnableNativeSessionResume = config.enableNativeSessionResume
		options.NativePiThinking = config.nativePiThinking
	}
	preparer, err := platform.NewPreparer(db, options)
	if err != nil {
		if tools != nil {
			err = errors.Join(err, tools.Close())
		}
		return nil, nil, nil, err
	}
	if config.enableCoordinationRoles {
		schedulerOptions.IntentPass, err = coordination.NewTrustedIntentPass(engine, func(err error) {
			// Report the complete aggregate once, including cancellation leaves.
			fmt.Fprintf(output, "协调意图扫描失败：%v\n", err)
		})
		if err != nil {
			if tools != nil {
				err = errors.Join(err, tools.Close())
			}
			return nil, nil, nil, err
		}
	}
	schedulerOptions.IntentPass, err = config.withBackgroundDispatchPass(db, preparer, providers, schedulerOptions.IntentPass, output)
	if err != nil {
		if tools != nil {
			err = errors.Join(err, tools.Close())
		}
		return nil, nil, nil, err
	}
	if config.enableThreadDecisionHandoff {
		schedulerOptions.PrepareThreadDecision, err = preparer.EnableThreadDecisionHandoff(context.Background())
		if err != nil {
			if tools != nil {
				err = errors.Join(err, tools.Close())
			}
			return nil, nil, nil, err
		}
	}
	if freshTaskWork != nil || investigationTaskWork != nil {
		// B's process-local readiness assertion, not Policy or authority. Set
		// it only after successful composition and before accepting requests.
		engine.EnableTaskWorkExecution()
	}
	return func(ctx context.Context) error {
		return scheduler.Run(ctx, db, schedulerOptions, preparer.Prepare)
	}, tools, preparer, nil
}

func privateExecutionRoot(dataDir, name string) (string, error) {
	path := filepath.Join(dataDir, name)
	if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("创建私有执行目录失败：%w", err)
	}
	// Ancestors and directory identities must remain trusted throughout execution.
	info, err := os.Lstat(path)
	if err != nil || info.Mode() != os.ModeDir|0700 {
		return "", errors.New("执行目录必须是非符号链接的 0700 私有目录")
	}
	return path, nil
}

func providerSnapshot(catalog *platform.ProviderCatalog) ([]provider.Provider, error) {
	var snapshot []provider.Provider
	for before := ""; ; {
		items, next, err := catalog.List(200, before)
		if err != nil {
			return nil, fmt.Errorf("读取提供方目录失败：%w", err)
		}
		snapshot = append(snapshot, items...)
		if next == "" {
			return snapshot, nil
		}
		before = next
	}
}

// Only pure cancellation is normal shutdown. Inspect aggregate leaves instead of
// using errors.Is on the aggregate, which would also hide real drain failures.
func shutdownError(err error) error {
	if cancellationOnly(err) {
		return nil
	}
	return err
}

func cancellationOnly(err error) bool {
	if err == context.Canceled {
		return true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		found := false
		for _, child := range wrapped.Unwrap() {
			if child != nil {
				found = true
				if !cancellationOnly(child) {
					return false
				}
			}
		}
		return found
	case interface{ Unwrap() error }:
		return cancellationOnly(wrapped.Unwrap())
	}
	return false
}
