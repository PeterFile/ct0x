package httpapi

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
)

func TestProviderAdapterRouteProjectsTrustedSnapshotOnly(t *testing.T) {
	root := t.TempDir()
	calls := filepath.Join(root, "calls")
	first := providerFixture(t, filepath.Join(root, "one"), provider.Pi, "1.4.0", "exit 1", calls)
	second := providerFixture(t, filepath.Join(root, "two"), provider.Codex, "codex-cli 0.162.1", "exit 1", calls)
	catalog := platform.DiscoverProviders(context.Background(), provider.Search{Explicit: []provider.Location{
		{Name: provider.Pi, Path: first}, {Name: provider.Codex, Path: second},
	}}, provider.Runner{Timeout: time.Second, OutputLimit: 4096})
	legacy, _, _ := catalog.List(0, "")
	p := legacy[0]
	report := platform.ProviderAdapterReport{ID: "fixture_adapter", Protocol: provider.OneShotProtocol, Enabled: false,
		Capabilities: &provider.AdapterCapabilities{PlatformTools: provider.SupportUnknown, NativeResume: provider.SupportUnsupported,
			PermissionInteraction: provider.SupportUnsupported, Cancel: provider.SupportUnsupported,
			TerminalEvidence: provider.SupportSupported, Steer: provider.SupportUnknown,
			Evidence: provider.CapabilityEvidence{Source: provider.BuiltinContractEvidence, ProviderID: p.ID,
				Name: p.Name, Path: p.Path, Version: p.Version, Protocol: provider.OneShotProtocol}}}
	snapshot, err := catalog.WithAdapterReports(map[string]platform.ProviderAdapterReport{p.ID: report, legacy[1].ID: {
		ID: "unverified_adapter", Protocol: provider.CodexAppServerProtocol, Enabled: false,
	}})
	if err != nil {
		t.Fatal(err)
	}
	a := &groupAPI{handler: mustServer(t, DefaultAddress, ProviderRoute(snapshot)).Handler}
	firstPage := decoded[listResult[platform.ProviderAdapterView]](t, a.request("GET", "/api/v1/providers?limit=1", ""), 200)
	if len(firstPage.Items) != 1 || firstPage.NextCursor == "" || !reflect.DeepEqual(firstPage.Items[0].Provider, p) || !reflect.DeepEqual(firstPage.Items[0].Adapter, &report) {
		t.Fatalf("trusted projection lost provider fields or report: %+v", firstPage)
	}
	lastPage := decoded[listResult[platform.ProviderAdapterView]](t, a.request("GET", "/api/v1/providers?limit=1&before="+firstPage.NextCursor, ""), 200)
	if len(lastPage.Items) != 1 || lastPage.NextCursor != "" || lastPage.Items[0].ID != legacy[1].ID || lastPage.Items[0].Adapter.Capabilities != nil {
		t.Fatal("route inferred unknown capabilities or changed cursors")
	}
	for _, query := range []string{"adapter=untrusted", "enabled=true", "capabilities=supported", "protocol=codex_app_server"} {
		w := a.request("GET", "/api/v1/providers?"+query, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	old := &groupAPI{handler: mustServer(t, DefaultAddress, ProviderRoute(catalog)).Handler}
	oldResponse := old.request("GET", "/api/v1/providers", "")
	expected, err := json.Marshal(listResult[provider.Provider]{Items: legacy})
	if err != nil || oldResponse.Code != 200 || strings.TrimSpace(oldResponse.Body.String()) != string(expected) {
		t.Fatalf("no reports changed the old HTTP JSON: %s %v", oldResponse.Body, err)
	}
}
