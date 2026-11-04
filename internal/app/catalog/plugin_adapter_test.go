package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"airmedy/internal/domain"
)

// TestSearchSources_ReadsTheDescribeTable: the button row comes from what the
// plugin reports, so a new searchable source is a plugin change, not a core one.
func TestSearchSources_ReadsTheDescribeTable(t *testing.T) {
	registry := &fakeRegistry{hosts: []domain.PluginHost{
		searchPlugin("wy-search",
			domain.CatalogSearchSource{ID: " WY ", Name: "网易云"},
			domain.CatalogSearchSource{ID: "KG"},
		),
	}}
	adapter := NewPluginSources(registry, quietLogger())

	sources, err := adapter.SearchSources(context.Background())
	if err != nil {
		t.Fatalf("SearchSources: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("sources = %+v, want 2", sources)
	}
	// Ids are normalised so the filter the UI sends back matches the table.
	if _, ok := sources["wy"]; !ok {
		t.Fatalf("sources = %+v, want the normalised wy key", sources)
	}
	// A source without a name falls back to its id, so the button is never blank.
	if sources["kg"].Name != "kg" {
		t.Fatalf("kg name = %q, want the id fallback", sources["kg"].Name)
	}
	if sources["wy"].Name != "网易云" {
		t.Fatalf("wy name = %q, want the declared name", sources["wy"].Name)
	}
}

// TestSearchSources_OnlyPluginsWithTheCapability: a plugin that can stream but
// cannot search must not contribute buttons.
func TestSearchSources_OnlyPluginsWithTheCapability(t *testing.T) {
	streamOnly := &fakeHost{
		manifest:     domain.PluginManifest{ID: "lx"},
		capabilities: []domain.Capability{domain.CapabilityStreamResolve},
		// Even a stray describe answer must be ignored: the capability is what
		// the contract keys on.
		describe: domain.PluginDescribe{SearchSources: []domain.CatalogSearchSource{{ID: "tx"}}},
	}
	registry := &fakeRegistry{hosts: []domain.PluginHost{
		streamOnly,
		searchPlugin("wy-search", domain.CatalogSearchSource{ID: "wy"}),
	}}
	adapter := NewPluginSources(registry, quietLogger())

	sources, err := adapter.SearchSources(context.Background())
	if err != nil {
		t.Fatalf("SearchSources: %v", err)
	}
	if _, ok := sources["tx"]; ok {
		t.Fatal("a stream-only plugin contributed a search source")
	}
	if _, ok := sources["wy"]; !ok {
		t.Fatalf("sources = %+v, want wy", sources)
	}
}

// TestSearchSources_OneBrokenPluginDoesNotHideTheOthers: a describe failure is
// reported but the rest of the row still appears.
func TestSearchSources_OneBrokenPluginDoesNotHideTheOthers(t *testing.T) {
	broken := searchPlugin("broken", domain.CatalogSearchSource{ID: "tx"})
	broken.describeErr = errors.New("process exited")
	registry := &fakeRegistry{hosts: []domain.PluginHost{
		broken,
		searchPlugin("good", domain.CatalogSearchSource{ID: "wy"}),
	}}
	adapter := NewPluginSources(registry, quietLogger())

	sources, err := adapter.SearchSources(context.Background())
	if err == nil {
		t.Fatal("SearchSources = nil error, want the broken plugin reported")
	}
	if _, ok := sources["wy"]; !ok {
		t.Fatalf("sources = %+v, want the healthy plugin's source", sources)
	}
}

// A plugin that does not implement describe is not a failure: it simply has no
// dynamic source list.
func TestSearchSources_MissingDescribeIsNotAnError(t *testing.T) {
	host := searchPlugin("static")
	host.describeErr = fmt.Errorf("%w: describe", domain.ErrPluginMethodNotFound)
	adapter := NewPluginSources(&fakeRegistry{hosts: []domain.PluginHost{host}}, quietLogger())

	sources, err := adapter.SearchSources(context.Background())
	if err != nil {
		t.Fatalf("SearchSources = %v, want nil for a plugin without describe", err)
	}
	if len(sources) != 0 {
		t.Fatalf("sources = %+v, want none", sources)
	}
}

// TestSearchSource_RoutesToTheOwningPlugin: one source, one call, and the params
// carry the normalised source plus the capped limit.
func TestSearchSource_RoutesToTheOwningPlugin(t *testing.T) {
	host := searchPlugin("wy-search", domain.CatalogSearchSource{ID: "wy"})
	host.searchResult = domain.PluginSearchResult{Rows: []domain.PluginSearchResultRow{
		{ExternalID: " 1 ", Title: " 晴天 ", Artists: []string{" 周杰伦 ", " "}},
	}}
	registry := &fakeRegistry{hosts: []domain.PluginHost{host}}
	adapter := NewPluginSources(registry, quietLogger())
	searcher := NewPluginSearcher(adapter, quietLogger())

	result, err := searcher.SearchSource(context.Background(), "WY", domain.PluginSearchRequest{
		Query: "晴天", Limit: 999,
	})
	if err != nil {
		t.Fatalf("SearchSource: %v", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("rows = %+v, want 1", result.Rows)
	}
	// The host trims the fields it sorts and renders on.
	if result.Rows[0].ExternalID != "1" || result.Rows[0].Title != "晴天" {
		t.Fatalf("row = %+v, want trimmed fields", result.Rows[0])
	}
	if calls := host.calls; len(calls) != 1 || calls[0] != domain.MethodSearch {
		t.Fatalf("calls = %v, want one catalog.search", calls)
	}
}

// TestSearchSource_CapsTheRowCount: a plugin that ignores Limit cannot make one
// answer enormous.
func TestSearchSource_CapsTheRowCount(t *testing.T) {
	host := searchPlugin("big", domain.CatalogSearchSource{ID: "wy"})
	rows := make([]domain.PluginSearchResultRow, 0, domain.MaxCatalogSearchPageSize+10)
	for index := 0; index < domain.MaxCatalogSearchPageSize+10; index++ {
		rows = append(rows, domain.PluginSearchResultRow{ExternalID: fmt.Sprintf("%d", index)})
	}
	host.searchResult = domain.PluginSearchResult{Rows: rows}

	adapter := NewPluginSources(&fakeRegistry{hosts: []domain.PluginHost{host}}, quietLogger())
	searcher := NewPluginSearcher(adapter, quietLogger())

	result, err := searcher.SearchSource(context.Background(), "wy", domain.PluginSearchRequest{Limit: 50})
	if err != nil {
		t.Fatalf("SearchSource: %v", err)
	}
	if len(result.Rows) > 50 {
		t.Fatalf("rows = %d, want at most the requested 50", len(result.Rows))
	}
}

// TestSearchSource_UnknownSourceNamesIt: asking for a source nobody reported is
// a routing miss that names the source.
func TestSearchSource_UnknownSourceNamesIt(t *testing.T) {
	adapter := NewPluginSources(&fakeRegistry{}, quietLogger())
	searcher := NewPluginSearcher(adapter, quietLogger())

	_, err := searcher.SearchSource(context.Background(), "tx", domain.PluginSearchRequest{Limit: 50})
	if !errors.Is(err, domain.ErrCatalogSourceUnavailable) {
		t.Fatalf("SearchSource = %v, want ErrCatalogSourceUnavailable", err)
	}
}

// TestSearchSource_MapsStructuredKinds: retry policy keys on data.kind, never on
// the wording.
func TestSearchSource_MapsStructuredKinds(t *testing.T) {
	cases := map[string]string{
		"rate limited": domain.CatalogSearchErrorRateLimited,
		"unavailable":  domain.CatalogSearchErrorUnavailable,
		"auth":         domain.CatalogSearchErrorAuth,
		"no kind":      "",
	}
	for name, kind := range cases {
		t.Run(name, func(t *testing.T) {
			host := searchPlugin("p", domain.CatalogSearchSource{ID: "wy"})
			data, _ := json.Marshal(domain.PluginErrorData{Kind: kind})
			host.searchErr = &domain.RPCError{Code: -32000, Message: "whatever", Data: data}

			adapter := NewPluginSources(&fakeRegistry{hosts: []domain.PluginHost{host}}, quietLogger())
			searcher := NewPluginSearcher(adapter, quietLogger())

			_, err := searcher.SearchSource(context.Background(), "wy", domain.PluginSearchRequest{})
			if !errors.Is(err, domain.ErrCatalogSearchFailed) {
				t.Fatalf("SearchSource = %v, want ErrCatalogSearchFailed", err)
			}
		})
	}
}

// A plugin that declares the capability but has no method gets an explanation,
// not a bare call error: retrying cannot grow a method.
func TestSearchSource_MissingMethodIsExplained(t *testing.T) {
	host := searchPlugin("p", domain.CatalogSearchSource{ID: "wy"})
	host.searchErr = fmt.Errorf("%w: catalog.search", domain.ErrPluginMethodNotFound)

	adapter := NewPluginSources(&fakeRegistry{hosts: []domain.PluginHost{host}}, quietLogger())
	searcher := NewPluginSearcher(adapter, quietLogger())

	_, err := searcher.SearchSource(context.Background(), "wy", domain.PluginSearchRequest{})
	if !errors.Is(err, domain.ErrCatalogSearchFailed) {
		t.Fatalf("SearchSource = %v, want ErrCatalogSearchFailed", err)
	}
	if !strings.Contains(err.Error(), domain.MethodSearch) {
		t.Fatalf("error = %v, want it to name the method", err)
	}
}
