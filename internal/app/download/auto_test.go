package download_test

import (
	"context"
	"testing"

	"airmedy/internal/app/download"
	"airmedy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShouldAutoDownload(t *testing.T) {
	remote := &domain.TrackDTO{Track: domain.Track{ID: "r", Path: domain.RemotePath("wy", "1"), Availability: domain.AvailabilityRemote}}
	local := &domain.TrackDTO{Track: domain.Track{ID: "l", Path: "/music/a.mp3", Availability: domain.AvailabilityReady}}
	on := &domain.AppSettings{AutoDownloadOnPlaylistAdd: true}
	off := &domain.AppSettings{AutoDownloadOnPlaylistAdd: false}
	active := &domain.DownloadJob{ID: "j", State: domain.DownloadStateQueued}

	cases := []struct {
		name     string
		settings *domain.AppSettings
		track    *domain.TrackDTO
		active   *domain.DownloadJob
		want     bool
	}{
		{"switch off", off, remote, nil, false},
		{"no settings", nil, remote, nil, false},
		{"local track", on, local, nil, false},
		{"nil track", on, nil, nil, false},
		{"already queued", on, remote, active, false},
		{"remote, switch on", on, remote, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, download.ShouldAutoDownload(tc.settings, tc.track, tc.active))
		})
	}
}

func TestOnRemoteTrackSaved_PlaylistAddEnqueuesWhenEnabled(t *testing.T) {
	h := newHarnessSetting(t, true)
	track := h.seedRemote(t, "t1")
	h.seedPlaylist(t, "pl-1")
	ctx := context.Background()

	state, err := h.svc.OnRemoteTrackSaved(ctx, domain.RemoteTrackSaved{Track: track, PlaylistID: "pl-1"})
	require.NoError(t, err)
	assert.Equal(t, domain.CatalogRowStateDownloading, state)

	job := h.activeJob(t, track.ID)
	require.NotNil(t, job, "an online track added to a playlist must be queued")
	assert.Equal(t, domain.DownloadStateQueued, job.State)
	require.NotNil(t, job.PlaylistID)
	assert.Equal(t, "pl-1", *job.PlaylistID)
}

func TestOnRemoteTrackSaved_PlaylistAddDoesNothingWhenDisabled(t *testing.T) {
	h := newHarnessSetting(t, false)
	track := h.seedRemote(t, "t1")

	state, err := h.svc.OnRemoteTrackSaved(context.Background(), domain.RemoteTrackSaved{Track: track, PlaylistID: "pl-1"})
	require.NoError(t, err)
	assert.Equal(t, domain.CatalogRowStateSaved, state)
	assert.Nil(t, h.activeJob(t, track.ID))
}

// TestOnRemoteTrackSaved_ExplicitSaveIgnoresThePlaylistSwitch: the favorite
// button (and a search-row save) is the user saying "I want this song", so it
// downloads even when the playlist switch is off.
func TestOnRemoteTrackSaved_ExplicitSaveIgnoresThePlaylistSwitch(t *testing.T) {
	h := newHarnessSetting(t, false)
	track := h.seedRemote(t, "t1")
	h.seedPlaylist(t, "pl-1")

	state, err := h.svc.OnRemoteTrackSaved(context.Background(), domain.RemoteTrackSaved{
		Track: track, PlaylistID: "pl-1", Explicit: true,
	})
	require.NoError(t, err)
	assert.Equal(t, domain.CatalogRowStateDownloading, state)
	assert.NotNil(t, h.activeJob(t, track.ID))
}

// TestOnRemoteTrackSaved_PrefersPromotion: the audio is already on disk, so the
// save must not queue a second download. It records a durable promotion request
// instead, and the row is not reported as downloading.
func TestOnRemoteTrackSaved_PrefersPromotion(t *testing.T) {
	h := newHarnessSetting(t, true)
	track := h.seedRemote(t, "t1")
	h.seedPlaylist(t, "pl-1")
	h.cached.found = true

	state, err := h.svc.OnRemoteTrackSaved(context.Background(), domain.RemoteTrackSaved{Track: track, PlaylistID: "pl-1"})
	require.NoError(t, err)
	assert.Equal(t, domain.CatalogRowStateSaved, state)
	assert.Nil(t, h.activeJob(t, track.ID), "a cached copy must not be downloaded again")
	assert.Equal(t, 1, h.cached.marked, "the entry must be flagged for promotion")
}

func TestOnRemoteTrackSaved_DoesNotQueueTwice(t *testing.T) {
	h := newHarnessSetting(t, true)
	track := h.seedRemote(t, "t1")
	h.seedPlaylist(t, "pl-1")
	ctx := context.Background()

	_, err := h.svc.OnRemoteTrackSaved(ctx, domain.RemoteTrackSaved{Track: track, PlaylistID: "pl-1"})
	require.NoError(t, err)
	first := h.activeJob(t, track.ID)
	require.NotNil(t, first)

	// Adding the same track to a second playlist must not start a second job.
	state, err := h.svc.OnRemoteTrackSaved(ctx, domain.RemoteTrackSaved{Track: track, PlaylistID: "pl-2"})
	require.NoError(t, err)
	assert.Equal(t, domain.CatalogRowStateSaved, state)
	jobs, err := h.svc.List(ctx)
	require.NoError(t, err)
	assert.Len(t, jobs, 1)
	assert.Equal(t, "pl-1", *jobs[0].PlaylistID, "the original request owns the job")
}

// unavailableResolver implements the optional availability capability and says
// no installed source owns the provider.
type unavailableResolver struct{}

func (unavailableResolver) Resolve(context.Context, *domain.TrackDTO, string) (domain.StreamRef, error) {
	return domain.StreamRef{}, domain.ErrStreamUnavailable
}

func (unavailableResolver) CanResolve(string) bool { return false }

// TestOnRemoteTrackSaved_NoResolverDoesNotQueue: the playlist hook bypasses the
// catalog action, so the coordinator must refuse a provider nothing can fetch.
func TestOnRemoteTrackSaved_NoResolverDoesNotQueue(t *testing.T) {
	h := newHarnessSetting(t, true)
	h.useResolver(unavailableResolver{})
	track := h.seedRemote(t, "t1")
	h.seedPlaylist(t, "pl-1")

	_, err := h.svc.OnRemoteTrackSaved(context.Background(), domain.RemoteTrackSaved{Track: track, PlaylistID: "pl-1"})
	assert.Error(t, err)
	assert.Nil(t, h.activeJob(t, track.ID), "a provider with no enabled source must not queue a job")
}

// newHarnessSetting is newHarness with the auto-download switch set, so the
// hook tests do not have to reach into the harness internals.
func newHarnessSetting(t *testing.T, enabled bool) *harness {
	t.Helper()
	h := newHarness(t)
	h.setAutoDownload(enabled)
	return h
}
