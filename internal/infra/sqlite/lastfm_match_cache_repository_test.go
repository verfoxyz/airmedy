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

func newMatchCacheStore(t *testing.T) (*DB, domain.LastFmMatchCacheRepository) {
	t.Helper()
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, NewLastFmMatchCacheRepository(db)
}

func TestMatchCacheSavesReadsAndExpires(t *testing.T) {
	_, repo := newMatchCacheStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	live := domain.LastFmMatchCacheEntry{
		MatchKey: "a\x00one", Provider: "wy", ExternalID: "1",
		CoverURL: "https://cover/1.jpg", Duration: 200,
		UpdatedAt: now, ExpiresAt: now.Add(14 * 24 * time.Hour),
	}
	local := domain.LastFmMatchCacheEntry{
		MatchKey: "a\x00two", LocalTrackID: "local-2", Suspicious: true,
		UpdatedAt: now, ExpiresAt: now.Add(7 * 24 * time.Hour),
	}
	expired := domain.LastFmMatchCacheEntry{
		MatchKey: "a\x00gone", NoSource: true,
		UpdatedAt: now.Add(-4 * 24 * time.Hour), ExpiresAt: now.Add(-time.Hour),
	}
	if err := repo.SaveMatchCache(
		ctx, []domain.LastFmMatchCacheEntry{live, local, expired}, now, 5000,
	); err != nil {
		t.Fatalf("SaveMatchCache: %v", err)
	}

	entries, err := repo.GetMatchCache(ctx, []string{live.MatchKey, local.MatchKey, expired.MatchKey}, now)
	if err != nil {
		t.Fatalf("GetMatchCache: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want the two live rows", entries)
	}
	byKey := make(map[string]domain.LastFmMatchCacheEntry, len(entries))
	for _, entry := range entries {
		byKey[entry.MatchKey] = entry
	}
	if got := byKey[live.MatchKey]; got.Provider != "wy" || got.ExternalID != "1" ||
		got.CoverURL != "https://cover/1.jpg" || got.Duration != 200 {
		t.Fatalf("live entry = %+v", got)
	}
	if got := byKey[local.MatchKey]; got.LocalTrackID != "local-2" || !got.Suspicious {
		t.Fatalf("local entry = %+v", got)
	}

	if err := repo.DeleteMatchCache(ctx, []string{live.MatchKey}); err != nil {
		t.Fatalf("DeleteMatchCache: %v", err)
	}
	entries, err = repo.GetMatchCache(ctx, []string{live.MatchKey}, now)
	if err != nil {
		t.Fatalf("GetMatchCache after delete: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("deleted entry still returned: %+v", entries)
	}
}

func TestMatchCachePruneKeepsNewestRows(t *testing.T) {
	db, repo := newMatchCacheStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	entries := make([]domain.LastFmMatchCacheEntry, 0, 5)
	for index := 0; index < 5; index++ {
		entries = append(entries, domain.LastFmMatchCacheEntry{
			MatchKey: "key-" + string(rune('a'+index)), Provider: "wy",
			UpdatedAt: now.Add(-time.Duration(index) * time.Hour),
			ExpiresAt: now.Add(24 * time.Hour),
		})
	}
	entries = append(entries, domain.LastFmMatchCacheEntry{
		MatchKey: "expired", NoSource: true,
		UpdatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute),
	})
	if err := repo.SaveMatchCache(ctx, entries, now, 5000); err != nil {
		t.Fatalf("SaveMatchCache: %v", err)
	}

	if err := repo.PruneMatchCache(ctx, now, 2); err != nil {
		t.Fatalf("PruneMatchCache: %v", err)
	}
	var count int
	if err := db.Get(&count, `SELECT COUNT(*) FROM lastfm_match_cache`); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 2 {
		t.Fatalf("rows = %d, want the newest two", count)
	}
	keys := make([]string, 0, 2)
	if err := db.Select(&keys, `SELECT match_key FROM lastfm_match_cache ORDER BY match_key`); err != nil {
		t.Fatalf("select keys: %v", err)
	}
	if len(keys) != 2 || keys[0] != "key-a" || keys[1] != "key-b" {
		t.Fatalf("keys = %v, want the two newest entries", keys)
	}
}

func TestResetRecommendationMatchesRequeuesRows(t *testing.T) {
	_, repo := newRecommendationStore(t)
	ctx := context.Background()
	matchKey := "artist\x00hit"
	if err := repo.SaveRecommendationList(ctx, &domain.RecommendationList{
		Kind: domain.LastFmRecommendationTopTracks, Title: "lastfm.top",
		Items: []domain.RecommendationItem{
			{
				Kind: domain.LastFmRecommendationTopTracks, MatchKey: matchKey,
				Title: "Hit", Artist: "Artist", Matched: true,
				Provider: "wy", ExternalID: "1", Sources: []string{"wy"}, Downloadable: true,
			},
			{
				Kind: domain.LastFmRecommendationTopTracks, MatchKey: "artist\x00other",
				Title: "Other", Artist: "Artist", Matched: true, Provider: "kg", ExternalID: "2",
			},
		},
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveRecommendationList: %v", err)
	}

	affected, err := repo.ResetRecommendationMatches(ctx, []string{matchKey})
	if err != nil {
		t.Fatalf("ResetRecommendationMatches: %v", err)
	}
	if affected != 1 {
		t.Fatalf("affected = %d, want 1", affected)
	}
	list, err := repo.GetRecommendationList(ctx, domain.LastFmRecommendationTopTracks)
	if err != nil || list == nil {
		t.Fatalf("GetRecommendationList = %+v, %v", list, err)
	}
	reset := list.Items[0]
	if !reset.Matching || reset.Provider != "" || reset.ExternalID != "" ||
		reset.Matched || reset.Downloadable {
		t.Fatalf("reset row = %+v, want it back in the queue", reset)
	}
	if other := list.Items[1]; other.Provider != "kg" || other.Matching {
		t.Fatalf("untouched row = %+v", other)
	}
}

func TestLastFmMatchCacheMigrationDownAndUp(t *testing.T) {
	db, _ := newMatchCacheStore(t)

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

	if !hasTable("lastfm_match_cache") {
		t.Fatal("the up migration did not create lastfm_match_cache")
	}
	if err := m.Migrate(108); err != nil {
		t.Fatalf("down migration failed: %v", err)
	}
	if hasTable("lastfm_match_cache") {
		t.Fatal("the down migration kept lastfm_match_cache")
	}
	if err := m.Up(); err != nil {
		t.Fatalf("re-applying up migrations failed: %v", err)
	}
	if !hasTable("lastfm_match_cache") {
		t.Fatal("the up migration did not recreate lastfm_match_cache")
	}
}
