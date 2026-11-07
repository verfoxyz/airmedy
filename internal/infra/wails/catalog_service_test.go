package wails

import (
	"context"
	"encoding/json"
	"testing"

	"airmedy/internal/app/catalog"
	"airmedy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The frontend reads these field names; the JSON shape is the contract. A
// removed field is a silent `undefined` in a JSDoc-typed model, so the Go test is
// the only guard.

func TestCatalogRowDTO_JSONShape(t *testing.T) {
	row := CatalogRowDTO{
		Provider:      "wy",
		ExternalID:    "509781655",
		Title:         "晴天",
		Artists:       []string{"周杰伦"},
		AlbumName:     "叶惠美",
		Duration:      239,
		Downloadable:  false,
		BlockedReason: "no_resolver",
		State:         domain.CatalogRowStateDownloading,
	}

	raw, err := json.Marshal(row)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))

	for _, key := range []string{
		"provider", "external_id", "title", "artists", "album_name",
		"duration", "downloadable", "blocked_reason", "state",
	} {
		assert.Contains(t, decoded, key, "the frontend reads %q", key)
	}
	assert.Equal(t, "wy", decoded["provider"])
	assert.Equal(t, "no_resolver", decoded["blocked_reason"])
	assert.Equal(t, "downloading", decoded["state"])
}

func TestCatalogSearchResultDTO_JSONShape(t *testing.T) {
	result := CatalogSearchResultDTO{
		Rows:      []CatalogRowDTO{{Provider: "wy", ExternalID: "1"}},
		HasMore:   true,
		Sources:   []CatalogSourceDTO{{ID: "wy", Name: "网易云音乐"}},
		Failures:  []CatalogFailureDTO{{Provider: "wy", Kind: "timeout", Message: "search timed out"}},
		ErrorKind: "",
	}

	raw, err := json.Marshal(result)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))

	for _, key := range []string{"rows", "has_more", "sources", "failures", "error_kind"} {
		assert.Contains(t, decoded, key, "the frontend reads %q", key)
	}
	assert.NotContains(t, decoded, "cover_batch", "the batch setting was removed")
	// Empty slices must serialize as [] rather than null: the UI maps over them.
	empty := CatalogSearchResultDTO{Rows: []CatalogRowDTO{}, Sources: []CatalogSourceDTO{}, Failures: []CatalogFailureDTO{}}
	rawEmpty, err := json.Marshal(empty)
	require.NoError(t, err)
	assert.Contains(t, string(rawEmpty), `"rows":[]`)
	assert.Contains(t, string(rawEmpty), `"sources":[]`)
	assert.Contains(t, string(rawEmpty), `"failures":[]`)
}

func TestCatalogActionDTO_JSONShape(t *testing.T) {
	request := CatalogActionRequest{
		Provider:   "wy",
		ExternalID: "1",
		Title:      "晴天",
		Artists:    []string{"周杰伦"},
		AlbumName:  "叶惠美",
		Duration:   239,
		CoverURL:   "https://p1.music.126.net/a.jpg",
		PlaylistID: "pl-1",
		Favorite:   true,
	}
	raw, err := json.Marshal(request)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	for _, key := range []string{
		"provider", "external_id", "title", "artists", "album_name",
		"duration", "cover_url", "playlist_id", "favorite",
	} {
		assert.Contains(t, decoded, key, "the frontend reads %q", key)
	}

	outcome := CatalogActionResultDTO{TrackID: "t1", State: domain.CatalogRowStateDownloading}
	rawOutcome, err := json.Marshal(outcome)
	require.NoError(t, err)
	assert.JSONEq(t, `{"track_id":"t1","state":"downloading"}`, string(rawOutcome))
}

// TestSearchCatalog_NoSourcesIsAnErrorKind: "nothing installed" is a state the
// UI explains, so it travels as a kind rather than an untranslatable Go error.
func TestSearchCatalog_NoSourcesIsAnErrorKind(t *testing.T) {
	service := NewCatalogService(catalog.NewService(catalog.Deps{}))

	result, err := service.SearchCatalog(CatalogSearchRequest{Query: "周杰伦"})
	require.NoError(t, err)
	assert.Equal(t, "no_sources", result.ErrorKind)
	assert.Empty(t, result.Rows)
	assert.NotNil(t, result.Rows, "rows must be an empty slice, not null")
}

func TestSearchCatalog_InvalidRequestIsAnErrorKind(t *testing.T) {
	service := NewCatalogService(catalog.NewService(catalog.Deps{}))

	result, err := service.SearchCatalog(CatalogSearchRequest{Query: "   "})
	require.NoError(t, err)
	assert.Equal(t, "invalid", result.ErrorKind)
}

func TestSaveCatalogResult_IncompleteIdentityFails(t *testing.T) {
	service := NewCatalogService(catalog.NewService(catalog.Deps{}))

	_, err := service.SaveCatalogResult(CatalogActionRequest{Provider: "wy", ExternalID: "  "})
	require.Error(t, err, "an action without an identity must not silently no-op")
}

func TestCatalogCoverDTO_JSONShape(t *testing.T) {
	request := CatalogCoverRequestDTO{
		Provider:   "wy",
		ExternalID: "1",
		URL:        "https://cdn.example/a.jpg",
	}
	raw, err := json.Marshal(request)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	for _, key := range []string{"provider", "external_id", "url"} {
		assert.Contains(t, decoded, key, "the frontend reads %q", key)
	}

	answer := CatalogCoverDTO{Provider: "wy", ExternalID: "1", URL: "/cover-cache/a.jpg"}
	rawAnswer, err := json.Marshal(answer)
	require.NoError(t, err)
	assert.JSONEq(t, `{"provider":"wy","external_id":"1","url":"/cover-cache/a.jpg","error":""}`, string(rawAnswer))
}

// TestGetCatalogCoversReturnsPerRowFailures: a batch never fails wholesale;
// each unusable row carries its own error and the rest still answer.
func TestGetCatalogCoversReturnsPerRowFailures(t *testing.T) {
	service := NewCatalogService(catalog.NewService(catalog.Deps{}))

	answer, err := service.GetCatalogCovers([]CatalogCoverRequestDTO{
		{Provider: "wy", ExternalID: "1", URL: "https://cdn.example/a.jpg"},
		{Provider: "wy", ExternalID: "2"},
	})
	require.NoError(t, err)
	require.Len(t, answer, 2)
	assert.Equal(t, "cache_unavailable", answer[0].Error)
	assert.Equal(t, "no_cover_url", answer[1].Error)
	assert.Empty(t, answer[0].URL)
}

// TestCatalogErrorKind covers the mapping directly, including the two values the
// UI branches on most.
func TestCatalogErrorKind(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{domain.ErrCatalogNoSources, "no_sources"},
		{domain.ErrCatalogSourceUnavailable, "source_unavailable"},
		{domain.ErrCatalogSearchInvalid, "invalid"},
		{domain.ErrCatalogSearchFailed, "unavailable"},
		{context.Canceled, ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, catalogErrorKind(c.err), "for %v", c.err)
	}
}
