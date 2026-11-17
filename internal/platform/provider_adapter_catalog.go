package platform

import (
	"strings"

	"agentworkspace/internal/provider"
)

// ProviderAdapterReport describes trusted process configuration, not readiness
// or authority. ID identifies the adapter; the catalog map key is Provider.ID.
type ProviderAdapterReport struct {
	ID           string                        `json:"id"`
	Protocol     provider.Protocol             `json:"protocol"`
	Enabled      bool                          `json:"enabled"`
	Capabilities *provider.AdapterCapabilities `json:"capabilities,omitempty"`
}

// ProviderAdapterView preserves every legacy Provider field without changing
// provider.Provider or persisting adapter selection in the provider catalog.
type ProviderAdapterView struct {
	provider.Provider
	Adapter *ProviderAdapterReport `json:"adapter,omitempty"`
}

// WithAdapterReports replaces reports in a new immutable snapshot. Only trusted
// composition supplies these reports; validation performs no external probes.
func (c *ProviderCatalog) WithAdapterReports(reports map[string]ProviderAdapterReport) (*ProviderCatalog, error) {
	if c == nil {
		return nil, provider.ErrAdapterConfiguration
	}
	providers := make(map[string]provider.Provider, len(c.items))
	for _, p := range c.items {
		providers[p.ID] = p
	}
	snapshot := &ProviderCatalog{items: c.items, adapterReports: make(map[string]ProviderAdapterReport, len(reports))}
	for id, report := range reports {
		p, exists := providers[id]
		if !exists || strings.TrimSpace(id) == "" || report.ID == "" || strings.TrimSpace(report.ID) != report.ID ||
			strings.ContainsRune(report.ID, 0) || !validAdapterProtocol(report.Protocol) {
			return nil, provider.ErrAdapterConfiguration
		}
		if report.Capabilities != nil {
			caps := report.Capabilities
			e := caps.Evidence
			if e.Source != provider.BuiltinContractEvidence || e.ProviderID != p.ID || e.Name != p.Name ||
				e.Path != p.Path || e.Version != p.Version || e.Protocol != report.Protocol {
				return nil, provider.ErrAdapterConfiguration
			}
			for _, support := range []provider.Support{caps.PlatformTools, caps.NativeResume, caps.PermissionInteraction, caps.Cancel, caps.TerminalEvidence, caps.Steer} {
				if support != provider.SupportSupported && support != provider.SupportUnsupported && support != provider.SupportUnknown {
					return nil, provider.ErrAdapterConfiguration
				}
			}
		}
		snapshot.adapterReports[id] = copyAdapterReport(report)
	}
	return snapshot, nil
}

// ListWithAdapters uses the unchanged legacy ID paging and never leaks pointers
// to reports or capability evidence stored in the snapshot.
func (c *ProviderCatalog) ListWithAdapters(limit int, before string) ([]ProviderAdapterView, string, error) {
	items, next, err := c.List(limit, before)
	if err != nil {
		return nil, "", err
	}
	views := make([]ProviderAdapterView, len(items))
	for i, p := range items {
		views[i].Provider = p
		if report, exists := c.adapterReports[p.ID]; exists {
			copy := copyAdapterReport(report)
			views[i].Adapter = &copy
		}
	}
	return views, next, nil
}

func copyAdapterReport(report ProviderAdapterReport) ProviderAdapterReport {
	if report.Capabilities != nil {
		caps := *report.Capabilities
		report.Capabilities = &caps
	}
	return report
}

func validAdapterProtocol(protocol provider.Protocol) bool {
	return protocol == provider.OneShotProtocol || protocol == provider.PiExtensionProtocol || protocol == provider.CodexAppServerProtocol
}
