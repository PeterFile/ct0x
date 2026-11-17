package platform

import (
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"agentworkspace/internal/provider"
)

func adapterCatalogFixture() (*ProviderCatalog, ProviderAdapterReport) {
	p := provider.Provider{ID: "pi_a", Name: provider.Pi, Path: "/fixture/pi", Version: "1.4.0",
		Installation: provider.Installed, Account: provider.AccountUnknown, Connectivity: provider.Unchecked,
		Capabilities: provider.Capabilities{Tools: true, Resume: true}, Evidence: []provider.Evidence{{Reason: "fixture"}}}
	c := &ProviderCatalog{items: []provider.Provider{p, {ID: "pi_b", Name: provider.Pi}}}
	return c, ProviderAdapterReport{ID: "pi_fixture", Protocol: provider.PiExtensionProtocol, Enabled: false,
		Capabilities: &provider.AdapterCapabilities{
			PlatformTools: provider.SupportSupported, NativeResume: provider.SupportUnknown,
			PermissionInteraction: provider.SupportUnsupported, Cancel: provider.SupportUnknown,
			TerminalEvidence: provider.SupportSupported, Steer: provider.SupportUnsupported,
			Evidence: provider.CapabilityEvidence{Source: provider.BuiltinContractEvidence, ProviderID: p.ID,
				Name: p.Name, Path: p.Path, Version: p.Version, Protocol: provider.PiExtensionProtocol},
		}}
}

func TestProviderAdapterSnapshotCopiesReportsAndViews(t *testing.T) {
	catalog, report := adapterCatalogFixture()
	expected := *report.Capabilities
	reports := map[string]ProviderAdapterReport{"pi_a": report}
	snapshot, err := catalog.WithAdapterReports(reports)
	if err != nil || snapshot == catalog {
		t.Fatalf("expected a new snapshot: %v", err)
	}
	report.Capabilities.PlatformTools = provider.SupportUnsupported
	report.Capabilities.Evidence.Path = "mutated"
	delete(reports, "pi_a")
	original, _, _ := catalog.ListWithAdapters(0, "")
	if original[0].Adapter != nil {
		t.Fatal("adding reports mutated the original catalog")
	}
	views, _, err := snapshot.ListWithAdapters(0, "")
	if err != nil || views[0].Adapter == nil || !reflect.DeepEqual(*views[0].Adapter.Capabilities, expected) || views[0].Adapter.Enabled {
		t.Fatalf("report copy changed support or enablement: %+v %v", views, err)
	}
	if views[0].Account != provider.AccountUnknown || views[0].Connectivity != provider.Unchecked || views[1].Adapter != nil {
		t.Fatal("adapter report changed independent states or fabricated another selection")
	}
	views[0].Adapter.Capabilities.Evidence.Version = "mutated"
	views[0].Adapter.Capabilities.NativeResume = provider.SupportSupported
	views[0].Adapter.ID = "mutated"
	views[0].Evidence[0].Reason = "mutated"
	again, _, _ := snapshot.ListWithAdapters(0, "")
	if !reflect.DeepEqual(*again[0].Adapter.Capabilities, expected) || again[0].Adapter.ID != "pi_fixture" || again[0].Evidence[0].Reason != "fixture" {
		t.Fatal("a view leaked mutable snapshot pointers")
	}
	cleared, err := snapshot.WithAdapterReports(nil)
	if err != nil {
		t.Fatal(err)
	}
	without, _, _ := cleared.ListWithAdapters(0, "")
	if without[0].Adapter != nil || again[0].Adapter == nil {
		t.Fatal("replacement reports mutated a previous snapshot")
	}
}

func TestProviderAdapterReportsRejectMismatchedEvidence(t *testing.T) {
	cases := map[string]func(*ProviderAdapterReport){
		"empty ID":         func(r *ProviderAdapterReport) { r.ID = "" },
		"spaced ID":        func(r *ProviderAdapterReport) { r.ID = " fixture " },
		"invalid protocol": func(r *ProviderAdapterReport) { r.Protocol = "future" },
		"account source":   func(r *ProviderAdapterReport) { r.Capabilities.Evidence.Source = "account" },
		"provider ID":      func(r *ProviderAdapterReport) { r.Capabilities.Evidence.ProviderID = "pi_b" },
		"name":             func(r *ProviderAdapterReport) { r.Capabilities.Evidence.Name = provider.Codex },
		"path":             func(r *ProviderAdapterReport) { r.Capabilities.Evidence.Path += "-other" },
		"version":          func(r *ProviderAdapterReport) { r.Capabilities.Evidence.Version = "1.4.1" },
		"protocol":         func(r *ProviderAdapterReport) { r.Capabilities.Evidence.Protocol = provider.OneShotProtocol },
		"invalid support":  func(r *ProviderAdapterReport) { r.Capabilities.Cancel = "ready" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			catalog, report := adapterCatalogFixture()
			mutate(&report)
			if snapshot, err := catalog.WithAdapterReports(map[string]ProviderAdapterReport{"pi_a": report}); snapshot != nil || !errors.Is(err, provider.ErrAdapterConfiguration) {
				t.Fatalf("invalid report accepted: %+v %v", snapshot, err)
			}
		})
	}
	for _, key := range []string{"", "unknown-provider"} {
		catalog, report := adapterCatalogFixture()
		if snapshot, err := catalog.WithAdapterReports(map[string]ProviderAdapterReport{key: report}); snapshot != nil || err == nil {
			t.Fatalf("unbound provider map key accepted: %q", key)
		}
	}
}

func TestProviderAdapterUnknownAndLegacyPagination(t *testing.T) {
	catalog, report := adapterCatalogFixture()
	for _, protocol := range []provider.Protocol{provider.OneShotProtocol, provider.PiExtensionProtocol, provider.CodexAppServerProtocol} {
		report.Protocol, report.Capabilities = protocol, nil
		snapshot, err := catalog.WithAdapterReports(map[string]ProviderAdapterReport{"pi_a": report})
		if err != nil {
			t.Fatal(err)
		}
		views, next, err := snapshot.ListWithAdapters(1, "")
		legacy, oldNext, oldErr := snapshot.List(1, "")
		if err != nil || oldErr != nil || next != oldNext || next != "pi_a" || !reflect.DeepEqual(views[0].Provider, legacy[0]) || views[0].Adapter.Capabilities != nil {
			t.Fatal("unknown capabilities were inferred from legacy flags or paging changed")
		}
		last, next, _ := snapshot.ListWithAdapters(1, next)
		if len(last) != 1 || last[0].ID != "pi_b" || last[0].Adapter != nil || next != "" {
			t.Fatal("exclusive ID pagination changed")
		}
	}
	for _, limit := range []int{-1, 201} {
		if _, _, err := catalog.ListWithAdapters(limit, ""); err == nil {
			t.Fatalf("invalid limit accepted: %d", limit)
		}
	}
	views, _, _ := catalog.ListWithAdapters(0, "")
	legacy, _, _ := catalog.List(0, "")
	newJSON, _ := json.Marshal(views)
	oldJSON, _ := json.Marshal(legacy)
	if string(newJSON) != string(oldJSON) {
		t.Fatalf("nil reports changed legacy JSON: %s != %s", newJSON, oldJSON)
	}
}

func TestProviderAdapterSnapshotConcurrentReads(t *testing.T) {
	catalog, report := adapterCatalogFixture()
	snapshot, err := catalog.WithAdapterReports(map[string]ProviderAdapterReport{"pi_a": report})
	if err != nil {
		t.Fatal(err)
	}
	var readers sync.WaitGroup
	for range 8 {
		readers.Go(func() {
			for range 10 {
				views, _, err := snapshot.ListWithAdapters(1, "")
				if err != nil || views[0].Adapter.Capabilities.NativeResume != provider.SupportUnknown {
					t.Error("concurrent reader changed capability evidence")
					return
				}
				views[0].Adapter.Capabilities.NativeResume = provider.SupportSupported
				views[0].Evidence[0].Reason = "local-only"
			}
		})
	}
	readers.Wait()
}
