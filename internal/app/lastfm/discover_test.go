package lastfm

import (
	"context"
	"fmt"
	"testing"
	"time"

	"airmedy/internal/domain"
)

func discoverListFromStore(t *testing.T, store *fakeRecommendationStore) domain.RecommendationList {
	t.Helper()
	list, err := store.GetRecommendationList(context.Background(), domain.LastFmRecommendationDiscover)
	if err != nil {
		t.Fatalf("stored discover list: %v", err)
	}
	if list == nil {
		t.Fatal("stored discover list is missing")
	}
	return *list
}

func discoverTitles(list domain.RecommendationList) []string {
	titles := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		titles = append(titles, item.Title)
	}
	return titles
}

func TestDiscoverSeedsRotateByWeekAndAreStableInsideOne(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Seed", "One")}
	source.topArtists["overall"] = []LastFmArtist{{Name: "Overall Artist"}}
	source.topArtists["6month"] = []LastFmArtist{{Name: "Recent Artist"}}
	recommender, _, _ := newStoredRecommender(source, sourceMatcher())
	wednesday := time.Date(2026, 1, 7, 12, 0, 0, 0, time.UTC)
	recommender.now = func() time.Time { return wednesday }

	if err := recommender.RefreshRecommendationList(
		context.Background(), domain.LastFmRecommendationDiscover); err != nil {
		t.Fatalf("first build: %v", err)
	}
	if err := recommender.RefreshRecommendationList(
		context.Background(), domain.LastFmRecommendationDiscover); err != nil {
		t.Fatalf("second build: %v", err)
	}
	if source.topArtistCalls != 1 {
		t.Fatalf("top artist calls = %d, want the weekly cache to answer", source.topArtistCalls)
	}

	recommender.now = func() time.Time { return wednesday.AddDate(0, 0, 7) }
	if err := recommender.RefreshRecommendationList(
		context.Background(), domain.LastFmRecommendationDiscover); err != nil {
		t.Fatalf("next week build: %v", err)
	}
	if source.topArtistCalls != 2 {
		t.Fatalf("top artist calls = %d, want a new seed set next week", source.topArtistCalls)
	}
	if source.topArtistPeriods[0] == source.topArtistPeriods[1] {
		t.Fatalf("periods = %v, want the artist window to rotate", source.topArtistPeriods)
	}
}

func TestDiscoverFiltersLocalRecentSeedsAndCapsPerArtist(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Seed", "One")}
	source.similarTracks[similarTracksKey("Seed", "One")] = []LastFmTrack{
		track("Artist A", "Heard Recently"),
		track("Artist A", "Owned Locally"),
		track("Artist A", "Fresh One"),
		track("Artist A", "Fresh Two"),
		track("Artist A", "Fresh Three"),
		track("Artist B", "Fresh Four"),
	}
	source.recent = []LastFmTrack{track("Artist A", "Heard Recently")}
	matcher := sourceMatcher()
	matcher.localRows = map[string]*domain.TrackDTO{
		"Artist A\x00Owned Locally": {Track: domain.Track{
			ID: "local-1", Title: "Owned Locally",
		}},
	}
	source.topArtists["overall"] = nil
	recommender, _, store := newStoredRecommender(source, matcher)
	recommender.now = fixedClock(1_800_000_000)

	if err := recommender.RefreshRecommendationList(
		context.Background(), domain.LastFmRecommendationDiscover); err != nil {
		t.Fatalf("build: %v", err)
	}

	list := discoverListFromStore(t, store)
	titles := discoverTitles(list)
	for _, unwanted := range []string{"Heard Recently", "Owned Locally", "One"} {
		for _, title := range titles {
			if title == unwanted {
				t.Fatalf("discover titles = %v, want %q excluded", titles, unwanted)
			}
		}
	}
	perArtist := make(map[string]int)
	for _, item := range list.Items {
		perArtist[item.Artist]++
	}
	if perArtist["Artist A"] > discoverPerArtist {
		t.Fatalf("Artist A rows = %d, want at most %d", perArtist["Artist A"], discoverPerArtist)
	}
	if len(titles) != 3 {
		t.Fatalf("discover titles = %v, want the three fresh rows", titles)
	}
}

func TestDiscoverStopsAtTheRequestBudget(t *testing.T) {
	source := connectedSource(nil)
	for index := 0; index < 10; index++ {
		source.top[topTrackPeriod] = append(
			source.top[topTrackPeriod], track(fmt.Sprintf("Seed %d", index), "Song"))
	}
	for index := 0; index < 10; index++ {
		artist := fmt.Sprintf("Artist %d", index)
		source.topArtists["overall"] = append(
			source.topArtists["overall"], LastFmArtist{Name: artist})
		for neighbour := 0; neighbour < 5; neighbour++ {
			source.similarArtists[artist] = append(
				source.similarArtists[artist], LastFmArtist{Name: fmt.Sprintf("%s N%d", artist, neighbour)})
		}
		for tag := 0; tag < 5; tag++ {
			tagName := fmt.Sprintf("%s tag %d", artist, tag)
			source.artistTopTags[artist] = append(
				source.artistTopTags[artist], LastFmTag{Name: tagName})
			source.tagTopTracks[tagName] = []LastFmTrack{
				track(fmt.Sprintf("%s tag artist", artist), tagName),
			}
		}
	}
	recommender, _, _ := newStoredRecommender(source, sourceMatcher())
	recommender.now = fixedClock(1_800_000_000)

	if err := recommender.RefreshRecommendationList(
		context.Background(), domain.LastFmRecommendationDiscover); err != nil {
		t.Fatalf("build: %v", err)
	}

	source.mu.Lock()
	used := source.similarTrackCalls + source.similarArtistCalls +
		source.artistTopTracksCalls + source.artistTopTagsCalls +
		source.tagTopTracksCalls + source.chartTopTracksCalls +
		source.topArtistCalls + len(source.recentPages)
	source.mu.Unlock()
	if used > discoverRequestBudget {
		t.Fatalf("requests = %d, want at most %d", used, discoverRequestBudget)
	}
	if used < 15 {
		t.Fatalf("requests = %d, want the expansion to actually run", used)
	}
}

func TestDiscoverLoadMoreAppendsPagesAndExhausts(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Seed", "One")}
	for index := 0; index < 45; index++ {
		source.similarTracks[similarTracksKey("Seed", "One")] = append(
			source.similarTracks[similarTracksKey("Seed", "One")],
			track(fmt.Sprintf("Artist %02d", index), "Song"),
		)
	}
	recommender, _, store := newStoredRecommender(source, sourceMatcher())
	recommender.now = fixedClock(1_800_000_000)

	if err := recommender.RefreshRecommendationList(
		context.Background(), domain.LastFmRecommendationDiscover); err != nil {
		t.Fatalf("build: %v", err)
	}
	if list := discoverListFromStore(t, store); len(list.Items) != discoverPageSize {
		t.Fatalf("first page = %d rows, want %d", len(list.Items), discoverPageSize)
	}

	first, err := recommender.LoadMoreRecommendationItems(
		context.Background(), domain.LastFmRecommendationDiscover)
	if err != nil {
		t.Fatalf("first load more: %v", err)
	}
	if len(first.Items) != 2*discoverPageSize || !first.HasMore {
		t.Fatalf("first load more = %d rows (more=%v), want %d and another page",
			len(first.Items), first.HasMore, 2*discoverPageSize)
	}

	second, err := recommender.LoadMoreRecommendationItems(
		context.Background(), domain.LastFmRecommendationDiscover)
	if err != nil {
		t.Fatalf("second load more: %v", err)
	}
	if len(second.Items) != 45 || second.HasMore {
		t.Fatalf("second load more = %d rows (more=%v), want every row and no more",
			len(second.Items), second.HasMore)
	}

	seen := make(map[string]struct{}, len(second.Items))
	for _, item := range second.Items {
		if _, duplicate := seen[item.MatchKey]; duplicate {
			t.Fatalf("duplicate row %q", item.MatchKey)
		}
		seen[item.MatchKey] = struct{}{}
	}
}

func TestDiscoverTTLIsCheckedOnlyWhenAutoRefreshIsOn(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Seed", "One")}
	source.similarTracks[similarTracksKey("Seed", "One")] = []LastFmTrack{
		track("Artist", "Fresh"),
	}
	recommender, _, store := newStoredRecommender(source, sourceMatcher())
	now := time.Unix(1_800_000_000, 0).UTC()
	recommender.now = func() time.Time { return now }

	if _, err := recommender.ListRecommendationListsWithRefresh(context.Background(), true); err != nil {
		t.Fatalf("first load: %v", err)
	}
	if source.topCalls != 1 {
		t.Fatalf("top calls = %d, want one build", source.topCalls)
	}

	store.mu.Lock()
	stale := store.lists[domain.LastFmRecommendationDiscover]
	stale.UpdatedAt = now.Add(-discoverTTL - time.Hour)
	store.lists[domain.LastFmRecommendationDiscover] = stale
	store.mu.Unlock()

	if _, err := recommender.ListRecommendationListsWithRefresh(context.Background(), false); err != nil {
		t.Fatalf("auto refresh off: %v", err)
	}
	if source.topCalls != 1 {
		t.Fatalf("top calls = %d with auto refresh off, want no rebuild", source.topCalls)
	}

	if _, err := recommender.ListRecommendationListsWithRefresh(context.Background(), true); err != nil {
		t.Fatalf("auto refresh on: %v", err)
	}
	if source.topCalls != 2 {
		t.Fatalf("top calls = %d with auto refresh on, want a rebuild", source.topCalls)
	}
}

func TestDiscoverSurvivesARestart(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Seed", "One")}
	source.similarTracks[similarTracksKey("Seed", "One")] = []LastFmTrack{
		track("Artist", "Stored"),
	}
	recommender, _, store := newStoredRecommender(source, sourceMatcher())
	recommender.now = fixedClock(1_800_000_000)
	if err := recommender.RefreshRecommendationList(
		context.Background(), domain.LastFmRecommendationDiscover); err != nil {
		t.Fatalf("build: %v", err)
	}

	restartedSource := connectedSource(nil)
	restarted, _ := newTopTrackRecommender(restartedSource, sourceMatcher())
	restarted.SetRecommendationStore(store)

	list, err := restarted.GetRecommendationList(
		context.Background(), domain.LastFmRecommendationDiscover)
	if err != nil {
		t.Fatalf("restart load: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Title != "Stored" {
		t.Fatalf("restored rows = %+v, want the stored discover row", list.Items)
	}
	if restartedSource.topCalls != 0 {
		t.Fatalf("restart top calls = %d, want the stored list", restartedSource.topCalls)
	}
}
