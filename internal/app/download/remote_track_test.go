package download_test

import (
	"context"
	"testing"

	"airmedy/internal/app/download"
	"airmedy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func remoteSpec(externalID string) domain.RemoteTrackSpec {
	return domain.RemoteTrackSpec{
		Provider:   "WY ",
		ExternalID: externalID,
		Title:      "晴天",
		Artists:    []string{"周杰伦", "  "},
		AlbumName:  "叶惠美",
		Duration:   269,
		CoverURL:   "http://cdn.example/cover.jpg",
	}
}

// TestEnsureRemoteTrack_CreatesAVirtualRow covers the missing entry point: the
// download queue needs a tracks row, and an online track has none until one is
// registered with a virtual path.
func TestEnsureRemoteTrack_CreatesAVirtualRow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	track, err := h.svc.EnsureRemoteTrack(ctx, remoteSpec("1001"))
	require.NoError(t, err)

	assert.Equal(t, domain.RemotePath("wy", "1001"), track.Path, "path must be the canonical virtual path")
	assert.Equal(t, domain.AvailabilityRemote, track.Availability)
	assert.Equal(t, "wy", track.Provider, "provider must be normalised")
	assert.Equal(t, "1001", track.ExternalID)
	assert.Equal(t, "晴天", track.Title)
	assert.Equal(t, "周杰伦", track.RawArtistNames, "blank artist names must be dropped")
	assert.Equal(t, "叶惠美", track.RawAlbumName, "the search album must reach the row so finalize can tag it")
	assert.Equal(t, 269, track.Duration)
	assert.True(t, track.IsRemote())
}

// TestEnsureRemoteTrack_BackfillsTheAlbumOnARegisteredRow: rows created before
// the album name was persisted carry no value, and a later search for the same
// song must be able to fill it before the download finishes.
func TestEnsureRemoteTrack_BackfillsTheAlbumOnARegisteredRow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	spec := remoteSpec("3003")
	spec.AlbumName = ""
	first, err := h.svc.EnsureRemoteTrack(ctx, spec)
	require.NoError(t, err)
	assert.Empty(t, first.RawAlbumName)

	again, err := h.svc.EnsureRemoteTrack(ctx, remoteSpec("3003"))
	require.NoError(t, err)
	assert.Equal(t, "叶惠美", again.RawAlbumName)
}

func TestEnsureRemoteTrack_IsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	first, err := h.svc.EnsureRemoteTrack(ctx, remoteSpec("2002"))
	require.NoError(t, err)

	// A different casing/whitespace of the same identity must resolve to the
	// same row instead of tripping the unique index.
	again, err := h.svc.EnsureRemoteTrack(ctx, remoteSpec(" 2002 "))
	require.NoError(t, err)

	assert.Equal(t, first.ID, again.ID, "same identity must return the same row")

	var count int
	require.NoError(t, h.db.GetContext(ctx, &count, `SELECT COUNT(*) FROM tracks WHERE provider = ? AND external_id = ?`, "wy", "2002"))
	assert.Equal(t, 1, count, "exactly one row per identity")
}

func TestEnsureRemoteTrack_RejectsIncompleteIdentity(t *testing.T) {
	h := newHarness(t)

	_, err := h.svc.EnsureRemoteTrack(context.Background(), domain.RemoteTrackSpec{Title: "no identity"})
	require.Error(t, err)

	_, err = h.svc.EnsureRemoteTrack(context.Background(), domain.RemoteTrackSpec{Provider: "wy"})
	require.Error(t, err)
}

func TestEnsureRemoteTrack_FallsBackToExternalIDAsTitle(t *testing.T) {
	h := newHarness(t)

	track, err := h.svc.EnsureRemoteTrack(context.Background(), domain.RemoteTrackSpec{
		Provider: "wy", ExternalID: "3003",
	})
	require.NoError(t, err)
	assert.Equal(t, "3003", track.Title)
}

// TestDownloadRemote_EnqueuesOnce covers the user intent path: asking to
// download the same online track twice must not fail or create a second job.
func TestDownloadRemote_EnqueuesOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	track, err := h.svc.DownloadRemote(ctx, remoteSpec("4004"), "", "320k")
	require.NoError(t, err)

	job, err := h.jobs.GetActiveByTrackID(ctx, track.ID)
	require.NoError(t, err)
	require.NotNil(t, job, "a job must be queued")
	assert.Equal(t, "320k", job.Quality)

	// Second request for the same track: same row, still exactly one job.
	again, err := h.svc.DownloadRemote(ctx, remoteSpec("4004"), "", "320k")
	require.NoError(t, err)
	assert.Equal(t, track.ID, again.ID)

	var jobs int
	require.NoError(t, h.db.GetContext(ctx, &jobs, `SELECT COUNT(*) FROM download_jobs WHERE track_id = ?`, track.ID))
	assert.Equal(t, 1, jobs)
}

func TestDownloadRemote_AttachesToPlaylist(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedPlaylist(t, "pl-1")

	track, err := h.svc.DownloadRemote(ctx, remoteSpec("5005"), "pl-1", "flac")
	require.NoError(t, err)

	job, err := h.jobs.GetActiveByTrackID(ctx, track.ID)
	require.NoError(t, err)
	require.NotNil(t, job)
	require.NotNil(t, job.PlaylistID, "playlist id must be recorded so the finished download lands there")
	assert.Equal(t, "pl-1", *job.PlaylistID)
}

// The default quality must be applied, not passed through as an empty string.
func TestDownloadRemote_DefaultsQuality(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	track, err := h.svc.DownloadRemote(ctx, remoteSpec("6006"), "", "")
	require.NoError(t, err)

	job, err := h.jobs.GetActiveByTrackID(ctx, track.ID)
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.Equal(t, domain.DefaultDownloadQuality, job.Quality)
}

var _ = download.NewMerger // keep the import explicit for readers
