package catalog

import (
	"context"
	"fmt"

	"airmedy/internal/domain"
)

// fakeHost is one running plugin: the capabilities it answered with, and the
// describe answer plus call result it produces.
type fakeHost struct {
	manifest     domain.PluginManifest
	capabilities []domain.Capability
	describe     domain.PluginDescribe
	describeErr  error
	searchResult domain.PluginSearchResult
	searchErr    error
	calls        []string
}

func (h *fakeHost) Manifest() domain.PluginManifest { return h.manifest }

func (h *fakeHost) Capabilities() []domain.Capability { return h.capabilities }

func (h *fakeHost) Call(_ context.Context, method string, params, result any) error {
	h.calls = append(h.calls, method)
	if method == domain.MethodSearch {
		if h.searchErr != nil {
			return h.searchErr
		}
		if target, ok := result.(*domain.PluginSearchResult); ok {
			*target = h.searchResult
		}
		return nil
	}
	return fmt.Errorf("%w: %q", domain.ErrPluginMethodNotFound, method)
}

func (h *fakeHost) Close() error { return nil }

// fakeRegistry is the registry slice the adapters consume.
type fakeRegistry struct {
	hosts []domain.PluginHost
}

func (r *fakeRegistry) RunningHosts(context.Context) []domain.PluginHost { return r.hosts }

// Describe answers from the fake's own field, which is what the source table
// reads. The production registry keeps this answer on its entry; a fake keeps it
// on the host, so the lookup goes through the concrete type.
func (r *fakeRegistry) Describe(_ context.Context, id string) (domain.PluginDescribe, error) {
	for _, host := range r.hosts {
		if host.Manifest().ID != id {
			continue
		}
		if concrete, ok := host.(*fakeHost); ok {
			return concrete.describe, concrete.describeErr
		}
		return domain.PluginDescribe{}, nil
	}
	return domain.PluginDescribe{}, fmt.Errorf("%w: %q", domain.ErrPluginNotFound, id)
}

// searchPlugin builds a host that declares catalog.search.
func searchPlugin(id string, sources ...domain.CatalogSearchSource) *fakeHost {
	return &fakeHost{
		manifest:     domain.PluginManifest{ID: id},
		capabilities: []domain.Capability{domain.CapabilityCatalogSearch},
		describe:     domain.PluginDescribe{SearchSources: sources},
	}
}
