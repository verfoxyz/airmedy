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

func newRecommendationStore(t *testing.T) (*DB, domain.LastFmRecommendationRepository) {
	t.Helper()
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, NewLastFmRecommendationRepository(db)
}

func TestRecommendationRepositorySavesReadsAndUpdatesRows(t *testing.T) {
	_, repo := newRecommendationStore(t)
	ctx := context.Background()
	updatedAt := time.Unix(1_700_000_000, 0).UTC()

	top := &domain.RecommendationList{
		Kind:  domain.LastFmRecommendationTopTracks,
		Title: "lastfm.top",
		Items: []domain.RecommendationItem{
			{
				Kind: domain.LastFmRecommendationTopTracks, MatchKey: "a\x00one",
				Title: "One", Artist: "A", Artists: []string{"A", "B"},
				Duration: 200, CoverURL: "https://cover/1.jpg", Sources: []string{"wy"},
				Matching: true,
			},
			{
				Kind: domain.LastFmRecommendationTopTracks, MatchKey: "a\x00two",
				Title: "Two", Artist: "A", Duration: 180, CoverURL: "https://cover/2.jpg",
				Matching: true,
			},
		},
		UpdatedAt: updatedAt,
	}
	similar := &domain.RecommendationList{
		Kind:  domain.LastFmRecommendationSimilar,
		Title: "lastfm.similar_tracks",
		Items: []domain.RecommendationItem{{
			Kind: domain.LastFmRecommendationSimilar, MatchKey: "b\x00rec",
			Title: "Rec", Artist: "B", Matching: true,
		}},
		UpdatedAt: updatedAt.Add(time.Minute),
	}
	for _, list := range []*domain.RecommendationList{top, similar} {
		if err := repo.SaveRecommendationList(ctx, list); err != nil {
			t.Fatalf("SaveRecommendationList(%s): %v", list.Kind, err)
		}
	}

	got, err := repo.GetRecommendationList(ctx, domain.LastFmRecommendationTopTracks)
	if err != nil || got == nil {
		t.Fatalf("GetRecommendationList = %+v, %v", got, err)
	}
	if len(got.Items) != 2 || got.Items[0].Title != "One" || got.Items[1].Title != "Two" {
		t.Fatalf("items = %+v, want Last.fm order", got.Items)
	}
	if len(got.Items[0].Artists) != 2 || got.Items[0].Sources[0] != "wy" {
		t.Fatalf("row json columns = %+v", got.Items[0])
	}

	summaries, err := repo.ListRecommendationLists(ctx)
	if err != nil {
		t.Fatalf("ListRecommendationLists: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summaries = %+v, want both lists", summaries)
	}
	for _, summary := range summaries {
		switch summary.Kind {
		case domain.LastFmRecommendationTopTracks:
			if summary.TrackCount != 2 || summary.MatchingCount != 2 ||
				summary.TotalDuration != 380 || len(summary.CoverURLs) != 2 {
				t.Fatalf("top summary = %+v", summary)
			}
		case domain.LastFmRecommendationSimilar:
			if summary.TrackCount != 1 || summary.MatchingCount != 1 {
				t.Fatalf("similar summary = %+v", summary)
			}
		}
	}

	pending, err := repo.PendingRecommendationItems(ctx)
	if err != nil || len(pending) != 3 {
		t.Fatalf("pending = %+v, %v, want every matching row", pending, err)
	}

	top.Items[0].Matching = false
	top.Items[0].Matched = true
	top.Items[0].Provider = "wy"
	top.Items[0].ExternalID = "9"
	top.Items[0].Sources = []string{"wy"}
	top.Items[0].Suspicious = true
	if err := repo.UpdateRecommendationItems(ctx, []domain.RecommendationItem{top.Items[0]}); err != nil {
		t.Fatalf("UpdateRecommendationItems: %v", err)
	}
	got, _ = repo.GetRecommendationList(ctx, domain.LastFmRecommendationTopTracks)
	if got.Items[0].Matching || !got.Items[0].Matched || got.Items[0].Provider != "wy" ||
		!got.Items[0].Suspicious {
		t.Fatalf("updated row = %+v, want the persisted match and flag", got.Items[0])
	}
	pending, _ = repo.PendingRecommendationItems(ctx)
	if len(pending) != 2 {
		t.Fatalf("pending after update = %d, want 2", len(pending))
	}
}

func TestRecommendationRepositoryMissingList(t *testing.T) {
	_, repo := newRecommendationStore(t)
	list, err := repo.GetRecommendationList(context.Background(), domain.LastFmRecommendationTopTracks)
	if err != nil || list != nil {
		t.Fatalf("missing list = %+v, %v, want nil, nil", list, err)
	}
}

func TestRecommendationRepositoryKeepsWeeklyFieldsAndCandidates(t *testing.T) {
	_, repo := newRecommendationStore(t)
	ctx := context.Background()
	const kind = "weekly_echo:1700000000"
	list := &domain.RecommendationList{
		Kind:      kind,
		Title:     "lastfm.weekly_echo",
		WeekFrom:  1_700_000_000,
		WeekTo:    1_700_604_800,
		UpdatedAt: time.Unix(1_700_500_000, 0).UTC(),
		Items: []domain.RecommendationItem{{
			Kind: kind, MatchKey: "a\x00echo", Title: "Echo", Artist: "A",
			PlayCount: 9, WeekFrom: 1_700_000_000, WeekTo: 1_700_604_800,
			Matching: true,
		}},
	}
	if err := repo.SaveRecommendationList(ctx, list); err != nil {
		t.Fatalf("SaveRecommendationList: %v", err)
	}

	got, err := repo.GetRecommendationList(ctx, kind)
	if err != nil || got == nil {
		t.Fatalf("GetRecommendationList = %+v, %v", got, err)
	}
	if got.WeekFrom != list.WeekFrom || got.WeekTo != list.WeekTo {
		t.Fatalf("header window = %d-%d, want %d-%d",
			got.WeekFrom, got.WeekTo, list.WeekFrom, list.WeekTo)
	}
	if len(got.Items) != 1 || got.Items[0].PlayCount != 9 ||
		got.Items[0].WeekFrom != list.WeekFrom || got.Items[0].WeekTo != list.WeekTo {
		t.Fatalf("weekly row = %+v, want playcount and window", got.Items)
	}

	summaries, err := repo.ListRecommendationLists(ctx)
	if err != nil || len(summaries) != 1 {
		t.Fatalf("summaries = %+v, %v", summaries, err)
	}
	if summaries[0].WeekFrom != list.WeekFrom || summaries[0].WeekTo != list.WeekTo {
		t.Fatalf("summary window = %+v", summaries[0])
	}

	pool := []domain.RecommendationItem{{
		Kind: kind, MatchKey: "b\x00next", Title: "Next", Artist: "B", Matching: true,
	}}
	if err := repo.SaveRecommendationCandidates(ctx, kind, pool); err != nil {
		t.Fatalf("SaveRecommendationCandidates: %v", err)
	}
	// Saving the rows again must not clear the pool stored in payload.
	if err := repo.SaveRecommendationList(ctx, list); err != nil {
		t.Fatalf("re-save list: %v", err)
	}
	gotPool, err := repo.GetRecommendationCandidates(ctx, kind)
	if err != nil || len(gotPool) != 1 || gotPool[0].Title != "Next" {
		t.Fatalf("candidates = %+v, %v, want the stored pool", gotPool, err)
	}
}

func TestRecommendationListsMigration_UpDown(t *testing.T) {
	db, _ := newRecommendationStore(t)
	ctx := context.Background()

	hasTable := func() bool {
		var count int
		if err := db.GetContext(ctx, &count,
			`SELECT COUNT(*) FROM sqlite_master
			 WHERE type = 'table' AND name = 'lastfm_recommendation_items'`,
		); err != nil {
			t.Fatalf("sqlite_master query: %v", err)
		}
		return count > 0
	}
	if !hasTable() {
		t.Fatal("lastfm_recommendation_items is missing after the up migration")
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

	if err := m.Migrate(99); err != nil {
		t.Fatalf("down migration failed: %v", err)
	}
	if hasTable() {
		t.Fatal("lastfm_recommendation_items should be gone after the down migration")
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("re-applying up migration failed: %v", err)
	}
	if !hasTable() {
		t.Fatal("lastfm_recommendation_items is missing after re-applying the up migration")
	}
}
