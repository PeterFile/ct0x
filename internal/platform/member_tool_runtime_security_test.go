package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestMemberToolRuntimeOptionValidation(t *testing.T) {
	f := setupMemberToolRuntime(t)
	for _, host := range []string{"127.0.0.1", "[::1]"} {
		for _, port := range []string{"1", "80", "7310", "65535", "00001", "00080"} {
			endpoint := "http://" + host + ":" + port + "/api/v1/member-tools"
			options := f.options
			options.Endpoint = endpoint
			runtime, err := NewMemberToolRuntime(f.db, options)
			if err != nil {
				t.Errorf("valid bridge endpoint rejected: %q", endpoint)
			} else if runtime.endpoint != endpoint {
				t.Errorf("supplied endpoint authority was normalized: %q", endpoint)
			}
		}
	}
	for _, endpoint := range []string{
		"", " http://127.0.0.1:7310/api/v1/member-tools", "https://127.0.0.1:7310/api/v1/member-tools",
		"http://localhost:7310/api/v1/member-tools", "http://0.0.0.0:7310/api/v1/member-tools", "http://192.0.2.1:7310/api/v1/member-tools",
		"http://127.0.0.2:65535/api/v1/member-tools", "http://127.255.255.255:7310/api/v1/member-tools",
		"http://[::]:7310/api/v1/member-tools", "http://[::1%25lo0]:7310/api/v1/member-tools",
		"http://[0:0:0:0:0:0:0:1]:7310/api/v1/member-tools", "http://[::0001]:7310/api/v1/member-tools",
		"http://[::ffff:127.0.0.1]:7310/api/v1/member-tools", "http://[::ffff:7f00:1]:7310/api/v1/member-tools",
		"http://127.0.0.1/api/v1/member-tools", "http://127.0.0.1:0/api/v1/member-tools", "http://127.0.0.1:65536/api/v1/member-tools",
		"http://127.0.0.1:00000/api/v1/member-tools", "http://[::1]:65536/api/v1/member-tools",
		"http://127.0.0.1:007310/api/v1/member-tools", "http://127.0.0.1:000001/api/v1/member-tools", "http://[::1]:000080/api/v1/member-tools",
		"http://127.0.0.1:+7310/api/v1/member-tools", "http://user@127.0.0.1:7310/api/v1/member-tools",
		"http://127.0.0.1:7310/api/v1/member-tools/", "http://127.0.0.1:7310/api/v1/member-tools?", "http://127.0.0.1:7310/api/v1/member-tools?x=1",
		"http://127.0.0.1:7310/api/v1/member-tools#", "http://127.0.0.1:7310/api/v1/member-tools#fragment",
		"http://127.0.0.1:7310/api/v1/%6dember-tools", "http://127.0.0.1:7310/other", "http:127.0.0.1:7310/api/v1/member-tools",
		"http://127.0.0.1:7310/api/v1/member-tools\r", "http://127.0.0.1:7310/api/v1/member-tools\n", "http://[::1]:7310/api/v1/member-tools\r\n",
		"http://127.0.0.1:7310\n/api/v1/member-tools", "http://[::1]:7310/api/v1/member-\rtools",
	} {
		options := f.options
		options.Endpoint = endpoint
		if runtime, err := NewMemberToolRuntime(f.db, options); err == nil || runtime != nil {
			t.Errorf("unsafe endpoint accepted: %q", endpoint)
		}
	}
	for _, operations := range [][]string{nil, {}, {""}, {"list_members", "list_members"}, {"list_members", "unknown_history"}, {"read_channel "}} {
		options := f.options
		options.Operations = operations
		if _, err := NewMemberToolRuntime(f.db, options); err == nil {
			t.Error("invalid or duplicate grants accepted")
		}
	}
	for _, capacity := range []int{-1, 201} {
		options := f.options
		options.MaxGroupMembers = capacity
		if _, err := NewMemberToolRuntime(f.db, options); err == nil {
			t.Error("out-of-range capacity accepted")
		}
	}
	for _, capacity := range []int{1, 200} {
		options := f.options
		options.MaxGroupMembers = capacity
		if _, err := NewMemberToolRuntime(f.db, options); err != nil {
			t.Error("valid capacity rejected")
		}
	}
	if _, err := NewMemberToolRuntime(nil, f.options); err == nil {
		t.Error("nil store accepted")
	}
	memberToolRuntimeEmpty(t, f.options.ConfigRoot)
	memberToolRuntimeEmpty(t, f.request.RuntimeDir)
}

func TestMemberToolRuntimeBindingValidation(t *testing.T) {
	f := setupMemberToolRuntime(t)
	factory := func(string, string, []string) (provider.Executor, error) {
		t.Error("Wrap invoked factory")
		return nil, nil
	}
	for _, field := range []string{"group", "member", "conversation", "delivery", "provider", "model"} {
		for _, invalid := range []string{"", " ", " padded", "padded ", "PRIVATE-\n", "PRIVATE-\r", "PRIVATE-\x00", "\xff"} {
			binding := f.binding
			switch field {
			case "group":
				binding.GroupID = invalid
			case "member":
				binding.MemberID = invalid
			case "conversation":
				binding.ConversationID = invalid
			case "delivery":
				binding.DeliveryID = invalid
			case "provider":
				binding.ProviderID = invalid
			case "model":
				binding.Model = invalid
			}
			if executor, err := f.runtime.Wrap(binding, factory); err == nil || executor != nil || strings.Contains(err.Error(), "PRIVATE-") {
				t.Errorf("unsafe %s binding accepted or leaked", field)
			}
		}
	}
	if _, err := f.runtime.Wrap(f.binding, nil); err == nil {
		t.Error("nil factory accepted")
	}
	options := f.options
	options.Operations = []string{"read_channel"}
	runtime, err := NewMemberToolRuntime(f.db, options)
	if err != nil {
		t.Fatal(err)
	}
	binding := f.binding
	binding.ProviderID, binding.Model = "", ""
	if _, err := runtime.Wrap(binding, factory); err != nil {
		t.Error("non-creation policy must allow the native default model")
	}
	memberToolRuntimeEmpty(t, f.options.ConfigRoot)
}

func TestMemberToolRuntimeRejectsUnsafeConfigRoots(t *testing.T) {
	f := setupMemberToolRuntime(t)
	base := t.TempDir()
	public, file, alias := filepath.Join(base, "public"), filepath.Join(base, "file"), filepath.Join(base, "alias")
	if err := os.Mkdir(public, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("outside-marker"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.options.ConfigRoot, alias); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"", "relative", public, file, alias, alias + "/", filepath.Join(base, "absent")} {
		options := f.options
		options.ConfigRoot = root
		if runtime, err := NewMemberToolRuntime(f.db, options); err == nil || runtime != nil {
			t.Error("unsafe config root accepted")
		}
	}
	data, err := os.ReadFile(file)
	if err != nil || string(data) != "outside-marker" {
		t.Fatal("constructor touched outside data")
	}
	memberToolRuntimeEmpty(t, f.options.ConfigRoot)
}

func TestMemberToolRuntimeConfigRootIdentityAndOverlap(t *testing.T) {
	for _, mutation := range []string{"permissions", "missing", "replacement", "symlink", "same", "config-under-runtime", "runtime-under-config", "ancestor-alias"} {
		t.Run(mutation, func(t *testing.T) {
			f := setupMemberToolRuntime(t)
			outside := t.TempDir()
			marker := filepath.Join(outside, "marker")
			if err := os.WriteFile(marker, []byte("outside-marker"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch mutation {
			case "permissions":
				err = os.Chmod(f.options.ConfigRoot, 0755)
			case "missing":
				err = os.Remove(f.options.ConfigRoot)
			case "replacement", "symlink":
				err = os.Rename(f.options.ConfigRoot, f.options.ConfigRoot+"-original")
				if err == nil {
					if mutation == "replacement" {
						err = os.Mkdir(f.options.ConfigRoot, 0700)
					} else {
						err = os.Symlink(outside, f.options.ConfigRoot)
					}
				}
			case "same":
				f.request.RuntimeDir = f.options.ConfigRoot
			case "config-under-runtime":
				f.request.RuntimeDir = filepath.Dir(f.options.ConfigRoot)
			case "runtime-under-config":
				f.request.RuntimeDir = filepath.Join(f.options.ConfigRoot, "logs")
				err = os.Mkdir(f.request.RuntimeDir, 0700)
			case "ancestor-alias":
				alias := filepath.Join(outside, "alias")
				err = os.Symlink(filepath.Dir(f.options.ConfigRoot), alias)
				f.request.RuntimeDir = filepath.Join(alias, "config")
			}
			if err != nil {
				t.Fatal(err)
			}
			called := false
			executor := f.wrap(t, func(string, string, []string) (provider.Executor, error) {
				called = true
				return nil, errors.New("PRIVATE-FACTORY")
			})
			receipt := executor.Execute(context.Background(), f.request, nil)
			if receipt.Outcome != provider.OutcomeNotStarted && receipt.Outcome != provider.OutcomeUnknown {
				t.Fatal("unsafe root fabricated a terminal outcome")
			}
			f.requirePreparationReceipt(t, receipt, receipt.Outcome)
			if called || len(f.runtime.Tools().capabilities) != 0 {
				t.Fatal("unsafe filesystem reached factory or retained authority")
			}
			data, err := os.ReadFile(marker)
			if err != nil || string(data) != "outside-marker" {
				t.Fatal("unsafe root operation touched outside marker")
			}
			if mutation == "replacement" || mutation == "symlink" {
				memberToolRuntimeEmpty(t, f.options.ConfigRoot+"-original")
			}
			if mutation == "replacement" {
				memberToolRuntimeEmpty(t, f.options.ConfigRoot)
			}
			f.requireUnsettled(t, domain.TurnStateStarting)
		})
	}
}

func TestMemberToolRuntimeExistingLogsRemainUnknown(t *testing.T) {
	for _, entry := range []string{"directory", "file", "symlink", "factory-race"} {
		t.Run(entry, func(t *testing.T) {
			f := setupMemberToolRuntime(t)
			path := filepath.Join(f.request.RuntimeDir, f.request.RunID)
			marker := filepath.Join(t.TempDir(), "marker")
			if err := os.WriteFile(marker, []byte("outside-marker"), 0600); err != nil {
				t.Fatal(err)
			}
			create := func() {
				t.Helper()
				var err error
				switch entry {
				case "directory", "factory-race":
					err = os.Mkdir(path, 0700)
				case "file":
					err = os.WriteFile(path, []byte("existing-run-marker"), 0600)
				case "symlink":
					err = os.Symlink(marker, path)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if entry != "factory-race" {
				create()
			}
			factories, natives := 0, 0
			executor := f.wrap(t, func(string, string, []string) (provider.Executor, error) {
				factories++
				create()
				return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
					natives++
					return provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded}
				}), nil
			})
			receipt := executor.Execute(context.Background(), f.request, nil)
			f.requirePreparationReceipt(t, receipt, provider.OutcomeUnknown)
			wantFactories := 0
			if entry == "factory-race" {
				wantFactories = 1
			}
			if factories != wantFactories || natives != 0 {
				t.Fatal("existing log identity launched native execution")
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("wrapper removed adapter-owned log entry")
			}
			data, err := os.ReadFile(marker)
			if err != nil || string(data) != "outside-marker" {
				t.Fatal("wrapper touched log symlink target")
			}
			f.requireUnsettled(t, domain.TurnStateStarting)
			if _, err := f.db.ApplyTurnReceipt(context.Background(), f.claim, receipt); err != nil {
				t.Fatal(err)
			}
			f.requireUnsettled(t, domain.TurnStateUnknown)
			memberToolRuntimeEmpty(t, f.options.ConfigRoot)
		})
	}
}

func TestMemberToolRuntimeConfigPermissionsChangeStillCleansOwnedFile(t *testing.T) {
	for _, phase := range []string{"factory", "native"} {
		t.Run(phase, func(t *testing.T) {
			f := setupMemberToolRuntime(t)
			var path, token string
			want := provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeUnknown, Terminal: provider.OutcomeFailed, Reason: "原生回执"}
			nativeCalled := false
			executor := f.wrap(t, func(configPath, _ string, _ []string) (provider.Executor, error) {
				path = configPath
				_, token = memberToolRuntimeConfig(t, path)
				if phase == "factory" {
					if err := os.Chmod(path, 0644); err != nil {
						t.Fatal(err)
					}
				}
				return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
					nativeCalled = true
					if err := os.Chmod(path, 0400); err != nil {
						t.Fatal(err)
					}
					return want
				}), nil
			})
			got := executor.Execute(context.Background(), f.request, nil)
			if phase == "factory" {
				f.requirePreparationReceipt(t, got, provider.OutcomeNotStarted)
				if nativeCalled {
					t.Error("unsafe config permissions reached native execution")
				}
			} else if got != want || !nativeCalled {
				t.Error("cleanup changed native receipt")
			}
			requireCapabilityDenied(t, capabilityUse(context.Background(), f.runtime.Tools(), token, "list_members"))
			memberToolRuntimeEmpty(t, f.options.ConfigRoot)
		})
	}
}

func TestMemberToolRuntimeCleanupOnlyOwnedConfig(t *testing.T) {
	for _, replacement := range []string{"file", "symlink", "directory", "root", "factory-root", "factory-config"} {
		t.Run(replacement, func(t *testing.T) {
			f := setupMemberToolRuntime(t)
			outside := t.TempDir()
			marker := filepath.Join(outside, "marker")
			if err := os.WriteFile(marker, []byte("outside-marker"), 0600); err != nil {
				t.Fatal(err)
			}
			var path, token, movedRoot string
			replace := func() {
				t.Helper()
				if replacement == "root" || replacement == "factory-root" {
					movedRoot = f.options.ConfigRoot + "-original"
					if err := os.Rename(f.options.ConfigRoot, movedRoot); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(f.options.ConfigRoot, 0700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Rename(path, path+"-original"); err != nil {
					t.Fatal(err)
				}
				var err error
				switch replacement {
				case "symlink":
					err = os.Symlink(marker, path)
				case "directory":
					err = os.Mkdir(path, 0700)
				default:
					err = os.WriteFile(path, []byte("replacement-marker"), 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			want := provider.ExecutionReceipt{Started: true, Accepted: true, Outcome: provider.OutcomeFailed, Terminal: provider.OutcomeFailed, NativeSessionID: "native", DirectChildReaped: true, OutputComplete: true, ExitCode: 7, Reason: "模型执行失败"}
			nativeCalled := false
			executor := f.wrap(t, func(configPath, _ string, _ []string) (provider.Executor, error) {
				path = configPath
				_, token = memberToolRuntimeConfig(t, path)
				if strings.HasPrefix(replacement, "factory-") {
					replace()
				}
				return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
					nativeCalled = true
					replace()
					return want
				}), nil
			})
			got := executor.Execute(context.Background(), f.request, nil)
			if strings.HasPrefix(replacement, "factory-") {
				f.requirePreparationReceipt(t, got, provider.OutcomeNotStarted)
				if nativeCalled {
					t.Error("replaced config/root reached native execution")
				}
			} else if got != want || !nativeCalled {
				t.Error("cleanup failure rewrote known native evidence")
			}
			requireCapabilityDenied(t, capabilityUse(context.Background(), f.runtime.Tools(), token, "list_members"))
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("cleanup deleted a replacement/unowned config entry")
			}
			if replacement != "directory" {
				data, err := os.ReadFile(path)
				if err != nil || !slices.Contains([]string{"outside-marker", "replacement-marker"}, string(data)) {
					t.Fatal("cleanup damaged replacement or symlink target")
				}
			}
			if movedRoot != "" {
				memberToolRuntimeEmpty(t, movedRoot)
			}
			data, err := os.ReadFile(marker)
			if err != nil || string(data) != "outside-marker" {
				t.Fatal("cleanup modified outside marker")
			}
			memberToolRuntimeEmpty(t, f.request.RuntimeDir)
			f.requireUnsettled(t, domain.TurnStateStarting)
		})
	}
}
