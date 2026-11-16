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

func seedTrack(t *testing.T, tracks domain.TrackRepository, id, provider, externalID string, availability string, path string) {
	t.Helper()
	require.NoError(t, tracks.Upsert(context.Background(), &domain.Track{
		ID: id, Path: path, Title: id, Availability: availability,
		Provider: provider, ExternalID: externalID,
	}))
}

// TestAddTracks_TriggersAutoDownloadPerRemoteTrack: multi-select and drag-drop
// add through AddTracks; it must fire the same save hook as AddTrack.
func TestAddTracks_TriggersAutoDownloadPerRemoteTrack(t *testing.T) {
	svc, tracks, playlists, calls := newAutoDownloadFixture(t)
	ctx := context.Background()

	seedPlaylist(t, playlists, "pl-1")
	seedTrack(t, tracks, "remote-1", "wy", "1", domain.AvailabilityRemote, domain.RemotePath("wy", "1"))
	seedTrack(t, tracks, "remote-2", "wy", "2", domain.AvailabilityRemote, domain.RemotePath("wy", "2"))
	seedTrack(t, tracks, "local-1", "", "", domain.AvailabilityReady, "/music/a.mp3")

	require.NoError(t, svc.AddTracks(ctx, "pl-1", []string{"remote-1", "local-1", "remote-2"}))

	require.Len(t, *calls, 2, "one hook per remote row; the local file needs none")
	assert.Equal(t, "remote-1", (*calls)[0].trackID)
	assert.Equal(t, "remote-2", (*calls)[1].trackID)
	for _, call := range *calls {
		assert.Equal(t, "pl-1", call.playlistID)
	}
}

// TestAddTracks_FiresOnceForDuplicateIDs: the repository ignores a repeated
// membership, so the hook must not enqueue the same save twice.
func TestAddTracks_FiresOnceForDuplicateIDs(t *testing.T) {
	svc, tracks, playlists, calls := newAutoDownloadFixture(t)
	ctx := context.Background()

	seedPlaylist(t, playlists, "pl-1")
	seedTrack(t, tracks, "remote-1", "wy", "1", domain.AvailabilityRemote, domain.RemotePath("wy", "1"))

	require.NoError(t, svc.AddTracks(ctx, "pl-1", []string{"remote-1", "remote-1"}))

	require.Len(t, *calls, 1)
}

func TestAddTracks_WithoutHookStillAdds(t *testing.T) {
	svc, tracks, playlists, _ := newAutoDownloadFixture(t)
	ctx := context.Background()
	svc.SetAutoDownload(nil)

	seedPlaylist(t, playlists, "pl-1")
	seedTrack(t, tracks, "remote-1", "wy", "1", domain.AvailabilityRemote, domain.RemotePath("wy", "1"))

	require.NoError(t, svc.AddTracks(ctx, "pl-1", []string{"remote-1"}))
}
