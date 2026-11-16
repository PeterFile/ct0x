package provider

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
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
		{Codex, "0.166.0", 3 * time.Second},
	} {
		t.Run(tc.name+"/"+tc.version, func(t *testing.T) {
			selected := Provider{ID: "exact-id", Name: tc.name, Version: tc.version, Path: path,
				Installation: Installed, Account: AccountUnknown, Connectivity: Unchecked,
				Capabilities: Capabilities{Resume: true, Tools: true}, Evidence: []Evidence{{Reason: "unchanged"}}}
			before := selected
			before.Evidence = append([]Evidence(nil), selected.Evidence...)
			if err := ValidateExecutor(selected, tc.timeout); err != nil || !reflect.DeepEqual(selected, before) {
				t.Fatalf("validation rejected verified metadata or mutated it: %v", err)
			}
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

// The first real stat and subsequent factory stat are a deterministic sequence,
// not a claim to reproduce interleaving within a complete Prepare call.
func TestNewExecutorPreservesCapturedStatFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		errno syscall.Errno
	}{
		{"removed", syscall.ENOENT},
		{"self-symlink", syscall.ELOOP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "PRIVATE-provider")
			contents := []byte("#!/bin/sh\nprintf invoked > \"$0.invoked\"\nexit 99\n")
			if err := os.WriteFile(path, contents, 0700); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
				t.Fatalf("executable preflight failed: %v", err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if tc.errno == syscall.ELOOP {
				if err := os.Symlink(path, path); err != nil {
					t.Fatal(err)
				}
			}
			selected := Provider{ID: "opaque-id", Name: Pi, Version: "1.4.0", Path: path, Installation: Installed}
			executor, captured := NewExecutor(selected, 0)
			validated := ValidateExecutor(selected, 0)
			// Restore before inspecting the captured error; later path state must
			// not be used to infer what the actual factory stat observed.
			if tc.errno == syscall.ELOOP {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, contents, 0700); err != nil {
				t.Fatal(err)
			}
			if executor != nil || !errors.Is(captured, errExecutorConfiguration) || !errors.Is(captured, tc.errno) {
				t.Fatalf("factory lost captured stat cause or returned an executor: %T, %v", executor, captured)
			}
			for _, observed := range []error{captured, validated} {
				var stat *os.PathError
				if !errors.Is(observed, errExecutorConfiguration) || !errors.As(observed, &stat) || stat.Op != "stat" || stat.Path != path || !errors.Is(stat.Err, tc.errno) {
					t.Fatal("factory/validator did not preserve the actual stat PathError")
				}
				if tc.errno == syscall.ENOENT && !errors.Is(observed, os.ErrNotExist) {
					t.Fatal("factory/validator lost os.ErrNotExist classification")
				}
				if observed.Error() != "提供方执行配置无效或版本不受支持" || strings.Contains(observed.Error(), "PRIVATE") {
					t.Fatal("factory/validator changed its exact sanitized error text")
				}
			}
			if _, err := os.Stat(path + ".invoked"); !os.IsNotExist(err) {
				t.Fatalf("factory invoked native executable: %v", err)
			}
		})
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
		{"unnormalized Pi v1.4.0", func(p *Provider) { p.Version = "v1.4.0" }},
		{"unsupported Pi 1.4.1", func(p *Provider) { p.Version = "1.4.1" }},
		{"unsupported Codex 0.162.0", func(p *Provider) { p.Name, p.Version = Codex, "0.162.0" }},
		{"unsupported Codex 0.165.0", func(p *Provider) { p.Name, p.Version = Codex, "0.165.0" }},
		{"unsupported Codex 0.166.1", func(p *Provider) { p.Name, p.Version = Codex, "0.166.1" }},
		{"newer Codex 0.167.0", func(p *Provider) { p.Name, p.Version = Codex, "0.167.0" }},
		{"prerelease Codex 0.166.0-rc.1", func(p *Provider) { p.Name, p.Version = Codex, "0.166.0-rc.1" }},
		{"unnormalized Codex v0.166.0", func(p *Provider) { p.Name, p.Version = Codex, "v0.166.0" }},
		{"version output Codex 0.162.1", func(p *Provider) { p.Name, p.Version = Codex, "codex-cli 0.162.1" }},
		{"version output Codex 0.166.0", func(p *Provider) { p.Name, p.Version = Codex, "codex-cli 0.166.0" }},
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
			validationErr := ValidateExecutor(selected, time.Second)
			if !errors.Is(validationErr, errExecutorConfiguration) || validationErr.Error() != "提供方执行配置无效或版本不受支持" ||
				(tc.name != "missing executable" && validationErr != errExecutorConfiguration) {
				t.Fatalf("validator configuration ordering or classification changed: %v", validationErr)
			}
			if e != nil || !errors.Is(err, errExecutorConfiguration) || err.Error() != "提供方执行配置无效或版本不受支持" {
				t.Fatalf("invalid configuration accepted or leaked: %T, %v", e, err)
			}
			if tc.name != "missing executable" && err != errExecutorConfiguration {
				t.Fatal("metadata, version or executable-kind sentinel identity changed")
			}
		})
	}
	selected := Provider{ID: "selected", Name: Pi, Version: "1.4.0", Path: path, Installation: Installed}
	if err := ValidateExecutor(selected, -time.Second); err != errExecutorConfiguration {
		t.Fatalf("validator timeout sentinel changed: %v", err)
	}
	if e, err := NewExecutor(selected, -time.Second); e != nil || err != errExecutorConfiguration {
		t.Fatalf("negative timeout accepted: %T, %v", e, err)
	}
}
