// Package provider discovers local CLIs without reading their credentials.
package provider

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Installation string
type Account string
type Connectivity string
type ProbeType string

const (
	Pi                           = "pi"
	Codex                        = "codex"
	Installed       Installation = "installed"
	AccountUnknown  Account      = "unknown"
	Authenticated   Account      = "authenticated"
	Unauthenticated Account      = "unauthenticated"
	Unchecked       Connectivity = "unchecked"
	DiscoveryProbe  ProbeType    = "discovery"
	VersionProbe    ProbeType    = "version"
	AccountProbe    ProbeType    = "account"
)

type Capabilities struct {
	OneShot bool `json:"one_shot"`
	Resume  bool `json:"resume"`
	Tools   bool `json:"tools"`
}

type Evidence struct {
	At     time.Time `json:"at"`
	Type   ProbeType `json:"type"`
	Reason string    `json:"reason"`
}

type Provider struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Path         string       `json:"path"`
	Version      string       `json:"version"`
	Installation Installation `json:"installation"`
	Account      Account      `json:"account"`
	Connectivity Connectivity `json:"connectivity"`
	Capabilities Capabilities `json:"capabilities"`
	Evidence     []Evidence   `json:"evidence"`
}

type Location struct{ Name, Path string }
type Search struct {
	Explicit             []Location
	PathDirs, CommonDirs []string
}

// Discover checks only supplied paths, in explicit/PATH/common priority order.
// The first alias wins; IDs use canonical paths, and hard links are deduplicated.
func Discover(search Search) []Provider {
	locations := append([]Location{}, search.Explicit...)
	for _, dirs := range [][]string{search.PathDirs, search.CommonDirs} {
		for _, dir := range dirs {
			if dir != "" {
				locations = append(locations, Location{Pi, filepath.Join(dir, Pi)}, Location{Codex, filepath.Join(dir, Codex)})
			}
		}
	}
	providers := []Provider{}
	seen := []os.FileInfo{}
next:
	for _, location := range locations {
		if (location.Name != Pi && location.Name != Codex) || location.Path == "" {
			continue
		}
		path, err := filepath.Abs(location.Path)
		if err != nil {
			continue
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		info, err := os.Stat(real)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			continue
		}
		for _, previous := range seen {
			if os.SameFile(info, previous) {
				continue next
			}
		}
		seen = append(seen, info)
		providers = append(providers, Provider{
			ID:   fmt.Sprintf("%s_%x", location.Name, sha256.Sum256([]byte(real))),
			Name: location.Name, Path: path, Installation: Installed,
			Account: AccountUnknown, Connectivity: Unchecked,
			Evidence: []Evidence{{time.Now().UTC(), DiscoveryProbe, "已发现可执行文件"}},
		})
	}
	return providers
}
