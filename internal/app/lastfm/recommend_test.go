package lastfm

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"testing"

	"airmedy/internal/domain"
)

// --- Fakes -------------------------------------------------------------------

type fakeTrackSource struct {
	mu sync.Mutex

	connected bool
	username  string
	avatar    string

	recent []LastFmTrack
	top    map[string][]LastFmTrack
	topErr map[string]error
	loved  []LastFmTrack

	similarTracks   map[string][]LastFmTrack
	similarArtists  map[string][]LastFmArtist
	similarTrackErr map[string]error

	similarTrackCalls  int
	similarArtistCalls int
	seedCalls          int

	lovedErr error
}

func (f *fakeTrackSource) GetStatus() (bool, string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected, f.username, f.avatar
}

func (f *fakeTrackSource) GetRecentTracks(_ context.Context, _ int) ([]LastFmTrack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seedCalls++
	return append([]LastFmTrack{}, f.recent...), nil
}

func (f *fakeTrackSource) GetTopTracks(_ context.Context, period string, _ int) ([]LastFmTrack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seedCalls++
	if err := f.topErr[period]; err != nil {
		return nil, err
	}
	return append([]LastFmTrack{}, f.top[period]...), nil
}

func (f *fakeTrackSource) GetLovedTracks(_ context.Context, _ int) ([]LastFmTrack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seedCalls++
	if f.lovedErr != nil {
		return nil, f.lovedErr
	}
	return append([]LastFmTrack{}, f.loved...), nil
}

func (f *fakeTrackSource) GetSimilarTracks(_ context.Context, artist, title string, _ int) ([]LastFmTrack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.similarTrackCalls++
	key := artist + "\x00" + title
	if err := f.similarTrackErr[key]; err != nil {
		return nil, err
	}
	return append([]LastFmTrack{}, f.similarTracks[key]...), nil
}

func (f *fakeTrackSource) GetSimilarArtists(_ context.Context, artist string, _ int) ([]LastFmArtist, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.similarArtistCalls++
	return append([]LastFmArtist{}, f.similarArtists[artist]...), nil
}

type fakeMatcher struct {
	mu sync.Mutex

	sources []domain.CatalogSearchSource
	rows    map[string][]domain.CatalogSearchResult
	errs    map[string]error
	calls   []string
}

func (f *fakeMatcher) Sources(context.Context) ([]domain.CatalogSearchSource, error) {
	return append([]domain.CatalogSearchSource{}, f.sources...), nil
}

func (f *fakeMatcher) MatchTrack(
	_ context.Context, title, artist string, _ int, _ []string,
) ([]domain.CatalogSearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := artist + "\x00" + title
	f.calls = append(f.calls, key)
	return append([]domain.CatalogSearchResult{}, f.rows[key]...), f.errs[key]
}

func (f *fakeMatcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeStreamRepo implements only the two lookups the recommender uses; the
// embedded nil interface keeps the rest of the repository contract off the test.
type fakeStreamRepo struct {
	domain.StreamCacheRepository
	byIdentity map[string]*domain.StreamCacheEntry
	byTitle    map[string]*domain.StreamCacheEntry
}

func (f *fakeStreamRepo) GetByIdentity(_ context.Context, provider, externalID string) (*domain.StreamCacheEntry, error) {
	return f.byIdentity[provider+"/"+externalID], nil
}

func (f *fakeStreamRepo) MatchByTitleArtist(
	_ context.Context, titleKey, artistKey string, _, _ int64,
) (*domain.StreamCacheEntry, error) {
	return f.byTitle[titleKey+"\x00"+artistKey], nil
}

func track(artist, title string) LastFmTrack {
	return LastFmTrack{Artist: artist, Title: title, Duration: 200}
}

func connectedSource(recent []LastFmTrack) *fakeTrackSource {
	return &fakeTrackSource{
		connected:      true,
		username:       "listener",
		avatar:         "https://example.test/avatar.png",
		recent:         recent,
		top:            map[string][]LastFmTrack{},
		similarTracks:  map[string][]LastFmTrack{},
		similarArtists: map[string][]LastFmArtist{},
	}
}

func newTestRecommender(client TrackSource, matcher CatalogMatcher, streams domain.StreamCacheRepository) *Recommender {
	return NewRecommender(client, matcher, streams, slog.Default())
}

// --- Seed tests --------------------------------------------------------------

func TestGatherSeedsDeduplicatesAndCaps(t *testing.T) {
	source := connectedSource(nil)
	for index := 0; index < 30; index++ {
		source.recent = append(source.recent, track("Recent Artist", storyTitle("recent", index)))
	}
	// A duplicate recent row must not consume a second seed slot.
	source.recent = append(source.recent, track("Recent Artist", storyTitle("recent", 0)))
	for index := 0; index < 25; index++ {
		source.top["7day"] = append(source.top["7day"], track("Top Artist", storyTitle("top", index)))
	}
	for index := 0; index < 25; index++ {
		source.top["1month"] = append(source.top["1month"], track("Top Artist", storyTitle("top", index)))
	}
	for index := 0; index < 25; index++ {
		source.loved = append(source.loved, track("Loved Artist", storyTitle("loved", index)))
	}

	recommender := newTestRecommender(source, &fakeMatcher{}, &fakeStreamRepo{})
	seeds, partial := recommender.gatherSeeds(context.Background())

	if partial {
		t.Fatalf("gatherSeeds reported a failure: %v", partial)
	}
	if len(seeds.recent) != seedTake {
		t.Fatalf("recent seeds = %d, want %d", len(seeds.recent), seedTake)
	}
	if len(seeds.top) != seedTake {
		t.Fatalf("top seeds = %d, want %d", len(seeds.top), seedTake)
	}
	if len(seeds.loved) != seedTake {
		t.Fatalf("loved seeds = %d, want %d", len(seeds.loved), seedTake)
	}
	if len(seeds.all) != maxSeeds {
		t.Fatalf("merged seeds = %d, want %d", len(seeds.all), maxSeeds)
	}
	seen := map[string]struct{}{}
	for _, item := range seeds.all {
		key := candidateKey(item.track.Artist, item.track.Title)
		if _, duplicate := seen[key]; duplicate {
			t.Fatalf("duplicate seed %q", key)
		}
		seen[key] = struct{}{}
	}
}

func TestGatherSeedsPartialWhenOneSourceFails(t *testing.T) {
	source := connectedSource([]LastFmTrack{track("A", "One")})
	source.lovedErr = errors.New("loved endpoint down")

	recommender := newTestRecommender(source, &fakeMatcher{}, &fakeStreamRepo{})
	seeds, partial := recommender.gatherSeeds(context.Background())

	if !partial {
		t.Fatal("expected partial because loved tracks failed")
	}
	if len(seeds.all) != 1 {
		t.Fatalf("seeds = %d, want 1 from the source that answered", len(seeds.all))
	}
}

func TestGatherSeedsKeepsTheTopWindowThatAnswered(t *testing.T) {
	source := connectedSource(nil)
	source.topErr = map[string]error{"7day": errors.New("7day window down")}
	source.top["1month"] = []LastFmTrack{track("Top Artist", "Monthly Hit")}

	recommender := newTestRecommender(source, &fakeMatcher{}, &fakeStreamRepo{})
	seeds, partial := recommender.gatherSeeds(context.Background())

	if !partial {
		t.Fatal("a failed window must mark the page partial")
	}
	if len(seeds.top) != 1 || seeds.top[0].Title != "Monthly Hit" {
		t.Fatalf("top seeds = %+v, want the window that answered", seeds.top)
	}
}

// --- Similar track aggregation -----------------------------------------------

func TestCollectSimilarTracksScoresAndFiltersSeeds(t *testing.T) {
	source := connectedSource(nil)
	seedOne := track("Seed Artist", "Seed One")
	seedTwo := track("Seed Artist", "Seed Two")
	source.recent = []LastFmTrack{seedOne}
	source.top["7day"] = []LastFmTrack{seedTwo}
	source.similarTracks["Seed Artist\x00Seed One"] = []LastFmTrack{
		track("Shared", "Hit"),
		track("Only", "Recent"),
		// The seed itself must never be recommended.
		seedTwo,
	}
	source.similarTracks["Seed Artist\x00Seed Two"] = []LastFmTrack{
		track("Shared", "Hit"),
		track("Only", "Top"),
	}

	recommender := newTestRecommender(source, &fakeMatcher{}, &fakeStreamRepo{})
	seeds, _ := recommender.gatherSeeds(context.Background())
	candidates, failed := recommender.collectSimilarTracks(context.Background(), seeds)
	if failed {
		t.Fatal("unexpected similar-track failure")
	}

	shared := candidates[candidateKey("Shared", "Hit")]
	if shared == nil {
		t.Fatal("shared candidate missing")
	}
	if shared.hits[seedRecent] != 1 || shared.hits[seedTop] != 1 {
		t.Fatalf("shared hits = %+v, want one recent and one top", shared.hits)
	}
	if shared.hits[seedLoved] != 0 {
		t.Fatalf("shared loved hits = %d, want 0", shared.hits[seedLoved])
	}
	if _, exists := candidates[candidateKey("Seed Artist", "Seed Two")]; exists {
		t.Fatal("a seed must not be its own recommendation")
	}

	combined := rankCombined(candidates)
	if len(combined) != 3 {
		t.Fatalf("combined candidates = %d, want 3", len(combined))
	}
	if combined[0].track.Title != "Hit" {
		t.Fatalf("first combined candidate = %q, want the one hit by two seeds", combined[0].track.Title)
	}

	recentOnly := rankKind(candidates, seedRecent, perKindLimit)
	if len(recentOnly) != 2 {
		t.Fatalf("recent list = %d, want 2", len(recentOnly))
	}
}

func TestCollectSimilarTracksSkipsSingleFailure(t *testing.T) {
	source := connectedSource([]LastFmTrack{track("A", "One"), track("B", "Two")})
	source.similarTracks["A\x00One"] = []LastFmTrack{track("C", "Three")}
	source.similarTrackErr = map[string]error{"B\x00Two": errors.New("similar endpoint down")}

	recommender := newTestRecommender(source, &fakeMatcher{}, &fakeStreamRepo{})
	seeds, _ := recommender.gatherSeeds(context.Background())
	candidates, failed := recommender.collectSimilarTracks(context.Background(), seeds)
	if !failed {
		t.Fatal("a failed seed query must be reported as partial")
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want the one the other seed produced", len(candidates))
	}
}

// --- Matching ----------------------------------------------------------------

func TestMatchPrefersLocalThenCacheThenDownloadable(t *testing.T) {
	source := connectedSource([]LastFmTrack{track("Seed Artist", "Seed")})
	source.similarTracks["Seed Artist\x00Seed"] = []LastFmTrack{
		track("Artist", "Local"),
		track("Artist", "Cached"),
		track("Artist", "Search"),
		track("Artist", "Unplayable"),
	}

	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "s1"}, {ID: "s2"}},
		rows: map[string][]domain.CatalogSearchResult{
			// The local row is second in the returned order: priority is by
			// state, not by source rank.
			"Artist\x00Local": {
				{Provider: "s2", ExternalID: "2", Title: "Local", Artists: []string{"Artist"}, Downloadable: true},
				{Provider: "s1", ExternalID: "1", Title: "Local", Artists: []string{"Artist"},
					State: domain.CatalogRowStateInLibrary, LocalTrackID: "track-local", Downloadable: true},
			},
			"Artist\x00Cached": {
				{Provider: "s1", ExternalID: "cached", Title: "Cached", Artists: []string{"Artist"}, Downloadable: true},
				{Provider: "s2", ExternalID: "2", Title: "Cached", Artists: []string{"Artist"}, Downloadable: true},
			},
			"Artist\x00Search": {
				{Provider: "s1", ExternalID: "3", Title: "Search", Artists: []string{"Artist"}, Downloadable: true},
			},
			"Artist\x00Unplayable": {
				{Provider: "s1", ExternalID: "4", Title: "Unplayable", Artists: []string{"Artist"}, Downloadable: false},
			},
		},
	}
	streams := &fakeStreamRepo{
		byIdentity: map[string]*domain.StreamCacheEntry{
			"s1/cached": {CacheKey: "cached"},
		},
		byTitle: map[string]*domain.StreamCacheEntry{},
	}

	recommender := newTestRecommender(source, matcher, streams)
	page, err := recommender.GetRecommendations(context.Background(), false)
	if err != nil {
		t.Fatalf("GetRecommendations: %v", err)
	}
	if page.Status != StatusOK {
		t.Fatalf("status = %q, want ok", page.Status)
	}

	got := map[string]RecommendedTrack{}
	for _, row := range page.Tracks {
		got[row.Title] = row
	}
	if len(got) != 3 {
		t.Fatalf("tracks = %d, want 3 (unplayable dropped): %+v", len(got), got)
	}
	if got["Local"].LocalTrackID != "track-local" {
		t.Fatalf("local row = %+v, want the in-library row", got["Local"])
	}
	if got["Cached"].ExternalID != "cached" {
		t.Fatalf("cached row = %+v, want the stream-cached identity", got["Cached"])
	}
	if got["Search"].ExternalID != "3" {
		t.Fatalf("search row = %+v, want the downloadable hit", got["Search"])
	}
	if _, exists := got["Unplayable"]; exists {
		t.Fatal("a row nothing can play must be dropped")
	}
}

func TestMatchUsesAtMostTwoSources(t *testing.T) {
	source := connectedSource([]LastFmTrack{track("Seed Artist", "Seed")})
	source.similarTracks["Seed Artist\x00Seed"] = []LastFmTrack{track("Artist", "One")}

	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "s3"}, {ID: "s1"}, {ID: "s2"}},
		rows: map[string][]domain.CatalogSearchResult{
			"Artist\x00One": {{Provider: "s1", ExternalID: "1", Title: "One", Artists: []string{"Artist"}, Downloadable: true}},
		},
	}
	recommender := newTestRecommender(source, matcher, &fakeStreamRepo{})
	sources := recommender.searchSources(context.Background())

	if len(sources) != 2 || sources[0] != "s1" || sources[1] != "s2" {
		t.Fatalf("sources = %v, want the first two sorted ids", sources)
	}
}

// --- Status and cache --------------------------------------------------------

func TestRecommendationsNotConnected(t *testing.T) {
	source := connectedSource(nil)
	source.connected = false
	recommender := newTestRecommender(source, &fakeMatcher{}, &fakeStreamRepo{})

	page, err := recommender.GetRecommendations(context.Background(), false)
	if err != nil {
		t.Fatalf("GetRecommendations: %v", err)
	}
	if page.Status != StatusNotConnected {
		t.Fatalf("status = %q, want not_connected", page.Status)
	}
	if source.similarTrackCalls != 0 || source.seedCalls != 0 {
		t.Fatal("an unconnected account must not call Last.fm")
	}
}

func TestRecommendationsCacheAndForce(t *testing.T) {
	source := connectedSource([]LastFmTrack{track("Seed Artist", "Seed")})
	source.similarTracks["Seed Artist\x00Seed"] = []LastFmTrack{track("Artist", "One")}

	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "s1"}},
		rows: map[string][]domain.CatalogSearchResult{
			"Artist\x00One": {{Provider: "s1", ExternalID: "1", Title: "One", Artists: []string{"Artist"}, Downloadable: true}},
		},
	}
	recommender := newTestRecommender(source, matcher, &fakeStreamRepo{})

	if _, err := recommender.GetRecommendations(context.Background(), false); err != nil {
		t.Fatalf("first build: %v", err)
	}
	firstSimilar := source.similarTrackCalls
	firstMatches := matcher.callCount()
	if firstSimilar != 1 || firstMatches != 1 {
		t.Fatalf("first build made %d similar / %d match calls, want 1/1", firstSimilar, firstMatches)
	}

	if _, err := recommender.GetRecommendations(context.Background(), false); err != nil {
		t.Fatalf("cached build: %v", err)
	}
	if source.similarTrackCalls != firstSimilar {
		t.Fatal("a cached page must not repeat similar-track requests")
	}
	if matcher.callCount() != firstMatches {
		t.Fatal("a cached page must not repeat catalogue matches")
	}

	if _, err := recommender.GetRecommendations(context.Background(), true); err != nil {
		t.Fatalf("forced build: %v", err)
	}
	if source.similarTrackCalls <= firstSimilar {
		t.Fatal("force must skip the page cache and re-query similar tracks")
	}
}

func TestRecommendationsPartialWhenSeedFails(t *testing.T) {
	source := connectedSource([]LastFmTrack{track("Seed Artist", "Seed")})
	// No answer for this seed is an empty list, not a failure. Use the seed
	// error path instead: loved fails while recent succeeds.
	source.lovedErr = errors.New("loved endpoint down")

	recommender := newTestRecommender(source, &fakeMatcher{}, &fakeStreamRepo{})
	page, err := recommender.GetRecommendations(context.Background(), false)
	if err != nil {
		t.Fatalf("GetRecommendations: %v", err)
	}
	if page.Status != StatusPartial {
		t.Fatalf("status = %q, want partial", page.Status)
	}
}

func TestRecommendationsEmptyWhenNoSources(t *testing.T) {
	source := connectedSource([]LastFmTrack{track("Seed Artist", "Seed")})
	source.similarTracks["Seed Artist\x00Seed"] = []LastFmTrack{track("Artist", "One")}

	recommender := newTestRecommender(source, &fakeMatcher{}, &fakeStreamRepo{})
	page, err := recommender.GetRecommendations(context.Background(), false)
	if err != nil {
		t.Fatalf("GetRecommendations: %v", err)
	}
	if len(page.Tracks) != 0 {
		t.Fatalf("tracks = %d, want 0 when no source can be searched", len(page.Tracks))
	}
	if page.Status != StatusOK {
		t.Fatalf("status = %q, want ok", page.Status)
	}
}

func TestSimilarArtistsAreMergedAndLimited(t *testing.T) {
	source := connectedSource([]LastFmTrack{
		track("A", "One"),
		track("B", "Two"),
	})
	source.similarArtists["A"] = []LastFmArtist{
		{Name: "Shared", ImageURL: "img-1", Listeners: 10},
		{Name: "Only A", Listeners: 5},
	}
	source.similarArtists["B"] = []LastFmArtist{
		{Name: "Shared", ImageURL: "img-2", Listeners: 99},
		{Name: "Only B", Listeners: 5},
	}

	recommender := newTestRecommender(source, &fakeMatcher{}, &fakeStreamRepo{})
	seeds, _ := recommender.gatherSeeds(context.Background())
	artists, failed := recommender.collectSimilarArtists(context.Background(), seeds)
	if failed {
		t.Fatal("unexpected similar-artist failure")
	}
	if len(artists) != 3 {
		t.Fatalf("artists = %d, want 3", len(artists))
	}
	if artists[0].Name != "Shared" {
		t.Fatalf("first artist = %q, want the one two seeds agreed on", artists[0].Name)
	}
	if artists[0].ImageURL != "img-1" || artists[0].Listeners != 10 {
		t.Fatalf("shared artist = %+v, want the first non-empty image/listeners", artists[0])
	}
}

func storyTitle(prefix string, index int) string {
	return prefix + " " + strconv.Itoa(index)
}
