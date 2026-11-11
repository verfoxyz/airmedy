package lastfm

import (
	"context"
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

func newTopTrackRecommender(source *fakeTrackSource, matcher *fakeMatcher) (*Recommender, *topTrackCapture) {
	recommender := newTestRecommender(source, matcher, &fakeStreamRepo{})
	capture := newTopTrackCapture()
	recommender.emitter = capture.emit
	return recommender, capture
}

func TestTopTracksUsesOneRequestAndReturnsEveryRow(t *testing.T) {
	source := connectedSource(nil)
	source.top["7day"] = []LastFmTrack{track("Artist", "One"), track("Artist", "Two")}

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

func TestTopTrackWorkersSearchOnlyTheirSourceOnce(t *testing.T) {
	source := connectedSource(nil)
	source.top["7day"] = []LastFmTrack{track("Artist", "Hit"), track("Artist", "Never")}
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

	cachedHit, _ := recommender.cachedSourceMatch(matchKey, "mg")
	if cachedHit == nil || cachedHit.Provider != "mg" {
		t.Fatalf("cached hit = %+v, want the mg match", cachedHit)
	}
}

func TestTopTrackMarksNoSourceAfterEveryWorkerFails(t *testing.T) {
	source := connectedSource(nil)
	source.top["7day"] = []LastFmTrack{track("Artist", "Obscure")}

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
	source.top["7day"] = []LastFmTrack{track("Artist", "Song")}

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
	source.top["7day"] = []LastFmTrack{track("Artist", "Hit")}

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
		source.top["7day"] = append(source.top["7day"], track("Artist", "Song "+string(rune('A'+index))))
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
	source.top["7day"] = []LastFmTrack{track("Artist", "Song")}
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
