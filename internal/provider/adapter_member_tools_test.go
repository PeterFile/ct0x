package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMemberToolAdaptersCapabilityBindingWithoutConfigOrProbe(t *testing.T) {
	selected, extension, config := memberToolFiles(t, `: > "$0.invoked"; exit 99`)
	for _, name := range []string{Pi, Codex} {
		t.Run(name, func(t *testing.T) {
			p, protocol, id := selected, PiExtensionProtocol, PiMemberToolAdapterID
			makeAdapter := func(path string, names []string) Adapter { return NewPiMemberToolAdapter(extension, path, names) }
			if name == Codex {
				p.Name, p.Version, protocol, id = Codex, "0.166.0", CodexAppServerProtocol, CodexAppServerAdapterID
				makeAdapter = NewCodexAppServerMemberToolAdapter
			}
			names := []string{"read_history", "list_members", "send_message", "read_channel", "search_history"}
			a := makeAdapter("", names)
			names[0] = "submit_result"
			before, _ := json.Marshal(p)
			caps, err := a.Capabilities(p)
			want := CapabilityEvidence{Source: BuiltinContractEvidence, ProviderID: p.ID, Name: p.Name, Path: p.Path, Version: p.Version, Protocol: protocol}
			if err != nil || caps.Evidence != want || !caps.Supports(CapabilityPlatformTools) || !caps.Supports(CapabilityTerminalEvidence) {
				t.Fatal("verified implementation did not bind capability evidence to the exact selection")
			}
			for _, required := range []Capability{CapabilityNativeResume, CapabilitySteer, CapabilityCancel, CapabilityPermissionInteraction} {
				if caps.Supports(required) {
					t.Error("unverified native capability was advertised")
				}
			}
			if e, err := a.NewExecutor(p, 0); err == nil || e != nil {
				t.Error("capability-only configuration constructed an executor")
			}
			registry, err := NewAdapterRegistry(AdapterRegistration{ID: id, Protocol: protocol, Adapter: makeAdapter(config, []string{"read_channel"})})
			if err != nil {
				t.Fatal(err)
			}
			if e, err := registry.NewExecutor(AdapterSelection{ID: id, Protocol: protocol}, p, time.Second, CapabilityPlatformTools, CapabilityTerminalEvidence); err != nil || e == nil {
				t.Fatal("explicit per-run registry did not construct the bound executor")
			}
			if _, err := DefaultAdapterRegistry().Capabilities(AdapterSelection{ID: id, Protocol: protocol}, p); err == nil {
				t.Error("member adapter was mounted by default")
			}
			for _, field := range []string{"version", "name", "path", "id", "installation"} {
				invalid := p
				switch field {
				case "version":
					invalid.Version = "0.162.1"
				case "name":
					invalid.Name = "other"
				case "path":
					invalid.Path = "relative"
				case "id":
					invalid.ID = " "
				case "installation":
					invalid.Installation = "absent"
				}
				if _, err := a.Capabilities(invalid); err == nil {
					t.Error("unverified metadata/path accepted")
				}
			}
			after, _ := json.Marshal(p)
			if !bytes.Equal(before, after) {
				t.Error("capability sampling changed provider metadata")
			}
		})
	}
	if _, err := os.Stat(selected.Path + ".invoked"); !os.IsNotExist(err) {
		t.Fatal("adapter construction invoked a CLI")
	}
}

func TestMemberToolAdapterProfilesMatchPiAndFreezeSelection(t *testing.T) {
	p, extension, config := memberToolFiles(t, `exit 99`)
	for _, names := range [][]string{
		{"read_history", "list_members", "send_message", "read_channel", "search_history"},
		{"decide_dispatch", "list_members", "read_channel", "search_history", "read_history"},
		{"submit_result", "read_channel", "search_history", "read_history"},
		{"read_channel"}, {}, {"*"}, {"read_channel", "read_channel"},
		{"decide_dispatch", "read_channel"}, {"submit_result", "read_channel", "search_history", "read_history", "decide_dispatch"},
	} {
		legacy, legacyErr := NewPiMemberToolExecutorWithTools(p, extension, config, names, 0)
		for _, a := range []Adapter{NewPiMemberToolAdapter(extension, config, names), NewCodexAppServerMemberToolAdapter(config, names)} {
			selected := p
			if a.(memberToolAdapter).name == Codex {
				selected.Name, selected.Version = Codex, "0.166.0"
			}
			e, err := a.NewExecutor(selected, 0)
			if (err == nil) != (legacyErr == nil) || (e != nil) != (legacy != nil) {
				t.Fatal("ordinary subset or complete-role selector diverged from the existing Pi contract")
			}
		}
	}
	input := []string{"read_history", "read_channel", "send_message", "search_history", "list_members"}
	a := NewPiMemberToolAdapter(extension, config, input)
	input[0] = "submit_result"
	e, err := a.NewExecutor(p, 0)
	if err != nil || !reflect.DeepEqual(e.(piExecutor).memberTools.toolNames, []string{"list_members", "read_channel", "send_message", "search_history", "read_history"}) {
		t.Fatal("trusted intake subset was mutated or mistaken for a coordinator profile")
	}
}

func TestMemberToolConfigRejectsUnsafeTransportAndFiles(t *testing.T) {
	p, extension, path := memberToolFiles(t, `exit 99`)
	for _, raw := range []string{
		`{"endpoint":"http://127.0.0.1:1/api/v1/member-tools","endpoint":"http://127.0.0.1:2/api/v1/member-tools","token":"safe"}`,
		`{"endpoint":"http://127.0.0.1:1/api/v1/member-tools","token":"safe","token":"other"}`,
		`{"endpoint":"http://127.0.0.1:1/api/v1/member-tools","token":"safe","origin":"http://external.invalid"}`,
		`{"endpoint":"http://127.0.0.1:1/api/v1/member-tools","token":"bad\r\nheader"}`,
		`{"endpoint":"http://127.0.0.1:1/api/v1/member-tools","token":""}`,
		memberToolTestConfig + `{}`, strings.Repeat(" ", codexAppServerMemberConfigLimit+1), "\xff",
	} {
		if os.WriteFile(path, []byte(raw), 0600) != nil {
			t.Fatal("config fixture unavailable")
		}
		for _, a := range []Adapter{NewPiMemberToolAdapter(extension, path, []string{"read_channel"}), NewCodexAppServerMemberToolAdapter(path, []string{"read_channel"})} {
			selected := p
			if a.(memberToolAdapter).name == Codex {
				selected.Name, selected.Version = Codex, "0.166.0"
			}
			if _, err := a.Capabilities(selected); err != nil {
				t.Fatal("implementation capability sampling read runtime config contents")
			}
			if e, err := a.NewExecutor(selected, 0); err == nil || e != nil || strings.Contains(err.Error(), memberToolTestToken) {
				t.Fatal("unsafe actual config constructed an executor or leaked contents")
			}
		}
	}
	for _, endpoint := range []string{"http://localhost:1", "https://127.0.0.1:1", "http://127.0.0.2:1", "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://user@127.0.0.1:1", "http://127.0.0.1:1/api/v1/member-tools?redirect=http://external.invalid", "http://127.0.0.1:1/api/v1/member-tools#fragment", "http://127.0.0.1:1/api/v1/else/../member-tools", "http://127.0.0.1:1/api/v1/member-tools/"} {
		if !strings.Contains(endpoint, "/api/") {
			endpoint += "/api/v1/member-tools"
		}
		raw, _ := json.Marshal(map[string]string{"endpoint": endpoint, "token": memberToolTestToken})
		_ = os.WriteFile(path, raw, 0600)
		if _, err := readMemberToolConfig(path); err == nil {
			t.Fatal("nonliteral loopback endpoint was accepted")
		}
	}
	for endpoint, origin := range map[string]string{"http://127.0.0.1:1": "http://127.0.0.1:1", "http://[::1]:65535": "http://[::1]:65535", "http://127.0.0.1:80": "http://127.0.0.1", "http://[::1]:00080": "http://[::1]"} {
		raw, _ := json.Marshal(map[string]string{"endpoint": endpoint + "/api/v1/member-tools", "token": memberToolTestToken})
		_ = os.WriteFile(path, raw, 0600)
		config, err := readMemberToolConfig(path)
		if err != nil || config.origin != origin || config.endpoint != endpoint+"/api/v1/member-tools" {
			t.Fatal("verified literal endpoint or native canonical origin was rejected")
		}
	}
	_ = os.WriteFile(path, []byte(memberToolTestConfig), 0600)
	link := path + ".link"
	if os.Symlink(path, link) != nil {
		t.Fatal("symlink fixture unavailable")
	}
	fifo := path + ".fifo"
	if syscall.Mkfifo(fifo, 0600) != nil {
		t.Fatal("fifo fixture unavailable")
	}
	for _, unsafe := range []string{"", "relative", link, link + "/", fifo, filepath.Dir(path)} {
		if _, err := readMemberToolConfig(unsafe); err == nil {
			t.Fatal("unsafe file path was accepted")
		}
	}
	for _, mode := range []os.FileMode{0644, 0660, 0700, 0600 | os.ModeSetuid} {
		_ = os.Chmod(path, mode)
		if _, err := readMemberToolConfig(path); err == nil {
			t.Fatal("non-0600 config was accepted")
		}
	}
}

func TestMemberToolConfigAcceptsOnlyIssuedContinuationMarker(t *testing.T) {
	_, _, path := memberToolFiles(t, `exit 99`)
	base := strings.TrimSuffix(strings.TrimSpace(memberToolTestConfig), "}")
	for _, tc := range []struct {
		field string
		valid bool
	}{
		{"", true}, {`,"continue_work":true`, true}, {`,"continue_work":false`, false},
		{`,"continue_work":null`, false}, {`,"continue_work":"true"`, false},
		{`,"continue_work":true,"continue_work":true`, false}, {`,"CONTINUE_WORK":true`, false},
	} {
		if err := os.WriteFile(path, []byte(base+tc.field+"}"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readMemberToolConfig(path); (err == nil) != tc.valid {
			t.Errorf("continuation field %q valid=%v: %v", tc.field, tc.valid, err)
		}
	}
}

func TestCodexAppServerMemberConfigDriftDoesNotStartProcess(t *testing.T) {
	for _, change := range []string{"contents", "replacement", "mode"} {
		t.Run(change, func(t *testing.T) {
			selected, path, request := appServerMemberFixture(t, "normal")
			e, err := NewCodexAppServerMemberToolExecutor(selected, path, []string{"read_channel"}, time.Second)
			if err != nil {
				t.Fatal("valid member config rejected")
			}
			switch change {
			case "contents":
				_ = os.WriteFile(path, []byte(strings.ReplaceAll(memberToolTestConfig, memberToolTestToken, "other-token")), 0600)
			case "replacement":
				other := path + ".new"
				_ = os.WriteFile(other, []byte(memberToolTestConfig), 0600)
				_ = os.Rename(other, path)
			case "mode":
				_ = os.Chmod(path, 0644)
			}
			r := e.Execute(context.Background(), request, nil)
			if r.Outcome != OutcomeNotStarted || r.Started {
				t.Fatal("changed config started a native process")
			}
			if entries, _ := os.ReadDir(request.RuntimeDir); len(entries) != 0 {
				t.Fatal("changed config created execution state")
			}
		})
	}
}
