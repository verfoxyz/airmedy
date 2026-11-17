package lastfm

import (
	"context"
	"strings"
	"time"

	"airmedy/internal/domain"
)

// Per-outcome lifetimes of a persisted match. A source identity is stable for
// weeks, a library hit even longer; a "no source" answer is the most volatile
// because catalogues add tracks all the time, and a suspicious local hit is
// retried early so a better copy can replace it.
const (
	persistentMatchTTLProvider   = 14 * 24 * time.Hour
	persistentMatchTTLLocal      = 30 * 24 * time.Hour
	persistentMatchTTLNoSource   = 3 * 24 * time.Hour
	persistentMatchTTLSuspicious = 7 * 24 * time.Hour
	// persistentMatchCacheLimit bounds the table; the newest rows win.
	persistentMatchCacheLimit = 5000
)

// SetMatchCache installs the shared persistent match cache. Optional: without
// it the recommender only uses its in-memory caches.
func (r *Recommender) SetMatchCache(store domain.LastFmMatchCacheRepository) {
	r.mu.Lock()
	r.matchCache = store
	r.mu.Unlock()
}

func (r *Recommender) matchCacheOrNil() domain.LastFmMatchCacheRepository {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.matchCache
}

// PruneMatchCache drops expired and overflow entries. It runs at startup, so a
// long-lived cache does not keep stale identities forever.
func (r *Recommender) PruneMatchCache(ctx context.Context) {
	store := r.matchCacheOrNil()
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, recommendationStoreTimeout)
	defer cancel()
	if err := store.PruneMatchCache(ctx, r.now().UTC(), persistentMatchCacheLimit); err != nil {
		r.logger.Warn("last.fm match cache: prune", "error", err)
	}
}

// liveMatchCache resolves the given keys to unexpired entries. A database error
// only degrades to an empty answer: matching continues as before.
func (r *Recommender) liveMatchCache(
	ctx context.Context, keys []string,
) map[string]domain.LastFmMatchCacheEntry {
	store := r.matchCacheOrNil()
	if store == nil || len(keys) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, recommendationStoreTimeout)
	defer cancel()
	entries, err := store.GetMatchCache(ctx, keys, r.now().UTC())
	if err != nil {
		r.logger.Warn("last.fm match cache: read", "error", err)
		return nil
	}
	if len(entries) == 0 {
		return nil
	}
	result := make(map[string]domain.LastFmMatchCacheEntry, len(entries))
	for _, entry := range entries {
		result[entry.MatchKey] = entry
	}
	return result
}

// storeMatchCacheEntries upserts finished matches. Failures are logged only:
// losing a cache write costs one future search, not the page.
func (r *Recommender) storeMatchCacheEntries(entries []domain.LastFmMatchCacheEntry) {
	store := r.matchCacheOrNil()
	if store == nil || len(entries) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recommendationStoreTimeout)
	defer cancel()
	if err := store.SaveMatchCache(ctx, entries, r.now().UTC(), persistentMatchCacheLimit); err != nil {
		r.logger.Warn("last.fm match cache: save", "error", err)
	}
}

// deleteMatchCacheKeys drops the given identities, so a manual re-match or a
// library change is not answered from the cache.
func (r *Recommender) deleteMatchCacheKeys(keys []string) {
	store := r.matchCacheOrNil()
	if store == nil || len(keys) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recommendationStoreTimeout)
	defer cancel()
	if err := store.DeleteMatchCache(ctx, keys); err != nil {
		r.logger.Warn("last.fm match cache: delete", "error", err)
	}
}

// persistentMatchTTL picks the lifetime of one outcome.
func persistentMatchTTL(local, noSource, suspicious bool) time.Duration {
	switch {
	case noSource:
		return persistentMatchTTLNoSource
	case suspicious:
		return persistentMatchTTLSuspicious
	case local:
		return persistentMatchTTLLocal
	default:
		return persistentMatchTTLProvider
	}
}

// matchCacheEntryForItem turns a finished row into a cache entry.
func (r *Recommender) matchCacheEntryForItem(
	item TopTrackItem, noSource bool,
) domain.LastFmMatchCacheEntry {
	local := strings.TrimSpace(item.LocalTrackID) != "" ||
		item.State == domain.CatalogRowStateInLibrary
	ttl := persistentMatchTTL(local, noSource, item.Suspicious)
	now := r.now().UTC()
	return domain.LastFmMatchCacheEntry{
		MatchKey:     item.MatchKey,
		Provider:     item.Provider,
		ExternalID:   item.ExternalID,
		LocalTrackID: item.LocalTrackID,
		NoSource:     noSource,
		Suspicious:   item.Suspicious,
		CoverURL:     item.CoverURL,
		Duration:     item.Duration,
		UpdatedAt:    now,
		ExpiresAt:    now.Add(ttl),
	}
}

// applyMatchCacheEntry writes a cached answer onto a row. The row keeps its
// fresh Last.fm metadata; only the match fields and a missing cover change.
func applyMatchCacheEntry(item *TopTrackItem, entry domain.LastFmMatchCacheEntry) {
	item.Matching = false
	item.Suspicious = entry.Suspicious
	switch {
	case strings.TrimSpace(entry.LocalTrackID) != "":
		item.LocalTrackID = entry.LocalTrackID
		item.State = domain.CatalogRowStateInLibrary
		item.Provider = ""
		item.ExternalID = ""
		item.Sources = []string{}
		item.Downloadable = false
		item.BlockedReason = ""
		item.Matched = true
		item.NoSource = false
	case strings.TrimSpace(entry.Provider) != "":
		item.Provider = entry.Provider
		item.ExternalID = entry.ExternalID
		item.State = ""
		item.Matched = true
		item.NoSource = false
	default:
		item.NoSource = entry.NoSource
		item.Matched = false
	}
	if item.CoverURL == "" && entry.CoverURL != "" {
		item.CoverURL = entry.CoverURL
	}
	if item.Duration == 0 && entry.Duration > 0 {
		item.Duration = entry.Duration
	}
}

// recommendedFromCache applies a persistent entry to the legacy page row,
// keeping the Last.fm metadata the row already carries.
func recommendedFromCache(
	base RecommendedTrack, entry domain.LastFmMatchCacheEntry,
) RecommendedTrack {
	result := base
	result.Matching = false
	switch {
	case strings.TrimSpace(entry.LocalTrackID) != "":
		result.LocalTrackID = entry.LocalTrackID
		result.State = domain.CatalogRowStateInLibrary
		result.Provider = ""
		result.ExternalID = ""
		result.Sources = []string{}
	default:
		if entry.Provider != "" {
			result.Provider = entry.Provider
			result.ExternalID = entry.ExternalID
		}
	}
	if result.CoverURL == "" && entry.CoverURL != "" {
		result.CoverURL = entry.CoverURL
		result.CoverURLLarge = entry.CoverURL
	}
	if result.Duration == 0 && entry.Duration > 0 {
		result.Duration = entry.Duration
	}
	return result
}

// legacyMatchCacheEntry builds the legacy page's cache write. A row whose
// sources all errored (err != nil) is not a definitive "no source" answer.
func (r *Recommender) legacyMatchCacheEntry(
	row RecommendedTrack, match *domain.CatalogSearchResult, searchErr error,
) *domain.LastFmMatchCacheEntry {
	if row.MatchKey == "" {
		return nil
	}
	now := r.now().UTC()
	entry := domain.LastFmMatchCacheEntry{MatchKey: row.MatchKey, UpdatedAt: now}
	if match != nil {
		entry.Provider = match.Provider
		entry.ExternalID = match.ExternalID
		entry.LocalTrackID = match.LocalTrackID
		entry.Suspicious = match.Suspicious
		entry.CoverURL = strings.TrimSpace(match.CoverURLLarge)
		if entry.CoverURL == "" {
			entry.CoverURL = strings.TrimSpace(match.CoverURL)
		}
		entry.Duration = match.Duration
		local := match.LocalTrackID != "" || match.State == domain.CatalogRowStateInLibrary
		entry.ExpiresAt = now.Add(persistentMatchTTL(local, false, match.Suspicious))
	} else {
		if searchErr != nil {
			return nil
		}
		entry.NoSource = true
		entry.ExpiresAt = now.Add(persistentMatchTTLNoSource)
	}
	return &entry
}

// inheritStoredMatches applies the previous version of the same list to the
// rows a refresh just built. Rows with a stable provider or local identity skip
// the matching queue; "no source" is deliberately not inherited, because its
// TTL lives in the persistent cache.
func (r *Recommender) inheritStoredMatches(
	ctx context.Context, kind string, items []TopTrackItem,
) []TopTrackItem {
	store := r.recommendationStoreOrNil()
	if store == nil || len(items) == 0 {
		return items
	}
	list, err := store.GetRecommendationList(ctx, kind)
	if err != nil {
		r.logger.Warn("last.fm recommendations: load rows to inherit",
			"kind", kind, "error", err)
		return items
	}
	if list == nil {
		return items
	}
	return applyInheritedMatches(list.Items, items)
}

// previousRecommendationRows loads one stored list as an inheritance lookup.
func (r *Recommender) previousRecommendationRows(
	ctx context.Context, kind string,
) map[string]domain.RecommendationItem {
	store := r.recommendationStoreOrNil()
	if store == nil {
		return nil
	}
	list, err := store.GetRecommendationList(ctx, kind)
	if err != nil {
		r.logger.Warn("last.fm recommendations: load rows to inherit",
			"kind", kind, "error", err)
		return nil
	}
	if list == nil {
		return nil
	}
	return inheritedMatchIndex(list.Items)
}

// inheritedMatchIndex keys the previous rows by their stable identity.
func inheritedMatchIndex(rows []domain.RecommendationItem) map[string]domain.RecommendationItem {
	index := make(map[string]domain.RecommendationItem, len(rows))
	for _, row := range rows {
		if row.MatchKey == "" {
			continue
		}
		index[row.MatchKey] = row
	}
	return index
}

func applyInheritedMatches(
	previous []domain.RecommendationItem, items []TopTrackItem,
) []TopTrackItem {
	index := inheritedMatchIndex(previous)
	if len(index) == 0 {
		return items
	}
	for position := range items {
		if row, ok := index[items[position].MatchKey]; ok {
			applyInheritedMatch(&items[position], row)
		}
	}
	return items
}

// applyInheritedMatch carries the settled match fields of a previous row.
func applyInheritedMatch(item *TopTrackItem, row domain.RecommendationItem) {
	if item.MatchKey == "" || row.MatchKey == "" {
		return
	}
	// A fresh Last.fm image wins; otherwise the previous cover is better than
	// the placeholder the row would show until it matches again.
	if item.CoverURL == "" {
		item.CoverURL = row.CoverURL
	}
	if item.CoverURLLarge == "" {
		item.CoverURLLarge = row.CoverURLLarge
	}
	switch {
	case strings.TrimSpace(row.LocalTrackID) != "":
		item.LocalTrackID = row.LocalTrackID
		item.State = domain.CatalogRowStateInLibrary
		item.Provider = ""
		item.ExternalID = ""
		item.Sources = []string{}
		item.Downloadable = false
		item.BlockedReason = ""
		item.Suspicious = row.Suspicious
		item.Matching = false
		item.Matched = true
		item.NoSource = false
	case strings.TrimSpace(row.Provider) != "":
		item.Provider = row.Provider
		item.ExternalID = row.ExternalID
		item.State = row.State
		item.Sources = append([]string{}, row.Sources...)
		item.Downloadable = row.Downloadable
		item.BlockedReason = row.BlockedReason
		item.Suspicious = row.Suspicious
		item.Matching = false
		item.Matched = true
		item.NoSource = false
	}
}

// InvalidateLocalMatch reacts to a library row being added or updated: the
// shared cache entry for its identity is dropped and every stored list row for
// that identity re-enters matching, so it can resolve to the local copy.
func (r *Recommender) InvalidateLocalMatch(track *domain.TrackDTO) {
	if track == nil || track.IsRemote() {
		return
	}
	keySet := localMatchKeys(track)
	if len(keySet) == 0 {
		return
	}
	keys := make([]string, 0, len(keySet))
	for key := range keySet {
		keys = append(keys, key)
	}
	r.deleteMatchCacheKeys(keys)
	r.dropMemoryMatches(keys)

	store := r.recommendationStoreOrNil()
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recommendationStoreTimeout)
	affected, err := store.ResetRecommendationMatches(ctx, keys)
	cancel()
	if err != nil {
		r.logger.Warn("last.fm recommendations: reset local rows", "error", err)
		return
	}
	if affected > 0 {
		r.resumeRecommendationMatching(context.Background(), "")
	}
}

// dropMemoryMatches clears the in-memory caches for the given identities.
func (r *Recommender) dropMemoryMatches(keys []string) {
	set := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		set[key] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range set {
		delete(r.matches, key)
	}
	for cacheKey := range r.sourceMatches {
		index := strings.LastIndexByte(cacheKey, '|')
		if index <= 0 {
			continue
		}
		ref := splitRecommendationRowKey(cacheKey[:index])
		if _, ok := set[ref.matchKey]; ok {
			delete(r.sourceMatches, cacheKey)
		}
	}
}
