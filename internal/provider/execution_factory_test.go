package provider

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestNewExecutorBindsExactSupportedProviderWithoutProbing(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "invoked")
	path := filepath.Join(root, "not-a-cli-alias")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho invoked > \""+marker+"\"\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, version string
		timeout       time.Duration
	}{
		{Pi, "1.4.0", 0},
		{Codex, "0.162.1", 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected := Provider{ID: "exact-id", Name: tc.name, Version: tc.version, Path: path,
				Installation: Installed, Account: AccountUnknown, Connectivity: Unchecked,
				Capabilities: Capabilities{Resume: true, Tools: true}, Evidence: []Evidence{{Reason: "unchanged"}}}
			before := selected
			before.Evidence = append([]Evidence(nil), selected.Evidence...)
			e, err := NewExecutor(selected, tc.timeout)
			if err != nil || e == nil {
				t.Fatalf("supported unknown-account provider rejected: %v", err)
			}
			var command executionCommand
			switch e := e.(type) {
			case piExecutor:
				if tc.name != Pi {
					t.Fatal("wrong adapter")
				}
				command = e.executionCommand
			case codexExecutor:
				if tc.name != Codex {
					t.Fatal("wrong adapter")
				}
				command = e.executionCommand
			default:
				t.Fatalf("unexpected adapter: %T", e)
			}
			wantTimeout := tc.timeout
			if wantTimeout == 0 {
				wantTimeout = 10 * time.Minute
			}
			if command.path != path || command.timeout != wantTimeout || !reflect.DeepEqual(selected, before) {
				t.Fatalf("binding or metadata changed: %+v, %+v", command, selected)
			}
		})
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("factory launched executable: %v", err)
	}
}

func TestNewExecutorRejectsUnverifiedConfiguration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "fixture")
	if err := os.WriteFile(path, []byte("must never execute"), 0700); err != nil {
		t.Fatal(err)
	}
	nonExecutable := filepath.Join(root, "not-executable")
	if err := os.WriteFile(nonExecutable, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*Provider)
	}{
		{"blank ID", func(p *Provider) { p.ID = " " }},
		{"unknown name", func(p *Provider) { p.Name = "other" }},
		{"CLI alias", func(p *Provider) { p.Name = "codex-cli" }},
		{"blank name", func(p *Provider) { p.Name = "" }},
		{"missing version", func(p *Provider) { p.Version = "" }},
		{"unnormalized version", func(p *Provider) { p.Version = "v1.4.0" }},
		{"unsupported Pi", func(p *Provider) { p.Version = "1.4.1" }},
		{"unsupported Codex", func(p *Provider) { p.Name, p.Version = Codex, "0.162.0" }},
		{"version output not version", func(p *Provider) { p.Name, p.Version = Codex, "codex-cli 0.162.1" }},
		{"missing installation", func(p *Provider) { p.Installation = "" }},
		{"absent installation", func(p *Provider) { p.Installation = "absent" }},
		{"missing path", func(p *Provider) { p.Path = "" }},
		{"relative path", func(p *Provider) { p.Path = "pi" }},
		{"NUL path", func(p *Provider) { p.Path = path + "\x00" }},
		{"missing executable", func(p *Provider) { p.Path = filepath.Join(root, "PRIVATE-MISSING") }},
		{"directory", func(p *Provider) { p.Path = root }},
		{"not executable", func(p *Provider) { p.Path = nonExecutable }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected := Provider{ID: "selected", Name: Pi, Version: "1.4.0", Path: path, Installation: Installed}
			tc.edit(&selected)
			e, err := NewExecutor(selected, time.Second)
			if e != nil || err == nil || err.Error() != "提供方执行配置无效或版本不受支持" {
				t.Fatalf("invalid configuration accepted or leaked: %T, %v", e, err)
			}
		})
	}
	selected := Provider{ID: "selected", Name: Pi, Version: "1.4.0", Path: path, Installation: Installed}
	if e, err := NewExecutor(selected, -time.Second); e != nil || err == nil {
		t.Fatalf("negative timeout accepted: %T, %v", e, err)
	}
}
