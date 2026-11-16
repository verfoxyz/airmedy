package lastfm

import (
	"context"
	"testing"
	"time"

	"airmedy/internal/domain"
)

type fakeTrackLookup struct {
	track *domain.TrackDTO
	err   error
}

func (f fakeTrackLookup) GetByID(context.Context, string) (*domain.TrackDTO, error) {
	return f.track, f.err
}

func localSurvivor() *domain.TrackDTO {
	return &domain.TrackDTO{
		Track: domain.Track{
			ID: "local-1", Path: `D:\Music\song.flac`, Title: "Song",
			Provider: "wy", ExternalID: "42", Availability: domain.AvailabilityReady,
		},
		Artists: []*domain.Artist{{Name: "Artist"}},
	}
}

// TestApplyTopTrackMatchClearsTheSourceForALocalHit: a promoted row keeps its
// remote identity, but the chip must read "in library" and nothing may remain
// to download.
func TestApplyTopTrackMatchClearsTheSourceForALocalHit(t *testing.T) {
	row := domain.CatalogSearchResult{
		Provider: "wy", ExternalID: "42", Title: "Song",
		State: domain.CatalogRowStateInLibrary, LocalTrackID: "local-1",
		Sources: []string{"wy"}, Downloadable: true,
	}
	item := TopTrackItem{Title: "Song", CoverURL: "https://lastfm/cover.jpg"}

	(&Recommender{}).applyTopTrackMatch(&item, &row)

	if item.Provider != "" || item.ExternalID != "" {
		t.Fatalf("item kept its source identity: %+v", item)
	}
	if len(item.Sources) != 0 {
		t.Fatalf("item kept its sources: %v", item.Sources)
	}
	if item.State != domain.CatalogRowStateInLibrary || item.LocalTrackID != "local-1" {
		t.Fatalf("item lost its local identity: %+v", item)
	}
}

// TestMatchedRecommendedClearsTheSourceForALocalHit: the older recommendation
// page applies the same rule.
func TestMatchedRecommendedClearsTheSourceForALocalHit(t *testing.T) {
	row := &domain.CatalogSearchResult{
		Provider: "wy", ExternalID: "42", Title: "Song",
		State: domain.CatalogRowStateInLibrary, LocalTrackID: "local-1",
		Sources: []string{"wy"},
	}
	got := (&Recommender{}).matchedRecommended(RecommendedTrack{Title: "Song", MatchKey: "k"}, row)

	if got.Provider != "" || got.ExternalID != "" || len(got.Sources) != 0 {
		t.Fatalf("recommendation kept its source identity: %+v", got)
	}
	if got.State != domain.CatalogRowStateInLibrary || got.LocalTrackID != "local-1" {
		t.Fatalf("recommendation lost its local identity: %+v", got)
	}
}

// TestRefreshLocalTrackUpdatesAnOpenRow: the page is open, so the in-memory
// copy and its per-source cache must stop reporting the provider.
func TestRefreshLocalTrackUpdatesAnOpenRow(t *testing.T) {
	recommender, capture := newTopTrackRecommender(connectedSource(nil), &fakeMatcher{})
	base := TopTrackItem{
		Kind: domain.LastFmRecommendationTopTracks, MatchKey: "artist\x00song", Title: "Song",
		Artist: "Artist", Artists: []string{"Artist"}, Matching: false,
	}
	recommender.topRows[recommendationRowKey(base.Kind, base.MatchKey)] = base
	cacheKey := recommendationRowKey(base.Kind, base.MatchKey) + "|wy"
	recommender.sourceMatches[cacheKey] = cachedSourceMatch{
		row:       &domain.CatalogSearchResult{Provider: "wy", ExternalID: "42", Title: "Song"},
		expiresAt: recommender.now().Add(time.Hour),
	}

	recommender.RefreshLocalTrack(localSurvivor())

	capture.mu.Lock()
	updates := append([]TopTrackUpdate{}, capture.updates...)
	capture.mu.Unlock()
	if len(updates) != 1 || len(updates[0].Items) != 1 {
		t.Fatalf("updates = %+v, want one row", updates)
	}
	item := updates[0].Items[0]
	if item.Provider != "" || item.State != domain.CatalogRowStateInLibrary || item.LocalTrackID != "local-1" {
		t.Fatalf("event row = %+v, want the local row", item)
	}

	recommender.mu.Lock()
	_, cached := recommender.sourceMatches[cacheKey]
	recommender.mu.Unlock()
	if cached {
		t.Fatal("the stale per-source match survived the refresh")
	}
}

// TestRefreshAfterMergeUpdatesStoredRows: a merged row is persisted as a local
// hit even when the list page is closed, and the event carries the new state.
func TestRefreshAfterMergeUpdatesStoredRows(t *testing.T) {
	recommender, capture, store := newStoredRecommender(connectedSource(nil), &fakeMatcher{})
	kind := domain.LastFmRecommendationTopTracks
	store.lists[kind] = domain.RecommendationList{
		Kind:  kind,
		Title: recommendationTitle(kind),
		Items: []domain.RecommendationItem{{
			Kind:       kind,
			MatchKey:   "artist\x00song",
			Title:      "Song",
			Artist:     "Artist",
			Artists:    []string{"Artist"},
			Provider:   "wy",
			ExternalID: "42",
			Sources:    []string{"wy"},
			Matched:    true,
		}},
	}
	recommender.SetTrackLookup(fakeTrackLookup{track: localSurvivor()})

	recommender.RefreshAfterMerge(context.Background(), "local-1")

	stored, err := store.GetRecommendationList(context.Background(), kind)
	if err != nil || stored == nil || len(stored.Items) != 1 {
		t.Fatalf("stored list = %+v, err = %v", stored, err)
	}
	row := stored.Items[0]
	if row.Provider != "" || row.ExternalID != "" || len(row.Sources) != 0 {
		t.Fatalf("stored row kept its source identity: %+v", row)
	}
	if row.State != domain.CatalogRowStateInLibrary || row.LocalTrackID != "local-1" {
		t.Fatalf("stored row was not marked local: %+v", row)
	}
	if !row.Matched || row.Matching || row.NoSource {
		t.Fatalf("stored row flags = %+v, want matched only", row)
	}

	capture.mu.Lock()
	updates := append([]TopTrackUpdate{}, capture.updates...)
	capture.mu.Unlock()
	if len(updates) != 1 || len(updates[0].Items) != 1 {
		t.Fatalf("updates = %+v, want one row", updates)
	}
	if updates[0].Kind != kind || updates[0].Items[0].LocalTrackID != "local-1" {
		t.Fatalf("event = %+v, want the top-tracks row for local-1", updates[0])
	}
}

// TestRefreshLocalTrackLeavesClosedListsAlone: a library update must not scan
// (or rewrite) the stored lists when nothing is cached in memory.
func TestRefreshLocalTrackLeavesClosedListsAlone(t *testing.T) {
	recommender, _, store := newStoredRecommender(connectedSource(nil), &fakeMatcher{})
	kind := domain.LastFmRecommendationTopTracks
	store.lists[kind] = domain.RecommendationList{
		Kind: kind,
		Items: []domain.RecommendationItem{{
			Kind: kind, MatchKey: "artist\x00song", Title: "Song",
			Provider: "wy", ExternalID: "42", Matched: true,
		}},
	}

	recommender.RefreshLocalTrack(localSurvivor())

	stored, _ := store.GetRecommendationList(context.Background(), kind)
	if stored == nil || stored.Items[0].Provider != "wy" {
		t.Fatalf("closed list was rewritten: %+v", stored)
	}
}
