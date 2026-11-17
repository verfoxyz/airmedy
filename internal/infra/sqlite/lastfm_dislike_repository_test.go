package sqlite

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"airmedy/internal/domain"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

func newDislikeStore(t *testing.T) (*DB, domain.LastFmDislikeRepository) {
	t.Helper()
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, NewLastFmDislikeRepository(db)
}

func TestDislikeRepositoryAddsListsAndDeletes(t *testing.T) {
	_, repo := newDislikeStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	trackKey := "artist" + "\x00" + "song"

	trackEntry := domain.LastFmDislike{
		ID: "d1", Kind: domain.LastFmDislikeTrack,
		MatchKey: trackKey, Title: "Song", Artist: "Artist", CreatedAt: now,
	}
	stored, err := repo.AddDislike(ctx, trackEntry)
	if err != nil {
		t.Fatalf("AddDislike: %v", err)
	}
	if stored.ID != trackEntry.ID || !stored.CreatedAt.Equal(now) {
		t.Fatalf("stored = %+v, want the inserted row", stored)
	}

	// A second dislike of the same identity is idempotent and keeps the first
	// row, so the settings list has one entry per song.
	repeat, err := repo.AddDislike(ctx, domain.LastFmDislike{
		ID: "d2", Kind: domain.LastFmDislikeTrack,
		MatchKey: trackKey, Title: "Song", Artist: "Artist",
		CreatedAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("AddDislike repeat: %v", err)
	}
	if repeat.ID != "d1" {
		t.Fatalf("repeat = %+v, want the original row", repeat)
	}

	if _, err := repo.AddDislike(ctx, domain.LastFmDislike{
		ID: "d3", Kind: domain.LastFmDislikeArtist,
		MatchKey: "artist", Artist: "Artist", CreatedAt: now,
	}); err != nil {
		t.Fatalf("AddDislike artist: %v", err)
	}

	rows, err := repo.ListDislikes(ctx)
	if err != nil {
		t.Fatalf("ListDislikes: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want one track and one artist", rows)
	}

	if err := repo.DeleteDislike(ctx, "d1"); err != nil {
		t.Fatalf("DeleteDislike: %v", err)
	}
	rows, err = repo.ListDislikes(ctx)
	if err != nil {
		t.Fatalf("ListDislikes after delete: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "d3" {
		t.Fatalf("rows = %+v, want only the artist row", rows)
	}
}

func TestLastFmDislikesMigrationDownAndUp(t *testing.T) {
	db, _ := newDislikeStore(t)

	hasTable := func(name string) bool {
		var count int
		if err := db.Get(&count,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name,
		); err != nil {
			t.Fatalf("sqlite_master query: %v", err)
		}
		return count > 0
	}

	driver, err := sqlite3.WithInstance(db.DB.DB, &sqlite3.Config{})
	if err != nil {
		t.Fatalf("failed to create migrate driver: %v", err)
	}
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("failed to create migration source: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite3", driver)
	if err != nil {
		t.Fatalf("failed to create migrator: %v", err)
	}

	if !hasTable("dislikes") {
		t.Fatal("the up migration did not create dislikes")
	}
	if err := m.Migrate(109); err != nil {
		t.Fatalf("down migration failed: %v", err)
	}
	if hasTable("dislikes") {
		t.Fatal("the down migration kept dislikes")
	}
	if err := m.Up(); err != nil {
		t.Fatalf("re-applying up migrations failed: %v", err)
	}
	if !hasTable("dislikes") {
		t.Fatal("the up migration did not recreate dislikes")
	}
}
