package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"airmedy/internal/domain"
)

// defaultSourceTimeout bounds one source's search.
//
// It is shorter than the host's 15s plugin call bound on purpose: the plugin
// contract asks a plugin to answer within that, and the *host* must return
// partial results rather than block the whole page on the slowest source. When
// a source misses this budget it becomes a reported failure, not a missing row.
const defaultSourceTimeout = 10 * time.Second

// Config tunes the service. The zero value is usable; every field has a
// sensible default so a test only sets what it exercises.
type Config struct {
	// SourceTimeout is how long one source may take before it is reported as a
	// failure. Zero means defaultSourceTimeout.
	SourceTimeout time.Duration
}

// Service assembles one ranked, paged answer from several plugin sources.
//
// It owns no transport: the plugin call lives behind CatalogSearcher and the
// source table behind CatalogSearchSourceRegistry, so this package is testable
// without a process, and app/catalog never imports app/plugin.
type Service struct {
	sources        domain.CatalogSearchSourceRegistry
	searcher       domain.CatalogSearcher
	resolver       ResolverAvailability
	states         domain.CatalogRowStateResolver
	library        RemoteTracks
	localTracks    LocalTracks
	favorites      FavoritesWriter
	playlists      PlaylistMembership
	settings       SettingsReader
	sourceSettings domain.CatalogSourceSettingReader
	covers         domain.CatalogCoverCache
	logger         *slog.Logger
	sourceTimeout  time.Duration

	// coverMu guards the in-memory cover cache. A search only hands URLs to the
	// UI; the cache keeps them so an action can still embed artwork after the
	// user turned result thumbnails off (the plugin then omits cover_url).
	coverMu    sync.RWMutex
	coverCache map[domain.CatalogIdentity]string
}

// ResolverAvailability answers "can the host actually download from this
// source?". It is a separate port because the answer comes from the stream
// router, not from the plugin registry: a source is downloadable when a resolver
// (built-in or plugin) claims it, and that is a different question from whether
// something can search it (catalog/plugins/README.md, "可搜 ≠ 可下").
type ResolverAvailability interface {
	// CanResolve reports whether a download for this provider would have an
	// owner to handle it.
	CanResolve(provider string) bool
}

// Deps are the collaborators the service needs.
//
// Every field is optional: a deployment (or test) without a capability gets the
// documented error instead of a panic. A missing resolver means "nothing is
// downloadable"; a missing searcher means "no source can answer"; a missing
// library means the actions fail while search still works.
type Deps struct {
	Sources        domain.CatalogSearchSourceRegistry
	Searcher       domain.CatalogSearcher
	Resolver       ResolverAvailability
	States         domain.CatalogRowStateResolver
	Library        RemoteTracks
	LocalTracks    LocalTracks
	Favorites      FavoritesWriter
	Playlists      PlaylistMembership
	Settings       SettingsReader
	SourceSettings domain.CatalogSourceSettingReader
	Covers         domain.CatalogCoverCache
	Logger         *slog.Logger
	Config         Config
}

// NewService builds the catalog service.
func NewService(deps Deps) *Service {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	timeout := deps.Config.SourceTimeout
	if timeout <= 0 {
		timeout = defaultSourceTimeout
	}
	return &Service{
		sources:        deps.Sources,
		searcher:       deps.Searcher,
		resolver:       deps.Resolver,
		states:         deps.States,
		library:        deps.Library,
		localTracks:    deps.LocalTracks,
		favorites:      deps.Favorites,
		playlists:      deps.Playlists,
		settings:       deps.Settings,
		sourceSettings: deps.SourceSettings,
		covers:         deps.Covers,
		logger:         logger,
		sourceTimeout:  timeout,
		coverCache:     make(map[domain.CatalogIdentity]string),
	}
}

// Search answers one page of merged results.
//
// Fan-out is concurrent and bounded by the caller's context: one slow source
// must not delay a fast one, and the page is assembled from whatever answered
// within SourceTimeout. Partial results are the point — a dead source is
// reported alongside the rows instead of blanking the page.
func (s *Service) Search(ctx context.Context, request domain.CatalogSearchRequest) (domain.CatalogSearchPage, error) {
	normalised, err := domain.ValidateCatalogSearchRequest(request)
	if err != nil {
		return domain.CatalogSearchPage{}, err
	}

	available, err := s.availableSources(ctx)
	if err != nil {
		return domain.CatalogSearchPage{}, fmt.Errorf("list catalog sources: %w", err)
	}
	if len(available) == 0 {
		return domain.CatalogSearchPage{}, domain.ErrCatalogNoSources
	}

	selected := selectSources(normalised.Sources, available)
	if len(selected) == 0 {
		// The caller asked for sources nobody offers. That is a routing miss,
		// not "no sources at all", so it names what was asked for.
		return domain.CatalogSearchPage{
			Sources: sourceList(available),
		}, fmt.Errorf("%w: requested %v", domain.ErrCatalogSourceUnavailable, normalised.Sources)
	}

	outcome := s.searchSources(ctx, selected, normalised)
	var ranked []domain.CatalogSearchResult
	if len(selected) == 1 {
		// A single source keeps its own order; only unusable identities are removed.
		if result, ok := outcome.results[selected[0]]; ok {
			ranked = rankSingleSource(selected[0], result)
		}
	} else {
		ranked = rankMultiSource(normalised.Query, selected, outcome.results)
	}
	s.rememberCovers(ranked)
	applyAvailability(available, s.resolver, ranked)

	if normalised.Offset >= domain.MaxCatalogSearchPageSize {
		// The UI stops at the window cap; an out-of-range offset is simply empty.
		return domain.CatalogSearchPage{Sources: sourceList(available)}, nil
	}
	limit := normalised.Limit
	if remaining := domain.MaxCatalogSearchPageSize - normalised.Offset; limit > remaining {
		limit = remaining
	}
	rows, hasMore := pageOf(ranked, normalised.Offset, limit)
	// A page is only the fetched window: the source is the authority on whether
	// rows exist past it. Without this a single source can never page.
	if !hasMore {
		hasMore = sourceHasMore(selected, outcome.results)
	}
	if normalised.Offset+limit >= domain.MaxCatalogSearchPageSize {
		hasMore = false
	}
	// State is resolved for the page the caller will render, not for the whole
	// merged list: a page of 100 identities is one cheap lookup each, and rows
	// past it are never shown.
	s.applyStates(ctx, rows)
	return domain.CatalogSearchPage{
		Rows:     rows,
		HasMore:  hasMore,
		Sources:  sourceList(available),
		Failures: outcome.failures,
	}, errors.Join(outcome.errs...)
}

// sourceHasMore reports whether any selected source answered with rows past
// the window the host fetched.
//
// The merged page can be shorter than the window even though a source has more
// (one source per selected provider is the normal case), so the source's own
// flag is the only reliable "load more" signal. Failed or unselected sources
// simply have no entry here and are skipped.
func sourceHasMore(selected []string, results map[string]domain.PluginSearchResult) bool {
	for _, source := range selected {
		if result, ok := results[source]; ok && result.HasMore {
			return true
		}
	}
	return false
}

// searchOutcome is the gathered answers of one search.
type searchOutcome struct {
	results map[string]domain.PluginSearchResult
	// failures describe, per source, what went wrong. They travel on the page so
	// the UI can render them inline; errs carries the same events as errors for
	// callers that only classify.
	failures []domain.CatalogSearchSourceFailure
	errs     []error
}

// searchSources fans out one call per source and collects what came back.
//
// Each call gets its own timeout: a source that hangs is abandoned without
// cancelling the others, because cancelling the parent context would turn one
// slow source into "the whole search failed".
func (s *Service) searchSources(
	ctx context.Context,
	sourceOrder []string,
	request domain.CatalogSearchRequest,
) searchOutcome {
	if s.searcher == nil {
		return searchOutcome{results: map[string]domain.PluginSearchResult{}, errs: []error{
			fmt.Errorf("%w: no plugin can search catalog sources", domain.ErrCatalogSearchFailed),
		}}
	}

	var (
		mu            sync.Mutex
		results       = make(map[string]domain.PluginSearchResult, len(sourceOrder))
		failures      []domain.CatalogSearchSourceFailure
		failureErrors []error
		wg            sync.WaitGroup
	)

	for _, source := range sourceOrder {
		source := source
		wg.Add(1)
		go func() {
			defer wg.Done()

			sourceCtx, cancel := context.WithTimeout(ctx, s.sourceTimeout)
			defer cancel()

			// The plugin is asked for enough rows to cover the merged page plus
			// the rows the caller already saw: a source that only returns the
			// first page cannot contribute a rank to page 2.
			limit := request.Offset + request.Limit
			if limit > domain.MaxCatalogSearchPageSize {
				limit = domain.MaxCatalogSearchPageSize
			}
			result, err := s.searcher.SearchSource(sourceCtx, source, domain.PluginSearchRequest{
				Source: source,
				Query:  request.Query,
				Limit:  limit,
				Offset: 0,
			})

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				s.logger.Warn("catalog: source failed", "source", source, "error", err)
				// The sentinel is preserved so a caller can classify "a source
				// failed" without inspecting the wording, and the source id is
				// in the message so the UI can say which one is missing.
				failures = append(failures, classifySourceFailure(source, err))
				failureErrors = append(failureErrors, fmt.Errorf("%w: source %q: %v",
					domain.ErrCatalogSearchFailed, source, err))
				return
			}
			results[source] = result
		}()
	}
	wg.Wait()

	return searchOutcome{results: results, failures: failures, errs: failureErrors}
}

// classifySourceFailure turns one source's error into the reason the UI renders.
//
// Only three kinds exist because only three change what the user can do about it:
// wait (timeout), install/enable the source (disabled), or try again later
// (upstream). The wording stays in the frontend.
func classifySourceFailure(source string, err error) domain.CatalogSearchSourceFailure {
	kind := domain.CatalogSearchFailureUpstream
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		kind = domain.CatalogSearchFailureTimeout
	case errors.Is(err, domain.ErrCatalogSourceUnavailable):
		kind = domain.CatalogSearchFailureDisabled
	}
	return domain.CatalogSearchSourceFailure{Provider: source, Kind: kind, Message: err.Error()}
}

// Covers resolves a batch of search-row covers into host-local asset URLs.
//
// A failure is per row and never fails the call: the UI turns every `Error`
// into the placeholder, so one unreachable image host must not blank the others.
func (s *Service) Covers(ctx context.Context, requests []domain.CatalogCoverRequest) []domain.CatalogCoverResult {
	results := make([]domain.CatalogCoverResult, 0, len(requests))
	for _, request := range requests {
		identity := domain.CatalogIdentity{
			Provider:   domain.NormalizeProvider(request.Identity.Provider),
			ExternalID: strings.TrimSpace(request.Identity.ExternalID),
		}
		if identity.Provider == "" || identity.ExternalID == "" {
			results = append(results, domain.CatalogCoverResult{Identity: identity, Error: "no_cover_url"})
			continue
		}

		remote := strings.TrimSpace(request.URL)
		if remote == "" {
			// A caller may ask by identity alone; the last search's URL is still
			// the right artwork for that row.
			remote = s.cachedCover(identity)
		}
		if remote == "" {
			results = append(results, domain.CatalogCoverResult{Identity: identity, Error: "no_cover_url"})
			continue
		}
		if s.covers == nil {
			results = append(results, domain.CatalogCoverResult{Identity: identity, Error: "cache_unavailable"})
			continue
		}

		assetURL, err := s.covers.Get(ctx, remote, s.coverCacheLimitBytes(ctx, identity.Provider))
		if err != nil {
			s.logger.Warn("catalog: cover cache failed",
				"provider", identity.Provider, "external_id", identity.ExternalID, "error", err)
			results = append(results, domain.CatalogCoverResult{Identity: identity, Error: "unavailable"})
			continue
		}
		results = append(results, domain.CatalogCoverResult{Identity: identity, URL: assetURL})
	}
	return results
}

// coverCacheLimitBytes resolves the plugin's `cover_cache_mb` setting.
//
// The value is clamped here, not in the cache: the cache is a storage adapter
// and must not know which plugin declared the limit.
func (s *Service) coverCacheLimitBytes(ctx context.Context, provider string) int64 {
	mb := domain.DefaultCatalogCoverCacheMB
	if s.sourceSettings != nil {
		mb = s.sourceSettings.SourceInt(ctx, provider, domain.CatalogSettingCoverCacheMB, mb)
	}
	if mb < domain.MinCatalogCoverCacheMB {
		mb = domain.MinCatalogCoverCacheMB
	}
	if mb > domain.MaxCatalogCoverCacheMB {
		mb = domain.MaxCatalogCoverCacheMB
	}
	return int64(mb) << 20
}

// Sources returns the source table without running a search, so the search page
// can render selectable chips before the user types anything.
func (s *Service) Sources(ctx context.Context) ([]domain.CatalogSearchSource, error) {
	available, err := s.availableSources(ctx)
	if err != nil {
		return nil, fmt.Errorf("list catalog sources: %w", err)
	}
	return sourceList(available), nil
}

// availableSources returns the source table every running plugin reported.
func (s *Service) availableSources(ctx context.Context) (map[string]domain.CatalogSearchSource, error) {
	if s.sources == nil {
		return nil, nil
	}
	available, err := s.sources.SearchSources(ctx)
	if err != nil {
		return nil, err
	}
	return available, nil
}

// selectSources resolves the caller's filter against what actually exists.
//
// An empty filter means "everything" (the UI's 全部). A filtered request keeps
// only sources that exist and are known, in the declared order, so a source the
// user disabled simply stops appearing instead of erroring the page.
func selectSources(filtered []string, available map[string]domain.CatalogSearchSource) []string {
	selected := make([]string, 0, len(available))
	if len(filtered) == 0 {
		for provider := range available {
			selected = append(selected, provider)
		}
		// The source table is a map; sort it so the fusion tie-break and the
		// rendered button order are stable across processes.
		sort.Strings(selected)
		return selected
	}

	seen := make(map[string]struct{}, len(filtered))
	for _, provider := range filtered {
		normalised := domain.NormalizeProvider(provider)
		if _, duplicate := seen[normalised]; duplicate {
			continue
		}
		if _, known := available[normalised]; !known {
			continue
		}
		seen[normalised] = struct{}{}
		selected = append(selected, normalised)
	}
	return selected
}

// sourceList turns the source map into the stable list the UI renders.
func sourceList(available map[string]domain.CatalogSearchSource) []domain.CatalogSearchSource {
	providers := make([]string, 0, len(available))
	for provider := range available {
		providers = append(providers, provider)
	}
	sort.Strings(providers)

	list := make([]domain.CatalogSearchSource, 0, len(providers))
	for _, provider := range providers {
		source := available[provider]
		if strings.TrimSpace(source.ID) == "" {
			source.ID = provider
		}
		list = append(list, source)
	}
	return list
}

// applyStates asks the library which rows are already registered.
//
// It runs after paging so only rendered rows are looked up. A resolver failure is
// logged and swallowed: the badge is a convenience, and losing it must not turn a
// working search into an error page. A row with no state keeps "" — the
// frontend reads that as "not downloaded yet".
func (s *Service) applyStates(ctx context.Context, rows []domain.CatalogSearchResult) {
	if s.states == nil || len(rows) == 0 {
		return
	}

	identities := make([]domain.CatalogIdentity, 0, len(rows))
	seen := make(map[domain.CatalogIdentity]struct{}, len(rows))
	for _, row := range rows {
		identity := domain.CatalogIdentity{Provider: row.Provider, ExternalID: row.ExternalID}
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}
		identities = append(identities, identity)
	}

	// A partial answer is still applied: one unreadable identity must not hide
	// the badge on the other 49 rows.
	states, err := s.states.CatalogRowStates(ctx, identities)
	if err != nil {
		s.logger.Warn("catalog: cannot resolve every row state", "error", err, "rows", len(rows))
	}
	for index := range rows {
		rows[index].State = states[domain.CatalogIdentity{
			Provider:   rows[index].Provider,
			ExternalID: rows[index].ExternalID,
		}]
	}

	// Rows still unknown to the identity lookup may exist as a file the user
	// imported. One extra call per page is cheaper than showing "download" for a
	// song that is already on disk.
	queries := make([]domain.CatalogLocalQuery, 0, len(rows))
	for _, row := range rows {
		if row.State != "" || row.Duration <= 0 {
			continue
		}
		queries = append(queries, domain.CatalogLocalQuery{
			Identity: domain.CatalogIdentity{Provider: row.Provider, ExternalID: row.ExternalID},
			Title:    row.Title,
			Artists:  row.Artists,
			Duration: row.Duration,
		})
	}
	if len(queries) == 0 {
		return
	}
	matches, err := s.states.CatalogLocalMatches(ctx, queries)
	if err != nil {
		s.logger.Warn("catalog: cannot resolve local candidates", "error", err, "rows", len(queries))
	}
	for index := range rows {
		if rows[index].State != "" {
			continue
		}
		identity := domain.CatalogIdentity{Provider: rows[index].Provider, ExternalID: rows[index].ExternalID}
		if id := matches[identity]; id != "" {
			rows[index].LocalTrackID = id
			rows[index].State = domain.CatalogRowStateInLibrary
		}
	}
}

// rememberCovers keeps any cover the sources did report, keyed by identity.
//
// The UI may hide thumbnails, and the plugin then omits cover_url. The saved
// download should still embed artwork, so the last URL the host saw is kept in
// memory. This is a convenience cache, not persistence: a restart loses it.
func (s *Service) rememberCovers(rows []domain.CatalogSearchResult) {
	s.coverMu.Lock()
	defer s.coverMu.Unlock()
	for _, row := range rows {
		cover := row.CoverURLLarge
		if cover == "" {
			cover = row.CoverURL
		}
		if cover == "" {
			continue
		}
		s.coverCache[domain.CatalogIdentity{Provider: row.Provider, ExternalID: row.ExternalID}] = cover
	}
}

// cachedCover returns the last cover seen for an identity, or "".
func (s *Service) cachedCover(identity domain.CatalogIdentity) string {
	s.coverMu.RLock()
	defer s.coverMu.RUnlock()
	return s.coverCache[identity]
}

// RowStates answers the row-state question for an arbitrary identity list. The
// search page uses it after `download:done`, when the remote row has already
// been merged away and cannot be looked up by track id.
func (s *Service) RowStates(ctx context.Context, identities []domain.CatalogIdentity) (map[domain.CatalogIdentity]string, error) {
	if s.states == nil || len(identities) == 0 {
		return map[domain.CatalogIdentity]string{}, nil
	}
	return s.states.CatalogRowStates(ctx, identities)
}

// applyAvailability stamps Downloadable and its reason onto every row.
//
// It runs after fusion so the reason travels with the row the UI renders, and it
// mutates in place because the rows are already in final order.
//
// The reason is a machine kind, not a sentence: the frontend owns the wording
// (catalog/plugins/README.md, i18n rule). "no_resolver" is the only kind today —
// the source can search but nothing can fetch it — and it is exactly the case
// §6.2 of the task book warns about.
func applyAvailability(
	available map[string]domain.CatalogSearchSource,
	resolver ResolverAvailability,
	rows []domain.CatalogSearchResult,
) {
	const reasonNoResolver = "no_resolver"

	for index := range rows {
		provider := rows[index].Provider
		if _, known := available[provider]; !known {
			rows[index].Downloadable = false
			rows[index].DownloadBlockedReason = reasonNoResolver
			continue
		}
		if resolver == nil || !resolver.CanResolve(provider) {
			rows[index].Downloadable = false
			rows[index].DownloadBlockedReason = reasonNoResolver
			continue
		}
		rows[index].Downloadable = true
		rows[index].DownloadBlockedReason = ""
	}
}
