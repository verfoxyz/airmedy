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
