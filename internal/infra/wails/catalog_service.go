package wails

import (
	"context"
	"errors"
	"fmt"

	"airmedy/internal/app/catalog"
	"airmedy/internal/domain"
)

// CatalogService is the Wails binding for online catalogue search: translation
// only, no logic (see AGENTS.md §3).
//
// The JSON shapes are the frontend contract, so they are explicit DTOs rather
// than domain structs: a domain field rename must not silently break the UI, and
// the page-level failure kind is a machine string the frontend words itself.
type CatalogService struct {
	svc *catalog.Service
}

// NewCatalogService binds catalogue search for the frontend.
func NewCatalogService(svc *catalog.Service) *CatalogService {
	return &CatalogService{svc: svc}
}

// CatalogSearchRequest is one search page request.
type CatalogSearchRequest struct {
	Query string `json:"query"`
	// Sources filters the search to these provider ids. Empty means "全部".
	Sources []string `json:"sources"`
	// Limit is the page size; the service caps it at 50.
	Limit int `json:"limit"`
	// Offset is how many merged rows the caller already has, for "load more".
	Offset int `json:"offset"`
}

// CatalogSourceDTO is one source button.
type CatalogSourceDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CatalogRowDTO is one search result the page renders.
type CatalogRowDTO struct {
	Provider   string   `json:"provider"`
	ExternalID string   `json:"external_id"`
	Title      string   `json:"title"`
	Artists    []string `json:"artists"`
	AlbumName  string   `json:"album_name"`
	Duration   int      `json:"duration"`
	// Sources lists every provider that returned this track. A single-source
	// search has one entry; a cross-source duplicate is one row with several.
	Sources []string `json:"sources"`
	// CoverURL is the thumbnail the source reported, or "" when covers are
	// hidden or the source has none. The page loads it lazily; the host never
	// fetches it during search.
	CoverURL string `json:"cover_url"`
	// CoverURLLarge is the 500×500 (or original) rendition for the file's
	// embedded artwork. The save action prefers it; empty falls back to CoverURL.
	CoverURLLarge string `json:"cover_url_large"`
	// Downloadable is whether the host can fetch this track. When false the
	// action is disabled and BlockedReason says why.
	Downloadable  bool   `json:"downloadable"`
	BlockedReason string `json:"blocked_reason"`
	// State is what the library already knows: "" (not registered), "in_library",
	// "downloading", "failed" or "saved".
	State string `json:"state"`
	// LocalTrackID is the on-disk row that already represents this song, even
	// though it was imported rather than downloaded from this provider. The UI
	// reuses it instead of downloading a duplicate.
	LocalTrackID string `json:"local_track_id"`
}

// CatalogFailureDTO is one source that could not contribute to the page.
type CatalogFailureDTO struct {
	Provider string `json:"provider"`
	// Kind is a machine kind the frontend words: "timeout", "upstream",
	// "source_disabled".
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// CatalogSearchResultDTO is one assembled page.
type CatalogSearchResultDTO struct {
	Rows    []CatalogRowDTO    `json:"rows"`
	HasMore bool               `json:"has_more"`
	Sources []CatalogSourceDTO `json:"sources"`
	// Failures are rendered inline, so a dead source is visible without hiding
	// the rows that worked.
	Failures []CatalogFailureDTO `json:"failures"`
	// ErrorKind is a page-level failure the UI must word itself: "no_sources"
	// (nothing installed), "source_unavailable" (the requested sources are not
	// running), "invalid" (the request was unusable) or "unavailable" (every
	// source failed, see Failures). Empty means the page is usable.
	//
	// It travels in the result rather than as a Go error because Go error text is
	// not translatable, and "no source installed" is a normal state to explain.
	ErrorKind string `json:"error_kind"`
}

// CatalogCoverRequestDTO asks for one search row's artwork.
//
// URL is the source's remote cover. Empty asks the host to use the URL the last
// search remembered for this identity.
type CatalogCoverRequestDTO struct {
	Provider   string `json:"provider"`
	ExternalID string `json:"external_id"`
	URL        string `json:"url"`
}

// CatalogCoverDTO is one resolved cover, or a per-row failure.
//
// URL is a host-local asset path and must be used as the <img> src directly;
// Error is a machine-free short reason the UI turns into the placeholder.
type CatalogCoverDTO struct {
	Provider   string `json:"provider"`
	ExternalID string `json:"external_id"`
	URL        string `json:"url"`
	Error      string `json:"error"`
}

// GetCatalogSources lists the selectable sources without running a search, so
// the search page can show its source chips before the user types.
func (s *CatalogService) GetCatalogSources() ([]CatalogSourceDTO, error) {
	sources, err := s.svc.Sources(context.Background())
	if err != nil {
		return nil, err
	}
	result := make([]CatalogSourceDTO, 0, len(sources))
	for _, source := range sources {
		result = append(result, CatalogSourceDTO{ID: source.ID, Name: source.Name})
	}
	return result, nil
}

// GetCatalogCovers resolves a batch of search-row covers into local asset URLs.
//
// The host downloads and caches the images, so the WebView renders only local
// files and a repeated search of the same keyword costs no network request.
// A per-row failure travels in its own `error` field; the call itself only
// fails when the batch could not be processed at all.
func (s *CatalogService) GetCatalogCovers(requests []CatalogCoverRequestDTO) ([]CatalogCoverDTO, error) {
	queries := make([]domain.CatalogCoverRequest, 0, len(requests))
	for _, request := range requests {
		queries = append(queries, domain.CatalogCoverRequest{
			Identity: domain.CatalogIdentity{
				Provider:   request.Provider,
				ExternalID: request.ExternalID,
			},
			URL: request.URL,
		})
	}

	results := s.svc.Covers(context.Background(), queries)
	answer := make([]CatalogCoverDTO, 0, len(results))
	for _, result := range results {
		answer = append(answer, CatalogCoverDTO{
			Provider:   result.Identity.Provider,
			ExternalID: result.Identity.ExternalID,
			URL:        result.URL,
			Error:      result.Error,
		})
	}
	return answer, nil
}

// CatalogActionRequest is the user acting on one search row: register the track,
// download it, and optionally favorite it or add it to a playlist.
type CatalogActionRequest struct {
	Provider   string   `json:"provider"`
	ExternalID string   `json:"external_id"`
	Title      string   `json:"title"`
	Artists    []string `json:"artists"`
	AlbumName  string   `json:"album_name"`
	Duration   int      `json:"duration"`
	CoverURL   string   `json:"cover_url"`
	// PlaylistID, when set, adds the track to that playlist immediately.
	PlaylistID string `json:"playlist_id"`
	// Favorite flags the track as a favorite.
	Favorite bool `json:"favorite"`
	// LocalTrackID, when set, reuses that library row instead of downloading.
	LocalTrackID string `json:"local_track_id"`
	// ForceDownload downloads even when LocalTrackID is set.
	ForceDownload bool `json:"force_download"`
}

// CatalogActionResultDTO is what the row shows after the action.
type CatalogActionResultDTO struct {
	TrackID string `json:"track_id"`
	State   string `json:"state"`
}

// SearchCatalog answers one page of merged results.
//
// A page-level problem is reported through ErrorKind and not as an error: "no
// source installed" is a state the UI explains, and a Go error string would be
// untranslatable. A returned error therefore means the call itself failed.
func (s *CatalogService) SearchCatalog(request CatalogSearchRequest) (CatalogSearchResultDTO, error) {
	page, err := s.svc.Search(context.Background(), domain.CatalogSearchRequest{
		Query:   request.Query,
		Sources: request.Sources,
		Limit:   request.Limit,
		Offset:  request.Offset,
	})

	result := CatalogSearchResultDTO{
		Rows:     make([]CatalogRowDTO, 0, len(page.Rows)),
		HasMore:  page.HasMore,
		Sources:  make([]CatalogSourceDTO, 0, len(page.Sources)),
		Failures: make([]CatalogFailureDTO, 0, len(page.Failures)),
	}
	for _, row := range page.Rows {
		result.Rows = append(result.Rows, CatalogRowDTO{
			Provider:      row.Provider,
			ExternalID:    row.ExternalID,
			Title:         row.Title,
			Artists:       append([]string{}, row.Artists...),
			AlbumName:     row.AlbumName,
			Duration:      row.Duration,
			Sources:       append([]string{}, row.Sources...),
			CoverURL:      row.CoverURL,
			CoverURLLarge: row.CoverURLLarge,
			Downloadable:  row.Downloadable,
			BlockedReason: row.DownloadBlockedReason,
			State:         row.State,
			LocalTrackID:  row.LocalTrackID,
		})
	}
	for _, source := range page.Sources {
		result.Sources = append(result.Sources, CatalogSourceDTO{ID: source.ID, Name: source.Name})
	}
	for _, failure := range page.Failures {
		result.Failures = append(result.Failures, CatalogFailureDTO{
			Provider: failure.Provider,
			Kind:     failure.Kind,
			Message:  failure.Message,
		})
	}

	kind := catalogErrorKind(err)
	// Rows that did arrive make the page usable: a partial source failure is
	// rendered inline, not as a page-level error.
	if kind == "unavailable" && len(result.Rows) > 0 {
		kind = ""
	}
	if kind != "" {
		result.ErrorKind = kind
		return result, nil
	}
	if err != nil {
		// Not a condition the UI knows how to word: hand the real error back so
		// it is not silently swallowed.
		return result, err
	}
	return result, nil
}

// catalogErrorKind classifies a page-level error for the UI.
//
// It returns "" when the page is usable (partial results are the normal case) or
// when the error is unexpected — an unexpected error is returned to the caller as
// an error by the caller of this helper.
func catalogErrorKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, domain.ErrCatalogNoSources):
		return "no_sources"
	case errors.Is(err, domain.ErrCatalogSourceUnavailable):
		return "source_unavailable"
	case errors.Is(err, domain.ErrCatalogSearchInvalid):
		return "invalid"
	case errors.Is(err, domain.ErrCatalogSearchFailed):
		return "unavailable"
	default:
		return ""
	}
}

// CatalogIdentityDTO is one search row identity.
type CatalogIdentityDTO struct {
	Provider   string `json:"provider"`
	ExternalID string `json:"external_id"`
}

// CatalogRowStateDTO is the state of one identity, as the search row renders it.
type CatalogRowStateDTO struct {
	Provider   string `json:"provider"`
	ExternalID string `json:"external_id"`
	State      string `json:"state"`
}

// GetCatalogRowStates re-reads the library state for a set of identities.
//
// A finished download deletes the remote row it started from, so the UI cannot
// re-derive the badge from the saved track id. Asking by identity still works
// after the merge because the downloaded row carries the identity.
func (s *CatalogService) GetCatalogRowStates(identities []CatalogIdentityDTO) ([]CatalogRowStateDTO, error) {
	queries := make([]domain.CatalogIdentity, 0, len(identities))
	for _, identity := range identities {
		queries = append(queries, domain.CatalogIdentity{
			Provider:   identity.Provider,
			ExternalID: identity.ExternalID,
		})
	}
	states, err := s.svc.RowStates(context.Background(), queries)
	if err != nil {
		return nil, err
	}
	result := make([]CatalogRowStateDTO, 0, len(queries))
	for _, identity := range queries {
		state := states[identity]
		if state == "" {
			continue
		}
		result = append(result, CatalogRowStateDTO{
			Provider:   identity.Provider,
			ExternalID: identity.ExternalID,
			State:      state,
		})
	}
	return result, nil
}

// SaveCatalogResult registers a search row and starts its download.
//
// Both buttons in the UI call this: "收藏" passes Favorite, "加入歌单" passes
// PlaylistID. Neither depends on the "download when adding to a playlist"
// setting — acting on a search result is itself the download intent.
func (s *CatalogService) SaveCatalogResult(request CatalogActionRequest) (CatalogActionResultDTO, error) {
	outcome, err := s.svc.Act(context.Background(), catalog.ActionRequest{
		Provider:      request.Provider,
		ExternalID:    request.ExternalID,
		Title:         request.Title,
		Artists:       request.Artists,
		AlbumName:     request.AlbumName,
		Duration:      request.Duration,
		CoverURL:      request.CoverURL,
		PlaylistID:    request.PlaylistID,
		Favorite:      request.Favorite,
		LocalTrackID:  request.LocalTrackID,
		ForceDownload: request.ForceDownload,
	})
	if err != nil {
		return CatalogActionResultDTO{}, translateCatalogActionError(err)
	}
	return CatalogActionResultDTO{TrackID: outcome.TrackID, State: outcome.State}, nil
}

// translateCatalogActionError turns a domain failure into the message the frontend
// surfaces. The machine sentinels are kept in the text so a bug report is
// searchable, and the wording stays short enough for a toast.
func translateCatalogActionError(err error) error {
	switch {
	case errors.Is(err, domain.ErrCatalogActionInvalid):
		return fmt.Errorf("catalog: the row has no provider or track id")
	default:
		return err
	}
}
