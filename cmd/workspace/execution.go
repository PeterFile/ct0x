package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func executionRunner(dataDir string, db *store.Store, catalog *platform.ProviderCatalog) (func(context.Context) error, error) {
	runtime, err := privateExecutionRoot(dataDir, "runtime")
	if err != nil {
		return nil, err
	}
	workspaces, err := privateExecutionRoot(dataDir, "workspaces")
	if err != nil {
		return nil, err
	}
	providers, err := providerSnapshot(catalog)
	if err != nil {
		return nil, err
	}
	// No external workspace grants: member preferences cannot authorize access.
	preparer, err := platform.NewPreparer(db, platform.PrepareOptions{
		Providers: providers, PrivateWorkspaceRoot: workspaces,
	})
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		return scheduler.Run(ctx, db, scheduler.Options{
			MaxActive: 1, Recovered: true, RuntimeDir: runtime,
		}, preparer.Prepare)
	}, nil
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
