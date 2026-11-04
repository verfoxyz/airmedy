package download_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"airmedy/internal/app/download"
	"airmedy/internal/domain"
	"airmedy/internal/infra/sqlite"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests run the real merge against a real SQLite database: the whole
// point of M1/M2 is the transaction (membership transfer + delete must not be
// half applied), which a fake repository cannot prove.

type mergeFixture struct {
	db       *sqlite.DB
	tracks   domain.TrackRepository
	playlist domain.PlaylistRepository
	merger   *download.Merger
}

func newMergeFixture(t *testing.T) *mergeFixture {
	t.Helper()
	db, err := sqlite.NewDB(":memory:", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	tracks := sqlite.NewTrackRepository(db)
	return &mergeFixture{
		db:       db,
		tracks:   tracks,
		playlist: sqlite.NewPlaylistRepository(db),
		merger:   download.NewMerger(tracks, sqlite.NewDownloadMergeRepository(db, tracks), sqlite.NewTxManager(db), slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
}

func (f *mergeFixture) seedRemote(t *testing.T, id, externalID string) {
	t.Helper()
	require.NoError(t, f.tracks.Upsert(context.Background(), &domain.Track{
		ID:           id,
		Path:         domain.RemotePath("wy", externalID),
		Title:        "晴天",
		SortTitle:    "晴天",
		Duration:     269,
		Availability: domain.AvailabilityDownloading,
		Provider:     "wy",
		ExternalID:   externalID,
	}))
}

func (f *mergeFixture) seedLocal(t *testing.T, id, path string) {
	t.Helper()
	require.NoError(t, f.tracks.Upsert(context.Background(), &domain.Track{
		ID:           id,
		Path:         path,
		Title:        "晴天",
		SortTitle:    "晴天",
		Duration:     269,
		Availability: domain.AvailabilityReady,
	}))
}

func (f *mergeFixture) seedPlaylist(t *testing.T, id string) {
	t.Helper()
	require.NoError(t, f.playlist.Save(context.Background(), &domain.Playlist{ID: id, Name: id}))
}

func (f *mergeFixture) addToPlaylist(t *testing.T, playlistID, trackID, position string) {
	t.Helper()
	require.NoError(t, f.playlist.AddTrack(context.Background(), playlistID, trackID, position))
}

func (f *mergeFixture) playlistTracks(t *testing.T, playlistID string) []string {
	t.Helper()
	var ids []string
	require.NoError(t, f.db.SelectContext(context.Background(), &ids,
		`SELECT track_id FROM playlist_tracks WHERE playlist_id = ? ORDER BY position`, playlistID))
	return ids
}

func (f *mergeFixture) trackCount(t *testing.T) int {
	t.Helper()
	n, err := f.tracks.Count(context.Background())
	require.NoError(t, err)
	return n
}

// --- M1 ---------------------------------------------------------------------

func TestMerge_RemoteRowWithoutIdentityIsRetiredNotDeleted(t *testing.T) {
	f := newMergeFixture(t)
	ctx := context.Background()

	// local-1 is a completed download of this song: it carries the remote
	// identity (design 4.1), is favorited, and was played three times.
	f.seedLocal(t, "local-1", `D:\Music\old\01.mp3`)
	prev, err := f.tracks.GetByID(ctx, "local-1")
	require.NoError(t, err)
	prev.Provider, prev.ExternalID = "wy", "2668397359"
	require.NoError(t, f.tracks.Upsert(ctx, &prev.Track))
	require.NoError(t, f.tracks.SetFavorite(ctx, "local-1", true))
	for i := 0; i < 3; i++ {
		require.NoError(t, f.tracks.IncrementPlayCount(ctx, "local-1"))
	}

	// remote-1 sits in a playlist but carries no identity (pre-077 data, or a
	// row created before its download finished). Invariant G1 keeps the
	// identity on one row, so this row has none.
	require.NoError(t, f.tracks.Upsert(ctx, &domain.Track{
		ID:           "remote-1",
		Path:         domain.RemotePath("wy", "2668397359"),
		Title:        "晴天",
		Duration:     269,
		Availability: domain.AvailabilityDownloading,
	}))
	require.NoError(t, f.tracks.SetFavorite(ctx, "remote-1", true))
	for i := 0; i < 2; i++ {
		require.NoError(t, f.tracks.IncrementPlayCount(ctx, "remote-1"))
	}
	f.seedPlaylist(t, "pl-1")
	f.addToPlaylist(t, "pl-1", "remote-1", "0|hzzzzz:")

	// Downloading that playlist entry lands the file on its own path.
	target := `D:\Music\new\03.mp3`
	f.seedLocal(t, "local-2", target)

	res, err := f.merger.Merge(ctx, download.MergeInput{
		RemotePath: domain.RemotePath("wy", "2668397359"), Provider: "wy", ExternalID: "2668397359",
		TargetPath: target, TargetTrackID: "local-2",
	})
	require.NoError(t, err)
	assert.Equal(t, "import", res.Rule)

	// The remote row is folded away and the row that owns the file survives: the
	// user's favorite, playlist entry and play count move onto it. Keeping the
	// remote row instead (with only its availability flipped) would leave it
	// pointing at a remote:// path no decoder can open — the bug this test pins.
	_, err = f.tracks.GetByID(ctx, "remote-1")
	require.NoError(t, err, "the retired row must be gone")

	survivor, err := f.tracks.GetByID(ctx, "local-2")
	require.NoError(t, err)
	require.NotNil(t, survivor)
	assert.Equal(t, domain.AvailabilityReady, survivor.Availability)
	assert.NotContains(t, survivor.Path, domain.RemotePathPrefix, "the survivor owns a real file")
	assert.True(t, survivor.IsFavorite, "the favorite must not be lost with the remote row")
	assert.Equal(t, 2, survivor.PlayCount, "the play count must not be lost")
	assert.Equal(t, []string{"local-2"}, f.playlistTracks(t, "pl-1"),
		"the playlist entry follows the song onto the surviving row")

	// The earlier download is untouched.
	kept, err := f.tracks.GetByID(ctx, "local-1")
	require.NoError(t, err)
	assert.Equal(t, 3, kept.PlayCount)
}

func TestMerge_MovesLyricsOntoTheSurvivor(t *testing.T) {
	f := newMergeFixture(t)
	ctx := context.Background()
	f.seedRemote(t, "remote-1", "1001")
	f.seedLocal(t, "imported-1", "/library/晴天.mp3")

	if _, err := f.db.ExecContext(ctx,
		`INSERT INTO lyrics (track_id, content, source) VALUES (?, ?, ?)`,
		"remote-1", "[00:01.00]one", "lrclib-synced"); err != nil {
		t.Fatal(err)
	}

	result, err := f.merger.Merge(ctx, download.MergeInput{
		RemotePath:    domain.RemotePath("wy", "1001"),
		Provider:      "wy",
		ExternalID:    "1001",
		TargetPath:    "/library/晴天.mp3",
		TargetTrackID: "imported-1",
	})
	require.NoError(t, err)
	require.Equal(t, "imported-1", result.SurvivorTrackID)

	var survivorLyrics, remoteLyrics int
	require.NoError(t, f.db.GetContext(ctx, &survivorLyrics,
		`SELECT COUNT(*) FROM lyrics WHERE track_id = ?`, "imported-1"))
	require.NoError(t, f.db.GetContext(ctx, &remoteLyrics,
		`SELECT COUNT(*) FROM lyrics WHERE track_id = ?`, "remote-1"))
	assert.Equal(t, 1, survivorLyrics, "the lyric must survive on the row that owns the file")
	assert.Zero(t, remoteLyrics, "the remote row is gone and its lyric must have moved")
}

func TestMerge_M1_RemoteRowOwningTheIdentityIsDeleted(t *testing.T) {
	f := newMergeFixture(t)
	ctx := context.Background()

	// Ready peer for the same song, without the identity yet.
	// Collected with the cwd on D: and a BEL in the name, so the fixture also
	// exercises a Windows path that sanitising must not mangle.
	f.seedLocal(t, "local-1", "D:\\Music\\a.mp3")

	// The remote row is the one that carries the identity, and the user has it
	// in a playlist. A second download of the same identity finished onto
	// local-1, so the remote row is redundant: it carries nothing the survivor
	// does not.
	f.seedRemote(t, "remote-1", "42")
	f.seedPlaylist(t, "pl-1")
	f.addToPlaylist(t, "pl-1", "remote-1", "0|hzzzzz:")

	// The importer gave the identity to the finished row.
	peer, err := f.tracks.GetByID(ctx, "local-1")
	require.NoError(t, err)
	require.NoError(t, f.tracks.Delete(ctx, "remote-1"))
	peer.Provider, peer.ExternalID = "wy", "42"
	require.NoError(t, f.tracks.Upsert(ctx, &peer.Track))
	// Recreate the playlist row after the identity moved, with the same
	// identity, to model the state right before the merge runs.
	require.NoError(t, f.tracks.Upsert(ctx, &domain.Track{
		ID: "remote-2", Path: domain.RemotePath("wy", "42"), Title: "晴天",
		Duration: 269, Availability: domain.AvailabilityDownloading,
	}))
	f.addToPlaylist(t, "pl-1", "remote-2", "0|i00000:")

	// remote-2 does not own the identity while local-1 does, so this is the
	// retire case; delete-and-migrate only happens for the owner.
	res, err := f.merger.Merge(ctx, download.MergeInput{
		RemotePath: domain.RemotePath("wy", "42"), Provider: "wy", ExternalID: "42",
		TargetPath: "D:\\Music\\a.mp3", TargetTrackID: "local-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "import", res.Rule)
	// The leftover remote row is folded into the row that already owns both the
	// file and the identity, so the playlist entry moves with the song.
	assert.Equal(t, []string{"local-1"}, f.playlistTracks(t, "pl-1"))
}

// --- default / no-op cases --------------------------------------------------

func TestMerge_NoSurvivorRuleFoldsIntoTheImportedRow(t *testing.T) {
	f := newMergeFixture(t)
	ctx := context.Background()

	f.seedRemote(t, "remote-1", "7")
	require.NoError(t, f.tracks.SetFavorite(ctx, "remote-1", true))
	f.seedLocal(t, "local-1", `D:\Music\new.mp3`)
	f.seedPlaylist(t, "pl-1")
	f.addToPlaylist(t, "pl-1", "remote-1", "0|hzzzzz:")

	res, err := f.merger.Merge(ctx, download.MergeInput{
		RemotePath: domain.RemotePath("wy", "7"), Provider: "wy", ExternalID: "7",
		TargetPath: `D:\Music\new.mp3`, TargetTrackID: "local-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "import", res.Rule)

	// The imported row survives and takes on the user's favorite and playlist
	// entry; the remote row is gone. The survivor must resolve to a real file.
	retired, err := f.tracks.GetByID(ctx, "remote-1")
	require.NoError(t, err)
	require.Nil(t, retired, "the remote row is folded into the imported one")

	survivor, err := f.tracks.GetByID(ctx, "local-1")
	require.NoError(t, err)
	require.NotNil(t, survivor)
	assert.NotContains(t, survivor.Path, domain.RemotePathPrefix, "the survivor owns a real file")
	assert.Equal(t, domain.AvailabilityReady, survivor.Availability)
	assert.False(t, survivor.IsRemote(), "the survivor must be playable")
	assert.True(t, survivor.IsFavorite, "the favorite must not be lost with the remote row")
	assert.Equal(t, []string{"local-1"}, f.playlistTracks(t, "pl-1"))
}

func TestMerge_IsNoOpWhenTheRowWasAlreadyReady(t *testing.T) {
	f := newMergeFixture(t)
	ctx := context.Background()

	// The import updated the remote row in place (same path, same id).
	f.seedRemote(t, "remote-1", "7")
	row, err := f.tracks.GetByID(ctx, "remote-1")
	require.NoError(t, err)
	row.Availability = domain.AvailabilityReady
	require.NoError(t, f.tracks.Upsert(ctx, &row.Track))

	res, err := f.merger.Merge(ctx, download.MergeInput{
		RemotePath: domain.RemotePath("wy", "7"), Provider: "wy", ExternalID: "7",
		TargetPath: row.Path, TargetTrackID: "remote-1",
	})
	require.NoError(t, err)
	assert.Empty(t, res.Rule)
	assert.Equal(t, 1, f.trackCount(t))
}

func TestMerge_IsNoOpWithoutARemoteRow(t *testing.T) {
	f := newMergeFixture(t)
	f.seedLocal(t, "local-1", `D:\Music\a.mp3`)

	res, err := f.merger.Merge(context.Background(), download.MergeInput{
		RemotePath: domain.RemotePath("wy", "404"), Provider: "wy", ExternalID: "404",
		TargetPath: `D:\Music\a.mp3`, TargetTrackID: "local-1",
	})
	require.NoError(t, err)
	assert.Empty(t, res.Rule)
	assert.Equal(t, 1, f.trackCount(t))
}

func TestMerge_KeepsPlaylistPosition(t *testing.T) {
	f := newMergeFixture(t)
	ctx := context.Background()

	f.seedRemote(t, "remote-1", "7")
	f.seedLocal(t, "local-1", `D:\Music\new.mp3`)
	f.seedPlaylist(t, "pl-1")

	// Two neighbours so a lost position would be visible as a reordering.
	f.addToPlaylist(t, "pl-1", "remote-1", "0|i00001:")
	var otherID string
	require.NoError(t, f.tracks.Upsert(ctx, &domain.Track{ID: "other", Path: `D:\Music\z.mp3`, Title: "z", Availability: domain.AvailabilityReady}))
	otherID = "other"
	f.addToPlaylist(t, "pl-1", otherID, "0|i00002:")

	_, err := f.merger.Merge(ctx, download.MergeInput{
		RemotePath: domain.RemotePath("wy", "7"), Provider: "wy", ExternalID: "7",
		TargetPath: `D:\Music\new.mp3`, TargetTrackID: "local-1",
	})
	require.NoError(t, err)

	// The entry moves onto the surviving row and keeps its position, so the
	// order the user built is preserved.
	assert.Equal(t, []string{"local-1", otherID}, f.playlistTracks(t, "pl-1"))
}

// --- M3 ---------------------------------------------------------------------

func TestMerge_M3_SimilarLocalTrackIsNotMerged(t *testing.T) {
	f := newMergeFixture(t)
	ctx := context.Background()

	// A live recording: same title and artist, duration 6s longer. Merging it
	// into the studio version would destroy a track the user wants.
	require.NoError(t, f.tracks.Upsert(ctx, &domain.Track{
		ID: "live", Path: `D:\Music\live.mp3`, Title: "晴天", RawArtistNames: "周杰伦",
		Duration: 275, Availability: domain.AvailabilityReady,
	}))

	f.seedRemote(t, "remote-1", "7")
	f.seedLocal(t, "local-1", `D:\Music\studio.mp3`)
	require.NoError(t, f.tracks.Upsert(ctx, &domain.Track{
		ID: "local-1", Path: `D:\Music\studio.mp3`, Title: "晴天", RawArtistNames: "周杰伦",
		Duration: 269, Availability: domain.AvailabilityReady,
	}))

	_, err := f.merger.Merge(ctx, download.MergeInput{
		RemotePath: domain.RemotePath("wy", "7"), Provider: "wy", ExternalID: "7",
		TargetPath: `D:\Music\studio.mp3`, TargetTrackID: "local-1",
	})
	require.NoError(t, err)

	// The live version survives: M3 only logs, it never merges or deletes.
	live, err := f.tracks.GetByID(ctx, "live")
	require.NoError(t, err)
	assert.NotNil(t, live, "M3 must never delete a similar local track")
	assert.Equal(t, domain.AvailabilityReady, live.Availability)
}

// TestMerge_SurvivingRowIsPlayable is the regression for "ma_player_load failed:
// -10 (path: remote://wy/<id>)".
//
// A finished download used to leave the remote row in place with its virtual
// path and only flip availability to 'ready'. The row was then unusable: the
// decoder was handed a path that is not a file. Whatever else the merge does, the
// row that carries the user's associations must resolve to real audio afterwards.
func TestMerge_SurvivingRowIsPlayable(t *testing.T) {
	f := newMergeFixture(t)
	ctx := context.Background()

	f.seedRemote(t, "remote-1", "5257138")
	require.NoError(t, f.tracks.SetFavorite(ctx, "remote-1", true))
	f.seedLocal(t, "local-1", `D:\Music\屋顶.mp3`)

	res, err := f.merger.Merge(ctx, download.MergeInput{
		RemotePath: domain.RemotePath("wy", "5257138"), Provider: "wy", ExternalID: "5257138",
		TargetPath: `D:\Music\屋顶.mp3`, TargetTrackID: "local-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "import", res.Rule)

	// The one row the library keeps must be playable.
	rows, err := f.tracks.GetAll(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1, "the merge must leave exactly one row for the song")
	row := rows[0]
	assert.False(t, row.IsRemote(), "the surviving row must resolve to a real file, not remote://")
	assert.Equal(t, domain.AvailabilityReady, row.Availability)
	assert.Equal(t, "wy", row.Provider, "the identity must survive, or the next download re-creates the remote row")
	assert.Equal(t, "5257138", row.ExternalID)
	assert.True(t, row.IsFavorite, "the favorite must move onto the survivor")
}

// TestMerge_IdentitySurvivesSoASecondDownloadDoesNotRecreateTheRemoteRow pins the
// follow-on failure: dropping the identity would make the next download of the
// same song register a fresh remote row.
func TestMerge_IdentitySurvivesSoASecondDownloadDoesNotRecreateTheRemoteRow(t *testing.T) {
	f := newMergeFixture(t)
	ctx := context.Background()

	f.seedRemote(t, "remote-1", "42")
	f.seedLocal(t, "local-1", `D:\Music\a.mp3`)

	_, err := f.merger.Merge(ctx, download.MergeInput{
		RemotePath: domain.RemotePath("wy", "42"), Provider: "wy", ExternalID: "42",
		TargetPath: `D:\Music\a.mp3`, TargetTrackID: "local-1",
	})
	require.NoError(t, err)

	// Looking the identity up must find the downloaded row, so EnsureRemoteTrack
	// hands back that row instead of inserting a new remote:// one.
	existing, err := f.merger.FindByRemoteIdentity(ctx, "wy", "42")
	require.NoError(t, err)
	require.NotNil(t, existing, "the identity must remain registered")
	assert.False(t, existing.IsRemote(), "and it must point at the downloaded file")
	assert.Equal(t, "local-1", existing.ID)
}
