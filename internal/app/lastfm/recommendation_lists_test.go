package lastfm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"airmedy/internal/domain"
)

// fakeRecommendationStore is the in-memory persisted list store.
type fakeRecommendationStore struct {
	mu    sync.Mutex
	lists map[string]domain.RecommendationList
}

func newFakeRecommendationStore() *fakeRecommendationStore {
	return &fakeRecommendationStore{lists: make(map[string]domain.RecommendationList)}
}

func (f *fakeRecommendationStore) ListRecommendationLists(
	context.Context,
) ([]domain.RecommendationListSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	summaries := make([]domain.RecommendationListSummary, 0, len(f.lists))
	for _, list := range f.lists {
		summary := domain.RecommendationListSummary{
			Kind:      list.Kind,
			Title:     list.Title,
			CoverURLs: []string{},
			UpdatedAt: list.UpdatedAt,
		}
		for _, item := range list.Items {
			summary.TrackCount++
			if item.Matching {
				summary.MatchingCount++
			}
			if item.CoverURL != "" && len(summary.CoverURLs) < 4 {
				summary.CoverURLs = append(summary.CoverURLs, item.CoverURL)
			}
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

func (f *fakeRecommendationStore) GetRecommendationList(
	_ context.Context, kind string,
) (*domain.RecommendationList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	list, ok := f.lists[kind]
	if !ok {
		return nil, nil
	}
	copyList := list
	copyList.Items = append([]domain.RecommendationItem{}, list.Items...)
	return &copyList, nil
}

func (f *fakeRecommendationStore) SaveRecommendationList(
	_ context.Context, list *domain.RecommendationList,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists[list.Kind] = domain.RecommendationList{
		Kind:      list.Kind,
		Title:     list.Title,
		Items:     append([]domain.RecommendationItem{}, list.Items...),
		UpdatedAt: list.UpdatedAt,
	}
	return nil
}

func (f *fakeRecommendationStore) UpdateRecommendationItems(
	_ context.Context, items []domain.RecommendationItem,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, item := range items {
		list, ok := f.lists[item.Kind]
		if !ok {
			continue
		}
		for index := range list.Items {
			if list.Items[index].MatchKey != item.MatchKey {
				continue
			}
			list.Items[index] = item
			break
		}
		f.lists[item.Kind] = list
	}
	return nil
}

func (f *fakeRecommendationStore) PendingRecommendationItems(
	context.Context,
) ([]domain.RecommendationItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := make([]domain.RecommendationItem, 0)
	for _, list := range f.lists {
		for _, item := range list.Items {
			if item.Matching {
				items = append(items, item)
			}
		}
	}
	return items, nil
}

func (f *fakeRecommendationStore) seed(kind, title, matchKey string, updatedAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists[kind] = domain.RecommendationList{
		Kind:  kind,
		Title: recommendationTitle(kind),
		Items: []domain.RecommendationItem{{
			Kind:     kind,
			MatchKey: matchKey,
			Title:    title,
			Artist:   "Stored",
			Matching: false,
		}},
		UpdatedAt: updatedAt,
	}
}

func newStoredRecommender(
	source *fakeTrackSource, matcher *fakeMatcher,
) (*Recommender, *topTrackCapture, *fakeRecommendationStore) {
	recommender, capture := newTopTrackRecommender(source, matcher)
	store := newFakeRecommendationStore()
	recommender.SetRecommendationStore(store)
	return recommender, capture, store
}

func sourceMatcher() *fakeMatcher {
	return &fakeMatcher{sources: []domain.CatalogSearchSource{{ID: "wy"}}}
}

func TestListRecommendationListsBuildsOnceAndThenReadsTheStore(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Artist", "One")}
	recommender, capture, store := newStoredRecommender(source, sourceMatcher())

	collection, err := recommender.ListRecommendationLists(context.Background())
	if err != nil {
		t.Fatalf("ListRecommendationLists: %v", err)
	}
	if source.topCalls != 1 {
		t.Fatalf("Last.fm top-track calls = %d, want 1", source.topCalls)
	}
	if len(collection.Lists) != 2 {
		t.Fatalf("lists = %d, want both cards", len(collection.Lists))
	}
	if collection.Lists[0].Kind != domain.LastFmRecommendationSimilar ||
		collection.Lists[1].Kind != domain.LastFmRecommendationTopTracks {
		t.Fatalf("card order = %+v, want similar then top_tracks", collection.Lists)
	}
	capture.waitDone(t)

	second, err := recommender.ListRecommendationLists(context.Background())
	if err != nil {
		t.Fatalf("second ListRecommendationLists: %v", err)
	}
	if source.topCalls != 1 {
		t.Fatalf("Last.fm top-track calls = %d after reopen, want the stored list", source.topCalls)
	}
	if len(second.Lists) != 2 || second.Lists[1].TrackCount != 1 {
		t.Fatalf("second summaries = %+v, want the stored row", second.Lists)
	}

	stored, err := store.GetRecommendationList(context.Background(), domain.LastFmRecommendationTopTracks)
	if err != nil || stored == nil {
		t.Fatalf("stored top list = %+v, %v", stored, err)
	}
	if len(stored.Items) != 1 || stored.Items[0].Title != "One" {
		t.Fatalf("stored items = %+v, want the built row", stored.Items)
	}
}

func TestRefreshRecommendationListLeavesTheOtherListUntouched(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Artist", "New")}
	recommender, capture, store := newStoredRecommender(source, sourceMatcher())

	oldTime := time.Unix(1_600_000_000, 0).UTC()
	store.seed(domain.LastFmRecommendationTopTracks, "Old", "old-key", oldTime)
	store.seed(domain.LastFmRecommendationSimilar, "Keep", "keep-key", oldTime)

	if err := recommender.RefreshRecommendationList(
		context.Background(), domain.LastFmRecommendationTopTracks); err != nil {
		t.Fatalf("RefreshRecommendationList: %v", err)
	}
	capture.waitDone(t)

	top, _ := store.GetRecommendationList(context.Background(), domain.LastFmRecommendationTopTracks)
	if top == nil || len(top.Items) != 1 || top.Items[0].Title != "New" {
		t.Fatalf("refreshed top list = %+v, want the new row", top)
	}
	similar, _ := store.GetRecommendationList(context.Background(), domain.LastFmRecommendationSimilar)
	if similar == nil || len(similar.Items) != 1 || similar.Items[0].Title != "Keep" {
		t.Fatalf("similar list = %+v, want the stored row untouched", similar)
	}
	if !similar.UpdatedAt.Equal(oldTime) {
		t.Fatalf("similar updated_at = %v, want %v", similar.UpdatedAt, oldTime)
	}
}

func TestRefreshRecommendationListKeepsTheOldRowsOnFailure(t *testing.T) {
	source := connectedSource(nil)
	source.topErr[topTrackPeriod] = errors.New("rate limited")
	recommender, _, store := newStoredRecommender(source, &fakeMatcher{})

	store.seed(domain.LastFmRecommendationTopTracks, "Old", "old-key", time.Unix(1_600_000_000, 0).UTC())

	if err := recommender.RefreshRecommendationList(
		context.Background(), domain.LastFmRecommendationTopTracks); err == nil {
		t.Fatal("RefreshRecommendationList must report the request failure")
	}

	top, _ := store.GetRecommendationList(context.Background(), domain.LastFmRecommendationTopTracks)
	if top == nil || len(top.Items) != 1 || top.Items[0].Title != "Old" {
		t.Fatalf("top list = %+v, want the previous rows kept", top)
	}
}

func TestRefreshAllRecommendationListsRebuildsBoth(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Artist", "New Top")}
	source.similarTracks[similarTracksKey("Artist", "New Top")] = []LastFmTrack{
		track("Other", "New Rec"),
	}
	recommender, capture, store := newStoredRecommender(source, sourceMatcher())

	oldTime := time.Unix(1_600_000_000, 0).UTC()
	store.seed(domain.LastFmRecommendationTopTracks, "Old Top", "old-top", oldTime)
	store.seed(domain.LastFmRecommendationSimilar, "Old Rec", "old-rec", oldTime)

	if err := recommender.RefreshAllRecommendationLists(context.Background()); err != nil {
		t.Fatalf("RefreshAllRecommendationLists: %v", err)
	}
	capture.waitDone(t)

	top, _ := store.GetRecommendationList(context.Background(), domain.LastFmRecommendationTopTracks)
	if top == nil || len(top.Items) != 1 || top.Items[0].Title != "New Top" {
		t.Fatalf("top list = %+v, want the rebuilt row", top)
	}
	similar, _ := store.GetRecommendationList(context.Background(), domain.LastFmRecommendationSimilar)
	if similar == nil || len(similar.Items) != 1 || similar.Items[0].Title != "New Rec" {
		t.Fatalf("similar list = %+v, want the rebuilt row", similar)
	}
}

func TestMatchedRowsArePersistedForTheNextSession(t *testing.T) {
	source := connectedSource(nil)
	source.top[topTrackPeriod] = []LastFmTrack{track("Artist", "Hit")}
	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "wy"}},
		sourceRows: map[string]map[string][]domain.CatalogSearchResult{
			"wy": {
				"Artist\x00Hit": {{
					Provider: "wy", ExternalID: "1", Title: "Hit",
					Artists: []string{"Artist"}, Downloadable: true,
				}},
			},
		},
	}
	recommender, capture, store := newStoredRecommender(source, matcher)

	if _, err := recommender.ListRecommendationLists(context.Background()); err != nil {
		t.Fatalf("ListRecommendationLists: %v", err)
	}
	capture.waitDone(t)

	// A restart reads the stored rows without another Last.fm request.
	restartedSource := connectedSource(nil)
	restarted, _ := newTopTrackRecommender(restartedSource, &fakeMatcher{})
	restarted.SetRecommendationStore(store)

	list, err := restarted.GetRecommendationList(
		context.Background(), domain.LastFmRecommendationTopTracks)
	if err != nil {
		t.Fatalf("GetRecommendationList after restart: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("items = %+v, want the stored row", list.Items)
	}
	item := list.Items[0]
	if !item.Matched || item.Provider != "wy" || item.ExternalID != "1" {
		t.Fatalf("restored item = %+v, want the persisted match", item)
	}
	if restartedSource.topCalls != 0 {
		t.Fatalf("Last.fm top-track calls = %d after restart, want none", restartedSource.topCalls)
	}
}
