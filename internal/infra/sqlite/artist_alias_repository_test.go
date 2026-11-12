package sqlite

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"airmedy/internal/domain"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

func newArtistAliasTestRepo(t *testing.T) domain.ArtistAliasRepository {
	t.Helper()
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewArtistAliasRepository(db)
}

func TestArtistAliasRepository_AddExpandDisableRemove(t *testing.T) {
	repo := newArtistAliasTestRepo(t)
	ctx := context.Background()

	if err := repo.AddArtistAlias(ctx, "camellia", "かめりあ", domain.ArtistAliasEvidenceMerge); err != nil {
		t.Fatalf("AddArtistAlias: %v", err)
	}

	// Expansion is bidirectional and keeps the input key.
	for _, input := range []string{"camellia", "かめりあ"} {
		expanded, err := repo.ExpandArtists(ctx, []string{input})
		if err != nil {
			t.Fatalf("ExpandArtists(%s): %v", input, err)
		}
		if len(expanded) != 2 {
			t.Fatalf("ExpandArtists(%s) = %v, want both keys", input, expanded)
		}
	}

	// Disabling keeps the row but removes it from matching.
	if err := repo.SetArtistAliasEnabled(ctx, "camellia", "かめりあ", false); err != nil {
		t.Fatalf("SetArtistAliasEnabled: %v", err)
	}
	expanded, err := repo.ExpandArtists(ctx, []string{"camellia"})
	if err != nil {
		t.Fatalf("ExpandArtists after disable: %v", err)
	}
	if len(expanded) != 1 || expanded[0] != "camellia" {
		t.Fatalf("ExpandArtists after disable = %v, want only the input", expanded)
	}
	list, err := repo.ListArtistAliases(ctx)
	if err != nil || len(list) != 1 || list[0].Enabled {
		t.Fatalf("ListArtistAliases after disable = %+v, %v", list, err)
	}

	// Re-enabling restores it immediately.
	if err := repo.SetArtistAliasEnabled(ctx, "camellia", "かめりあ", true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	expanded, _ = repo.ExpandArtists(ctx, []string{"camellia"})
	if len(expanded) != 2 {
		t.Fatalf("ExpandArtists after re-enable = %v, want both keys", expanded)
	}

	// Removing either direction deletes the pair.
	if err := repo.RemoveArtistAlias(ctx, "かめりあ", "camellia"); err != nil {
		t.Fatalf("RemoveArtistAlias: %v", err)
	}
	list, _ = repo.ListArtistAliases(ctx)
	if len(list) != 0 {
		t.Fatalf("ListArtistAliases after remove = %+v, want empty", list)
	}

	// A removed pair can be learned again (the next merge re-adds it).
	if err := repo.AddArtistAlias(ctx, "camellia", "かめりあ", domain.ArtistAliasEvidenceMerge); err != nil {
		t.Fatalf("re-adding a removed pair: %v", err)
	}
}

func TestArtistAliasRepository_RejectsDuplicateInEitherDirection(t *testing.T) {
	repo := newArtistAliasTestRepo(t)
	ctx := context.Background()

	if err := repo.AddArtistAlias(ctx, "a", "b", domain.ArtistAliasEvidenceManual); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := repo.AddArtistAlias(ctx, "b", "a", domain.ArtistAliasEvidenceManual); err != domain.ErrArtistAliasExists {
		t.Fatalf("reverse add error = %v, want ErrArtistAliasExists", err)
	}
	if err := repo.AddArtistAlias(ctx, "a", "b", domain.ArtistAliasEvidenceMerge); err != domain.ErrArtistAliasExists {
		t.Fatalf("same add error = %v, want ErrArtistAliasExists", err)
	}
}

func TestArtistAliasRepository_ExpandsOnlyOneHop(t *testing.T) {
	repo := newArtistAliasTestRepo(t)
	ctx := context.Background()

	if err := repo.AddArtistAlias(ctx, "a", "b", domain.ArtistAliasEvidenceManual); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddArtistAlias(ctx, "b", "c", domain.ArtistAliasEvidenceManual); err != nil {
		t.Fatal(err)
	}

	expanded, err := repo.ExpandArtists(ctx, []string{"a"})
	if err != nil {
		t.Fatalf("ExpandArtists: %v", err)
	}
	if len(expanded) != 2 {
		t.Fatalf("ExpandArtists(a) = %v, want a and b only", expanded)
	}
	for _, key := range expanded {
		if key == "c" {
			t.Fatal("expansion followed a second hop to c")
		}
	}
}

// TestArtistAliasMigration_UpDown proves the table and its index can be added
// and removed cleanly.
func TestArtistAliasMigration_UpDown(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "airmedy.db"), slog.Default())
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	hasTable := func() bool {
		var count int
		if err := db.Get(&count,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'artist_aliases'`,
		); err != nil {
			t.Fatalf("sqlite_master query: %v", err)
		}
		return count > 0
	}
	if !hasTable() {
		t.Fatal("artist_aliases is missing after the up migration")
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

	if err := m.Migrate(98); err != nil {
		t.Fatalf("down migration failed: %v", err)
	}
	if hasTable() {
		t.Fatal("artist_aliases should be gone after the down migration")
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("re-applying up migration failed: %v", err)
	}
	if !hasTable() {
		t.Fatal("artist_aliases is missing after re-applying the up migration")
	}
}
