package library

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"airmedy/internal/domain"
	"airmedy/internal/infra/sqlite"
)

// erroringAlbumRepo wraps a real album repository so a test can make the
// best-effort orphan cleanup fail without breaking the rest of the fixture.
type erroringAlbumRepo struct {
	domain.AlbumRepository
}

func (r erroringAlbumRepo) DeleteOrphaned(context.Context) ([]string, error) {
	return nil, errors.New("orphan cleanup failed")
}

// newDeleteOrphanFixture builds the service on a real in-memory database. Only
// the artwork cache and the search index are stubbed, so the assertions below
// exercise the actual DeleteOrphaned SQL instead of a mock's return value.
//
// wrapAlbums, when set, lets a test replace the album repository (the cleanup
// failure regression); nil keeps the real one.
func newDeleteOrphanFixture(t *testing.T, artwork *mockArtworkCache, wrapAlbums func(domain.AlbumRepository) domain.AlbumRepository) (*LibraryService, *sqlite.DB) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := sqlite.NewDB(":memory:", logger)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	// ":memory:" gives every pooled connection its own empty database, so the
	// service's transaction (which runs on whichever connection is free) would
	// not see the tables without a single connection. Production uses a file.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	albums := sqlite.NewAlbumRepository(db)
	if wrapAlbums != nil {
		albums = wrapAlbums(albums)
	}

	svc, err := NewLibraryService(
		sqlite.NewTrackRepository(db),
		albums,
		sqlite.NewArtistRepository(db),
		sqlite.NewGenreRepository(db),
		sqlite.NewComposerRepository(db),
		sqlite.NewPlaylistRepository(db),
		sqlite.NewWatchedFolderRepository(db),
		sqlite.NewSettingsRepository(db),
		sqlite.NewLibrarySyncStateRepository(db),
		&mockMetadataExtractor{},
		&stubDurationProbe{},
		&mockMetadataWriter{},
		artwork,
		&mockSearchService{},
		sqlite.NewTxManager(db),
		sqlite.NewDownloadJobRepository(db),
		nil,
		logger,
	)
	if err != nil {
		t.Fatalf("NewLibraryService: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	return svc, db
}

// seedOrphanFixtureTrack writes the audio file and inserts the minimum rows
// DeleteOrphaned looks at: the album, the artist, the track and the track →
// artist link. It returns the file path so a test can assert removal.
func seedOrphanFixtureTrack(t *testing.T, db *sqlite.DB, trackID, albumID, artistID string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), trackID+".mp3")
	if err := os.WriteFile(path, []byte("audio"), 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", trackID, err)
	}
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO albums (id, title, sort_title, normalization_key) VALUES (?, ?, ?, ?)`,
		albumID, albumID, albumID, albumID,
	); err != nil {
		t.Fatalf("insert album %s: %v", albumID, err)
	}
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO artists (id, name, sort_name, normalization_key) VALUES (?, ?, ?, ?)`,
		artistID, artistID, artistID, artistID,
	); err != nil {
		t.Fatalf("insert artist %s: %v", artistID, err)
	}
	if _, err := db.Exec(
		`INSERT INTO tracks (id, path, title, sort_title, album_id, mtime) VALUES (?, ?, ?, ?, ?, ?)`,
		trackID, path, trackID, trackID, albumID, time.Now(),
	); err != nil {
		t.Fatalf("insert track %s: %v", trackID, err)
	}
	if _, err := db.Exec(
		`INSERT INTO track_artists (track_id, artist_id, position) VALUES (?, ?, 0)`,
		trackID, artistID,
	); err != nil {
		t.Fatalf("link track %s to artist %s: %v", trackID, artistID, err)
	}
	return path
}

func countRows(t *testing.T, db *sqlite.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Get(&n, query, args...); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func TestDeleteTrackFile_RemovesOrphanedAlbumAndArtist(t *testing.T) {
	artwork := &mockArtworkCache{}
	svc, db := newDeleteOrphanFixture(t, artwork, nil)

	// Album A keeps a track, so it and its shared artist must survive.
	seedOrphanFixtureTrack(t, db, "a1", "album-a", "shared")
	seedOrphanFixtureTrack(t, db, "a2", "album-a", "shared")
	// Album B is emptied by this deletion; its sole artist goes with it.
	seedOrphanFixtureTrack(t, db, "b1", "album-b", "solo")

	if err := svc.DeleteTrackFile(context.Background(), "b1"); err != nil {
		t.Fatalf("DeleteTrackFile: %v", err)
	}

	if n := countRows(t, db, `SELECT COUNT(*) FROM albums WHERE id = 'album-b'`); n != 0 {
		t.Fatalf("emptied album row still exists: %d", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM albums WHERE id = 'album-a'`); n != 1 {
		t.Fatalf("album with remaining tracks was removed: %d", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM artists WHERE id = 'solo'`); n != 0 {
		t.Fatalf("unreferenced artist row still exists: %d", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM artists WHERE id = 'shared'`); n != 1 {
		t.Fatalf("artist shared with another album was removed: %d", n)
	}
}

func TestDeleteTrackFile_KeepsAlbumWhenTracksRemain(t *testing.T) {
	svc, db := newDeleteOrphanFixture(t, &mockArtworkCache{}, nil)
	seedOrphanFixtureTrack(t, db, "a1", "album-a", "shared")
	seedOrphanFixtureTrack(t, db, "a2", "album-a", "shared")

	if err := svc.DeleteTrackFile(context.Background(), "a1"); err != nil {
		t.Fatalf("DeleteTrackFile: %v", err)
	}

	if n := countRows(t, db, `SELECT COUNT(*) FROM albums WHERE id = 'album-a'`); n != 1 {
		t.Fatalf("album lost while it still had a track: %d", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM tracks WHERE album_id = 'album-a'`); n != 1 {
		t.Fatalf("tracks left in album = %d, want 1", n)
	}
}

func TestDeleteTrackFile_CleansOrphanedArtworks(t *testing.T) {
	artwork := &mockArtworkCache{}
	svc, db := newDeleteOrphanFixture(t, artwork, nil)
	seedOrphanFixtureTrack(t, db, "b1", "album-b", "solo")

	if err := svc.DeleteTrackFile(context.Background(), "b1"); err != nil {
		t.Fatalf("DeleteTrackFile: %v", err)
	}
	if artwork.cleanupCalls != 1 {
		t.Fatalf("artwork cleanup calls = %d, want 1", artwork.cleanupCalls)
	}
}

func TestDeleteTrackFile_ArtworkCleanupFailureDoesNotFailDeletion(t *testing.T) {
	artwork := &mockArtworkCache{cleanupErr: errors.New("artwork cache unavailable")}
	svc, db := newDeleteOrphanFixture(t, artwork, nil)
	path := seedOrphanFixtureTrack(t, db, "b1", "album-b", "solo")

	if err := svc.DeleteTrackFile(context.Background(), "b1"); err != nil {
		t.Fatalf("DeleteTrackFile must stay successful, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still on disk: %v", err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM tracks WHERE id = 'b1'`); n != 0 {
		t.Fatalf("track row still exists: %d", n)
	}
}

func TestDeleteTrackFile_EntityCleanupFailureDoesNotFailDeletion(t *testing.T) {
	svc, db := newDeleteOrphanFixture(t, &mockArtworkCache{}, func(inner domain.AlbumRepository) domain.AlbumRepository {
		return erroringAlbumRepo{AlbumRepository: inner}
	})
	path := seedOrphanFixtureTrack(t, db, "b1", "album-b", "solo")

	if err := svc.DeleteTrackFile(context.Background(), "b1"); err != nil {
		t.Fatalf("DeleteTrackFile must stay successful, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still on disk: %v", err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM tracks WHERE id = 'b1'`); n != 0 {
		t.Fatalf("track row still exists: %d", n)
	}
}
