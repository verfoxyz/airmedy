package sqlite

import (
	"context"
	"log/slog"
	"testing"

	"airmedy/internal/domain"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

func newStreamCacheTestRepo(t *testing.T) (*DB, domain.StreamCacheRepository) {
	t.Helper()
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, NewStreamCacheRepository(db)
}

func TestStreamCacheRepository_UpsertAndUpdates(t *testing.T) {
	_, repo := newStreamCacheTestRepo(t)
	ctx := context.Background()

	entry := &domain.StreamCacheEntry{
		CacheKey: "key-1", Provider: "wy", ExternalID: "42",
		Title: "晴天", TitleKey: "晴天", Artist: "周杰伦", ArtistKey: "周杰伦",
		Artists: "周杰伦", Album: "叶惠美", DurationMs: 269000,
		Quality: "320k", AudioPath: "key-1.audio",
		CreatedAt: 100, LastUsed: 100,
	}
	if err := repo.Upsert(ctx, entry); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := repo.GetByIdentity(ctx, "wy", "42")
	if err != nil {
		t.Fatalf("GetByIdentity: %v", err)
	}
	if got == nil || got.CacheKey != "key-1" || got.DurationMs != 269000 || got.Quality != "320k" {
		t.Fatalf("GetByIdentity = %+v", got)
	}

	if err := repo.UpdateProgress(ctx, "key-1", 42.5); err != nil {
		t.Fatalf("UpdateProgress: %v", err)
	}
	if err := repo.UpdateComplete(ctx, "key-1", 1000, 400, false); err != nil {
		t.Fatalf("UpdateComplete: %v", err)
	}
	got, _ = repo.GetByIdentity(ctx, "wy", "42")
	if got.PlaybackProgress != 42.5 || got.TotalBytes != 1000 || got.ReceivedBytes != 400 {
		t.Fatalf("updates not applied: %+v", got)
	}

	// created_at must survive a re-upsert.
	entry.LastUsed = 200
	if err := repo.Upsert(ctx, entry); err != nil {
		t.Fatalf("Upsert again: %v", err)
	}
	got, _ = repo.GetByIdentity(ctx, "wy", "42")
	if got.LastUsed != 200 || got.CreatedAt != 100 {
		t.Fatalf("timestamps after upsert = (%d, %d), want (100, 200)", got.CreatedAt, got.LastUsed)
	}

	if err := repo.Delete(ctx, "key-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, _ = repo.GetByIdentity(ctx, "wy", "42"); got != nil {
		t.Fatalf("entry survived Delete: %+v", got)
	}
}

func TestStreamCacheRepository_MatchByTitleArtist(t *testing.T) {
	_, repo := newStreamCacheTestRepo(t)
	ctx := context.Background()

	seed := func(key, title, artist string, durationMs, lastUsed int64) {
		t.Helper()
		if err := repo.Upsert(ctx, &domain.StreamCacheEntry{
			CacheKey: key, Provider: "wy", ExternalID: key,
			Title: title, TitleKey: domain.NormalizationKey(title),
			Artist: artist, ArtistKey: domain.NormalizationKey(artist),
			DurationMs: durationMs, AudioPath: key + ".audio", LastUsed: lastUsed,
		}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}

	seed("older", "屋顶", "周杰伦", 200000, 10)
	seed("newer", "屋顶", "周杰伦", 203000, 20)
	seed("other-duration", "屋顶", "周杰伦", 260000, 30)
	seed("other-artist", "屋顶", "温岚", 200000, 40)

	key := domain.NormalizationKey("屋顶")
	artistKey := domain.NormalizationKey("周杰伦")

	got, err := repo.MatchByTitleArtist(ctx, key, artistKey, 201000, 5000)
	if err != nil {
		t.Fatalf("MatchByTitleArtist: %v", err)
	}
	if got == nil || got.CacheKey != "newer" {
		t.Fatalf("match = %+v, want the most recently used within tolerance (newer)", got)
	}

	// 60 s away from every seeded duration: no match.
	if got, err = repo.MatchByTitleArtist(ctx, key, artistKey, 320000, 5000); err != nil {
		t.Fatalf("MatchByTitleArtist out of tolerance: %v", err)
	} else if got != nil {
		t.Fatalf("out-of-tolerance match = %+v, want nil", got)
	}

	// A row without a duration still matches; the query only compares when both
	// sides know it.
	seed("unknown-duration", "屋顶", "周杰伦", 0, 50)
	if got, err = repo.MatchByTitleArtist(ctx, key, artistKey, 999000, 5000); err != nil {
		t.Fatalf("MatchByTitleArtist zero duration: %v", err)
	} else if got == nil || got.CacheKey != "unknown-duration" {
		t.Fatalf("zero-duration match = %+v, want unknown-duration", got)
	}

	// And a zero-duration query ignores the stored durations too.
	if got, err = repo.MatchByTitleArtist(ctx, key, artistKey, 0, 5000); err != nil {
		t.Fatalf("MatchByTitleArtist zero query: %v", err)
	} else if got == nil || got.LastUsed != 50 {
		t.Fatalf("zero-duration query = %+v, want the newest row", got)
	}
}

func TestStreamCacheRepository_ListForEvictionIsLeastRecentlyUsedFirst(t *testing.T) {
	_, repo := newStreamCacheTestRepo(t)
	ctx := context.Background()

	for key, lastUsed := range map[string]int64{"a": 30, "b": 10, "c": 20} {
		if err := repo.Upsert(ctx, &domain.StreamCacheEntry{
			CacheKey: key, Provider: "wy", ExternalID: key,
			AudioPath: key + ".audio", LastUsed: lastUsed,
		}); err != nil {
			t.Fatalf("Upsert %s: %v", key, err)
		}
	}

	entries, err := repo.ListForEviction(ctx)
	if err != nil {
		t.Fatalf("ListForEviction: %v", err)
	}
	if len(entries) != 3 || entries[0].CacheKey != "b" || entries[1].CacheKey != "c" || entries[2].CacheKey != "a" {
		t.Fatalf("order = %+v, want b, c, a", entries)
	}
}

func TestStreamCacheMigration_UpDown(t *testing.T) {
	db, _ := newStreamCacheTestRepo(t)
	tableExists := func() bool {
		var count int
		if err := db.Get(&count,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'stream_cache'`,
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

	if !tableExists() {
		t.Fatal("stream_cache is missing after the up migration")
	}
	// The stream_cache migration is 91; step past the later ones so the down/up
	// under test is that migration.
	if err := m.Migrate(90); err != nil {
		t.Fatalf("down migration failed: %v", err)
	}
	if tableExists() {
		t.Fatal("stream_cache should be gone after the down migration")
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("re-applying up migration failed: %v", err)
	}
	if !tableExists() {
		t.Fatal("stream_cache is missing after re-applying the up migration")
	}
}

// TestRetiredPromotionSchemaIsDropped proves the promotion columns and history
// table still exist at migration 103 (the upgrade point) and are gone at the
// latest version, where media requests are authoritative.
func TestRetiredPromotionSchemaIsDropped(t *testing.T) {
	db, _ := newStreamCacheTestRepo(t)

	hasColumn := func(name string) bool {
		var count int
		if err := db.Get(&count,
			`SELECT COUNT(*) FROM pragma_table_info('stream_cache') WHERE name = ?`, name,
		); err != nil {
			t.Fatalf("pragma_table_info query: %v", err)
		}
		return count > 0
	}
	hasTable := func(name string) bool {
		var count int
		if err := db.Get(&count,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name,
		); err != nil {
			t.Fatalf("sqlite_master query: %v", err)
		}
		return count > 0
	}

	if hasColumn("promoted") || hasColumn("discard_pending") || hasColumn("pending_promote") {
		t.Fatal("the retired promotion columns are still present at the latest version")
	}
	if hasTable("promotion_tasks") {
		t.Fatal("promotion_tasks is still present at the latest version")
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

	if err := m.Migrate(103); err != nil {
		t.Fatalf("down migration failed: %v", err)
	}
	if !hasColumn("promoted") || !hasColumn("discard_pending") || !hasTable("promotion_tasks") {
		t.Fatal("the promotion schema should exist before 000104/000105")
	}
	if err := m.Up(); err != nil {
		t.Fatalf("re-applying up migrations failed: %v", err)
	}
	if hasColumn("promoted") || hasColumn("discard_pending") || hasTable("promotion_tasks") {
		t.Fatal("the promotion schema is back after re-applying the up migrations")
	}
}

// TestUpgradeFromVersion95RetiresThePromotionSchema covers the upgrade path that
// a fresh-database test cannot: a database that already ran 000094/000095 must
// reach the latest version without being marked dirty, and must not keep the
// retired columns.
func TestUpgradeFromVersion95RetiresThePromotionSchema(t *testing.T) {
	db, _ := newStreamCacheTestRepo(t)

	hasColumn := func(name string) bool {
		var count int
		if err := db.Get(&count,
			`SELECT COUNT(*) FROM pragma_table_info('stream_cache') WHERE name = ?`, name,
		); err != nil {
			t.Fatalf("pragma_table_info query: %v", err)
		}
		return count > 0
	}
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

	// Stop at 95: promoted exists, discard_pending does not yet.
	if err := m.Migrate(95); err != nil {
		t.Fatalf("migrate to 95: %v", err)
	}
	if !hasColumn("promoted") || hasColumn("discard_pending") {
		t.Fatal("the version-95 fixture does not match a database upgraded before 000096")
	}

	if err := m.Up(); err != nil {
		t.Fatalf("upgrade to latest: %v", err)
	}
	version, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("migration version: %v", err)
	}
	if dirty {
		t.Fatal("the upgrade left the schema dirty")
	}
	if version != 108 {
		t.Fatalf("version = %d, want 108", version)
	}
	if hasColumn("promoted") || hasColumn("discard_pending") || hasTable("promotion_tasks") {
		t.Fatal("the upgrade kept the retired promotion schema")
	}
	if !hasTable("media_requests") {
		t.Fatal("the upgraded database is missing media_requests")
	}
}
