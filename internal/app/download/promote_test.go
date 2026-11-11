package download_test

import (
	"context"
	"errors"
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
	h.cached.pending = append(h.cached.pending, &domain.StreamCacheEntry{
		CacheKey:       remote.ID,
		Provider:       domain.NormalizeProvider(remote.Provider),
		ExternalID:     remote.ExternalID,
		Title:          remote.Title,
		Quality:        cacheQuality,
		PendingPromote: true,
	})
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

			h.svc.PromoteOnSave(context.Background(), remote)
			h.svc.RunCacheMaintenanceSweep(context.Background())

			require.Equal(t, 1, h.finalizer.count(), "the cached file must be promoted")
			_, discarded := h.cached.counts()
			assert.Equal(t, 1, discarded, "the cache entry is deleted after the move")
			assert.Equal(t, []string{remote.ID}, h.cached.promoted)

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

			h.svc.PromoteOnSave(context.Background(), remote)

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
			waitFor(t, "the request to be handed to the download queue", func() bool {
				h.cached.mu.Lock()
				defer h.cached.mu.Unlock()
				return len(h.cached.cleared) == 1 && len(h.cached.pending) == 0
			})
		})
	}
}

func TestPromotionSweep_WaitsForPlaybackToReleaseTheFile(t *testing.T) {
	h := newHarness(t)
	remote := seedPromotion(t, h, "remote-delayed", domain.Quality320k, true)
	h.cached.setActive("wy", remote.ExternalID, true)

	h.svc.PromoteOnSave(context.Background(), remote)
	h.svc.RunCacheMaintenanceSweep(context.Background())

	if h.finalizer.count() != 0 {
		t.Fatal("promotion ran while a session still owned the file")
	}
	h.cached.mu.Lock()
	assert.Len(t, h.cached.pending, 1, "the request must survive the skipped round")
	h.cached.mu.Unlock()

	// The session closes: the next sweep (woken by OnSessionClosed in production)
	// performs the move.
	h.cached.setActive("wy", remote.ExternalID, false)
	h.svc.RunCacheMaintenanceSweep(context.Background())
	waitFor(t, "the deferred promotion", func() bool { return h.finalizer.count() == 1 })
}

func TestPromotionSweep_PurgesAPromotedEntryWithoutCompletingIt(t *testing.T) {
	h := newHarness(t)
	seedPromotion(t, h, "remote-promoted", domain.Quality320k, true)
	// A move happened but the cleanup did not: promoted=1 means "delete only".
	h.cached.mu.Lock()
	h.cached.pending[0].Promoted = true
	h.cached.mu.Unlock()

	h.svc.RunCacheMaintenanceSweep(context.Background())

	_, discarded := h.cached.counts()
	assert.Equal(t, 1, discarded)
	h.cached.mu.Lock()
	assert.Equal(t, 0, h.cached.ensureCalls, "a promoted entry must never be completed again")
	assert.Empty(t, h.cached.pending)
	h.cached.mu.Unlock()
	assert.Equal(t, 0, h.finalizer.count())
}

func TestPromoteOnSave_RecordsTheHistoryTransitions(t *testing.T) {
	h := newHarness(t)
	remote := seedPromotion(t, h, "remote-history", domain.Quality320k, true)

	h.svc.PromoteOnSave(context.Background(), remote)
	// A repeated request while the row is still queued must reuse it.
	h.svc.PromoteOnSave(context.Background(), remote)

	var rows int
	require.NoError(t, h.db.Get(&rows, `SELECT COUNT(*) FROM promotion_tasks`))
	assert.Equal(t, 1, rows, "a repeated request must not add a second active row")

	var state string
	require.NoError(t, h.db.Get(&state, `SELECT state FROM promotion_tasks`))
	assert.Equal(t, domain.PromotionStateQueued, state)
	queuedUpdates := h.sink.taskUpdates()
	assert.Greater(t, queuedUpdates, 0, "the tasks page is told about the request")

	h.svc.RunCacheMaintenanceSweep(context.Background())

	assert.Greater(t, h.sink.taskUpdates(), queuedUpdates,
		"finishing the promotion must emit task:updated for the search page")
	require.NoError(t, h.db.Get(&state, `SELECT state FROM promotion_tasks`))
	assert.Equal(t, domain.PromotionStateDone, state)
	var started, finished bool
	require.NoError(t, h.db.Get(&started, `SELECT started_at IS NOT NULL FROM promotion_tasks`))
	require.NoError(t, h.db.Get(&finished, `SELECT finished_at IS NOT NULL FROM promotion_tasks`))
	assert.True(t, started)
	assert.True(t, finished)
}

func TestPromotionSweep_FailureKeepsTheRequestForARetry(t *testing.T) {
	h := newHarness(t)
	remote := seedPromotion(t, h, "remote-fail", domain.Quality320k, true)
	h.finalizer.mu.Lock()
	h.finalizer.err = errors.New("target directory is read-only")
	h.finalizer.mu.Unlock()

	h.svc.PromoteOnSave(context.Background(), remote)
	h.svc.RunCacheMaintenanceSweep(context.Background())

	var state, lastError string
	require.NoError(t, h.db.Get(&state, `SELECT state FROM promotion_tasks`))
	require.NoError(t, h.db.Get(&lastError, `SELECT error FROM promotion_tasks`))
	assert.Equal(t, domain.PromotionStateFailed, state)
	assert.Contains(t, lastError, "read-only")
	assert.Greater(t, h.sink.taskUpdates(), 0, "a failed attempt must still emit task:updated")

	h.cached.mu.Lock()
	assert.Len(t, h.cached.pending, 1, "the entry stays pending so the next sweep retries")
	h.cached.mu.Unlock()
}

func TestDownloadCompletionFlagsTheCacheWhenPlaybackStillOwnsIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	track := h.seedRemote(t, "remote-downloading")
	// The decoder is still reading the cached copy.
	h.cached.mu.Lock()
	h.cached.discardErr = domain.ErrStreamDiscardDeferred
	h.cached.mu.Unlock()

	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", domain.Quality320k))
	job, err := h.jobs.ClaimNext(ctx, time.Now().Add(24*time.Hour))
	require.NoError(t, err)
	require.NotNil(t, job)
	h.svc.ProcessJobForTest(job)

	h.cached.mu.Lock()
	require.Len(t, h.cached.discards, 1, "a deferred discard must be flagged for the worker")
	assert.False(t, h.cached.discards[0].Promoted, "a normal download must not pretend it promoted")
	h.cached.discardErr = nil
	h.cached.mu.Unlock()

	// Once playback releases the file, the maintenance sweep removes the copy.
	h.svc.RunCacheMaintenanceSweep(ctx)
	h.cached.mu.Lock()
	assert.Empty(t, h.cached.discards)
	h.cached.mu.Unlock()
}

func TestPromotionOnStartSweepsRequestsLeftByAPreviousRun(t *testing.T) {
	h := newHarness(t)
	remote := seedPromotion(t, h, "remote-startup", domain.Quality320k, true)
	// A previous run recorded the request and flagged the entry, then quit.
	require.NoError(t, h.cached.MarkPendingPromote(context.Background(), remote))

	require.NoError(t, h.svc.Start(context.Background()))
	t.Cleanup(func() { _ = h.svc.Stop(context.Background()) })

	waitFor(t, "the pending request to finish", func() bool { return h.finalizer.count() == 1 })
}
