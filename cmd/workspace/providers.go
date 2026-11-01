package main

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"agentworkspace/internal/provider"
)

func run(ctx context.Context, args []string, output io.Writer) error {
	return runWithSearch(ctx, args, output, defaultProviderSearch())
}

func defaultProviderSearch() provider.Search {
	search := provider.Search{
		PathDirs:   filepath.SplitList(os.Getenv("PATH")),
		CommonDirs: []string{"/usr/local/bin", "/opt/homebrew/bin", "/usr/bin"},
	}
	if home, err := os.UserHomeDir(); err == nil {
		search.CommonDirs = append(search.CommonDirs, filepath.Join(home, ".local", "bin"))
	}
	return search
}
