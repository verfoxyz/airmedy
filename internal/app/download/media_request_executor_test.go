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

func TestExecutePromotesACompleteCacheThatMeetsTheWantedQuality(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	track := h.seedRemote(t, "execute-promote")
	cacheFile := filepath.Join(t.TempDir(), "cached.audio")
	require.NoError(t, os.WriteFile(cacheFile, []byte("cached-audio"), 0o644))
	h.cached.path = cacheFile
	h.cached.found = true
	h.cached.complete = true
	h.cached.quality = domain.QualityFlac
	h.finalizer.onFinalize = func(in download.FinalizeInput) (download.FinalizeResult, error) {
		target := filepath.Join(in.Root, "local-execute.mp3")
		require.NoError(t, h.tracks.Upsert(ctx, &domain.Track{
			ID: "local-execute", Path: target, Title: in.Track.Title,
			Availability: domain.AvailabilityReady,
		}))
		return download.FinalizeResult{TrackID: "local-execute", TargetPath: target}, nil
	}

	phase, err := h.svc.Execute(ctx, track, domain.MediaRequestDesiredPromote, domain.Quality320k)
	require.NoError(t, err)
	assert.Equal(t, domain.MediaRequestPhasePromoting, phase)
	assert.Equal(t, 1, h.finalizer.count(), "the cached file must be promoted in place")
	assert.Equal(t, 1, h.cached.counts(), "the cache is discarded after the move")

	jobs, err := h.svc.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, jobs, "a promotion must not queue a download")
}

func TestExecuteCompletesAPartialCacheInsteadOfDownloadingASecondCopy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	track := h.seedRemote(t, "execute-partial")
	h.cached.found = true
	h.cached.complete = false
	h.cached.quality = domain.Quality320k
	h.cached.playingSession = true

	phase, err := h.svc.Execute(ctx, track, domain.MediaRequestDesiredPromote, domain.Quality320k)
	require.ErrorIs(t, err, domain.ErrMediaRequestDeferred)
	assert.Equal(t, domain.MediaRequestPhaseDownloading, phase)
	assert.Equal(t, 1, h.cached.ensureCalls, "the partial cache must be completed, not duplicated")

	jobs, err := h.svc.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, jobs, "completing the cache must not queue a download")
}

func TestExecuteDefersWhilePlaybackOwnsTheCacheFile(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	track := h.seedRemote(t, "execute-deferred")
	h.cached.found = true
	h.cached.complete = true
	h.cached.quality = domain.QualityFlac
	h.cached.setActive("wy", track.ExternalID, true)

	phase, err := h.svc.Execute(ctx, track, domain.MediaRequestDesiredPromote, domain.Quality320k)
	require.ErrorIs(t, err, domain.ErrMediaRequestDeferred)
	assert.Equal(t, domain.MediaRequestPhasePromoting, phase)
	assert.Zero(t, h.finalizer.count(), "playback still owns the file")
	assert.Zero(t, h.cached.counts(), "its cache must not be deleted either")
}

func TestExecuteDownloadsWhenTheCacheIsBelowTheWantedQuality(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	track := h.seedRemote(t, "execute-low-quality")
	h.cached.found = true
	h.cached.complete = true
	h.cached.quality = domain.Quality128k

	phase, err := h.svc.Execute(ctx, track, domain.MediaRequestDesiredPromote, domain.QualityFlac)
	require.NoError(t, err)
	assert.Equal(t, domain.MediaRequestPhaseDownloading, phase)
	assert.Zero(t, h.finalizer.count(), "a lower-tier cache must not be promoted")

	jobs, err := h.svc.List(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, domain.DownloadStateQueued, jobs[0].State)
}

func TestExecuteDownloadsWithoutACache(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	track := h.seedRemote(t, "execute-no-cache")

	phase, err := h.svc.Execute(ctx, track, domain.MediaRequestDesiredPromote, domain.Quality320k)
	require.NoError(t, err)
	assert.Equal(t, domain.MediaRequestPhaseDownloading, phase)

	jobs, err := h.svc.List(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
}

func TestExecuteKeepsAnActiveJobInsteadOfStartingASecond(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	track := h.seedRemote(t, "execute-active")
	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", domain.Quality320k))

	// A complete flac cache would normally promote, but the job already owns the
	// work: starting a promotion beside it would duplicate or get refused.
	h.cached.found = true
	h.cached.complete = true
	h.cached.quality = domain.QualityFlac

	phase, err := h.svc.Execute(ctx, track, domain.MediaRequestDesiredPromote, domain.QualityFlac)
	require.NoError(t, err)
	assert.Equal(t, domain.MediaRequestPhaseDownloading, phase)
	assert.Zero(t, h.finalizer.count())

	jobs, err := h.svc.List(ctx)
	require.NoError(t, err)
	assert.Len(t, jobs, 1, "a second job must not be created")
}

func TestStatusReportsTheNewestDelegatedFailure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	track := h.seedRemote(t, "status-failure")
	// Start the request a minute ago so the freshly created job is newer even
	// when the driver stores timestamps at a coarser precision.
	since := time.Now().Add(-time.Minute)

	require.NoError(t, h.jobs.Create(ctx, &domain.DownloadJob{
		ID: "job-status-failed", TrackID: track.ID, State: domain.DownloadStateFailed,
		LastError: "403 forbidden",
	}))

	failure, err := h.svc.Status(ctx, track, since)
	require.NoError(t, err)
	assert.Equal(t, "403 forbidden", failure)

	// A row older than the request must not fail a fresh retry.
	older, err := h.svc.Status(ctx, track, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, older)
}

func TestStatusIgnoresRunningWork(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	track := h.seedRemote(t, "status-running")
	since := time.Now().Add(-time.Minute)

	require.NoError(t, h.jobs.Create(ctx, &domain.DownloadJob{
		ID: "job-status-active", TrackID: track.ID, State: domain.DownloadStateQueued,
	}))

	failure, err := h.svc.Status(ctx, track, since)
	require.NoError(t, err)
	assert.Empty(t, failure)
}
