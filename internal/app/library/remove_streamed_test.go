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

type fakeStreamCacheDiscarder struct {
	ids  []string
	err  error
	note func()
}

func (f *fakeStreamCacheDiscarder) DiscardEntry(_ context.Context, track *domain.TrackDTO) error {
	f.ids = append(f.ids, track.ID)
	if f.note != nil {
		f.note()
	}
	return f.err
}

type fakePlaybackRemover struct {
	ids  []string
	err  error
	note func()
}

func (f *fakePlaybackRemover) RemoveFromPlayback(_ context.Context, trackID string) error {
	f.ids = append(f.ids, trackID)
	if f.note != nil {
		f.note()
	}
	return f.err
}

// newRemoveStreamedFixture builds the service on a real database so the
// assertions cover the actual row deletion, index and orphan cleanup.
func newRemoveStreamedFixture(t *testing.T) (*LibraryService, *sqlite.DB, *mockSearchService, *mockArtworkCache) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := sqlite.NewDB(":memory:", logger)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	search := &mockSearchService{}
	artwork := &mockArtworkCache{}
	svc, err := NewLibraryService(
		sqlite.NewTrackRepository(db),
		sqlite.NewAlbumRepository(db),
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
		search,
		sqlite.NewTxManager(db),
		sqlite.NewDownloadJobRepository(db),
		nil,
		logger,
	)
	if err != nil {
		t.Fatalf("NewLibraryService: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	return svc, db, search, artwork
}

func seedRemoteRow(t *testing.T, db *sqlite.DB, id string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO tracks (id, path, title, sort_title, availability, provider, external_id, mtime)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, domain.RemotePath("wy", "42"), "云端", "云端", domain.AvailabilityRemote, "wy", "42", time.Now(),
	); err != nil {
		t.Fatalf("insert remote row %s: %v", id, err)
	}
}

func trackExists(t *testing.T, db *sqlite.DB, id string) bool {
	t.Helper()
	var count int
	if err := db.Get(&count, `SELECT COUNT(*) FROM tracks WHERE id = ?`, id); err != nil {
		t.Fatalf("count track %s: %v", id, err)
	}
	return count > 0
}

func TestRemoveTrackFromLibrary_DropsTheRowItsCacheAndIndex(t *testing.T) {
	svc, db, search, artwork := newRemoveStreamedFixture(t)
	ctx := context.Background()
	seedRemoteRow(t, db, "remote-1")

	var order []string
	discarder := &fakeStreamCacheDiscarder{note: func() { order = append(order, "discard") }}
	remover := &fakePlaybackRemover{note: func() { order = append(order, "playback") }}
	svc.SetStreamCacheDiscarder(discarder)
	svc.SetPlaybackRemover(remover)

	var deleted []string
	svc.AddTrackDeletedListener(func(ids []string) { deleted = append(deleted, ids...) })

	if err := svc.RemoveTrackFromLibrary(ctx, "remote-1"); err != nil {
		t.Fatalf("RemoveTrackFromLibrary: %v", err)
	}

	if trackExists(t, db, "remote-1") {
		t.Fatal("the remote row must be gone")
	}
	if len(discarder.ids) != 1 || discarder.ids[0] != "remote-1" {
		t.Fatalf("discarded = %v, want the removed row", discarder.ids)
	}
	if len(remover.ids) != 1 || remover.ids[0] != "remote-1" {
		t.Fatalf("playback removals = %v, want the removed row", remover.ids)
	}
	// The decoder must release the proxy URL before the cache entry goes away.
	if len(order) != 2 || order[0] != "playback" || order[1] != "discard" {
		t.Fatalf("order = %v, want playback then discard", order)
	}
	if len(search.deletedTrackIDs) != 1 || search.deletedTrackIDs[0] != "remote-1" {
		t.Fatalf("index deletions = %v", search.deletedTrackIDs)
	}
	if len(deleted) != 1 || deleted[0] != "remote-1" {
		t.Fatalf("listeners got %v", deleted)
	}
	if artwork.cleanupCalls == 0 {
		t.Fatal("orphaned artwork cleanup must run")
	}
}

func TestRemoveTrackFromLibrary_RefusesALocalRow(t *testing.T) {
	svc, db, _, _ := newRemoveStreamedFixture(t)
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "local.flac")
	if err := os.WriteFile(path, []byte("audio"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO tracks (id, path, title, sort_title, availability, mtime)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"local-1", path, "本地", "本地", domain.AvailabilityReady, time.Now(),
	); err != nil {
		t.Fatalf("insert local row: %v", err)
	}

	discarder := &fakeStreamCacheDiscarder{}
	svc.SetStreamCacheDiscarder(discarder)

	if err := svc.RemoveTrackFromLibrary(ctx, "local-1"); err == nil {
		t.Fatal("a local row must be refused")
	}
	if !trackExists(t, db, "local-1") {
		t.Fatal("the local row must stay")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the local file must stay: %v", err)
	}
	if len(discarder.ids) != 0 {
		t.Fatal("a refused removal must not touch the stream cache")
	}
}

func TestRemoveTrackFromLibrary_KeepsGoingWhenTheCacheCannotBeCleared(t *testing.T) {
	svc, db, _, _ := newRemoveStreamedFixture(t)
	ctx := context.Background()
	seedRemoteRow(t, db, "remote-2")

	svc.SetStreamCacheDiscarder(&fakeStreamCacheDiscarder{err: errors.New("file in use")})

	if err := svc.RemoveTrackFromLibrary(ctx, "remote-2"); err != nil {
		t.Fatalf("a cache failure must not fail the removal: %v", err)
	}
	if trackExists(t, db, "remote-2") {
		t.Fatal("the row must be gone even when the cache could not be cleared")
	}
}

func TestRemoveTrackFromLibrary_WithoutACacheStillWorks(t *testing.T) {
	svc, db, _, _ := newRemoveStreamedFixture(t)
	ctx := context.Background()
	seedRemoteRow(t, db, "remote-3")

	if err := svc.RemoveTrackFromLibrary(ctx, "remote-3"); err != nil {
		t.Fatalf("RemoveTrackFromLibrary: %v", err)
	}
	if trackExists(t, db, "remote-3") {
		t.Fatal("the row must be gone")
	}
}

func TestRemoveTrackFromLibrary_StopsWhenPlaybackCannotBeReleased(t *testing.T) {
	svc, db, _, _ := newRemoveStreamedFixture(t)
	ctx := context.Background()
	seedRemoteRow(t, db, "remote-4")

	discarder := &fakeStreamCacheDiscarder{}
	svc.SetStreamCacheDiscarder(discarder)
	svc.SetPlaybackRemover(&fakePlaybackRemover{err: errors.New("decoder busy")})

	if err := svc.RemoveTrackFromLibrary(ctx, "remote-4"); err == nil {
		t.Fatal("a decoder that cannot be released must abort the removal")
	}
	if !trackExists(t, db, "remote-4") {
		t.Fatal("the row must stay when playback could not be released")
	}
	if len(discarder.ids) != 0 {
		t.Fatal("the cache must not be cleared while the decoder still holds it")
	}
}
