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

// fakeMatchCacheStore is the in-memory shared match cache.
type fakeMatchCacheStore struct {
	mu      sync.Mutex
	entries map[string]domain.LastFmMatchCacheEntry
}

func newFakeMatchCacheStore() *fakeMatchCacheStore {
	return &fakeMatchCacheStore{entries: make(map[string]domain.LastFmMatchCacheEntry)}
}

func (f *fakeMatchCacheStore) GetMatchCache(
	_ context.Context, keys []string, now time.Time,
) ([]domain.LastFmMatchCacheEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]domain.LastFmMatchCacheEntry, 0, len(keys))
	for _, key := range keys {
		entry, ok := f.entries[key]
		if !ok || !entry.ExpiresAt.After(now) {
			continue
		}
		result = append(result, entry)
	}
	return result, nil
}

func (f *fakeMatchCacheStore) SaveMatchCache(
	_ context.Context, entries []domain.LastFmMatchCacheEntry, now time.Time, limit int,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, entry := range entries {
		if entry.MatchKey != "" {
			f.entries[entry.MatchKey] = entry
		}
	}
	f.pruneLocked(now, limit)
	return nil
}

func (f *fakeMatchCacheStore) DeleteMatchCache(
	_ context.Context, keys []string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, key := range keys {
		delete(f.entries, key)
	}
	return nil
}

func (f *fakeMatchCacheStore) PruneMatchCache(
	_ context.Context, now time.Time, limit int,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneLocked(now, limit)
	return nil
}

func (f *fakeMatchCacheStore) pruneLocked(now time.Time, limit int) {
	for key, entry := range f.entries {
		if !entry.ExpiresAt.After(now) {
			delete(f.entries, key)
		}
	}
	if limit <= 0 || len(f.entries) <= limit {
		return
	}
	ordered := make([]domain.LastFmMatchCacheEntry, 0, len(f.entries))
	for _, entry := range f.entries {
		ordered = append(ordered, entry)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].UpdatedAt.Equal(ordered[j].UpdatedAt) {
			return ordered[i].UpdatedAt.After(ordered[j].UpdatedAt)
		}
		return ordered[i].MatchKey < ordered[j].MatchKey
	})
	for _, entry := range ordered[limit:] {
		delete(f.entries, entry.MatchKey)
	}
}

func (f *fakeMatchCacheStore) entry(key string) (domain.LastFmMatchCacheEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.entries[key]
	return entry, ok
}

func (f *fakeMatchCacheStore) put(entry domain.LastFmMatchCacheEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if entry.UpdatedAt.IsZero() {
		entry.UpdatedAt = time.Now().UTC()
	}
	if entry.ExpiresAt.IsZero() {
		entry.ExpiresAt = entry.UpdatedAt.Add(time.Hour)
	}
	f.entries[entry.MatchKey] = entry
}

func liveMatchEntry(key, provider, externalID string) domain.LastFmMatchCacheEntry {
	now := time.Now().UTC()
	return domain.LastFmMatchCacheEntry{
		MatchKey:   key,
		Provider:   provider,
		ExternalID: externalID,
		UpdatedAt:  now,
		ExpiresAt:  now.Add(persistentMatchTTLProvider),
	}
}

func pendingTopItem(artist, title string) TopTrackItem {
	return TopTrackItem{
		Title:    title,
		Artist:   artist,
		Artists:  []string{artist},
		Kind:     domain.LastFmRecommendationTopTracks,
		Matching: true,
		MatchKey: candidateKey(artist, title),
	}
}

func TestMatchCacheHitResolvesWithoutSearch(t *testing.T) {
	cases := []struct {
		name  string
		entry domain.LastFmMatchCacheEntry
		check func(t *testing.T, item TopTrackItem)
	}{
		{
			name:  "provider match",
			entry: liveMatchEntry("key", "wy", "42"),
			check: func(t *testing.T, item TopTrackItem) {
				t.Helper()
				if item.Provider != "wy" || item.ExternalID != "42" || !item.Matched {
					t.Fatalf("item = %+v, want the cached provider match", item)
				}
			},
		},
		{
			name: "local match",
			entry: domain.LastFmMatchCacheEntry{
				MatchKey: "key", LocalTrackID: "local-1",
				UpdatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
			},
			check: func(t *testing.T, item TopTrackItem) {
				t.Helper()
				if item.LocalTrackID != "local-1" || item.State != domain.CatalogRowStateInLibrary {
					t.Fatalf("item = %+v, want the cached local match", item)
				}
				if item.Provider != "" {
					t.Fatalf("provider = %q, want empty for a local match", item.Provider)
				}
			},
		},
		{
			name: "no source",
			entry: domain.LastFmMatchCacheEntry{
				MatchKey: "key", NoSource: true,
				UpdatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
			},
			check: func(t *testing.T, item TopTrackItem) {
				t.Helper()
				if !item.NoSource || item.Matched || item.Matching {
					t.Fatalf("item = %+v, want the cached no_source state", item)
				}
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			source := connectedSource(nil)
			matcher := &fakeMatcher{sources: []domain.CatalogSearchSource{{ID: "wy"}}}
			recommender, events := newTopTrackRecommender(source, matcher)
			cache := newFakeMatchCacheStore()
			recommender.SetMatchCache(cache)

			item := pendingTopItem("Artist", "Hit")
			testCase.entry.MatchKey = item.MatchKey
			cache.put(testCase.entry)

			states := recommender.seedTopTrackStates(
				context.Background(), []TopTrackItem{item}, []string{"wy"})
			if len(states) != 1 || !states[0].done {
				t.Fatalf("states = %+v, want one resolved row", states)
			}
			testCase.check(t, states[0].item)

			recommender.startTopTrackMatching("listener", "", states, []string{"wy"})
			events.waitDone(t)
			if calls := matcher.sourcesCalled(); len(calls) != 0 {
				t.Fatalf("source calls = %v, want none for a cache hit", calls)
			}
		})
	}
}

func TestExpiredMatchCacheEntryRematches(t *testing.T) {
	source := connectedSource(nil)
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
	recommender, events := newTopTrackRecommender(source, matcher)
	cache := newFakeMatchCacheStore()
	recommender.SetMatchCache(cache)

	item := pendingTopItem("Artist", "Hit")
	expired := liveMatchEntry(item.MatchKey, "wy", "old")
	expired.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	cache.put(expired)

	states := recommender.seedTopTrackStates(
		context.Background(), []TopTrackItem{item}, []string{"wy"})
	if states[0].done {
		t.Fatalf("expired entry resolved the row: %+v", states[0].item)
	}
	recommender.startTopTrackMatching("listener", "", states, []string{"wy"})
	updates := events.waitDone(t)
	if len(updates) != 1 || updates[0].ExternalID != "1" {
		t.Fatalf("updates = %+v, want a fresh source match", updates)
	}
	if calls := matcher.sourcesCalled(); len(calls) == 0 {
		t.Fatal("an expired entry must send the row back to the source")
	}
	if entry, ok := cache.entry(item.MatchKey); !ok || entry.ExternalID != "1" {
		t.Fatalf("cache entry = %+v, want the refreshed match", entry)
	}
}

func TestRefreshInheritsSettledRows(t *testing.T) {
	recommender := newTestRecommender(connectedSource(nil), nil, &fakeStreamRepo{})
	store := newFakeRecommendationStore()
	recommender.SetRecommendationStore(store)
	ctx := context.Background()

	rows := []domain.RecommendationItem{
		{
			Kind: domain.LastFmRecommendationTopTracks, MatchKey: candidateKey("Artist", "Provider"),
			Title: "Provider", Artist: "Artist", CoverURL: "https://old/cover.jpg",
			Matching: false, Matched: true, Provider: "wy", ExternalID: "1",
			Sources: []string{"wy"}, Downloadable: true,
		},
		{
			Kind: domain.LastFmRecommendationTopTracks, MatchKey: candidateKey("Artist", "Local"),
			Title: "Local", Artist: "Artist", CoverURL: "https://old/local.jpg",
			Matching: false, Matched: true,
			LocalTrackID: "local-1", State: domain.CatalogRowStateInLibrary,
			Provider: "wy", ExternalID: "9",
		},
		{
			Kind: domain.LastFmRecommendationTopTracks, MatchKey: candidateKey("Artist", "Gone"),
			Title: "Gone", Artist: "Artist", Matching: false, NoSource: true,
		},
	}
	if err := store.SaveRecommendationList(ctx, &domain.RecommendationList{
		Kind: domain.LastFmRecommendationTopTracks, Title: "lastfm.top",
		Items: rows, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed list: %v", err)
	}

	tracks := []LastFmTrack{
		{Artist: "Artist", Title: "Provider", Duration: 200, ImageURL: "https://new/provider.jpg"},
		{Artist: "Artist", Title: "Local", Duration: 200},
		{Artist: "Artist", Title: "Gone", Duration: 200},
		{Artist: "Artist", Title: "New", Duration: 200},
	}
	items := recommender.buildTopTrackItems(tracks)
	items = recommender.inheritStoredMatches(
		ctx, domain.LastFmRecommendationTopTracks, items)
	if len(items) != 4 {
		t.Fatalf("items = %d, want 4", len(items))
	}
	byKey := make(map[string]TopTrackItem, len(items))
	for _, item := range items {
		byKey[item.MatchKey] = item
	}

	provider := byKey[candidateKey("Artist", "Provider")]
	if provider.Matching || provider.Provider != "wy" || provider.ExternalID != "1" ||
		!provider.Downloadable || provider.CoverURL != "https://new/provider.jpg" {
		t.Fatalf("provider row = %+v, want the inherited match and the fresh Last.fm cover", provider)
	}
	local := byKey[candidateKey("Artist", "Local")]
	if local.Matching || local.LocalTrackID != "local-1" ||
		local.State != domain.CatalogRowStateInLibrary || local.Provider != "" ||
		local.CoverURL != "https://old/local.jpg" {
		t.Fatalf("local row = %+v, want the inherited library hit and its cover", local)
	}
	if gone := byKey[candidateKey("Artist", "Gone")]; !gone.Matching || gone.NoSource {
		t.Fatalf("no_source row = %+v, want it re-queued (TTL lives in the cache)", gone)
	}
	if fresh := byKey[candidateKey("Artist", "New")]; !fresh.Matching || fresh.Provider != "" {
		t.Fatalf("new row = %+v, want it pending", fresh)
	}
}

func TestRefreshQueuesOnlyRowsWithoutACachedAnswer(t *testing.T) {
	recommender := newTestRecommender(connectedSource(nil), nil, &fakeStreamRepo{})
	cache := newFakeMatchCacheStore()
	recommender.SetMatchCache(cache)

	items := []TopTrackItem{
		pendingTopItem("Artist", "Provider"),
		pendingTopItem("Artist", "Local"),
		pendingTopItem("Artist", "Gone"),
		pendingTopItem("Artist", "New"),
	}
	applyInheritedMatches([]domain.RecommendationItem{
		{
			Kind: domain.LastFmRecommendationTopTracks, MatchKey: items[0].MatchKey,
			Matched: true, Provider: "wy", ExternalID: "1",
		},
		{
			Kind: domain.LastFmRecommendationTopTracks, MatchKey: items[1].MatchKey,
			Matched: true, LocalTrackID: "local-1", State: domain.CatalogRowStateInLibrary,
		},
	}, items)
	noSource := pendingTopItem("Artist", "Gone")
	noSource.MatchKey = items[2].MatchKey
	noSource.NoSource = true
	noSource.Matching = false
	cache.put(domain.LastFmMatchCacheEntry{
		MatchKey: items[2].MatchKey, NoSource: true,
		UpdatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	})

	states := recommender.seedTopTrackStates(
		context.Background(), items, []string{"wy"})
	for index, state := range states {
		wantDone := index < 3
		if state.done != wantDone {
			t.Fatalf("row %d done = %v, want %v (%+v)",
				index, state.done, wantDone, state.item)
		}
	}
}

func TestManualRefreshBypassesMatchCache(t *testing.T) {
	source := connectedSource(nil)
	matcher := &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "kg"}},
		sourceRows: map[string]map[string][]domain.CatalogSearchResult{
			"kg": {
				"Artist\x00Hit": {{
					Provider: "kg", ExternalID: "2", Title: "Hit",
					Artists: []string{"Artist"}, Downloadable: true,
				}},
			},
		},
	}
	recommender, events := newTopTrackRecommender(source, matcher)
	store := newFakeRecommendationStore()
	recommender.SetRecommendationStore(store)
	cache := newFakeMatchCacheStore()
	recommender.SetMatchCache(cache)
	ctx := context.Background()

	key := candidateKey("Artist", "Hit")
	if err := store.SaveRecommendationList(ctx, &domain.RecommendationList{
		Kind: domain.LastFmRecommendationTopTracks, Title: "lastfm.top",
		Items: []domain.RecommendationItem{{
			Kind: domain.LastFmRecommendationTopTracks, MatchKey: key,
			Title: "Hit", Artist: "Artist", Matched: true, Provider: "wy", ExternalID: "1",
		}},
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed list: %v", err)
	}
	cache.put(liveMatchEntry(key, "wy", "1"))

	if err := recommender.RefreshRecommendationItem(
		ctx, domain.LastFmRecommendationTopTracks, key); err != nil {
		t.Fatalf("RefreshRecommendationItem: %v", err)
	}
	if _, ok := cache.entry(key); ok {
		t.Fatal("the manual refresh must drop the shared cache entry")
	}
	final := events.waitFinal(t, key)
	if final.Provider != "kg" || final.ExternalID != "2" || final.Matching {
		t.Fatalf("refreshed item = %+v, want the fresh kg match", final)
	}
}

func TestLocalLibraryUpdateInvalidatesMatchCache(t *testing.T) {
	recommender := newTestRecommender(connectedSource(nil), nil, &fakeStreamRepo{})
	store := newFakeRecommendationStore()
	recommender.SetRecommendationStore(store)
	cache := newFakeMatchCacheStore()
	recommender.SetMatchCache(cache)
	// Keep the resumed worker out of this test; the reset itself is the contract.
	recommender.markMatchingActive(domain.LastFmRecommendationTopTracks, true)
	ctx := context.Background()

	key := candidateKey("Artist", "Hit")
	if err := store.SaveRecommendationList(ctx, &domain.RecommendationList{
		Kind: domain.LastFmRecommendationTopTracks, Title: "lastfm.top",
		Items: []domain.RecommendationItem{{
			Kind: domain.LastFmRecommendationTopTracks, MatchKey: key,
			Title: "Hit", Artist: "Artist", Matched: true, Provider: "wy", ExternalID: "1",
			Sources: []string{"wy"}, Downloadable: true,
		}},
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed list: %v", err)
	}
	cache.put(liveMatchEntry(key, "wy", "1"))

	recommender.InvalidateLocalMatch(&domain.TrackDTO{
		Track: domain.Track{ID: "local-1", Title: "Hit", RawArtistNames: "Artist"},
	})

	if _, ok := cache.entry(key); ok {
		t.Fatal("the match cache entry must be dropped after a library import")
	}
	list, err := store.GetRecommendationList(ctx, domain.LastFmRecommendationTopTracks)
	if err != nil || list == nil {
		t.Fatalf("GetRecommendationList = %+v, %v", list, err)
	}
	row := list.Items[0]
	if !row.Matching || row.Provider != "" || row.LocalTrackID != "" {
		t.Fatalf("stored row = %+v, want it re-queued", row)
	}
}

func TestNoSourceIsCachedOnlyForAnsweredSources(t *testing.T) {
	run := func(t *testing.T, matcher *fakeMatcher) *fakeMatchCacheStore {
		t.Helper()
		source := connectedSource(nil)
		recommender, events := newTopTrackRecommender(source, matcher)
		cache := newFakeMatchCacheStore()
		recommender.SetMatchCache(cache)
		item := pendingTopItem("Artist", "Hit")
		states := recommender.seedTopTrackStates(
			context.Background(), []TopTrackItem{item}, []string{"wy"})
		recommender.startTopTrackMatching("listener", "", states, []string{"wy"})
		events.waitDone(t)
		return cache
	}

	t.Run("every source errored", func(t *testing.T) {
		matcher := &fakeMatcher{
			sources: []domain.CatalogSearchSource{{ID: "wy"}},
			errs:    map[string]error{"Artist\x00Hit": errors.New("provider offline")},
		}
		cache := run(t, matcher)
		if entry, ok := cache.entry(candidateKey("Artist", "Hit")); ok {
			t.Fatalf("cache entry = %+v, want no answer for an errored source", entry)
		}
	})

	t.Run("source answered without a playable row", func(t *testing.T) {
		matcher := &fakeMatcher{sources: []domain.CatalogSearchSource{{ID: "wy"}}}
		cache := run(t, matcher)
		entry, ok := cache.entry(candidateKey("Artist", "Hit"))
		if !ok || !entry.NoSource {
			t.Fatalf("cache entry = %+v, want a no_source answer", entry)
		}
		if ttl := time.Until(entry.ExpiresAt); ttl < persistentMatchTTLNoSource-time.Minute {
			t.Fatalf("ttl = %v, want the no-source window", ttl)
		}
	})
}

func TestPersistentMatchTTLByOutcome(t *testing.T) {
	cases := []struct {
		name       string
		local      bool
		noSource   bool
		suspicious bool
		want       time.Duration
	}{
		{"provider", false, false, false, persistentMatchTTLProvider},
		{"local", true, false, false, persistentMatchTTLLocal},
		{"no source", false, true, false, persistentMatchTTLNoSource},
		{"suspicious provider", false, false, true, persistentMatchTTLSuspicious},
		{"suspicious local", true, false, true, persistentMatchTTLSuspicious},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := persistentMatchTTL(testCase.local, testCase.noSource, testCase.suspicious)
			if got != testCase.want {
				t.Fatalf("ttl = %v, want %v", got, testCase.want)
			}
		})
	}
}
