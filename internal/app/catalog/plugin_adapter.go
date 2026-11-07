// Package catalog assembles one ranked, paged answer from several plugin
// sources.
//
// The host owns the ranking, de-duplication, paging and the "can this be
// downloaded?" decision; each plugin owns one source's search endpoint. That
// split is what keeps a new source a plugin change instead of a core change
// (catalog/plugins/README.md, "Everything is a plugin").
//
// This package depends on domain only: the plugin registry and the stream router
// are reached through narrow ports (CatalogSearchSourceRegistry,
// CatalogSearcher, ResolverAvailability), so app never imports app/plugin or
// infra.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"airmedy/internal/domain"
)

// PluginRegistry is the slice of the plugin registry the adapters need.
//
// It is deliberately small: the source table comes from what running plugins
// reported through `describe`, and one search is one host.Call on the host the
// table named. *plugin.Service satisfies it.
type PluginRegistry interface {
	// RunningHosts returns every running plugin, sorted by id. The source table
	// and the search call both start here: a plugin that is not running reports
	// nothing and cannot be asked.
	RunningHosts(ctx context.Context) []domain.PluginHost
	// Describe asks a running plugin for its current routes and sources. The
	// searchable source list travels on that answer, so this feature adds no
	// protocol method (docs/plugin-api.md §6.1).
	Describe(ctx context.Context, id string) (domain.PluginDescribe, error)
}

// PluginSources implements domain.CatalogSearchSourceRegistry over the plugins
// that declared `catalog.search`.
//
// The source list is read from `describe`, which the contract already defines:
// a plugin whose searchable set depends on user content reports it there, and a
// plugin that never answers describe keeps whatever its manifest declared.
// Reusing it means this feature adds no protocol method (docs/plugin-api.md
// §6.1).
type PluginSources struct {
	registry PluginRegistry
	logger   *slog.Logger
}

// NewPluginSources builds the source-table adapter.
func NewPluginSources(registry PluginRegistry, logger *slog.Logger) *PluginSources {
	if logger == nil {
		logger = slog.Default()
	}
	return &PluginSources{registry: registry, logger: logger}
}

// SearchSources implements domain.CatalogSearchSourceRegistry.
//
// Only plugins that declare `catalog.search` contribute, and only sources with a
// non-empty id: the button row renders one per entry, so an empty id would be a
// button that filters nothing.
func (p *PluginSources) SearchSources(ctx context.Context) (map[string]domain.CatalogSearchSource, error) {
	if p.registry == nil {
		return map[string]domain.CatalogSearchSource{}, nil
	}

	sources := make(map[string]domain.CatalogSearchSource)
	var errs []error
	for _, host := range p.registry.RunningHosts(ctx) {
		id := host.Manifest().ID
		if !domain.HasCapability(host.Capabilities(), domain.CapabilityCatalogSearch) {
			continue
		}

		describe, err := p.registry.Describe(ctx, id)
		if err != nil {
			if errors.Is(err, domain.ErrPluginMethodNotFound) {
				// A plugin may declare the capability without implementing
				// describe; it then has no dynamic source list, and that is not
				// worth an error or a log line.
				continue
			}
			// One broken plugin must not remove every other source from the
			// row, so the failure is reported and the rest still contribute.
			errs = append(errs, fmt.Errorf("describe %q: %w", id, err))
			continue
		}

		for _, source := range describe.SearchSources {
			provider := domain.NormalizeProvider(source.ID)
			if provider == "" {
				continue
			}
			if _, exists := sources[provider]; exists {
				// Two plugins claiming the same searchable source would make
				// the source button non-deterministic. First wins, and the
				// conflict is logged: unlike routing there is no user choice to
				// honour here, and a working source is better than none.
				p.logger.Warn("catalog: two plugins report the same searchable source",
					"source", provider, "plugin", id)
				continue
			}
			if strings.TrimSpace(source.Name) == "" {
				source.Name = provider
			}
			source.ID = provider
			sources[provider] = source
		}
	}
	return sources, errors.Join(errs...)
}

// pluginForSource finds the running plugin that reported a source.
//
// It is a lookup over the same describe answers the source table is built from,
// so a source the user just disabled cannot be searched by a stale table: the
// question is asked of the plugin, not of a cache.
func (p *PluginSources) pluginForSource(ctx context.Context, source string) (domain.PluginHost, bool) {
	if p.registry == nil {
		return nil, false
	}
	for _, host := range p.registry.RunningHosts(ctx) {
		if !domain.HasCapability(host.Capabilities(), domain.CapabilityCatalogSearch) {
			continue
		}
		describe, err := p.registry.Describe(ctx, host.Manifest().ID)
		if err != nil {
			continue
		}
		for _, reported := range describe.SearchSources {
			if domain.NormalizeProvider(reported.ID) == source {
				return host, true
			}
		}
	}
	return nil, false
}

// PluginSettingReader reads the host-owned value of a setting a plugin declared.
// *plugin.SettingStore satisfies it; the interface keeps app/catalog free of an
// app/plugin import.
type PluginSettingReader interface {
	GetBool(ctx context.Context, pluginID, key string, fallback bool) bool
	// GetInt reads a `type: "number"` declaration. A missing or unparsable value
	// returns fallback, so an older plugin image cannot break paging.
	GetInt(ctx context.Context, pluginID, key string, fallback int) int
}

// SourceSettingReader resolves a setting through the plugin that owns a source.
//
// The catalog service knows providers, not plugin ids, while the setting lives
// in the plugin's KV. This adapter joins the two without either package
// importing the other.
type SourceSettingReader struct {
	sources  *PluginSources
	settings PluginSettingReader
}

// NewSourceSettingReader builds the per-source setting lookup.
func NewSourceSettingReader(sources *PluginSources, settings PluginSettingReader) *SourceSettingReader {
	return &SourceSettingReader{sources: sources, settings: settings}
}

// SourceInt implements domain.CatalogSourceSettingReader.
func (r *SourceSettingReader) SourceInt(ctx context.Context, source, key string, fallback int) int {
	if r == nil || r.sources == nil || r.settings == nil {
		return fallback
	}
	host, ok := r.sources.pluginForSource(ctx, source)
	if !ok {
		return fallback
	}
	return r.settings.GetInt(ctx, host.Manifest().ID, key, fallback)
}

// PluginSearcher implements domain.CatalogSearcher by calling the one plugin that
// owns a source.
type PluginSearcher struct {
	sources  *PluginSources
	settings PluginSettingReader
	logger   *slog.Logger
}

// NewPluginSearcher builds the search adapter over the same source table, so
// routing and rendering can never disagree about who owns a source. settings may
// be nil, in which case covers are requested (the declared default).
func NewPluginSearcher(sources *PluginSources, settings PluginSettingReader, logger *slog.Logger) *PluginSearcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &PluginSearcher{sources: sources, settings: settings, logger: logger}
}

// SearchSource implements domain.CatalogSearcher.
func (s *PluginSearcher) SearchSource(
	ctx context.Context, source string, request domain.PluginSearchRequest,
) (domain.PluginSearchResult, error) {
	provider := domain.NormalizeProvider(source)
	if provider == "" {
		return domain.PluginSearchResult{}, fmt.Errorf("%w: empty source", domain.ErrCatalogSearchInvalid)
	}
	if s.sources == nil {
		return domain.PluginSearchResult{}, fmt.Errorf("%w: %q", domain.ErrCatalogSourceUnavailable, provider)
	}

	host, ok := s.sources.pluginForSource(ctx, provider)
	if !ok {
		return domain.PluginSearchResult{}, fmt.Errorf("%w: %q", domain.ErrCatalogSourceUnavailable, provider)
	}

	// The host caps the row count regardless of what the plugin returns: a
	// plugin that ignores Limit must not be able to make one answer enormous
	// (the wire caps a line at 8 MiB).
	limit := request.Limit
	if limit <= 0 || limit > domain.MaxCatalogSearchPageSize {
		limit = domain.MaxCatalogSearchPageSize
	}
	request.Source = provider
	request.Limit = limit
	// The plugin decides whether to include cover URLs. The host reads its
	// declared switch from the plugin's KV; a plugin that never declared it keeps
	// the default (true).
	request.IncludeCover = true
	if !request.ForceCover && s.settings != nil {
		request.IncludeCover = s.settings.GetBool(ctx, host.Manifest().ID,
			domain.CatalogSettingIncludeCover, true)
	}

	var result domain.PluginSearchResult
	if err := host.Call(ctx, domain.MethodSearch, request, &result); err != nil {
		return domain.PluginSearchResult{}, mapSearchError(host.Manifest().ID, provider, err)
	}
	if len(result.Rows) > limit {
		result.Rows = result.Rows[:limit]
	}
	return normalizeResult(result), nil
}

// normalizeResult trims the fields the host sorts and renders on, so a plugin
// returning stray whitespace cannot produce a row that looks blank or sorts oddly.
func normalizeResult(result domain.PluginSearchResult) domain.PluginSearchResult {
	rows := make([]domain.PluginSearchResultRow, 0, len(result.Rows))
	for _, row := range result.Rows {
		row.ExternalID = strings.TrimSpace(row.ExternalID)
		row.Title = strings.TrimSpace(row.Title)
		row.AlbumName = strings.TrimSpace(row.AlbumName)
		row.CoverURL = strings.TrimSpace(row.CoverURL)
		row.CoverURLLarge = strings.TrimSpace(row.CoverURLLarge)
		rows = append(rows, row)
	}
	result.Rows = rows
	return result
}

// mapSearchError turns a failed plugin call into a sentinel the service and
// binding can classify.
//
// Like stream.resolve, only the structured `data.kind` counts: the message is
// never parsed for words, because wording is free text and a retry decision must
// not depend on it.
func mapSearchError(pluginID, source string, err error) error {
	var rpcErr *domain.RPCError
	if errors.As(err, &rpcErr) {
		switch pluginErrorKind(rpcErr) {
		case domain.CatalogSearchErrorRateLimited:
			return fmt.Errorf("%w: plugin %q rate limited searching %q: %s",
				domain.ErrCatalogSearchFailed, pluginID, source, rpcErr.Message)
		case domain.CatalogSearchErrorUnavailable:
			return fmt.Errorf("%w: plugin %q cannot search %q: %s",
				domain.ErrCatalogSearchFailed, pluginID, source, rpcErr.Message)
		case domain.CatalogSearchErrorAuth:
			return fmt.Errorf("%w: plugin %q needs credentials to search %q",
				domain.ErrCatalogSearchFailed, pluginID, source)
		}
	}

	if errors.Is(err, domain.ErrPluginMethodNotFound) {
		// The plugin declared the capability but has no method: retrying cannot
		// grow one, so fail with an explanation instead of a bare call error.
		return fmt.Errorf("%w: plugin %q does not implement %s",
			domain.ErrCatalogSearchFailed, pluginID, domain.MethodSearch)
	}
	return fmt.Errorf("%w: plugin %q searching %q: %w",
		domain.ErrCatalogSearchFailed, pluginID, source, err)
}

// pluginErrorKind decodes the structured kind from an error's data object. A
// missing or malformed object yields "", which leaves the failure unclassified.
func pluginErrorKind(rpcErr *domain.RPCError) string {
	if len(rpcErr.Data) == 0 {
		return ""
	}
	var data domain.PluginErrorData
	if err := json.Unmarshal(rpcErr.Data, &data); err != nil {
		return ""
	}
	return domain.NormalizeProvider(data.Kind)
}

// Compile-time proof that the adapters satisfy the ports the service consumes.
var (
	_ domain.CatalogSearchSourceRegistry = (*PluginSources)(nil)
	_ domain.CatalogSearcher             = (*PluginSearcher)(nil)
	_ domain.CatalogSourceSettingReader  = (*SourceSettingReader)(nil)
)
