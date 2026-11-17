package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func claudeFixtureSelection(t *testing.T) (Provider, ClaudeSDKOptions) {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "not-a-native-cli")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	bridge := filepath.Join(dir, "fixture.mjs")
	if err := os.WriteFile(bridge, []byte("throw new Error('must remain closed');\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "member.json")
	if err := os.WriteFile(config, []byte(`{"endpoint":"http://127.0.0.1:7310/api/v1/member-tools","token":"fixture-secret-token"}`), 0600); err != nil {
		t.Fatal(err)
	}
	return Provider{ID: "fixture-claude", Name: "claude", Version: "2.1.326", Path: stub, Installation: Installed}, ClaudeSDKOptions{
		NodePath: stub, BridgePath: bridge, ConfigPath: config, ToolNames: []string{"read_channel"},
	}
}

func TestClaudeSDKClosedCapabilitiesAndPublicExecution(t *testing.T) {
	selected, options := claudeFixtureSelection(t)
	a := NewClaudeSDKAdapter(options)
	capabilities, err := a.Capabilities(selected)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []Capability{CapabilityPlatformTools, CapabilityNativeResume, CapabilityPermissionInteraction, CapabilityCancel, CapabilityTerminalEvidence, CapabilitySteer} {
		if capabilities.Supports(required) {
			t.Fatalf("unverified capability promoted: %s", required)
		}
	}
	if capabilities.Evidence.Protocol != ClaudeSDKProtocol {
		t.Fatalf("bad protocol: %+v", capabilities)
	}
	if _, err := DefaultAdapterRegistry().NewExecutor(AdapterSelection{ID: ClaudeSDKAdapterID, Protocol: ClaudeSDKProtocol}, selected, 0); err == nil {
		t.Fatal("Claude registered by default")
	}
	e, err := a.NewExecutor(selected, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r := e.Execute(context.Background(), ExecuteRequest{}, func(Event) error { t.Fatal("closed entry emitted event"); return nil })
	if r.Started || r.Accepted || r.DirectChildReaped || r.OutputComplete || r.Outcome != OutcomeNotStarted {
		t.Fatalf("closed entry started execution: %+v", r)
	}
}

func TestClaudeSDKConstructorRejectsConfigAndVersionDrift(t *testing.T) {
	selected, options := claudeFixtureSelection(t)
	selected.Version = "2.1.325"
	if _, err := NewClaudeSDKExecutor(selected, options, 0); err == nil {
		t.Fatal("old CLI accepted")
	}
	selected.Version = "2.1.326"
	if err := os.Chmod(options.ConfigPath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClaudeSDKExecutor(selected, options, 0); err == nil {
		t.Fatal("public credential config accepted")
	}
}
