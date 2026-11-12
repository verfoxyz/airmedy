package lastfm

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"airmedy/internal/domain"
)

type topTrackCapture struct {
	mu      sync.Mutex
	updates []TopTrackUpdate
	ch      chan TopTrackUpdate
}

func newTopTrackCapture() *topTrackCapture {
	return &topTrackCapture{ch: make(chan TopTrackUpdate, 256)}
}

func (c *topTrackCapture) emit(name string, payload any) {
	if name != recommendationsMatchedEvent {
		return
	}
	update, ok := payload.(TopTrackUpdate)
	if !ok {
		return
	}
	c.mu.Lock()
	c.updates = append(c.updates, update)
	c.mu.Unlock()
	c.ch <- update
}

// waitDone drains updates until Done and returns every item that changed state.
func (c *topTrackCapture) waitDone(t *testing.T) []TopTrackItem {
	t.Helper()
	deadline := time.After(5 * time.Second)
	items := make([]TopTrackItem, 0)
	for {
		select {
		case update := <-c.ch:
			items = append(items, update.Items...)
			if update.Done {
				return items
			}
		case <-deadline:
			t.Fatal("timed out waiting for the top-tracks Done event")
			return nil
		}
	}
}

// waitFinal drains updates until the named row reaches a non-matching state.
func (c *topTrackCapture) waitFinal(t *testing.T, matchKey string) TopTrackItem {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case update := <-c.ch:
			for _, item := range update.Items {
				if item.MatchKey == matchKey && !item.Matching {
					return item
				}
			}
		case <-deadline:
			t.Fatal("timed out waiting for a failover result")
			return TopTrackItem{}
		}
	}
}

func newTopTrackRecommender(source *fakeTrackSource, matcher *fakeMatcher) (*Recommender, *topTrackCapture) {
	recommender := newTestRecommender(source, matcher, &fakeStreamRepo{})
	capture := newTopTrackCapture()
	recommender.emitter = capture.emit
	return recommender, capture
}

func TestTopTracksUsesOneRequestAndReturnsEveryRow(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Artist", "One"), track("Artist", "Two")}

	block := make(chan struct{})
	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "s1"}},
		block:   block,
	}
	recommender, events := newTopTrackRecommender(source, matcher)

	page, err := recommender.GetTopTrackRecommendations(context.Background())
	if err != nil {
		t.Fatalf("GetTopTrackRecommendations: %v", err)
	}
	if source.topCalls != 1 {
		t.Fatalf("Last.fm top-track calls = %d, want 1", source.topCalls)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %d, want every Last.fm row", len(page.Items))
	}
	for _, item := range page.Items {
		if !item.Matching || item.Provider != "" || item.Matched || item.NoSource {
			t.Fatalf("initial item = %+v, want a pending row", item)
		}
	}

	close(block)
	events.waitDone(t)
}

// TestTopTracksRequestUsesOverallAndFifty pins the data window: overall keeps an
// account useful after a quiet week, and fifty is the page size the MVP renders.
func TestTopTracksRequestUsesOverallAndFifty(t *testing.T) {
	source := connectedSource(nil)
	for index := 0; index < 60; index++ {
		source.top[topTrackPeriod] = append(source.top[topTrackPeriod], track("Artist", "Song "+strconv.Itoa(index)))
	}
	recommender, _ := newTopTrackRecommender(source, &fakeMatcher{})

	page, err := recommender.GetTopTrackRecommendations(context.Background())
	if err != nil {
		t.Fatalf("GetTopTrackRecommendations: %v", err)
	}
	if len(source.topPeriods) != 1 || source.topPeriods[0] != "overall" {
		t.Fatalf("requested periods = %v, want [overall]", source.topPeriods)
	}
	if len(source.topLimits) != 1 || source.topLimits[0] != 50 {
		t.Fatalf("requested limits = %v, want [50]", source.topLimits)
	}
	if len(page.Items) != 50 {
		t.Fatalf("items = %d, want the fifty requested rows", len(page.Items))
	}
}

func TestTopTrackWorkersSearchOnlyTheirSourceOnce(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Artist", "Hit"), track("Artist", "Never")}
	matchKey := candidateKey("Artist", "Hit")
	rawHitKey := "Artist\x00Hit"
	rawNeverKey := "Artist\x00Never"

	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{
			{ID: "wy"}, {ID: "kg"}, {ID: "tx"}, {ID: "kw"}, {ID: "mg"},
		},
		sourceRows: map[string]map[string][]domain.CatalogSearchResult{
			"mg": {
				rawHitKey: {
					{Provider: "mg", ExternalID: "9", Title: "Hit", Artists: []string{"Artist"}, Downloadable: true},
				},
			},
		},
	}
	recommender, events := newTopTrackRecommender(source, matcher)
	page, err := recommender.GetTopTrackRecommendations(context.Background())
	if err != nil {
		t.Fatalf("GetTopTrackRecommendations: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %d, want both rows", len(page.Items))
	}

	events.waitDone(t)

	calls := matcher.sourcesCalled()
	seen := make(map[string]struct{}, len(calls))
	perItem := make(map[string]int)
	for _, call := range calls {
		if _, duplicate := seen[call]; duplicate {
			t.Fatalf("source searched an item twice: %q", call)
		}
		seen[call] = struct{}{}
		parts := strings.SplitN(call, "|", 2)
		if len(parts) == 2 {
			perItem[parts[1]]++
		}
	}
	// A hit stops the remaining sources; the never-matched row is tried by all
	// five, and no pair is repeated.
	if len(calls) > 10 {
		t.Fatalf("source calls = %d, want at most 10", len(calls))
	}
	if perItem[rawHitKey] == 0 || perItem[rawHitKey] > 5 {
		t.Fatalf("hit row source calls = %d, want 1–5", perItem[rawHitKey])
	}
	if perItem[rawNeverKey] != 5 {
		t.Fatalf("unmatched row source calls = %d, want 5", perItem[rawNeverKey])
	}

	cachedHit, _ := recommender.cachedSourceMatch(domain.LastFmRecommendationTopTracks, matchKey, "mg")
	if cachedHit == nil || cachedHit.Provider != "mg" {
		t.Fatalf("cached hit = %+v, want the mg match", cachedHit)
	}
}

func TestTopTrackMarksNoSourceAfterEveryWorkerFails(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Artist", "Obscure")}

	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "wy"}, {ID: "kg"}},
	}
	recommender, events := newTopTrackRecommender(source, matcher)
	page, err := recommender.GetTopTrackRecommendations(context.Background())
	if err != nil {
		t.Fatalf("GetTopTrackRecommendations: %v", err)
	}
	if !page.Items[0].Matching {
		t.Fatalf("initial item = %+v, want it pending", page.Items[0])
	}

	items := events.waitDone(t)
	if len(items) != 1 || !items[0].NoSource || items[0].Matched || items[0].Matching {
		t.Fatalf("final item = %+v, want no_source", items[0])
	}
}

func TestTopTrackWithNoSourcesFinishesImmediately(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Artist", "Song")}

	recommender, _ := newTopTrackRecommender(source, &fakeMatcher{})
	page, err := recommender.GetTopTrackRecommendations(context.Background())
	if err != nil {
		t.Fatalf("GetTopTrackRecommendations: %v", err)
	}
	if len(page.Items) != 1 || !page.Items[0].NoSource || page.Items[0].Matching {
		t.Fatalf("item = %+v, want no_source without a source", page.Items[0])
	}
}

func TestTopTrackCacheSkipsSourceSearchesOnRefresh(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Artist", "Hit")}

	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "wy"}},
		rows: map[string][]domain.CatalogSearchResult{
			"Artist\x00Hit": {
				{Provider: "wy", ExternalID: "1", Title: "Hit", Artists: []string{"Artist"}, Downloadable: true},
			},
		},
	}
	recommender, events := newTopTrackRecommender(source, matcher)

	if _, err := recommender.GetTopTrackRecommendations(context.Background()); err != nil {
		t.Fatalf("first call: %v", err)
	}
	events.waitDone(t)
	firstCalls := len(matcher.sourcesCalled())

	page, err := recommender.GetTopTrackRecommendations(context.Background())
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if source.topCalls != 2 {
		t.Fatalf("top-track calls = %d, want one per page load", source.topCalls)
	}
	if len(matcher.sourcesCalled()) != firstCalls {
		t.Fatal("a cached match must not repeat the source search")
	}
	if len(page.Items) != 1 || !page.Items[0].Matched || page.Items[0].Matching || page.Items[0].Provider != "wy" {
		t.Fatalf("cached item = %+v, want the previous match applied immediately", page.Items[0])
	}
}

func TestTopTrackEventsEndWithDone(t *testing.T) {
	source := connectedSource(nil)
	for index := 0; index < 4; index++ {
		source.top[topTrackPeriod] = append(source.top[topTrackPeriod], track("Artist", "Song "+string(rune('A'+index))))
	}
	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "wy"}},
	}
	recommender, events := newTopTrackRecommender(source, matcher)
	if _, err := recommender.GetTopTrackRecommendations(context.Background()); err != nil {
		t.Fatalf("GetTopTrackRecommendations: %v", err)
	}

	items := events.waitDone(t)
	if len(items) != 4 {
		t.Fatalf("updates = %d, want every row resolved", len(items))
	}
	for _, item := range items {
		if item.Matching {
			t.Fatalf("item = %+v, a Done update must not carry matching rows", item)
		}
	}
}

func TestTopTrackMatchKeyIsArtistAndTitle(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Artist", "Song")}
	recommender, _ := newTopTrackRecommender(source, &fakeMatcher{})

	page, err := recommender.GetTopTrackRecommendations(context.Background())
	if err != nil {
		t.Fatalf("GetTopTrackRecommendations: %v", err)
	}
	want := candidateKey("Artist", "Song")
	if len(page.Items) != 1 || page.Items[0].MatchKey != want || !strings.Contains(want, "\x00") {
		t.Fatalf("item = %+v, want match key %q", page.Items, want)
	}
}

// TestPlaybackFailureRequeuesFromTheNextSource is the failover contract: the
// provider that could not play a matched row is remembered and skipped, and the
// next source fills the same row in.
func TestPlaybackFailureRequeuesFromTheNextSource(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Artist", "Hit")}
	matchKey := candidateKey("Artist", "Hit")
	rawKey := "Artist\x00Hit"

	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "wy"}, {ID: "kg"}},
		sourceRows: map[string]map[string][]domain.CatalogSearchResult{
			"wy": {
				rawKey: {{Provider: "wy", ExternalID: "1", Title: "Hit", Artists: []string{"Artist"}, Downloadable: true}},
			},
		},
	}
	recommender, events := newTopTrackRecommender(source, matcher)
	if _, err := recommender.GetTopTrackRecommendations(context.Background()); err != nil {
		t.Fatalf("GetTopTrackRecommendations: %v", err)
	}
	initial := events.waitDone(t)
	if len(initial) != 1 || initial[0].Provider != "wy" || !initial[0].Matched {
		t.Fatalf("initial match = %+v, want the wy row", initial)
	}
	wyCallsBefore := countSourceCalls(matcher.sourcesCalled(), "wy|"+rawKey)

	// The second provider only answers after the first one failed to play.
	matcher.mu.Lock()
	matcher.sourceRows["kg"] = map[string][]domain.CatalogSearchResult{
		rawKey: {{Provider: "kg", ExternalID: "2", Title: "Hit", Artists: []string{"Artist"}, Downloadable: true}},
	}
	matcher.mu.Unlock()

	recommender.HandlePlaybackFailure("wy", "1")

	final := events.waitFinal(t, matchKey)
	if final.Provider != "kg" || !final.Matched || final.Matching || final.NoSource {
		t.Fatalf("failover item = %+v, want a matched kg row", final)
	}
	if got := countSourceCalls(matcher.sourcesCalled(), "wy|"+rawKey); got != wyCallsBefore {
		t.Fatalf("the failed source was searched again: %d -> %d calls", wyCallsBefore, got)
	}
	if got := countSourceCalls(matcher.sourcesCalled(), "kg|"+rawKey); got == 0 {
		t.Fatal("the next source was never searched")
	}
	if _, cached := recommender.cachedSourceMatch(
		domain.LastFmRecommendationTopTracks, matchKey, "wy"); cached {
		t.Fatal("the failed source's cached match must be dropped")
	}
	if failed := recommender.failedTopSources(domain.LastFmRecommendationTopTracks, matchKey); !failed["wy"] {
		t.Fatalf("failed sources = %v, want wy recorded", failed)
	}
}

// TestPlaybackFailureWithoutAnotherSourceMarksNoSource: a row whose only source
// failed must settle as "no match" instead of retrying forever.
func TestPlaybackFailureWithoutAnotherSourceMarksNoSource(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Artist", "Hit")}
	matchKey := candidateKey("Artist", "Hit")
	rawKey := "Artist\x00Hit"

	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "wy"}},
		sourceRows: map[string]map[string][]domain.CatalogSearchResult{
			"wy": {
				rawKey: {{Provider: "wy", ExternalID: "1", Title: "Hit", Artists: []string{"Artist"}, Downloadable: true}},
			},
		},
	}
	recommender, events := newTopTrackRecommender(source, matcher)
	if _, err := recommender.GetTopTrackRecommendations(context.Background()); err != nil {
		t.Fatalf("GetTopTrackRecommendations: %v", err)
	}
	events.waitDone(t)
	callsBefore := len(matcher.sourcesCalled())

	recommender.HandlePlaybackFailure("wy", "1")

	final := events.waitFinal(t, matchKey)
	if !final.NoSource || final.Matched || final.Matching {
		t.Fatalf("failover item = %+v, want no_source", final)
	}
	if got := len(matcher.sourcesCalled()); got != callsBefore {
		t.Fatalf("source calls = %d, want no retry (was %d)", got, callsBefore)
	}

	// A refresh must keep the previous failure rather than trying wy again.
	page, err := recommender.GetTopTrackRecommendations(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if len(page.Items) != 1 || !page.Items[0].NoSource || page.Items[0].Matching {
		t.Fatalf("refreshed item = %+v, want no_source", page.Items)
	}
}

type fakeCoverAssets struct{ asset string }

func (f fakeCoverAssets) AssetForIdentity(context.Context, string, string) (string, error) {
	return f.asset, nil
}

func (f fakeCoverAssets) RemoteForIdentity(context.Context, string, string) (string, error) {
	return "", nil
}

func (f fakeCoverAssets) BytesForIdentity(context.Context, string, string) ([]byte, string, error) {
	return nil, "", nil
}

func TestApplyTopTrackMatchCoverPriority(t *testing.T) {
	row := domain.CatalogSearchResult{
		Provider: "wy", ExternalID: "42", Title: "Song",
		CoverURL: "https://wy/cover.jpg", CoverURLLarge: "https://wy/cover-large.jpg",
	}

	t.Run("Last.fm image wins", func(t *testing.T) {
		r := &Recommender{coverAssets: fakeCoverAssets{asset: "/cover-cache/id.jpg"}}
		item := TopTrackItem{Title: "Song", CoverURL: "https://lastfm/cover.jpg"}
		r.applyTopTrackMatch(&item, &row)
		if item.CoverURL != "https://lastfm/cover.jpg" {
			t.Fatalf("CoverURL = %q, want the Last.fm image", item.CoverURL)
		}
	})

	t.Run("source large cover when Last.fm has none", func(t *testing.T) {
		r := &Recommender{coverAssets: fakeCoverAssets{asset: "/cover-cache/id.jpg"}}
		item := TopTrackItem{Title: "Song"}
		r.applyTopTrackMatch(&item, &row)
		if item.CoverURL != "https://wy/cover-large.jpg" {
			t.Fatalf("CoverURL = %q, want the source's large cover", item.CoverURL)
		}
	})

	t.Run("cached identity cover when neither reports one", func(t *testing.T) {
		r := &Recommender{coverAssets: fakeCoverAssets{asset: "/cover-cache/id.jpg"}}
		item := TopTrackItem{Title: "Song"}
		bare := row
		bare.CoverURL = ""
		bare.CoverURLLarge = ""
		r.applyTopTrackMatch(&item, &bare)
		if item.CoverURL != "/cover-cache/id.jpg" {
			t.Fatalf("CoverURL = %q, want the cached cover", item.CoverURL)
		}
	})

	t.Run("empty when nothing is known", func(t *testing.T) {
		r := &Recommender{}
		item := TopTrackItem{Title: "Song"}
		bare := row
		bare.CoverURL = ""
		bare.CoverURLLarge = ""
		r.applyTopTrackMatch(&item, &bare)
		if item.CoverURL != "" {
			t.Fatalf("CoverURL = %q, want empty so the page renders its placeholder", item.CoverURL)
		}
	})
}

func TestMatchedRecommendedKeepsTheLastFmCover(t *testing.T) {
	r := &Recommender{}
	base := RecommendedTrack{CoverURL: "https://lastfm/cover.jpg", Score: 0.9, MatchKey: "k"}
	row := &domain.CatalogSearchResult{Provider: "wy", ExternalID: "42", CoverURL: "https://wy/cover.jpg"}
	got := r.matchedRecommended(base, row)
	if got.CoverURL != "https://lastfm/cover.jpg" {
		t.Fatalf("CoverURL = %q, want the Last.fm image to win", got.CoverURL)
	}
	if got.Score != base.Score || got.MatchKey != base.MatchKey {
		t.Fatal("recommendation-only fields must be preserved")
	}
}

// TestRunTopTracksPrefersTheLocalFile: a library copy short-circuits the
// workers, so no source is searched for that row.
func TestRunTopTracksPrefersTheLocalFile(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Sad Keyboard Guy", "Synthetic Fracture")}

	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "wy"}},
		localRows: map[string]*domain.TrackDTO{
			"Sad Keyboard Guy\x00Synthetic Fracture": {Track: domain.Track{
				ID: "local-1", Title: "Synthetic Fracture", Duration: 200,
			}},
		},
	}
	recommender, capture := newTopTrackRecommender(source, matcher)

	if _, err := recommender.GetTopTrackRecommendations(context.Background()); err != nil {
		t.Fatalf("GetTopTrackRecommendations: %v", err)
	}

	items := capture.waitDone(t)
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].LocalTrackID != "local-1" || items[0].Provider != "" {
		t.Fatalf("item = %+v, want the local row", items[0])
	}
	if got := len(matcher.sourcesCalled()); got != 0 {
		t.Fatalf("source calls = %d, want none when a local file exists", got)
	}
}

// TestLocalMatchCarriesTheSuspiciousFlag: a duration-disagreed local match is
// still reused, but the row carries the flag so the UI can ask the user.
func TestLocalMatchCarriesTheSuspiciousFlag(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Gardens", "Owned Song")}
	key := "Gardens\x00Owned Song"

	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "wy"}},
		localRows: map[string]*domain.TrackDTO{
			key: {Track: domain.Track{
				ID: "local-1", Title: "Owned Song", Duration: 260,
			}},
		},
		localSuspicious: map[string]bool{key: true},
	}
	recommender, capture := newTopTrackRecommender(source, matcher)

	if _, err := recommender.GetTopTrackRecommendations(context.Background()); err != nil {
		t.Fatalf("GetTopTrackRecommendations: %v", err)
	}

	var matched *TopTrackItem
	for _, update := range capture.waitDone(t) {
		item := update
		if item.Title == "Owned Song" {
			matched = &item
		}
	}
	if matched == nil || matched.LocalTrackID != "local-1" || !matched.Matched {
		t.Fatalf("matched update = %+v, want the local row", matched)
	}
	if !matched.Suspicious {
		t.Fatalf("matched update = %+v, want suspicious=true", matched)
	}
	if got := len(matcher.sourcesCalled()); got != 0 {
		t.Fatalf("source calls = %d, want none for a local match", got)
	}
}

// --- "For you" (similar tracks) ----------------------------------------------

func similarTracksKey(artist, title string) string {
	return artist + "\x00" + title
}

func findSimilarItem(items []TopTrackItem, title string) *TopTrackItem {
	for index := range items {
		if items[index].Title == title {
			return &items[index]
		}
	}
	return nil
}

func TestSimilarRecommendationsUseTrackGetSimilar(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Seed", "Seed Song")}
	source.similarTracks[similarTracksKey("Seed", "Seed Song")] = []LastFmTrack{
		track("Other", "Recommended"),
		track("Seed", "Seed Song"), // a seed is never its own recommendation
	}
	recommender, events := newTopTrackRecommender(source, &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "wy"}},
	})

	page, err := recommender.GetTopTrackRecommendations(context.Background())
	if err != nil {
		t.Fatalf("GetTopTrackRecommendations: %v", err)
	}
	if source.similarTrackCalls != 1 {
		t.Fatalf("track.getSimilar calls = %d, want 1", source.similarTrackCalls)
	}
	if source.similarArtistCalls != 0 || source.artistTopTracksCalls != 0 {
		t.Fatalf("artist fallback ran for a seed with similar tracks: %d/%d",
			source.similarArtistCalls, source.artistTopTracksCalls)
	}
	if len(page.SimilarItems) != 1 || page.SimilarItems[0].Title != "Recommended" {
		t.Fatalf("similar items = %+v, want only the recommendation", page.SimilarItems)
	}
	events.waitDone(t)
}

func TestSimilarRecommendationsFallBackToSimilarArtists(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Seed", "Cold Seed")}
	source.similarArtists["Seed"] = []LastFmArtist{{Name: "Neighbour"}}
	source.artistTopTracks["Neighbour"] = []LastFmTrack{track("Neighbour", "Fresh Song")}
	recommender, events := newTopTrackRecommender(source, &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "wy"}},
	})

	page, err := recommender.GetTopTrackRecommendations(context.Background())
	if err != nil {
		t.Fatalf("GetTopTrackRecommendations: %v", err)
	}
	if source.similarArtistCalls != 1 || source.artistTopTracksCalls != 1 {
		t.Fatalf("fallback calls = %d/%d, want 1/1",
			source.similarArtistCalls, source.artistTopTracksCalls)
	}
	if findSimilarItem(page.SimilarItems, "Fresh Song") == nil {
		t.Fatalf("similar items = %+v, want the neighbour artist's top track", page.SimilarItems)
	}
	events.waitDone(t)
}

func TestSimilarRecommendationsRespectTheRequestBudget(t *testing.T) {
	source := connectedSource(nil)
	for index := 0; index < similarSeedLimit; index++ {
		artist := "Seed " + strconv.Itoa(index)
		source.top[topTrackPeriod] = append(source.top[topTrackPeriod], track(artist, "Song"))
		for neighbour := 0; neighbour < 5; neighbour++ {
			name := artist + " neighbour " + strconv.Itoa(neighbour)
			source.similarArtists[artist] = append(source.similarArtists[artist], LastFmArtist{Name: name})
			source.artistTopTracks[name] = []LastFmTrack{track(name, "Track")}
		}
	}
	recommender, events := newTopTrackRecommender(source, &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "wy"}},
	})

	if _, err := recommender.GetTopTrackRecommendations(context.Background()); err != nil {
		t.Fatalf("GetTopTrackRecommendations: %v", err)
	}
	if source.similarTrackCalls > similarSeedLimit {
		t.Fatalf("track.getSimilar calls = %d, want at most %d", source.similarTrackCalls, similarSeedLimit)
	}
	if source.similarArtistCalls > similarSeedLimit {
		t.Fatalf("artist.getSimilar calls = %d, want at most %d", source.similarArtistCalls, similarSeedLimit)
	}
	if source.artistTopTracksCalls > similarSeedLimit*fallbackSimilarArtistLimit {
		t.Fatalf("artist.getTopTracks calls = %d, want at most %d",
			source.artistTopTracksCalls, similarSeedLimit*fallbackSimilarArtistLimit)
	}
	events.waitDone(t)
}

// TestSimilarRecommendationsKeepLocalTracks: a recommendation the user already
// owns must stay in the list and become playable through the local-first pass.
func TestSimilarRecommendationsKeepLocalTracks(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Seed", "Seed Song")}
	source.similarTracks[similarTracksKey("Seed", "Seed Song")] = []LastFmTrack{
		track("Gardens", "Owned Song"),
	}
	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "wy"}},
		localRows: map[string]*domain.TrackDTO{
			similarTracksKey("Gardens", "Owned Song"): {Track: domain.Track{
				ID: "local-1", Title: "Owned Song", Duration: 200,
				ArtworkKey: "owned.jpg",
			}},
		},
	}
	recommender, events := newTopTrackRecommender(source, matcher)

	page, err := recommender.GetTopTrackRecommendations(context.Background())
	if err != nil {
		t.Fatalf("GetTopTrackRecommendations: %v", err)
	}
	if len(page.SimilarItems) != 1 {
		t.Fatalf("similar items = %+v, want the local track kept", page.SimilarItems)
	}

	var matched *TopTrackItem
	for _, update := range events.waitDone(t) {
		item := update
		if item.Title == "Owned Song" {
			matched = &item
		}
	}
	if matched == nil || matched.LocalTrackID != "local-1" || !matched.Matched {
		t.Fatalf("matched update = %+v, want the local row with local_track_id", matched)
	}
	if matched.CoverURL != "/artwork/owned.jpg?size=sm" {
		t.Fatalf("cover = %q, want the local artwork path", matched.CoverURL)
	}
}

func TestSimilarRecommendationsDeduplicateAcrossSeeds(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Seed A", "A"), track("Seed B", "B")}
	shared := track("Shared", "Same Song")
	source.similarTracks[similarTracksKey("Seed A", "A")] = []LastFmTrack{shared}
	source.similarTracks[similarTracksKey("Seed B", "B")] = []LastFmTrack{shared}
	recommender, events := newTopTrackRecommender(source, &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "wy"}},
	})

	page, err := recommender.GetTopTrackRecommendations(context.Background())
	if err != nil {
		t.Fatalf("GetTopTrackRecommendations: %v", err)
	}
	if len(page.SimilarItems) != 1 {
		t.Fatalf("similar items = %+v, want one row for the shared song", page.SimilarItems)
	}
	events.waitDone(t)
}

func TestSimilarRecommendationsPartialFailureKeepsTheRest(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Broken", "A"), track("Working", "B")}
	source.similarTrackErr[similarTracksKey("Broken", "A")] = errors.New("rate limited")
	source.similarTracks[similarTracksKey("Working", "B")] = []LastFmTrack{track("Other", "Kept")}
	recommender, events := newTopTrackRecommender(source, &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "wy"}},
	})

	page, err := recommender.GetTopTrackRecommendations(context.Background())
	if err != nil {
		t.Fatalf("GetTopTrackRecommendations: %v", err)
	}
	if page.Status != StatusPartial {
		t.Fatalf("status = %q, want %q", page.Status, StatusPartial)
	}
	if findSimilarItem(page.SimilarItems, "Kept") == nil {
		t.Fatalf("similar items = %+v, want the successful seed's row", page.SimilarItems)
	}
	events.waitDone(t)
}

func TestSimilarRecommendationsAreCached(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Seed", "Song")}
	source.similarTracks[similarTracksKey("Seed", "Song")] = []LastFmTrack{track("Other", "Rec")}
	recommender, events := newTopTrackRecommender(source, &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "wy"}},
	})

	if _, err := recommender.GetTopTrackRecommendations(context.Background()); err != nil {
		t.Fatalf("first load: %v", err)
	}
	events.waitDone(t)
	calls := source.similarTrackCalls

	if _, err := recommender.GetTopTrackRecommendations(context.Background()); err != nil {
		t.Fatalf("second load: %v", err)
	}
	if source.similarTrackCalls != calls {
		t.Fatalf("track.getSimilar calls = %d after refresh, want the cached %d",
			source.similarTrackCalls, calls)
	}
}

func countSourceCalls(calls []string, want string) int {
	count := 0
	for _, call := range calls {
		if call == want {
			count++
		}
	}
	return count
}
