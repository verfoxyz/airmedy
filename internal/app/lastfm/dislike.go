package lastfm

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"airmedy/internal/domain"

	"github.com/google/uuid"
)

// dislikeUpdatedEvent announces a change to the local exclusion list. The
// payload is the full list, so a page can either refresh its cache or filter
// the rows it already rendered.
const dislikeUpdatedEvent = "dislike:updated"

// DislikeUpdate is the payload of dislike:updated.
type DislikeUpdate struct {
	Items []domain.LastFmDislike `json:"items"`
}

// DislikeSettingsReader reads the purge toggle. nil means "off".
type DislikeSettingsReader interface {
	GetSettings(ctx context.Context) (*domain.AppSettings, error)
}

// DislikeLocalPurger deletes the on-disk side of a dislike. *library.Service
// implements it; the port keeps app/lastfm out of the library pipeline.
type DislikeLocalPurger interface {
	// CountDislikeTargets reports how many local tracks the dislike would
	// delete. It changes nothing.
	CountDislikeTargets(ctx context.Context, kind, matchKey, title, artist string) (int, error)
	// PurgeDislikeTargets deletes those tracks through the normal delete path
	// (file, row, artwork, stream cache, saved lyrics) and returns how many were
	// removed. A partial failure is returned as an error alongside the count.
	PurgeDislikeTargets(ctx context.Context, kind, matchKey, title, artist string) (int, error)
}

// DislikeResult is one dislike and what the optional file purge did.
type DislikeResult struct {
	Entry domain.LastFmDislike
	// DeletedTracks counts local files removed by this dislike.
	DeletedTracks int
	// PurgeError is non-empty when the mark was saved but some files could not
	// be deleted. The two outcomes are reported separately on purpose.
	PurgeError string
}

// dislikeSnapshot is the in-memory form of the exclusion list: one set of
// track identities and one set of individual artist keys, so a row is filtered
// without a database round trip.
type dislikeSnapshot struct {
	tracks  map[string]struct{}
	artists map[string]struct{}
}

func newDislikeSnapshot(rows []domain.LastFmDislike) dislikeSnapshot {
	snapshot := dislikeSnapshot{
		tracks:  make(map[string]struct{}, len(rows)),
		artists: make(map[string]struct{}, len(rows)),
	}
	for _, row := range rows {
		switch row.Kind {
		case domain.LastFmDislikeTrack:
			if row.MatchKey != "" {
				snapshot.tracks[row.MatchKey] = struct{}{}
			}
		case domain.LastFmDislikeArtist:
			// The stored key is an ArtistSetKey; splitting it back into
			// individual keys lets a track whose credit list contains the
			// artist be blocked too. A row stored without the rendered credit
			// falls back to the key's own parts.
			parts := domain.ArtistKeyParts([]string{row.Artist})
			if len(parts) == 0 {
				for _, part := range strings.Split(row.MatchKey, "\x1f") {
					if part != "" {
						parts = append(parts, part)
					}
				}
			}
			for _, key := range parts {
				snapshot.artists[key] = struct{}{}
			}
		}
	}
	return snapshot
}

// blocksTrack reports whether the identity, or one of its credited artists, is
// excluded.
func (s dislikeSnapshot) blocksTrack(matchKey, artist string) bool {
	if _, blocked := s.tracks[matchKey]; blocked {
		return true
	}
	return s.blocksArtist(artist)
}

func (s dislikeSnapshot) blocksArtist(name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	for _, key := range domain.ArtistKeyParts([]string{name}) {
		if _, blocked := s.artists[key]; blocked {
			return true
		}
	}
	return false
}

// SetDislikeStore installs the local exclusion store. Optional: without it the
// recommender and radio simply never exclude anything.
func (r *Recommender) SetDislikeStore(store domain.LastFmDislikeRepository) {
	r.mu.Lock()
	r.dislikes = store
	r.mu.Unlock()
}

// SetDislikeTracePurger installs the transactional cleanup of the recommendation
// rows and match-cache entries a dislike makes obsolete.
func (r *Recommender) SetDislikeTracePurger(purger domain.LastFmDislikeTracePurger) {
	r.mu.Lock()
	r.dislikeTracePurger = purger
	r.mu.Unlock()
}

// SetDislikeLocalPurger installs the on-disk deletion used when the purge
// setting is on.
func (r *Recommender) SetDislikeLocalPurger(purger DislikeLocalPurger) {
	r.mu.Lock()
	r.dislikePurger = purger
	r.mu.Unlock()
}

// SetDislikeSettings installs the reader for the purge toggle.
func (r *Recommender) SetDislikeSettings(reader DislikeSettingsReader) {
	r.mu.Lock()
	r.dislikeSettings = reader
	r.mu.Unlock()
}

// DislikePurgeEnabled reports whether a dislike should also delete local files.
func (r *Recommender) DislikePurgeEnabled(ctx context.Context) bool {
	r.mu.Lock()
	reader := r.dislikeSettings
	r.mu.Unlock()
	if reader == nil {
		return false
	}
	settings, err := reader.GetSettings(ctx)
	if err != nil || settings == nil {
		return false
	}
	return settings.DislikePurgeFiles
}

// CountDislikeTargets reports how many local tracks a dislike would delete while
// the purge setting is on; zero when it is off.
func (r *Recommender) CountDislikeTargets(
	ctx context.Context, kind, matchKey, title, artist string,
) (int, error) {
	r.mu.Lock()
	purger := r.dislikePurger
	r.mu.Unlock()
	if purger == nil || !r.DislikePurgeEnabled(ctx) {
		return 0, nil
	}
	return purger.CountDislikeTargets(ctx, kind, matchKey, title, artist)
}

func (r *Recommender) dislikeStoreOrNil() domain.LastFmDislikeRepository {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dislikes
}

// currentDislikes returns the exclusion set, loading it once per session and
// after every change. A database error degrades to "nothing excluded" instead
// of failing the recommendation page.
func (r *Recommender) currentDislikes(ctx context.Context) dislikeSnapshot {
	r.mu.Lock()
	cached := r.dislikeCache
	store := r.dislikes
	r.mu.Unlock()
	if cached != nil {
		return *cached
	}
	if store == nil {
		return dislikeSnapshot{}
	}
	callCtx, cancel := context.WithTimeout(ctx, recommendationStoreTimeout)
	defer cancel()
	rows, err := store.ListDislikes(callCtx)
	if err != nil {
		// Do not cache the failure: a later call retries the read instead of
		// disabling filtering for the rest of the session.
		r.logger.Warn("last.fm dislikes: read", "error", err)
		return dislikeSnapshot{}
	}
	snapshot := newDislikeSnapshot(rows)
	r.mu.Lock()
	r.dislikeCache = &snapshot
	r.mu.Unlock()
	return snapshot
}

// Dislike adds one exclusion. A repeat of the same identity is idempotent and
// still refreshes the frontend cache.
//
// The mark always saves first. Purging the recommendation traces then happens
// in one transaction; an error there is returned while the mark stays. The
// optional local-file purge is reported separately: a locked file must not make
// the dislike look unsaved.
func (r *Recommender) Dislike(
	ctx context.Context, kind, matchKey, title, artist string, purge bool,
) (DislikeResult, error) {
	kind = strings.TrimSpace(kind)
	title = strings.TrimSpace(title)
	artist = strings.TrimSpace(artist)
	key, err := dislikeMatchKey(kind, matchKey, title, artist)
	if err != nil {
		return DislikeResult{}, err
	}
	if purge && !r.DislikePurgeEnabled(ctx) {
		return DislikeResult{}, fmt.Errorf("lastfm dislikes: file purge is disabled in settings")
	}
	store := r.dislikeStoreOrNil()
	if store == nil {
		return DislikeResult{}, fmt.Errorf("lastfm dislikes: storage is unavailable")
	}
	entry := domain.LastFmDislike{
		ID:        uuid.NewString(),
		Kind:      kind,
		MatchKey:  key,
		Title:     title,
		Artist:    artist,
		CreatedAt: r.now().UTC(),
	}
	callCtx, cancel := context.WithTimeout(ctx, recommendationStoreTimeout)
	stored, err := store.AddDislike(callCtx, entry)
	cancel()
	if err != nil {
		return DislikeResult{}, err
	}
	result := DislikeResult{Entry: stored}

	traceKeys := r.dislikeTraceKeys(ctx, kind, stored.MatchKey, artist)
	if purger := r.dislikeTracePurgerOrNil(); purger != nil && len(traceKeys) > 0 {
		purgeCtx, purgeCancel := context.WithTimeout(ctx, recommendationStoreTimeout)
		purgeErr := purger.PurgeDislikeTraces(purgeCtx, traceKeys)
		purgeCancel()
		if purgeErr != nil {
			r.logger.Warn("last.fm dislikes: cannot purge recommendation traces", "error", purgeErr)
			if _, reloadErr := r.reloadDislikes(ctx); reloadErr != nil {
				r.logger.Warn("last.fm dislikes: reload after purge failure", "error", reloadErr)
			}
			return result, purgeErr
		}
		r.dropMemoryMatches(traceKeys)
		r.mu.Lock()
		// The legacy page caches a built page; a purged row must not reappear
		// from it on the next open.
		r.pages = make(map[string]cachedPage)
		r.mu.Unlock()
	}

	if purge {
		purger := r.dislikeLocalPurgerOrNil()
		if purger == nil {
			result.PurgeError = "local purge is unavailable"
		} else {
			deleted, purgeErr := purger.PurgeDislikeTargets(ctx, kind, stored.MatchKey, title, artist)
			result.DeletedTracks = deleted
			if purgeErr != nil {
				// The mark is saved; the files that could not be removed are
				// reported on their own.
				result.PurgeError = purgeErr.Error()
			}
		}
	}

	if _, err := r.reloadDislikes(ctx); err != nil {
		// The exclusion is stored; only the cache refresh failed.
		r.logger.Warn("last.fm dislikes: reload after add", "error", err)
	}
	return result, nil
}

// dislikeLocalPurgerOrNil reads the file purger under the lock.
func (r *Recommender) dislikeLocalPurgerOrNil() DislikeLocalPurger {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dislikePurger
}

// dislikeTracePurgerOrNil reads the purger under the lock.
func (r *Recommender) dislikeTracePurgerOrNil() domain.LastFmDislikeTracePurger {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dislikeTracePurger
}

// dislikeTraceKeys collects the stored identities a dislike invalidates. A
// track dislike names one key; an artist dislike walks every stored row and the
// discover candidate pool for that artist's keys.
func (r *Recommender) dislikeTraceKeys(
	ctx context.Context, kind, matchKey, artist string,
) []string {
	keys := make(map[string]struct{})
	if kind == domain.LastFmDislikeTrack {
		if matchKey != "" {
			keys[matchKey] = struct{}{}
		}
		return sortedKeys(keys)
	}
	wanted := make(map[string]struct{})
	for _, part := range domain.ArtistKeyParts([]string{artist}) {
		wanted[part] = struct{}{}
	}
	if len(wanted) == 0 {
		return nil
	}
	blocked := func(name string) bool {
		for _, part := range domain.ArtistKeyParts([]string{name}) {
			if _, hit := wanted[part]; hit {
				return true
			}
		}
		return false
	}
	store := r.recommendationStoreOrNil()
	if store == nil {
		return nil
	}
	summaries, err := store.ListRecommendationLists(ctx)
	if err != nil {
		r.logger.Warn("last.fm dislikes: cannot list rows to purge", "error", err)
	}
	for _, summary := range summaries {
		list, listErr := store.GetRecommendationList(ctx, summary.Kind)
		if listErr != nil || list == nil {
			continue
		}
		for _, item := range list.Items {
			if item.MatchKey != "" && blocked(item.Artist) {
				keys[item.MatchKey] = struct{}{}
			}
		}
	}
	if pool, poolErr := store.GetRecommendationCandidates(
		ctx, domain.LastFmRecommendationDiscover); poolErr == nil {
		for _, item := range pool {
			if item.MatchKey != "" && blocked(item.Artist) {
				keys[item.MatchKey] = struct{}{}
			}
		}
	}
	return sortedKeys(keys)
}

func sortedKeys(keys map[string]struct{}) []string {
	result := make([]string, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

// Undislike removes one exclusion; the next refresh or refill can return the
// song or artist again.
func (r *Recommender) Undislike(ctx context.Context, id string) error {
	store := r.dislikeStoreOrNil()
	if store == nil {
		return fmt.Errorf("lastfm dislikes: storage is unavailable")
	}
	callCtx, cancel := context.WithTimeout(ctx, recommendationStoreTimeout)
	defer cancel()
	if err := store.DeleteDislike(callCtx, id); err != nil {
		return err
	}
	if _, err := r.reloadDislikes(ctx); err != nil {
		r.logger.Warn("last.fm dislikes: reload after delete", "error", err)
	}
	return nil
}

// ListDislikes returns the stored exclusions for the settings page.
func (r *Recommender) ListDislikes(ctx context.Context) ([]domain.LastFmDislike, error) {
	store := r.dislikeStoreOrNil()
	if store == nil {
		return []domain.LastFmDislike{}, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, recommendationStoreTimeout)
	defer cancel()
	rows, err := store.ListDislikes(callCtx)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// reloadDislikes re-reads the list, drops the seed and discovery caches that
// were built with the old set, and announces the new list.
func (r *Recommender) reloadDislikes(ctx context.Context) ([]domain.LastFmDislike, error) {
	store := r.dislikeStoreOrNil()
	r.mu.Lock()
	r.dislikeCache = nil
	r.seedCache = weeklyRecommendationSeedCache{}
	r.discoverCache = cachedDiscoverSeeds{}
	r.mu.Unlock()
	if store == nil {
		return []domain.LastFmDislike{}, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, recommendationStoreTimeout)
	defer cancel()
	rows, err := store.ListDislikes(callCtx)
	if err != nil {
		return nil, err
	}
	snapshot := newDislikeSnapshot(rows)
	r.mu.Lock()
	r.dislikeCache = &snapshot
	r.mu.Unlock()
	r.emitDislikes(rows)
	return rows, nil
}

func (r *Recommender) emitDislikes(items []domain.LastFmDislike) {
	if r.emitter == nil {
		return
	}
	if items == nil {
		items = []domain.LastFmDislike{}
	}
	r.emitter(dislikeUpdatedEvent, DislikeUpdate{Items: items})
}

// dislikeMatchKey derives the identity a dislike is stored under. A track keeps
// the caller's match key when it has one, because the row already shares it
// with the match cache; otherwise it is rebuilt from the credits.
func dislikeMatchKey(kind, matchKey, title, artist string) (string, error) {
	switch kind {
	case domain.LastFmDislikeTrack:
		if key := strings.TrimSpace(matchKey); key != "" {
			return key, nil
		}
		if artist == "" || title == "" {
			return "", fmt.Errorf("lastfm dislikes: a track needs an artist and a title")
		}
		return candidateKey(artist, title), nil
	case domain.LastFmDislikeArtist:
		// The artist key is always derived: a caller's match key would be the
		// artist's track identity, not the artist set.
		if key := domain.ArtistSetKey([]string{artist}); key != "" {
			return key, nil
		}
		if key := strings.TrimSpace(matchKey); key != "" {
			return key, nil
		}
		return "", fmt.Errorf("lastfm dislikes: an artist needs a name")
	default:
		return "", fmt.Errorf("lastfm dislikes: unknown kind %q", kind)
	}
}

// filterDislikedTracks drops excluded Last.fm rows. Candidates carry the
// Last.fm artist string, which may itself be a multi-artist credit.
func (r *Recommender) filterDislikedTracks(
	ctx context.Context, tracks []LastFmTrack,
) []LastFmTrack {
	snapshot := r.currentDislikes(ctx)
	if len(snapshot.tracks) == 0 && len(snapshot.artists) == 0 {
		return tracks
	}
	filtered := make([]LastFmTrack, 0, len(tracks))
	for _, track := range tracks {
		if snapshot.blocksTrack(candidateKey(track.Artist, track.Title), track.Artist) {
			continue
		}
		filtered = append(filtered, track)
	}
	return filtered
}

// filterDislikedItems drops rows already stored in a list, so a rewrite never
// keeps an exclusion.
func (r *Recommender) filterDislikedItems(
	ctx context.Context, items []TopTrackItem,
) []TopTrackItem {
	snapshot := r.currentDislikes(ctx)
	if len(snapshot.tracks) == 0 && len(snapshot.artists) == 0 {
		return items
	}
	filtered := make([]TopTrackItem, 0, len(items))
	for _, item := range items {
		artist := item.Artist
		if artist == "" && len(item.Artists) > 0 {
			artist = item.Artists[0]
		}
		if snapshot.blocksTrack(item.MatchKey, artist) {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

// filterDislikedRows applies the same rule to persisted rows.
func (r *Recommender) filterDislikedRows(
	ctx context.Context, rows []domain.RecommendationItem,
) []domain.RecommendationItem {
	snapshot := r.currentDislikes(ctx)
	if len(snapshot.tracks) == 0 && len(snapshot.artists) == 0 {
		return rows
	}
	filtered := make([]domain.RecommendationItem, 0, len(rows))
	for _, row := range rows {
		artist := row.Artist
		if artist == "" && len(row.Artists) > 0 {
			artist = row.Artists[0]
		}
		if snapshot.blocksTrack(row.MatchKey, artist) {
			continue
		}
		filtered = append(filtered, row)
	}
	return filtered
}
