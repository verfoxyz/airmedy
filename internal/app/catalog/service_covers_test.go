package catalog

import (
	"context"
	"errors"
	"testing"

	"airmedy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeCoverCache struct {
	calls []coverCall
	asset string
	err   error
}

type coverCall struct {
	url        string
	provider   string
	externalID string
	limit      int64
}

func (f *fakeCoverCache) Get(_ context.Context, remoteURL, provider, externalID string, limitBytes int64) (string, error) {
	f.calls = append(f.calls, coverCall{url: remoteURL, provider: provider, externalID: externalID, limit: limitBytes})
	if f.err != nil {
		return "", f.err
	}
	return f.asset, nil
}

type fakeKeyedSettings struct {
	values map[string]int
}

func (f fakeKeyedSettings) SourceInt(_ context.Context, _ string, key string, fallback int) int {
	if value, ok := f.values[key]; ok {
		return value
	}
	return fallback
}

func TestCoversPassesURLAndResolvedLimit(t *testing.T) {
	cache := &fakeCoverCache{asset: "/cover-cache/a.jpg"}
	settings := fakeKeyedSettings{values: map[string]int{
		domain.CatalogSettingCoverCacheMB: 256,
	}}
	service := NewService(Deps{Covers: cache, SourceSettings: settings})

	results := service.Covers(context.Background(), []domain.CatalogCoverRequest{{
		Identity: domain.CatalogIdentity{Provider: "CW", ExternalID: " 1 "},
		URL:      "https://cdn.example/a.jpg",
	}})

	require.Len(t, results, 1)
	assert.Equal(t, domain.CatalogIdentity{Provider: "cw", ExternalID: "1"}, results[0].Identity)
	assert.Equal(t, "/cover-cache/a.jpg", results[0].URL)
	assert.Empty(t, results[0].Error)
	require.Len(t, cache.calls, 1)
	assert.Equal(t, "https://cdn.example/a.jpg", cache.calls[0].url)
	assert.Equal(t, int64(256)<<20, cache.calls[0].limit)
}

func TestCoversFallsBackToTheLastSearchURL(t *testing.T) {
	cache := &fakeCoverCache{asset: "/cover-cache/a.jpg"}
	service := NewService(Deps{Covers: cache})
	service.rememberCovers([]domain.CatalogSearchResult{{
		Provider:      "wy",
		ExternalID:    "1",
		CoverURL:      "https://cdn.example/small.jpg",
		CoverURLLarge: "https://cdn.example/large.jpg",
	}})

	results := service.Covers(context.Background(), []domain.CatalogCoverRequest{{
		Identity: domain.CatalogIdentity{Provider: "wy", ExternalID: "1"},
	}})

	require.Len(t, results, 1)
	assert.Equal(t, "/cover-cache/a.jpg", results[0].URL)
	assert.Equal(t, "https://cdn.example/large.jpg", cache.calls[0].url,
		"the large rendition is the one embedded in the downloaded file")
}

func TestCoversReportsPerRowFailures(t *testing.T) {
	service := NewService(Deps{Covers: nil})

	results := service.Covers(context.Background(), []domain.CatalogCoverRequest{
		{Identity: domain.CatalogIdentity{Provider: "", ExternalID: "1"}},
		{Identity: domain.CatalogIdentity{Provider: "wy", ExternalID: "2"}, URL: "https://cdn.example/b.jpg"},
	})

	require.Len(t, results, 2)
	assert.Equal(t, "no_cover_url", results[0].Error)
	assert.Equal(t, "cache_unavailable", results[1].Error)
	assert.Empty(t, results[1].URL)
}

func TestCoversTurnsACacheErrorIntoOneRowError(t *testing.T) {
	cache := &fakeCoverCache{err: errors.New("network down")}
	service := NewService(Deps{Covers: cache})

	results := service.Covers(context.Background(), []domain.CatalogCoverRequest{{
		Identity: domain.CatalogIdentity{Provider: "wy", ExternalID: "1"},
		URL:      "https://cdn.example/a.jpg",
	}})

	require.Len(t, results, 1)
	assert.Equal(t, "unavailable", results[0].Error)
	assert.Empty(t, results[0].URL)
}

func TestCoverCacheLimitClampsTheDeclaredSetting(t *testing.T) {
	service := NewService(Deps{SourceSettings: fakeKeyedSettings{values: map[string]int{
		domain.CatalogSettingCoverCacheMB: 1,
	}}})
	assert.Equal(t, int64(domain.MinCatalogCoverCacheMB)<<20,
		service.coverCacheLimitBytes(context.Background(), "wy"))

	service = NewService(Deps{SourceSettings: fakeKeyedSettings{values: map[string]int{
		domain.CatalogSettingCoverCacheMB: 99999,
	}}})
	assert.Equal(t, int64(domain.MaxCatalogCoverCacheMB)<<20,
		service.coverCacheLimitBytes(context.Background(), "wy"))
}

type fakeCoverAssets struct {
	asset  string
	remote string
	err    error
}

func (f fakeCoverAssets) AssetForIdentity(context.Context, string, string) (string, error) {
	return f.asset, f.err
}

func (f fakeCoverAssets) RemoteForIdentity(context.Context, string, string) (string, error) {
	return f.remote, f.err
}

func (f fakeCoverAssets) BytesForIdentity(context.Context, string, string) ([]byte, string, error) {
	return nil, "", nil
}

func TestResolveTrackCoverUsesTheRowsOwnIdentity(t *testing.T) {
	cache := &fakeCoverCache{asset: "/cover-cache/a.jpg"}
	service := NewService(Deps{
		Covers:      cache,
		CoverAssets: fakeCoverAssets{asset: "/cover-cache/a.jpg"},
		LocalTracks: fakeLocalTracks{track: &domain.TrackDTO{Track: domain.Track{
			ID:           "remote-1",
			Path:         domain.RemotePath("wy", "42"),
			Availability: domain.AvailabilityRemote,
			Provider:     "wy",
			ExternalID:   "42",
		}}},
	})

	url, err := service.ResolveTrackCover(context.Background(), "remote-1")
	require.NoError(t, err)
	assert.Equal(t, "/cover-cache/a.jpg", url)
	assert.Empty(t, cache.calls, "a cached asset must not trigger a download")
}

func TestResolveTrackCoverRedownloadsWhenTheFileWasEvicted(t *testing.T) {
	cache := &fakeCoverCache{asset: "/cover-cache/a.jpg"}
	service := NewService(Deps{
		Covers:      cache,
		CoverAssets: fakeCoverAssets{remote: "https://cdn.example/a.jpg"},
		LocalTracks: fakeLocalTracks{track: &domain.TrackDTO{Track: domain.Track{
			ID:           "remote-1",
			Path:         domain.RemotePath("wy", "42"),
			Availability: domain.AvailabilityRemote,
			Provider:     "wy",
			ExternalID:   "42",
		}}},
	})

	url, err := service.ResolveTrackCover(context.Background(), "remote-1")
	require.NoError(t, err)
	assert.Equal(t, "/cover-cache/a.jpg", url)
	require.Len(t, cache.calls, 1)
	assert.Equal(t, "https://cdn.example/a.jpg", cache.calls[0].url)
}

func TestResolveTrackCoverIgnoresLocalRowsAndUnknownIdentities(t *testing.T) {
	service := NewService(Deps{
		Covers:      &fakeCoverCache{},
		CoverAssets: fakeCoverAssets{},
		LocalTracks: fakeLocalTracks{track: &domain.TrackDTO{Track: domain.Track{
			ID: "local-1", Path: "C:/Music/a.flac", Availability: domain.AvailabilityReady,
		}}},
	})

	url, err := service.ResolveTrackCover(context.Background(), "local-1")
	require.NoError(t, err)
	assert.Empty(t, url, "a local row renders through its artwork_key")
}
