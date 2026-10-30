package platform

import (
	"context"
	"sort"
	"time"

	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// ProviderCatalog is a startup-only, memory-only snapshot. It grants no execution
// capability; a member's provider preference can outlive a discovery result.
type ProviderCatalog struct{ items []provider.Provider }

func DiscoverProviders(ctx context.Context, search provider.Search, runner provider.Runner) *ProviderCatalog {
	items := provider.Discover(search)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Keep every discovery even after the shared budget expires. Probe preserves
	// unknown states and supplies sanitized evidence without executing on GET.
	for i, p := range items {
		p = runner.Probe(ctx, p, provider.VersionProbe)
		items[i] = runner.Probe(ctx, p, provider.AccountProbe)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return &ProviderCatalog{items: items}
}

// List returns copies, including evidence, so callers cannot mutate the snapshot.
// IDs are ascending and before is an exclusive position in that fixed order.
func (c *ProviderCatalog) List(limit int, before string) ([]provider.Provider, string, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 {
		return nil, "", store.ErrInvalid
	}
	start := sort.Search(len(c.items), func(i int) bool { return c.items[i].ID > before })
	end := min(start+limit, len(c.items))
	items := append([]provider.Provider{}, c.items[start:end]...)
	for i := range items {
		items[i].Evidence = append([]provider.Evidence{}, items[i].Evidence...)
	}
	next := ""
	if end < len(c.items) {
		next = items[len(items)-1].ID
	}
	return items, next, nil
}
