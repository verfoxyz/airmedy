package lastfm

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

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

	artistTopTracks    map[string][]LastFmTrack
	artistTopTracksErr map[string]error
	userTopTags        []LastFmTag
	userTopTagsErr     error
	artistTopTags      map[string][]LastFmTag
	artistTopTagsErr   map[string]error
	tagTopTracks       map[string][]LastFmTrack
	tagTopTracksErr    map[string]error
	chartTopTracks     []LastFmTrack
	chartTopTracksErr  error

	similarTrackCalls  int
	similarArtistCalls int
	seedCalls          int
	topCalls           int

	// Weekly chart and discovery seed answers.
	weeks            []LastFmWeek
	weeksErr         error
	weeklyCharts     map[int64][]LastFmTrack
	weeklyChartErr   map[int64]error
	topArtists       map[string][]LastFmArtist
	topArtistsErr    map[string]error
	weeklyCalls      int
	weeklyChartCalls int
	topArtistCalls   int
	topArtistPeriods []string
	recentPages      []int
	// topPeriods and topLimits record what the top-tracks page asked for, so a
	// test can pin the window and page size.
	topPeriods []string
	topLimits  []int
	// Fallback call counters, kept separate from similarArtistCalls so a test
	// can assert the fallback budget without the similar-artists grid noise.
	artistTopTracksCalls int
	userTopTagsCalls     int
	artistTopTagsCalls   int
	tagTopTracksCalls    int
	chartTopTracksCalls  int

	lovedErr error
}

func (f *fakeTrackSource) GetStatus() (bool, string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected, f.username, f.avatar
}

func (f *fakeTrackSource) GetRecentTracks(_ context.Context, _ int) ([]LastFmTrack, error) {
	return f.GetRecentTracksPage(context.Background(), 1, 0)
}

func (f *fakeTrackSource) GetRecentTracksPage(_ context.Context, page, _ int) ([]LastFmTrack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seedCalls++
	f.recentPages = append(f.recentPages, page)
	return append([]LastFmTrack{}, f.recent...), nil
}

func (f *fakeTrackSource) GetWeeklyChartList(_ context.Context) ([]LastFmWeek, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.weeklyCalls++
	if f.weeksErr != nil {
		return nil, f.weeksErr
	}
	return append([]LastFmWeek{}, f.weeks...), nil
}

func (f *fakeTrackSource) GetWeeklyTrackChart(
	_ context.Context, from, to int64,
) ([]LastFmTrack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.weeklyChartCalls++
	if err := f.weeklyChartErr[from]; err != nil {
		return nil, err
	}
	return append([]LastFmTrack{}, f.weeklyCharts[from]...), nil
}

func (f *fakeTrackSource) GetUserTopArtists(
	_ context.Context, period string, _ int,
) ([]LastFmArtist, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.topArtistCalls++
	f.topArtistPeriods = append(f.topArtistPeriods, period)
	if err := f.topArtistsErr[period]; err != nil {
		return nil, err
	}
	return append([]LastFmArtist{}, f.topArtists[period]...), nil
}

func (f *fakeTrackSource) GetTopTracks(_ context.Context, period string, limit int) ([]LastFmTrack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seedCalls++
	f.topCalls++
	f.topPeriods = append(f.topPeriods, period)
	f.topLimits = append(f.topLimits, limit)
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

func (f *fakeTrackSource) GetArtistTopTracks(_ context.Context, artist string, _ int) ([]LastFmTrack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.artistTopTracksCalls++
	if err := f.artistTopTracksErr[artist]; err != nil {
		return nil, err
	}
	return append([]LastFmTrack{}, f.artistTopTracks[artist]...), nil
}

func (f *fakeTrackSource) GetUserTopTags(_ context.Context, _ int) ([]LastFmTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userTopTagsCalls++
	if f.userTopTagsErr != nil {
		return nil, f.userTopTagsErr
	}
	return append([]LastFmTag{}, f.userTopTags...), nil
}

func (f *fakeTrackSource) GetArtistTopTags(_ context.Context, artist string, _ int) ([]LastFmTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.artistTopTagsCalls++
	if err := f.artistTopTagsErr[artist]; err != nil {
		return nil, err
	}
	return append([]LastFmTag{}, f.artistTopTags[artist]...), nil
}

func (f *fakeTrackSource) GetTagTopTracks(_ context.Context, tag string, _ int) ([]LastFmTrack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tagTopTracksCalls++
	if err := f.tagTopTracksErr[tag]; err != nil {
		return nil, err
	}
	return append([]LastFmTrack{}, f.tagTopTracks[tag]...), nil
}

func (f *fakeTrackSource) GetChartTopTracks(_ context.Context, _ int) ([]LastFmTrack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chartTopTracksCalls++
	if f.chartTopTracksErr != nil {
		return nil, f.chartTopTracksErr
	}
	return append([]LastFmTrack{}, f.chartTopTracks...), nil
}

type fakeMatcher struct {
	mu sync.Mutex

	sources []domain.CatalogSearchSource
	rows    map[string][]domain.CatalogSearchResult
	errs    map[string]error
	// sourceRows / sourceErrs let a test make one source fail and the next
	// answer, or make a source return no match.
	sourceRows map[string]map[string][]domain.CatalogSearchResult
	sourceErrs map[string]map[string]error
	// localRows answers MatchLocalTrack, keyed by artist+"\x00"+title.
	localRows map[string]*domain.TrackDTO
	// localSuspicious marks one local answer as a duration-disagreed match.
	localSuspicious map[string]bool
	// block, when set, holds every search until the test closes it. It proves
	// GetRecommendations returns before matching completes.
	block chan struct{}

	calls       []string
	sourceCalls []string
}

func (f *fakeMatcher) Sources(context.Context) ([]domain.CatalogSearchSource, error) {
	return append([]domain.CatalogSearchSource{}, f.sources...), nil
}

// MatchLocalTrack answers the local-first lookup. localRows is keyed by
// artist+title, the same key MatchTrack uses.
func (f *fakeMatcher) MatchLocalTrack(
	_ context.Context, title string, artists []string, _ int,
) (*domain.LocalTrackMatch, error) {
	artist := ""
	if len(artists) > 0 {
		artist = artists[0]
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := artist + "\x00" + title
	if track, ok := f.localRows[key]; ok {
		return &domain.LocalTrackMatch{Track: track, Suspicious: f.localSuspicious[key]}, nil
	}
	return nil, nil
}

func (f *fakeMatcher) MatchTrack(
	_ context.Context, title, artist string, _ int, sources []string,
) ([]domain.CatalogSearchResult, error) {
	source := ""
	if len(sources) > 0 {
		source = sources[0]
	}

	f.mu.Lock()
	block := f.block
	f.mu.Unlock()
	if block != nil {
		<-block
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	key := artist + "\x00" + title
	f.calls = append(f.calls, key)
	f.sourceCalls = append(f.sourceCalls, source+"|"+key)
	if rows, ok := f.sourceRows[source][key]; ok {
		return append([]domain.CatalogSearchResult{}, rows...), f.sourceErrs[source][key]
	}
	return append([]domain.CatalogSearchResult{}, f.rows[key]...), f.errs[key]
}

func (f *fakeMatcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeMatcher) sourcesCalled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.sourceCalls...)
}

// eventCapture records the matching progress events a Recommender emits.
type eventCapture struct {
	mu      sync.Mutex
	updates []RecommendationMatchUpdate
	ch      chan RecommendationMatchUpdate
}

func newEventCapture() *eventCapture {
	return &eventCapture{ch: make(chan RecommendationMatchUpdate, 256)}
}

func (c *eventCapture) emit(name string, payload any) {
	if name != recommendationsMatchedEvent {
		return
	}
	update, ok := payload.(RecommendationMatchUpdate)
	if !ok {
		return
	}
	c.mu.Lock()
	c.updates = append(c.updates, update)
	c.mu.Unlock()
	c.ch <- update
}

// waitDone drains updates until Done and returns every matched item.
func (c *eventCapture) waitDone(t *testing.T) []RecommendedTrack {
	t.Helper()
	deadline := time.After(5 * time.Second)
	items := make([]RecommendedTrack, 0)
	for {
		select {
		case update := <-c.ch:
			items = append(items, update.Items...)
			if update.Done {
				return items
			}
		case <-deadline:
			t.Fatal("timed out waiting for the final matching event")
			return nil
		}
	}
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
		connected:          true,
		username:           "listener",
		avatar:             "https://example.test/avatar.png",
		recent:             recent,
		top:                map[string][]LastFmTrack{},
		topErr:             map[string]error{},
		similarTracks:      map[string][]LastFmTrack{},
		similarArtists:     map[string][]LastFmArtist{},
		similarTrackErr:    map[string]error{},
		artistTopTracks:    map[string][]LastFmTrack{},
		artistTopTracksErr: map[string]error{},
		artistTopTags:      map[string][]LastFmTag{},
		artistTopTagsErr:   map[string]error{},
		tagTopTracks:       map[string][]LastFmTrack{},
		tagTopTracksErr:    map[string]error{},
		weeklyCharts:       map[int64][]LastFmTrack{},
		weeklyChartErr:     map[int64]error{},
		topArtists:         map[string][]LastFmArtist{},
		topArtistsErr:      map[string]error{},
	}
}

func newTestRecommender(client TrackSource, matcher CatalogMatcher, streams domain.StreamCacheRepository) *Recommender {
	return NewRecommender(client, matcher, streams, slog.Default())
}

func newCapturingRecommender(
	client TrackSource, matcher CatalogMatcher, streams domain.StreamCacheRepository,
) (*Recommender, *eventCapture) {
	recommender := newTestRecommender(client, matcher, streams)
	capture := newEventCapture()
	recommender.emitter = capture.emit
	return recommender, capture
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

// --- Fallback ----------------------------------------------------------------

func TestNeighbourFallbackReplacesEmptySimilarTracks(t *testing.T) {
	source := connectedSource([]LastFmTrack{track("Seed Artist", "Obscure")})
	// No similarTracks entry: Last.fm has nothing for this seed.
	source.similarArtists["Seed Artist"] = []LastFmArtist{{Name: "Neighbour"}}
	source.artistTopTracks["Neighbour"] = []LastFmTrack{track("Neighbour", "Fallback Hit")}

	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "s1"}},
		rows: map[string][]domain.CatalogSearchResult{
			"Neighbour\x00Fallback Hit": {
				{Provider: "s1", ExternalID: "1", Title: "Fallback Hit", Artists: []string{"Neighbour"}, Downloadable: true},
			},
		},
	}
	recommender, events := newCapturingRecommender(source, matcher, &fakeStreamRepo{})
	page, err := recommender.GetRecommendations(context.Background(), false)
	if err != nil {
		t.Fatalf("GetRecommendations: %v", err)
	}
	if len(page.Tracks) != 1 || !page.Tracks[0].Matching || page.Tracks[0].Provider != "" {
		t.Fatalf("initial tracks = %+v, want the raw fallback row still matching", page.Tracks)
	}
	items := events.waitDone(t)
	if len(items) != 1 {
		t.Fatalf("matched items = %+v, want the neighbour fallback row", items)
	}
	if items[0].Title != "Fallback Hit" || items[0].Source != "fallback" {
		t.Fatalf("track = %+v, want a row marked as fallback", items[0])
	}
	if source.artistTopTracksCalls != 1 {
		t.Fatalf("artist top-track calls = %d, want 1", source.artistTopTracksCalls)
	}
}

func TestNeighbourFallbackRunsForTheFirstTenSeedsOnly(t *testing.T) {
	source := connectedSource(nil)
	source.similarArtists = map[string][]LastFmArtist{}
	source.artistTopTracks = map[string][]LastFmTrack{}
	for index := 0; index < 15; index++ {
		artist := "Seed Artist " + strconv.Itoa(index)
		source.recent = append(source.recent, track(artist, "Song "+strconv.Itoa(index)))
		neighbour := "Neighbour " + strconv.Itoa(index)
		source.similarArtists[artist] = []LastFmArtist{{Name: neighbour}}
		source.artistTopTracks[neighbour] = []LastFmTrack{track(neighbour, "Hit "+strconv.Itoa(index))}
	}

	recommender, events := newCapturingRecommender(source, &fakeMatcher{}, &fakeStreamRepo{})
	if _, err := recommender.GetRecommendations(context.Background(), false); err != nil {
		t.Fatalf("GetRecommendations: %v", err)
	}
	events.waitDone(t)
	if source.artistTopTracksCalls != fallbackSeedLimit {
		t.Fatalf("artist top-track calls = %d, want %d", source.artistTopTracksCalls, fallbackSeedLimit)
	}
}

func TestNeighbourFallbackExcludesSeedsAndRanksByOccurrence(t *testing.T) {
	source := connectedSource(nil)
	seedTrack := track("Seed Artist", "Seed Song")
	source.similarArtists["Seed Artist"] = []LastFmArtist{
		{Name: "A"}, {Name: "B"}, {Name: "C"},
	}
	source.artistTopTracks["A"] = []LastFmTrack{track("Shared", "Hit"), track("A", "Only A"), seedTrack}
	source.artistTopTracks["B"] = []LastFmTrack{track("Shared", "Hit"), track("B", "Only B")}
	source.artistTopTracks["C"] = []LastFmTrack{track("Shared", "Hit")}

	recommender := newTestRecommender(source, &fakeMatcher{}, &fakeStreamRepo{})
	hits, failed := recommender.fallbackForSeed(context.Background(), seedTrack)
	if failed {
		t.Fatal("an empty answer is not a failure")
	}
	if len(hits) != 3 {
		t.Fatalf("hits = %+v, want the seed excluded and three neighbours merged", hits)
	}
	if hits[0].track.Title != "Hit" || hits[0].count != 3 {
		t.Fatalf("first hit = %+v, want the track all three neighbours listed", hits[0])
	}
	for _, hit := range hits {
		if hit.track.Title == seedTrack.Title {
			t.Fatal("a seed must not appear in its own fallback")
		}
	}

	// The same rows flow through the candidate map with the occurrence count as
	// the per-kind hit count.
	seeds := seedSet{all: []seed{{track: seedTrack, kind: seedRecent}}}
	candidates, _ := recommender.collectSimilarTracks(context.Background(), seeds)
	shared := candidates[candidateKey("Shared", "Hit")]
	if shared == nil || shared.hits[seedRecent] != 3 || shared.source() != "fallback" {
		t.Fatalf("shared candidate = %+v, want three fallback hits", shared)
	}
}

func TestAllSeedsEmptyIsNotAnError(t *testing.T) {
	source := connectedSource([]LastFmTrack{track("Seed Artist", "Obscure")})
	recommender := newTestRecommender(source, &fakeMatcher{}, &fakeStreamRepo{})

	page, err := recommender.GetRecommendations(context.Background(), false)
	if err != nil {
		t.Fatalf("GetRecommendations: %v", err)
	}
	if len(page.Tracks) != 0 || len(page.RecentTracks) != 0 {
		t.Fatalf("page = %+v, want empty lists rather than an error", page)
	}
}

func TestGlobalFallbackUsesTagsThenChart(t *testing.T) {
	source := connectedSource([]LastFmTrack{track("Seed Artist", "Obscure")})
	source.userTopTags = []LastFmTag{{Name: "rock", Count: 10}}
	source.tagTopTracks = map[string][]LastFmTrack{
		"rock": {track("Tag Artist", "Tag Hit")},
	}

	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "s1"}},
		rows: map[string][]domain.CatalogSearchResult{
			"Tag Artist\x00Tag Hit": {
				{Provider: "s1", ExternalID: "1", Title: "Tag Hit", Artists: []string{"Tag Artist"}, Downloadable: true},
			},
		},
	}
	recommender, events := newCapturingRecommender(source, matcher, &fakeStreamRepo{})
	page, err := recommender.GetRecommendations(context.Background(), false)
	if err != nil {
		t.Fatalf("GetRecommendations: %v", err)
	}
	if len(page.Tracks) != 1 || page.Tracks[0].Source != "fallback" || !page.Tracks[0].Matching {
		t.Fatalf("tracks = %+v, want the raw tag fallback row", page.Tracks)
	}
	items := events.waitDone(t)
	if len(items) != 1 || items[0].Source != "fallback" || items[0].Provider == "" {
		t.Fatalf("matched items = %+v, want the cached tag fallback row", items)
	}
	if source.tagTopTracksCalls == 0 {
		t.Fatal("the tag fallback must query tag.getTopTracks")
	}
	if source.chartTopTracksCalls != 0 {
		t.Fatal("the chart is only a last resort")
	}
}

func TestGlobalFallbackFallsBackToTheChart(t *testing.T) {
	source := connectedSource([]LastFmTrack{track("Seed Artist", "Obscure")})
	source.chartTopTracks = []LastFmTrack{track("Chart Artist", "Chart Hit")}

	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "s1"}},
		rows: map[string][]domain.CatalogSearchResult{
			"Chart Artist\x00Chart Hit": {
				{Provider: "s1", ExternalID: "1", Title: "Chart Hit", Artists: []string{"Chart Artist"}, Downloadable: true},
			},
		},
	}
	recommender, events := newCapturingRecommender(source, matcher, &fakeStreamRepo{})
	page, err := recommender.GetRecommendations(context.Background(), false)
	if err != nil {
		t.Fatalf("GetRecommendations: %v", err)
	}
	if len(page.Tracks) != 1 || page.Tracks[0].Title != "Chart Hit" || page.Tracks[0].Source != "fallback" {
		t.Fatalf("tracks = %+v, want the chart fallback row", page.Tracks)
	}
	items := events.waitDone(t)
	if len(items) != 1 || items[0].Title != "Chart Hit" || items[0].Provider == "" {
		t.Fatalf("matched items = %+v, want the chart fallback row", items)
	}
	if source.chartTopTracksCalls != 1 {
		t.Fatalf("chart calls = %d, want 1", source.chartTopTracksCalls)
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

	recommender, events := newCapturingRecommender(source, matcher, streams)
	page, err := recommender.GetRecommendations(context.Background(), false)
	if err != nil {
		t.Fatalf("GetRecommendations: %v", err)
	}
	if page.Status != StatusOK {
		t.Fatalf("status = %q, want ok", page.Status)
	}
	if len(page.Tracks) != 4 || !page.Tracks[0].Matching {
		t.Fatalf("initial tracks = %+v, want every Last.fm row before matching", page.Tracks)
	}

	items := events.waitDone(t)
	got := map[string]RecommendedTrack{}
	for _, row := range items {
		got[row.Title] = row
	}
	if len(got) != 3 {
		t.Fatalf("matched items = %d, want 3 (unplayable dropped): %+v", len(got), got)
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
		t.Fatal("a row nothing can play must not be emitted")
	}
}

func TestMatchSourcesPrioritizeFastProviders(t *testing.T) {
	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "mg"}, {ID: "other"}, {ID: "kw"}, {ID: "wy"}},
	}
	recommender := newTestRecommender(connectedSource(nil), matcher, &fakeStreamRepo{})
	sources := recommender.matchSources(context.Background())

	if len(sources) != 2 || sources[0] != "wy" || sources[1] != "kw" {
		t.Fatalf("sources = %v, want the two fastest configured sources", sources)
	}
}

func TestMatchSlowFallsBackToTheSecondSource(t *testing.T) {
	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "s1"}, {ID: "s2"}},
		sourceRows: map[string]map[string][]domain.CatalogSearchResult{
			"s2": {
				"Artist\x00Song": {
					{Provider: "s2", ExternalID: "2", Title: "Song", Artists: []string{"Artist"}, Downloadable: true},
				},
			},
		},
		sourceErrs: map[string]map[string]error{
			"s1": {"Artist\x00Song": errors.New("source down")},
		},
	}
	recommender := newTestRecommender(connectedSource(nil), matcher, &fakeStreamRepo{})
	row, err := recommender.matchSlow(
		RecommendedTrack{Title: "Song", Artists: []string{"Artist"}}, []string{"s1", "s2"},
	)
	if err != nil {
		t.Fatalf("matchSlow: %v", err)
	}
	if row == nil || row.Provider != "s2" {
		t.Fatalf("row = %+v, want the fallback source hit", row)
	}
	calls := matcher.sourcesCalled()
	if len(calls) != 2 || calls[0] != "s1|Artist\x00Song" || calls[1] != "s2|Artist\x00Song" {
		t.Fatalf("source calls = %v, want one attempt per source in order", calls)
	}
}

func TestRecommendationsReturnBeforeMatchingCompletes(t *testing.T) {
	source := connectedSource([]LastFmTrack{track("Seed Artist", "Seed")})
	source.similarTracks["Seed Artist\x00Seed"] = []LastFmTrack{track("Artist", "One")}

	block := make(chan struct{})
	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "s1"}},
		rows: map[string][]domain.CatalogSearchResult{
			"Artist\x00One": {{Provider: "s1", ExternalID: "1", Title: "One", Artists: []string{"Artist"}, Downloadable: true}},
		},
		block: block,
	}
	recommender, events := newCapturingRecommender(source, matcher, &fakeStreamRepo{})

	page, err := recommender.GetRecommendations(context.Background(), false)
	if err != nil {
		t.Fatalf("GetRecommendations: %v", err)
	}
	// The call returned while the only source search is still blocked.
	if len(page.Tracks) != 1 || !page.Tracks[0].Matching || !page.Matching {
		t.Fatalf("page = %+v, want the raw row returned before matching", page)
	}

	close(block)
	items := events.waitDone(t)
	if len(items) != 1 || items[0].Provider != "s1" {
		t.Fatalf("items = %+v, want the blocked search's result", items)
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
	recommender, events := newCapturingRecommender(source, matcher, &fakeStreamRepo{})

	if _, err := recommender.GetRecommendations(context.Background(), false); err != nil {
		t.Fatalf("first build: %v", err)
	}
	events.waitDone(t)
	firstSimilar := source.similarTrackCalls
	firstMatches := matcher.callCount()
	if firstSimilar != 1 || firstMatches != 1 {
		t.Fatalf("first build made %d similar / %d match calls, want 1/1", firstSimilar, firstMatches)
	}

	cached, err := recommender.GetRecommendations(context.Background(), false)
	if err != nil {
		t.Fatalf("cached build: %v", err)
	}
	if cached.Matching || len(cached.Tracks) != 1 || cached.Tracks[0].Provider == "" {
		t.Fatalf("cached page = %+v, want the match cache applied without a job", cached)
	}
	if source.similarTrackCalls != firstSimilar {
		t.Fatal("a cached page must not repeat similar-track requests")
	}
	if matcher.callCount() != firstMatches {
		t.Fatal("a cached page must not repeat catalogue matches")
	}

	forced, err := recommender.GetRecommendations(context.Background(), true)
	if err != nil {
		t.Fatalf("forced build: %v", err)
	}
	if source.similarTrackCalls <= firstSimilar {
		t.Fatal("force must skip the page cache and re-query similar tracks")
	}
	if matcher.callCount() != firstMatches {
		t.Fatal("force must reuse the 30-minute match cache")
	}
	if forced.Matching || len(forced.Tracks) != 1 || forced.Tracks[0].Provider == "" {
		t.Fatalf("forced page = %+v, want the cached match applied", forced)
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

func TestRecommendationsKeepRowsWhenNoSourceCanBeSearched(t *testing.T) {
	source := connectedSource([]LastFmTrack{track("Seed Artist", "Seed")})
	source.similarTracks["Seed Artist\x00Seed"] = []LastFmTrack{track("Artist", "One")}

	recommender, events := newCapturingRecommender(source, &fakeMatcher{}, &fakeStreamRepo{})
	page, err := recommender.GetRecommendations(context.Background(), false)
	if err != nil {
		t.Fatalf("GetRecommendations: %v", err)
	}
	if len(page.Tracks) != 1 || !page.Tracks[0].Matching {
		t.Fatalf("tracks = %+v, want the Last.fm row before matching", page.Tracks)
	}
	if page.Status != StatusOK {
		t.Fatalf("status = %q, want ok", page.Status)
	}

	items := events.waitDone(t)
	if len(items) != 0 {
		t.Fatalf("items = %+v, want none without a source", items)
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
