package sqlite

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"airmedy/internal/domain"
)

func TestDislikePurgeClearsItemsCandidatesAndMatchCache(t *testing.T) {
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	now := time.Now().UTC()

	lists := NewLastFmRecommendationRepository(db)
	cache := NewLastFmMatchCacheRepository(db)
	purger := NewLastFmDislikePurgeRepository(db)

	drop := "drop\x00song"
	keep := "keep\x00song"
	if err := lists.SaveRecommendationList(ctx, &domain.RecommendationList{
		Kind:  domain.LastFmRecommendationDiscover,
		Title: "lastfm.discover",
		Items: []domain.RecommendationItem{
			{Kind: domain.LastFmRecommendationDiscover, MatchKey: drop, Title: "Song", Artist: "Drop"},
			{Kind: domain.LastFmRecommendationDiscover, MatchKey: keep, Title: "Song", Artist: "Keep"},
		},
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save list: %v", err)
	}
	if err := lists.SaveRecommendationCandidates(ctx, domain.LastFmRecommendationDiscover,
		[]domain.RecommendationItem{
			{Kind: domain.LastFmRecommendationDiscover, MatchKey: drop, Title: "Song", Artist: "Drop"},
			{Kind: domain.LastFmRecommendationDiscover, MatchKey: keep, Title: "Song", Artist: "Keep"},
		}); err != nil {
		t.Fatalf("save candidates: %v", err)
	}
	if err := cache.SaveMatchCache(ctx, []domain.LastFmMatchCacheEntry{{
		MatchKey: drop, Provider: "wy", ExternalID: "1",
		UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}, {
		MatchKey: keep, Provider: "tx", ExternalID: "2",
		UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}}, now, 100); err != nil {
		t.Fatalf("save cache: %v", err)
	}

	if err := purger.PurgeDislikeTraces(ctx, []string{drop}); err != nil {
		t.Fatalf("PurgeDislikeTraces: %v", err)
	}

	list, err := lists.GetRecommendationList(ctx, domain.LastFmRecommendationDiscover)
	if err != nil || list == nil {
		t.Fatalf("GetRecommendationList = %+v, %v", list, err)
	}
	if len(list.Items) != 1 || list.Items[0].MatchKey != keep {
		t.Fatalf("items = %+v, want only the kept row", list.Items)
	}
	pool, err := lists.GetRecommendationCandidates(ctx, domain.LastFmRecommendationDiscover)
	if err != nil {
		t.Fatalf("GetRecommendationCandidates: %v", err)
	}
	if len(pool) != 1 || pool[0].MatchKey != keep {
		t.Fatalf("pool = %+v, want only the kept row", pool)
	}
	entries, err := cache.GetMatchCache(ctx, []string{drop, keep}, now)
	if err != nil {
		t.Fatalf("GetMatchCache: %v", err)
	}
	if len(entries) != 1 || entries[0].MatchKey != keep {
		t.Fatalf("cache = %+v, want only the kept entry", entries)
	}
}
