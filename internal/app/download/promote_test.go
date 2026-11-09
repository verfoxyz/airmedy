package download_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"airmedy/internal/app/download"
	"airmedy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPromoteCachedFile_ReusesTheDownloadChain proves a cached file becomes a
// library track without resolving or downloading anything, and that the remote
// row is merged away. A second call is a no-op that reports the survivor.
func TestPromoteCachedFile_ReusesTheDownloadChain(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	remote := h.seedRemote(t, "remote-1")

	cacheFile := filepath.Join(t.TempDir(), "cached.audio")
	require.NoError(t, os.WriteFile(cacheFile, []byte("cached-audio-bytes"), 0o644))

	h.finalizer.onFinalize = func(in download.FinalizeInput) (download.FinalizeResult, error) {
		target := filepath.Join(in.Root, "晴天.mp3")
		require.NoError(t, os.WriteFile(target, []byte("tagged-audio"), 0o644))
		require.NoError(t, h.tracks.Upsert(ctx, &domain.Track{
			ID:           "local-1",
			Path:         target,
			Title:        in.Track.Title,
			Duration:     in.Track.Duration,
			Availability: domain.AvailabilityReady,
		}))
		return download.FinalizeResult{
			TrackID:         "local-1",
			TargetPath:      target,
			DurationSeconds: 200,
			Bitrate:         320,
		}, nil
	}

	survivor, err := h.svc.PromoteCachedFile(ctx, remote, cacheFile, "320k")
	require.NoError(t, err)
	assert.Equal(t, "local-1", survivor)
	assert.Equal(t, cacheFile, h.finalizer.last.Path, "the cached file must be the finalize input")
	assert.Equal(t, "wy", h.finalizer.last.Track.Provider)

	// The merge moved the identity onto the imported row and removed the remote.
	gone, err := h.tracks.GetByID(ctx, "remote-1")
	require.NoError(t, err)
	assert.Nil(t, gone)
	row, err := h.tracks.GetByID(ctx, "local-1")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "wy", row.Provider)
	assert.Equal(t, "remote-1", row.ExternalID)
	assert.False(t, row.IsRemote())

	// A repeat call must neither finalize again nor create a duplicate row.
	calls := h.finalizer.calls
	again, err := h.svc.PromoteCachedFile(ctx, remote, cacheFile, "320k")
	require.NoError(t, err)
	assert.Equal(t, "local-1", again)
	assert.Equal(t, calls, h.finalizer.calls, "the second call must not re-import")

	var tracks int
	require.NoError(t, h.db.Get(&tracks,
		`SELECT COUNT(*) FROM tracks WHERE provider = 'wy' AND external_id = 'remote-1'`))
	assert.Equal(t, 1, tracks, "promotion must leave exactly one row for the identity")
}

func TestPromoteCachedFile_MissingCacheFileFails(t *testing.T) {
	h := newHarness(t)
	remote := h.seedRemote(t, "remote-2")

	_, err := h.svc.PromoteCachedFile(context.Background(), remote,
		filepath.Join(t.TempDir(), "absent.audio"), "320k")
	require.Error(t, err)
	assert.Equal(t, 0, h.finalizer.count())
}

// seedPromotion prepares a remote row, a cached file and a finalizer that
// imports a distinguishable local row, so the promotion branches can be driven
// end to end.
func seedPromotion(t *testing.T, h *harness, id, cacheQuality string, complete bool) *domain.TrackDTO {
	t.Helper()
	ctx := context.Background()
	remote := h.seedRemote(t, id)

	cacheFile := filepath.Join(t.TempDir(), "cached.audio")
	require.NoError(t, os.WriteFile(cacheFile, []byte("cached:"+id), 0o644))
	h.cached.mu.Lock()
	h.cached.path = cacheFile
	h.cached.quality = cacheQuality
	h.cached.complete = complete
	h.cached.found = true
	h.cached.mu.Unlock()

	h.finalizer.mu.Lock()
	h.finalizer.onFinalize = func(in download.FinalizeInput) (download.FinalizeResult, error) {
		localID := "local-" + in.Track.ID
		target := filepath.Join(in.Root, localID+".mp3")
		require.NoError(t, os.WriteFile(target, []byte("library:"+localID), 0o644))
		require.NoError(t, h.tracks.Upsert(ctx, &domain.Track{
			ID:           localID,
			Path:         target,
			Title:        in.Track.Title,
			Duration:     in.Track.Duration,
			Availability: domain.AvailabilityReady,
		}))
		return download.FinalizeResult{
			TrackID:         localID,
			TargetPath:      target,
			DurationSeconds: 200,
			Bitrate:         320,
		}, nil
	}
	h.finalizer.mu.Unlock()
	return remote
}

func TestPromoteOnSave_UsesTheCacheAtEqualOrHigherQuality(t *testing.T) {
	tests := []struct {
		name         string
		cacheQuality string
		download     string
	}{
		{name: "equal", cacheQuality: domain.Quality320k, download: domain.Quality320k},
		{name: "cache higher", cacheQuality: domain.QualityFlac, download: domain.Quality320k},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.settings.quality = tt.download
			h.rebuild()
			remote := seedPromotion(t, h, "remote-"+tt.name, tt.cacheQuality, true)

			h.svc.PromoteOnSave(context.Background(), remote, "playlist-1")

			waitFor(t, "the cached file to be promoted", func() bool {
				return h.finalizer.count() == 1
			})
			waitFor(t, "the cache entry to be dropped", func() bool {
				_, discarded := h.cached.counts()
				return discarded > 0
			})

			// The merge carried the identity to the imported row.
			row, err := h.tracks.GetByID(context.Background(), "local-remote-"+tt.name)
			require.NoError(t, err)
			require.NotNil(t, row)
			assert.Equal(t, "remote-"+tt.name, row.ExternalID)
		})
	}
}

func TestPromoteOnSave_RedownloadsWhenTheDownloadQualityIsHigher(t *testing.T) {
	tests := []struct {
		name         string
		cacheQuality string
	}{
		{name: "known lower", cacheQuality: domain.Quality320k},
		{name: "unknown", cacheQuality: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.settings.quality = domain.QualityFlac
			h.rebuild()
			require.NoError(t, h.svc.Start(context.Background()))
			t.Cleanup(func() { _ = h.svc.Stop(context.Background()) })
			remote := seedPromotion(t, h, "remote-"+tt.name, tt.cacheQuality, true)

			h.svc.PromoteOnSave(context.Background(), remote, "")

			waitFor(t, "the higher-quality job to finish", func() bool {
				jobs, err := h.jobs.List(context.Background())
				if err != nil {
					return false
				}
				for _, job := range jobs {
					if job.TrackID == remote.ID && job.State == domain.DownloadStateDone {
						return job.Quality == domain.QualityFlac
					}
				}
				return false
			})
			waitFor(t, "the old cache entry to be dropped", func() bool {
				_, discarded := h.cached.counts()
				return discarded > 0
			})
		})
	}
}

func TestPromoteOnSave_WaitsForPlaybackToReleaseTheFile(t *testing.T) {
	h := newHarness(t)
	remote := seedPromotion(t, h, "remote-delayed", domain.Quality320k, true)

	release := make(chan error, 1)
	h.cached.mu.Lock()
	h.cached.ensureWait = release
	h.cached.mu.Unlock()

	h.svc.PromoteOnSave(context.Background(), remote, "")

	// The completion channel has not fired: a playback session still holds it.
	time.Sleep(100 * time.Millisecond)
	if h.finalizer.count() != 0 {
		t.Fatal("promotion ran while the cache was still reported busy")
	}

	release <- nil
	close(release)

	waitFor(t, "the deferred promotion", func() bool { return h.finalizer.count() == 1 })
}
