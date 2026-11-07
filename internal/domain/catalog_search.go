package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// MethodSearch is the plugin method that searches an online catalogue.
//
// It is the second capability wired into a host feature after stream.resolve, and
// the first one whose *results* the host merges: a search asks several plugins and
// the host assembles one ranked page (see app/catalog).
const MethodSearch = "catalog.search"

// MaxCatalogSearchPageSize bounds the merged window one search may request.
//
// The host asks each source for offset+limit rows and merges them, then slices
// out one page. The UI starts at 5 and adds 10 per "load more", so this is the
// furthest window a page request can reach, not the page size itself. 100 is the
// compromise for the KuGou source, whose upstream returns at most 30 rows per
// request (a 100-row window is four upstream calls, merges into one answer).
const MaxCatalogSearchPageSize = 100

// MaxCatalogSearchSources bounds how many sources one plugin may report. A source
// is one platform a plugin can search; the UI renders one button per source, and
// an unbounded list would push the real sources off the row.
const MaxCatalogSearchSources = 32

// MaxCatalogQueryLength bounds the keyword the host forwards. Search keywords are
// short; a bound keeps a pasted document from becoming a request body.
const MaxCatalogQueryLength = 200

// CatalogSettingIncludeCover is the well-known switch key a search plugin may
// declare through describe. The host stores its value in the plugin's KV and
// sends it as the `include_cover` search parameter.
const CatalogSettingIncludeCover = "cover_in_results"

// CatalogSettingCoverCacheMB is the well-known number key a search plugin may
// declare. It caps the persisted remote-cover cache in megabytes.
const CatalogSettingCoverCacheMB = "cover_cache_mb"

// DefaultCatalogCoverCacheMB is the cache cap when a plugin does not declare one.
const DefaultCatalogCoverCacheMB = 1024

// MinCatalogCoverCacheMB / MaxCatalogCoverCacheMB bound the setting.
const (
	MinCatalogCoverCacheMB = 128
	MaxCatalogCoverCacheMB = 8192
)

// CatalogSearchErrors. A plugin reports a failure kind in the JSON-RPC error's
// `data` object, never by wording its message, so retry policy is not hostage to
// free text — the same rule stream.resolve follows.
const (
	// CatalogSearchErrorRateLimited means the platform asked the plugin to slow
	// down. The host retries with backoff.
	CatalogSearchErrorRateLimited = "rate_limited"
	// CatalogSearchErrorUnavailable means the source cannot answer right now
	// (upstream error, withdrawn endpoint). Retrying may help later, but the
	// host does not loop on it within one search.
	CatalogSearchErrorUnavailable = "unavailable"
	// CatalogSearchErrorAuth means the source needs credentials the plugin does
	// not have. Retrying cannot help.
	CatalogSearchErrorAuth = "auth"
)

// CatalogSearchSource is one searchable source a plugin reports.
//
// Sources are declared by plugins, never hard-coded by the host: the search
// screen's buttons come from this list, so "add a source" is a plugin change
// (catalog/plugins/README.md, "Everything is a plugin").
type CatalogSearchSource struct {
	// ID is the provider id this source searches ("wy", "kg", ...). It is the
	// value the UI sends back as a filter, and the value the host routes by.
	ID string `json:"id"`
	// Name is the label the UI shows. Empty falls back to ID.
	Name string `json:"name,omitempty"`
}

// PluginSearchRequest is the params object of catalog.search.
//
// The plugin receives exactly one source: the host resolves "全部" into one
// per-source call rather than asking a plugin to fan out internally. That keeps
// each plugin's timeout honest and makes the merged ranking a host concern.
type PluginSearchRequest struct {
	// Source is the provider id to search. Required: a plugin that serves
	// several sources is not asked which one is meant by an empty value.
	Source string `json:"source"`
	// Query is the keyword, already trimmed and length-checked by the host.
	Query string `json:"query"`
	// Limit is how many rows the plugin should return at most.
	Limit int `json:"limit"`
	// Offset is the plugin-side offset, so a later page does not refetch the
	// first one. It is advisory: the host also caps the returned row count.
	Offset int `json:"offset"`
	// IncludeCover asks the plugin to include cover URLs. The host sends the
	// plugin's own "show covers" setting; when false the rows carry no cover and
	// the UI renders a placeholder.
	IncludeCover bool `json:"include_cover"`
	// ForceCover overrides the switch for a save-time lookup: the user hides
	// thumbnails but the downloaded file should still embed artwork, so the host
	// may ask the plugin once more with covers included. It is not sent on the
	// wire.
	ForceCover bool `json:"-"`
}

// PluginSearchResultRow is one search hit as a plugin reports it.
//
// The host turns this into a CatalogSearchResult with a rank; a plugin never
// reports a rank itself, because ranks from different platforms are not
// comparable (see app/catalog's rank fusion).
type PluginSearchResultRow struct {
	// ExternalID is the source's own track id. It is the identity the download
	// path uses, so it must be stable and non-empty.
	ExternalID string   `json:"externalId"`
	Title      string   `json:"title"`
	Artists    []string `json:"artists,omitempty"`
	AlbumName  string   `json:"album,omitempty"`
	// Duration is seconds; 0 when the source does not say.
	Duration int `json:"duration,omitempty"`
	// CoverURL is a thumbnail URL the source provides. Empty when the source has
	// no artwork or the host asked not to include it. It is only handed to the
	// browser; the host never fetches it during search.
	CoverURL string `json:"cover_url,omitempty"`
	// CoverURLLarge is the higher-resolution rendition used for the file's
	// embedded artwork and the library cover. Sources that do not distinguish
	// sizes leave it empty; the action then falls back to CoverURL.
	CoverURLLarge string `json:"cover_url_large,omitempty"`
	// Fee is the source's own "is this paid/VIP" hint when it has one. The
	// host maps it onto Downloadable so a VIP-only row can be disabled with a
	// reason instead of failing after the user acts.
	VIP bool `json:"vip,omitempty"`
}

// PluginSearchResult is the result object of catalog.search.
type PluginSearchResult struct {
	// Rows are the hits in this source's own relevance order. The order is the
	// only ranking input the host uses: it feeds rank fusion, so a plugin does
	// not need to (and must not) calibrate a score.
	Rows []PluginSearchResultRow `json:"rows"`
	// HasMore reports whether the source has more rows past this page.
	HasMore bool `json:"hasMore,omitempty"`
}

// CatalogSearchRequest is what the app service accepts from the UI.
type CatalogSearchRequest struct {
	Query string
	// Sources filters the search to these provider ids. Empty means "every
	// source any plugin reports", which is what the UI's "全部" does.
	Sources []string
	// Limit is the page size, capped at MaxCatalogSearchPageSize.
	Limit int
	// Offset is how many merged rows the caller has already seen, so "load
	// more" continues instead of restarting.
	Offset int
}

// CatalogSearchResult is one merged, ranked, de-duplicated row.
//
// Provider + ExternalID is the identity: it is what a download registers, and two
// rows with the same identity are the same track (from the same source) even if
// the source returned it twice.
type CatalogSearchResult struct {
	Provider   string
	ExternalID string
	Title      string
	Artists    []string
	AlbumName  string
	Duration   int
	// Sources lists the provider ids this row represents. A single-source search
	// keeps one entry; a multi-source merge appends every source that returned the
	// same title+artists, so the UI can show one row with several source labels.
	Sources []string
	// CoverURL is the thumbnail the source reported, or "" when it was not
	// requested or unavailable. The save action forwards it so the downloaded
	// file can embed the artwork.
	CoverURL string
	// CoverURLLarge is the high-resolution rendition for the file's embedded
	// artwork, or "" when the source does not distinguish sizes. The save action
	// prefers it over CoverURL.
	CoverURLLarge string
	// Rank is the fused score the host computed; higher sorts first. It exists
	// so ordering is explainable and testable, not because the UI shows it.
	Rank float64
	// Downloadable reports whether the host can actually fetch this track. A
	// source that can search but has no resolver would otherwise produce a row
	// whose action fails after the click (catalog/plugins/README.md, "可搜 ≠ 可下").
	Downloadable bool
	// DownloadBlockedReason explains Downloadable=false. It is a machine kind,
	// not a sentence: the frontend owns the wording.
	DownloadBlockedReason string
	// State is what the library already knows about this identity: "" (never
	// registered), CatalogRowStateInLibrary, CatalogRowStateDownloading,
	// CatalogRowStateFailed or CatalogRowStateSaved. It is orthogonal to
	// Downloadable — a finished row is still "downloadable" in the sense that the
	// source can fetch it, but the UI must not offer a second download.
	State string
	// LocalTrackID is a library row that already represents this song even though
	// it never came from this provider (an imported file). Non-empty means the
	// row is in the library through that row; actions reuse it instead of
	// registering a new remote identity.
	LocalTrackID string
}

// CatalogSearchPage is one assembled page.
type CatalogSearchPage struct {
	Rows []CatalogSearchResult
	// HasMore reports whether more merged rows exist past this page. It is
	// computed from what the sources said, not guessed from the page being full.
	HasMore bool
	// Sources is every source that was available to search, so the UI can render
	// buttons from the same answer that produced the rows.
	Sources []CatalogSearchSource
	// Failures are the selected sources that could not contribute to this page.
	//
	// They travel on the page rather than only in the returned error because the
	// UI renders them inline ("wy timed out") while still showing the rows that
	// did arrive. The error is still returned for callers that classify.
	Failures []CatalogSearchSourceFailure
}

// Catalog row states. A row the user has already registered is not plain
// "downloadable" any more: the UI must show that it is in the library, already
// being fetched, or waiting after a failure, or the same track would be offered
// for download twice.
const (
	// CatalogRowStateInLibrary means the audio is on disk and playable.
	CatalogRowStateInLibrary = "in_library"
	// CatalogRowStateDownloading means a queued or active download job exists.
	CatalogRowStateDownloading = "downloading"
	// CatalogRowStateFailed means the last download attempt failed and no job is
	// active. The row offers a retry instead of a fresh enqueue.
	CatalogRowStateFailed = "failed"
	// CatalogRowStateSaved means the track is registered in the library but no
	// audio download was started: the user asked for the metadata only (a
	// playlist add while "download when adding to a playlist" is off).
	CatalogRowStateSaved = "saved"
)

// CatalogIdentity is one track's identity across the search and library sides.
//
// It is a comparable struct because it doubles as a map key when a page's states
// are resolved in one call: the provider id is already normalised, and the
// external id is the source's own stable id.
type CatalogIdentity struct {
	Provider   string
	ExternalID string
}

// CatalogCoverRequest asks the host to resolve one search row's artwork.
//
// The URL is the source's remote cover (preferring the large rendition). When
// it is empty the resolver falls back to the last URL the search saw for that
// identity, so a caller can ask by identity alone.
type CatalogCoverRequest struct {
	Identity CatalogIdentity
	URL      string
}

// CatalogCoverResult is one resolved cover.
//
// URL is a host-local asset path (/cover-cache/...), never the remote URL: the
// WebView renders only files the host has already downloaded and indexed.
// Error is one of "no_cover_url", "unavailable" or "cache_unavailable"; it is
// a machine-free short reason because the UI treats every failure as "show the
// placeholder".
type CatalogCoverResult struct {
	Identity CatalogIdentity
	URL      string
	Error    string
}

// CatalogCoverCache persists remote covers and answers with a local asset URL.
//
// The cache lives in infra; app/catalog reaches it through this port so it
// never imports the storage implementation. `limitBytes` is resolved per call
// from the plugin's `cover_cache_mb` setting, so a settings change applies to
// the next request without reconstructing the cache.
type CatalogCoverCache interface {
	Get(ctx context.Context, remoteURL string, limitBytes int64) (assetURL string, err error)
}

// CatalogLocalQuery asks whether an on-disk track already represents a search
// row. Matching is deliberately strict (same normalised title, same primary
// artist, duration within ±3s) so a live version or a cover is not mistaken for
// the studio track.
type CatalogLocalQuery struct {
	Identity CatalogIdentity
	Title    string
	Artists  []string
	// Duration is seconds; 0 means the source did not know it. A query without a
	// duration cannot be matched safely and is skipped.
	Duration int
}

// CatalogRowStateResolver reports what the library already knows about search
// results.
//
// It is a port rather than a direct repository call for the same reason the
// search ports are: the state depends on both the track row and the download
// queue, and app/catalog must not import app/download. A nil resolver is legal —
// then every row reports no state and the UI treats it as not registered.
type CatalogRowStateResolver interface {
	// CatalogRowStates maps each identity to a CatalogRowState constant, or
	// omits it when the track was never registered. An error is reported but
	// never fails the search: losing the badge must not blank the page.
	CatalogRowStates(ctx context.Context, identities []CatalogIdentity) (map[CatalogIdentity]string, error)
	// CatalogLocalMatches maps identities to an existing on-disk track id when
	// the library already holds the same song outside this provider. The search
	// page uses it to avoid a duplicate download for an imported file.
	CatalogLocalMatches(ctx context.Context, queries []CatalogLocalQuery) (map[CatalogIdentity]string, error)
}

// CatalogSearchFailureKind names why one source could not be searched.
const (
	// CatalogSearchFailureDisabled means no running plugin reported that source.
	CatalogSearchFailureDisabled = "source_disabled"
	// CatalogSearchFailureTimeout means the plugin did not answer in time.
	CatalogSearchFailureTimeout = "timeout"
	// CatalogSearchFailureUpstream means the plugin reported an upstream failure.
	CatalogSearchFailureUpstream = "upstream"
)

// CatalogSearchSourceFailure is one source that could not contribute.
//
// Partial results are the point: one dead source must not blank the page. The
// failures travel alongside the rows so the UI can say which source is missing
// instead of silently showing fewer results.
type CatalogSearchSourceFailure struct {
	Provider string
	Kind     string
	Message  string
}

// CatalogSearchSourceRegistry reports which sources a plugin can search.
//
// It is a port rather than a direct registry call so app/catalog stays free of
// app/plugin (the same shape app/stream's ProviderRegistry uses).
type CatalogSearchSourceRegistry interface {
	// SearchSources returns the sources every running plugin reports, keyed by
	// normalised provider id, in a stable order.
	SearchSources(ctx context.Context) (map[string]CatalogSearchSource, error)
}

// CatalogSourceSettingReader reads one source plugin's stored setting. The
// catalog package must apply a plugin-declared number (cover_cache_mb) without
// importing the plugin registry, so the lookup goes through this narrow port.
type CatalogSourceSettingReader interface {
	// SourceInt returns the stored integer for a setting the plugin declared,
	// falling back when no plugin owns the source or the value is unreadable.
	SourceInt(ctx context.Context, source, key string, fallback int) int
}

// CatalogSearcher turns one (source, query, page) request into plugin rows.
//
// It is separated from CatalogSearchService so the ranking/paging logic can be
// tested without plugins, and so the plugin transport is one implementation.
type CatalogSearcher interface {
	// SearchSource asks the single plugin that owns source. A source no plugin
	// owns returns ErrCatalogSourceUnavailable.
	SearchSource(ctx context.Context, source string, request PluginSearchRequest) (PluginSearchResult, error)
}

// Catalog search errors. Callers classify with errors.Is; the wording is added
// at the binding boundary.
var (
	// ErrCatalogSourceUnavailable means no running plugin reported the source.
	ErrCatalogSourceUnavailable = errors.New("catalog source unavailable")
	// ErrCatalogNoSources means no plugin reported any searchable source at all,
	// which is different from "the query found nothing": the UI explains how to
	// install one instead of showing an empty list.
	ErrCatalogNoSources = errors.New("no catalog source is available")
	// ErrCatalogSearchInvalid means the request itself was unusable (blank
	// query, unknown paging values).
	ErrCatalogSearchInvalid = errors.New("catalog search request invalid")
	// ErrCatalogSearchFailed means a plugin call failed for a reason the caller
	// must surface rather than retry silently.
	ErrCatalogSearchFailed = errors.New("catalog search failed")
	// ErrCatalogActionInvalid means the action request itself was unusable
	// (unknown provider, blank external id). Registering such a track would
	// create a row nothing can ever resolve.
	ErrCatalogActionInvalid = errors.New("catalog action invalid")
)

// ValidateCatalogSearchRequest normalises and checks a search request.
//
// Normalisation lives here rather than in the caller so the page-size cap and the
// query trim cannot drift between the binding and the service.
func ValidateCatalogSearchRequest(request CatalogSearchRequest) (CatalogSearchRequest, error) {
	request.Query = strings.TrimSpace(request.Query)
	if request.Query == "" {
		return request, fmt.Errorf("%w: query is empty", ErrCatalogSearchInvalid)
	}
	if len(request.Query) > MaxCatalogQueryLength {
		request.Query = request.Query[:MaxCatalogQueryLength]
	}
	if request.Offset < 0 {
		return request, fmt.Errorf("%w: offset %d is negative", ErrCatalogSearchInvalid, request.Offset)
	}
	if request.Limit <= 0 {
		request.Limit = MaxCatalogSearchPageSize
	}
	if request.Limit > MaxCatalogSearchPageSize {
		request.Limit = MaxCatalogSearchPageSize
	}

	sources := make([]string, 0, len(request.Sources))
	seen := make(map[string]struct{}, len(request.Sources))
	for _, source := range request.Sources {
		normalised := NormalizeProvider(source)
		if normalised == "" {
			continue
		}
		if _, duplicate := seen[normalised]; duplicate {
			continue
		}
		seen[normalised] = struct{}{}
		sources = append(sources, normalised)
	}
	request.Sources = sources
	return request, nil
}
