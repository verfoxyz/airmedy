package sqlite

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// TestCoverCacheURLMigration_UpDown proves the column a promotion needs to find
// the remote image again can be added and removed cleanly.
func TestCoverCacheURLMigration_UpDown(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "airmedy.db"), slog.Default())
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	hasColumn := func() bool {
		var count int
		if err := db.Get(&count,
			`SELECT COUNT(*) FROM pragma_table_info('cover_cache') WHERE name = 'url'`,
		); err != nil {
			t.Fatalf("pragma_table_info query: %v", err)
		}
		return count > 0
	}

	if !hasColumn() {
		t.Fatal("cover_cache.url is missing after the up migration")
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

	if err := m.Migrate(92); err != nil {
		t.Fatalf("down migration failed: %v", err)
	}
	if hasColumn() {
		t.Fatal("cover_cache.url should be gone after the down migration")
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("re-applying up migration failed: %v", err)
	}
	if !hasColumn() {
		t.Fatal("cover_cache.url is missing after re-applying the up migration")
	}
}

// TestCoverCacheLinksMigrationBackfillsIdentity proves the many-to-many links
// table replaces the single-value identity columns without losing them: rows
// written before the migration get their identity copied into links.
func TestCoverCacheLinksMigrationBackfillsIdentity(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "airmedy.db"), slog.Default())
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

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

	// Step back to the schema that had only the single-value identity columns.
	if err := m.Migrate(96); err != nil {
		t.Fatalf("migrate to 96: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO cover_cache (url_hash, path, bytes, mime, last_used, provider, external_id, pinned, url)
		 VALUES ('hash-1', 'hash-1.jpg', 10, 'image/jpeg', 42, 'wy', '1336689106', 0, 'https://cdn/cover.jpg')`,
	); err != nil {
		t.Fatalf("insert legacy cover row: %v", err)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("migrate to latest: %v", err)
	}

	var provider, externalID string
	if err := db.QueryRow(
		`SELECT provider, external_id FROM cover_cache_links WHERE url_hash = 'hash-1'`,
	).Scan(&provider, &externalID); err != nil {
		t.Fatalf("backfilled link missing: %v", err)
	}
	if provider != "wy" || externalID != "1336689106" {
		t.Fatalf("backfilled link = %s/%s, want wy/1336689106", provider, externalID)
	}

	// The down migration removes the links table and leaves cover_cache intact.
	if err := m.Migrate(96); err != nil {
		t.Fatalf("down migration failed: %v", err)
	}
	var tables int
	if err := db.Get(&tables,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'cover_cache_links'`,
	); err != nil {
		t.Fatalf("sqlite_master query: %v", err)
	}
	if tables != 0 {
		t.Fatal("cover_cache_links should be gone after the down migration")
	}
	var rows int
	if err := db.Get(&rows, `SELECT COUNT(*) FROM cover_cache`); err != nil {
		t.Fatalf("count cover rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("cover_cache rows = %d, want the legacy row kept", rows)
	}
}
