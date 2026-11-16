package download_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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

func TestPromoteCachedFile_EmbedsTheCachedCover(t *testing.T) {
	h := newHarness(t)
	h.settings.withCover = true
	h.rebuild()
	h.coverAssets.data = []byte("cached-jpeg")
	h.coverAssets.mime = "image/jpeg"
	h.coverAssets.remote = "https://cdn.example/cover.jpg"

	ctx := context.Background()
	remote := h.seedRemote(t, "remote-cover")
	cacheFile := filepath.Join(t.TempDir(), "cached.audio")
	require.NoError(t, os.WriteFile(cacheFile, []byte("cached-audio"), 0o644))

	_, err := h.svc.PromoteCachedFile(ctx, remote, cacheFile, "320k")
	require.NoError(t, err)
	assert.Equal(t, []byte("cached-jpeg"), h.finalizer.last.CoverData,
		"the cached image is embedded without another download")
	assert.Equal(t, "image/jpeg", h.finalizer.last.CoverMIME)
	assert.Equal(t, "https://cdn.example/cover.jpg", h.finalizer.last.CoverURL)
}

func TestPromoteCachedFile_MissingCacheFileFails(t *testing.T) {
	h := newHarness(t)
	remote := h.seedRemote(t, "remote-2")

	_, err := h.svc.PromoteCachedFile(context.Background(), remote,
		filepath.Join(t.TempDir(), "absent.audio"), "320k")
	require.Error(t, err)
	assert.Equal(t, 0, h.finalizer.count())
}
