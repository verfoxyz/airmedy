package lastfm

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"airmedy/internal/domain"
)

// fakeDislikeStore is the in-memory repository the recommender tests use.
type fakeDislikeStore struct {
	mu   sync.Mutex
	rows []domain.LastFmDislike
}

func (f *fakeDislikeStore) ListDislikes(context.Context) ([]domain.LastFmDislike, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.LastFmDislike{}, f.rows...), nil
}

func (f *fakeDislikeStore) AddDislike(
	_ context.Context, entry domain.LastFmDislike,
) (domain.LastFmDislike, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row.Kind == entry.Kind && row.MatchKey == entry.MatchKey {
			return row, nil
		}
	}
	f.rows = append(f.rows, entry)
	return entry, nil
}

func (f *fakeDislikeStore) DeleteDislike(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.rows[:0]
	for _, row := range f.rows {
		if row.ID != id {
			kept = append(kept, row)
		}
	}
	f.rows = kept
	return nil
}

func (f *fakeDislikeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func dislikeDiscoverFixture(t *testing.T) (*Recommender, *fakeRecommendationStore, *fakeDislikeStore, *fakeTrackSource) {
	t.Helper()
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Seed", "One")}
	source.topArtists["6month"] = []LastFmArtist{{Name: "Seed"}}
	source.similarTracks[similarTracksKey("Seed", "One")] = []LastFmTrack{
		track("Keep", "Keeper"),
		track("Drop", "Dropped"),
	}
	recommender, _, store := newStoredRecommender(source, sourceMatcher())
	dislikes := &fakeDislikeStore{}
	recommender.SetDislikeStore(dislikes)
	recommender.now = fixedClock(1_800_000_000)
	return recommender, store, dislikes, source
}

func storedDiscoverTitles(t *testing.T, store *fakeRecommendationStore) []string {
	t.Helper()
	return discoverTitles(discoverListFromStore(t, store))
}

func containsTitle(titles []string, want string) bool {
	for _, title := range titles {
		if title == want {
			return true
		}
	}
	return false
}

func TestDislikeHidesDiscoverRowsAndUndislikeRestores(t *testing.T) {
	ctx := context.Background()
	recommender, store, _, _ := dislikeDiscoverFixture(t)

	if _, err := recommender.Dislike(
		ctx, domain.LastFmDislikeTrack, candidateKey("Drop", "Dropped"), "Dropped", "Drop", false,
	); err != nil {
		t.Fatalf("Dislike: %v", err)
	}
	if err := recommender.RefreshRecommendationList(ctx, domain.LastFmRecommendationDiscover); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	titles := storedDiscoverTitles(t, store)
	if containsTitle(titles, "Dropped") {
		t.Fatalf("discover titles = %v, want the disliked row gone", titles)
	}
	if !containsTitle(titles, "Keeper") {
		t.Fatalf("discover titles = %v, want the other row kept", titles)
	}

	rows, err := recommender.ListDislikes(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListDislikes = %+v, %v", rows, err)
	}
	if err := recommender.Undislike(ctx, rows[0].ID); err != nil {
		t.Fatalf("Undislike: %v", err)
	}
	if err := recommender.RefreshRecommendationList(ctx, domain.LastFmRecommendationDiscover); err != nil {
		t.Fatalf("refresh after undislike: %v", err)
	}
	if titles := storedDiscoverTitles(t, store); !containsTitle(titles, "Dropped") {
		t.Fatalf("discover titles = %v, want the row back after undislike", titles)
	}
}

func TestDislikeArtistHidesEveryTrackOfThatArtist(t *testing.T) {
	ctx := context.Background()
	recommender, store, _, source := dislikeDiscoverFixture(t)
	source.similarTracks[similarTracksKey("Seed", "One")] = []LastFmTrack{
		track("Drop", "One"),
		track("Drop", "Two"),
		track("Keep", "Three"),
	}

	if _, err := recommender.Dislike(
		ctx, domain.LastFmDislikeArtist, "", "", "Drop", false,
	); err != nil {
		t.Fatalf("Dislike artist: %v", err)
	}
	if err := recommender.RefreshRecommendationList(ctx, domain.LastFmRecommendationDiscover); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	titles := storedDiscoverTitles(t, store)
	for _, unwanted := range []string{"One", "Two"} {
		if containsTitle(titles, unwanted) {
			t.Fatalf("discover titles = %v, want %q excluded", titles, unwanted)
		}
	}
	if !containsTitle(titles, "Three") {
		t.Fatalf("discover titles = %v, want the other artist kept", titles)
	}
}

func TestDislikeIsIdempotent(t *testing.T) {
	ctx := context.Background()
	recommender, _, dislikes, _ := dislikeDiscoverFixture(t)

	firstResult, err := recommender.Dislike(
		ctx, domain.LastFmDislikeTrack, "", "Dropped", "Drop", false,
	)
	if err != nil {
		t.Fatalf("first Dislike: %v", err)
	}
	secondResult, err := recommender.Dislike(
		ctx, domain.LastFmDislikeTrack, "", "Dropped", "Drop", false,
	)
	if err != nil {
		t.Fatalf("second Dislike: %v", err)
	}
	first, second := firstResult.Entry, secondResult.Entry
	if first.ID != second.ID || dislikes.count() != 1 {
		t.Fatalf("rows = %d, ids = %q/%q, want one stable row", dislikes.count(), first.ID, second.ID)
	}
	if first.MatchKey != candidateKey("Drop", "Dropped") {
		t.Fatalf("match key = %q, want the normalised pair", first.MatchKey)
	}
}

func TestSeedsSkipDislikedTracksAndArtists(t *testing.T) {
	ctx := context.Background()
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{
		track("Drop", "Song"),
		track("Keep", "Song"),
	}
	source.topArtists["6month"] = []LastFmArtist{{Name: "Drop Artist"}, {Name: "Keep Artist"}}
	recommender, _ := newTopTrackRecommender(source, sourceMatcher())
	recommender.SetDislikeStore(&fakeDislikeStore{rows: []domain.LastFmDislike{
		{ID: "1", Kind: domain.LastFmDislikeTrack, MatchKey: candidateKey("Drop", "Song"), Artist: "Drop", Title: "Song"},
		{ID: "2", Kind: domain.LastFmDislikeArtist, MatchKey: domain.ArtistSetKey([]string{"Drop Artist"}), Artist: "Drop Artist"},
	}})

	set := recommender.buildRecommendationSeeds(ctx, nil)
	for _, seed := range set.tracks {
		if seed.track.Artist == "Drop" {
			t.Fatalf("seed tracks = %+v, want the disliked track skipped", set.tracks)
		}
	}
	for _, artist := range set.artists {
		if artist.name == "Drop Artist" {
			t.Fatalf("seed artists = %+v, want the disliked artist skipped", set.artists)
		}
	}
	if len(set.tracks) == 0 {
		t.Fatal("seed tracks are empty, want the kept track")
	}
}

func TestRadioSkipsDislikedCandidates(t *testing.T) {
	source := connectedSource(nil)
	candidates := radioCandidatesFor(source, "Seed Artist", "Seed Song", "Radio", 10)
	matcher := radioMatcherFor(candidates)
	radio, _, _, _, _ := newRadioForTest(t, source, matcher, &fakeSeedRepository{})
	blocked := candidates[0]
	radio.recommender.SetDislikeStore(&fakeDislikeStore{rows: []domain.LastFmDislike{{
		ID:       "1",
		Kind:     domain.LastFmDislikeTrack,
		MatchKey: candidateKey(blocked.Artist, blocked.Title),
		Artist:   blocked.Artist,
		Title:    blocked.Title,
	}}})

	result, err := radio.StartRadio(context.Background(), RadioSeedRequest{TrackID: "seed"})
	if err != nil {
		t.Fatalf("StartRadio: %v", err)
	}
	if len(result.Tracks) != 9 {
		t.Fatalf("tracks = %d, want the disliked candidate skipped", len(result.Tracks))
	}
	for _, track := range result.Tracks {
		if track.ExternalID == "0" {
			t.Fatalf("track %+v is the disliked candidate", track)
		}
	}
}

// fakePurgeSettings answers the purge toggle.
type fakePurgeSettings struct{ enabled bool }

func (f fakePurgeSettings) GetSettings(context.Context) (*domain.AppSettings, error) {
	return &domain.AppSettings{DislikePurgeFiles: f.enabled}, nil
}

// fakeTracePurger records the keys a dislike invalidates.
type fakeTracePurger struct {
	keys []string
	err  error
}

func (f *fakeTracePurger) PurgeDislikeTraces(_ context.Context, keys []string) error {
	f.keys = append([]string{}, keys...)
	return f.err
}

// fakeLocalPurger records the local purge call.
type fakeLocalPurger struct {
	calls   int
	deleted int
	err     error
	kind    string
	artist  string
}

func (f *fakeLocalPurger) CountDislikeTargets(
	context.Context, string, string, string, string,
) (int, error) {
	return f.deleted, nil
}

func (f *fakeLocalPurger) PurgeDislikeTargets(
	_ context.Context, kind, _, _, artist string,
) (int, error) {
	f.calls++
	f.kind = kind
	f.artist = artist
	return f.deleted, f.err
}

func TestDislikePurgesRecommendationTracesWithoutTouchingFiles(t *testing.T) {
	ctx := context.Background()
	recommender, _, _, _ := dislikeDiscoverFixture(t)
	purger := &fakeTracePurger{}
	recommender.SetDislikeTracePurger(purger)
	local := &fakeLocalPurger{deleted: 1}
	recommender.SetDislikeLocalPurger(local)

	key := candidateKey("Drop", "Dropped")
	result, err := recommender.Dislike(
		ctx, domain.LastFmDislikeTrack, key, "Dropped", "Drop", false)
	if err != nil {
		t.Fatalf("Dislike: %v", err)
	}
	if len(purger.keys) != 1 || purger.keys[0] != key {
		t.Fatalf("trace keys = %v, want [%s]", purger.keys, key)
	}
	if local.calls != 0 {
		t.Fatal("the local files were touched with the purge setting off")
	}
	if result.DeletedTracks != 0 || result.PurgeError != "" {
		t.Fatalf("result = %+v, want no file purge", result)
	}
}

func TestDislikeWithPurgeDeletesLocalFiles(t *testing.T) {
	ctx := context.Background()
	recommender, _, _, _ := dislikeDiscoverFixture(t)
	recommender.SetDislikeLocalPurger(&fakeLocalPurger{deleted: 2})
	recommender.SetDislikeSettings(fakePurgeSettings{enabled: true})

	result, err := recommender.Dislike(
		ctx, domain.LastFmDislikeTrack, candidateKey("Drop", "Dropped"), "Dropped", "Drop", true)
	if err != nil {
		t.Fatalf("Dislike: %v", err)
	}
	if result.DeletedTracks != 2 {
		t.Fatalf("deleted = %d, want 2", result.DeletedTracks)
	}
	if result.Entry.ID == "" {
		t.Fatal("the dislike row was not saved")
	}
}

func TestDislikePurgeFailureStillKeepsTheMark(t *testing.T) {
	ctx := context.Background()
	recommender, _, dislikes, _ := dislikeDiscoverFixture(t)
	local := &fakeLocalPurger{deleted: 1, err: errors.New("file is locked")}
	recommender.SetDislikeLocalPurger(local)
	recommender.SetDislikeSettings(fakePurgeSettings{enabled: true})

	result, err := recommender.Dislike(
		ctx, domain.LastFmDislikeTrack, "", "Dropped", "Drop", true)
	if err != nil {
		t.Fatalf("Dislike: %v", err)
	}
	if result.PurgeError != "file is locked" {
		t.Fatalf("purge error = %q, want the delete failure", result.PurgeError)
	}
	if dislikes.count() != 1 {
		t.Fatalf("dislikes = %d, want the mark kept", dislikes.count())
	}
}

func TestDislikeRejectsPurgeWhenTheSettingIsOff(t *testing.T) {
	ctx := context.Background()
	recommender, _, dislikes, _ := dislikeDiscoverFixture(t)
	recommender.SetDislikeLocalPurger(&fakeLocalPurger{})
	recommender.SetDislikeSettings(fakePurgeSettings{enabled: false})

	if _, err := recommender.Dislike(
		ctx, domain.LastFmDislikeTrack, "", "Dropped", "Drop", true); err == nil {
		t.Fatal("Dislike accepted a purge with the setting off")
	}
	if dislikes.count() != 0 {
		t.Fatalf("dislikes = %d, want nothing written", dislikes.count())
	}
}

func TestArtistDislikeCollectsEveryStoredTraceKey(t *testing.T) {
	ctx := context.Background()
	recommender, store, _, _ := dislikeDiscoverFixture(t)
	purger := &fakeTracePurger{}
	recommender.SetDislikeTracePurger(purger)

	one := candidateKey("Drop", "One")
	two := candidateKey("Drop", "Two")
	other := candidateKey("Keep", "Three")
	list := &domain.RecommendationList{
		Kind:  domain.LastFmRecommendationDiscover,
		Title: "lastfm.discover",
		Items: []domain.RecommendationItem{
			{Kind: domain.LastFmRecommendationDiscover, MatchKey: one, Title: "One", Artist: "Drop"},
			{Kind: domain.LastFmRecommendationDiscover, MatchKey: two, Title: "Two", Artist: "Drop"},
			{Kind: domain.LastFmRecommendationDiscover, MatchKey: other, Title: "Three", Artist: "Keep"},
		},
		UpdatedAt: time.Now().UTC(),
	}
	if err := store.SaveRecommendationList(ctx, list); err != nil {
		t.Fatalf("SaveRecommendationList: %v", err)
	}
	pool := []domain.RecommendationItem{
		{Kind: domain.LastFmRecommendationDiscover, MatchKey: candidateKey("Drop", "Four"), Title: "Four", Artist: "Drop"},
	}
	if err := store.SaveRecommendationCandidates(
		ctx, domain.LastFmRecommendationDiscover, pool); err != nil {
		t.Fatalf("SaveRecommendationCandidates: %v", err)
	}

	if _, err := recommender.Dislike(
		ctx, domain.LastFmDislikeArtist, "", "", "Drop", false); err != nil {
		t.Fatalf("Dislike artist: %v", err)
	}
	want := []string{one, two, candidateKey("Drop", "Four")}
	sort.Strings(want)
	if len(purger.keys) != len(want) {
		t.Fatalf("trace keys = %v, want %v", purger.keys, want)
	}
	for index, key := range want {
		if purger.keys[index] != key {
			t.Fatalf("trace keys = %v, want %v", purger.keys, want)
		}
	}
}
