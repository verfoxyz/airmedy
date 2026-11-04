package playlist

import (
	"context"
	"log/slog"
	"testing"

	"airmedy/internal/domain"
	"airmedy/internal/infra/sqlite"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The auto-download hook must fire after the membership is committed (the job's
// playlist foreign key needs the row to exist) and only for remote tracks.

type autoDownloadCall struct {
	trackID    string
	playlistID string
}

func newAutoDownloadFixture(t *testing.T) (*PlaylistService, domain.TrackRepository, domain.PlaylistRepository, *[]autoDownloadCall) {
	t.Helper()
	db, err := sqlite.NewDB(":memory:", slog.Default())
	require.NoError(t, err)
	db.SetMaxOpenConns(1) // one connection: an in-memory DB per pooled connection
	t.Cleanup(func() { _ = db.Close() })

	tracks := sqlite.NewTrackRepository(db)
	playlists := sqlite.NewPlaylistRepository(db)
	// Create() would index into the search backend; saving directly keeps this
	// test to the behaviour under test.
	svc := NewPlaylistService(playlists, tracks, nil, nil, slog.Default())

	calls := &[]autoDownloadCall{}
	svc.SetAutoDownload(func(_ context.Context, track *domain.TrackDTO, playlistID string) {
		*calls = append(*calls, autoDownloadCall{trackID: track.ID, playlistID: playlistID})
	})
	return svc, tracks, playlists, calls
}

func seedPlaylist(t *testing.T, repo domain.PlaylistRepository, id string) {
	t.Helper()
	require.NoError(t, repo.Save(context.Background(), &domain.Playlist{ID: id, Name: id}))
}

func TestAddTrack_TriggersAutoDownloadForRemoteTrack(t *testing.T) {
	svc, tracks, playlists, calls := newAutoDownloadFixture(t)
	ctx := context.Background()

	seedPlaylist(t, playlists, "pl-1")
	require.NoError(t, tracks.Upsert(ctx, &domain.Track{
		ID: "remote-1", Path: domain.RemotePath("wy", "1"), Title: "t",
		Availability: domain.AvailabilityRemote, Provider: "wy", ExternalID: "1",
	}))

	require.NoError(t, svc.AddTrack(ctx, "pl-1", "remote-1"))

	require.Len(t, *calls, 1)
	assert.Equal(t, "remote-1", (*calls)[0].trackID)
	assert.Equal(t, "pl-1", (*calls)[0].playlistID)
}

func TestAddTrack_SkipsAutoDownloadForLocalTrack(t *testing.T) {
	svc, tracks, playlists, calls := newAutoDownloadFixture(t)
	ctx := context.Background()

	seedPlaylist(t, playlists, "pl-1")
	require.NoError(t, tracks.Upsert(ctx, &domain.Track{
		ID: "local-1", Path: "/music/a.mp3", Title: "t", Availability: domain.AvailabilityReady,
	}))

	require.NoError(t, svc.AddTrack(ctx, "pl-1", "local-1"))
	assert.Empty(t, *calls, "a file already on disk needs no download")
}

func TestAddTrack_WithoutHookStillAdds(t *testing.T) {
	svc, tracks, playlists, _ := newAutoDownloadFixture(t)
	ctx := context.Background()
	svc.SetAutoDownload(nil)

	seedPlaylist(t, playlists, "pl-1")
	require.NoError(t, tracks.Upsert(ctx, &domain.Track{
		ID: "remote-1", Path: domain.RemotePath("wy", "1"), Title: "t",
		Availability: domain.AvailabilityRemote, Provider: "wy", ExternalID: "1",
	}))

	require.NoError(t, svc.AddTrack(ctx, "pl-1", "remote-1"))
}
