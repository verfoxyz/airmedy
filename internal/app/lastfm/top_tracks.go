package lastfm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"

	"airmedy/internal/domain"
)

// TopTracks MVP constants. The page deliberately makes one Last.fm request and
// shows every row immediately; source matching then fills the rows in.
const (
	// topTrackPeriod is the whole-time window. The page is "most played tracks",
	// and a 7-day window is empty for an account that stopped listening a while
	// ago (it returned two rows for a real account while overall returned fifty).
	topTrackPeriod = "overall"
	topTrackLimit  = 50
	// topTrackSourceLimit is the number of source workers. Each worker owns one
	// source and searches only that source.
	topTrackSourceLimit = 5
	// similarSeedLimit is how many of the top tracks seed "for you". Each seed
	// costs one track.getSimilar request, so the seed count is the request budget.
	similarSeedLimit = 10
	// similarRecommendationTake bounds the rendered "for you" list.
	similarRecommendationTake = 50
)

// TopTrackItem is one row of the MVP Last.fm page.
//
// It starts as a Last.fm-only row (Matching=true, no provider). A source worker
// then either marks it Matched with a provider identity or NoSource after every
// source was tried.
type TopTrackItem struct {
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Album    string `json:"album"`
	Duration int    `json:"duration"`
	// Artists is the credit list the playback action needs.
	Artists       []string `json:"artists"`
	CoverURL      string   `json:"cover_url"`
	CoverURLLarge string   `json:"cover_url_large"`

	Matching      bool     `json:"matching"`
	Matched       bool     `json:"matched"`
	NoSource      bool     `json:"no_source"`
	Provider      string   `json:"provider"`
	ExternalID    string   `json:"external_id"`
	State         string   `json:"state"`
	LocalTrackID  string   `json:"local_track_id"`
	BlockedReason string   `json:"blocked_reason"`
	Sources       []string `json:"sources"`
	Downloadable  bool     `json:"downloadable"`
	// Suspicious reports a local match whose duration disagrees with Last.fm by
	// more than the accepted hint window. The row stays playable; the UI asks
	// the user to confirm instead of auto-saving or downloading.
	Suspicious bool `json:"suspicious"`
	// Kind is the list this row belongs to: top_tracks or similar. It is
	// persisted with the row, so an event can update the right list.
	Kind string `json:"kind"`
	// MatchKey is artist + "\x00" + title, the stable key the frontend uses to
	// update a row in place.
	MatchKey string `json:"match_key"`
}

// TopTrackPage is the immediate response: every Last.fm top track, plus the
// status chips already known from the match cache. SimilarItems is the
// "for you" list derived from the top tracks.
type TopTrackPage struct {
	Status       string         `json:"status"`
	Items        []TopTrackItem `json:"items"`
	SimilarItems []TopTrackItem `json:"similar_items"`
}

// TopTrackUpdate is the payload of lastfm:recommendations-matched for this MVP.
type TopTrackUpdate struct {
	// Kind is empty for the combined run that resolves both lists, and set for
	// a single-list refresh, so a detail page can ignore another kind's updates.
	Kind  string         `json:"kind"`
	Items []TopTrackItem `json:"items"`
	Done  bool           `json:"done"`
}

type topTrackState struct {
	item   TopTrackItem
	tried  map[string]bool
	active bool
	done   bool
}

type cachedSourceMatch struct {
	row       *domain.CatalogSearchResult
	expiresAt time.Time
}

// cachedSimilarRecommendation is one seed set's "for you" answer. Only the raw
// Last.fm rows are cached, so each page load re-applies the current match cache
// instead of freezing a stale provider identity.
type cachedSimilarRecommendation struct {
	tracks    []LastFmTrack
	partial   bool
	expiresAt time.Time
}

// recommendationCollection is one Last.fm fetch turned into page rows. Source
// matching starts only after this returns, so a caller can persist the rows
// before their first match update arrives.
type recommendationCollection struct {
	status       string
	topItems     []TopTrackItem
	similarItems []TopTrackItem
	sources      []string
	wantTop      bool
	wantSimilar  bool
}

// GetTopTrackRecommendations returns the user's all-time most played tracks.
//
// It makes exactly one Last.fm request and returns every row before any source
// search runs. Cached per-source outcomes are applied first, so a refresh shows
// the previous matches immediately; the remaining rows are matched by five
// source workers.
func (r *Recommender) GetTopTrackRecommendations(ctx context.Context) (TopTrackPage, error) {
	collected, err := r.collectRecommendations(ctx, true, true, false)
	if err != nil {
		return TopTrackPage{Status: collected.status, Items: []TopTrackItem{}}, err
	}
	if collected.status == StatusNotConnected {
		return TopTrackPage{Status: StatusNotConnected, Items: []TopTrackItem{}}, nil
	}
	topItems, similarItems := r.startRecommendationMatching(ctx, collected, true)
	return TopTrackPage{
		Status:       collected.status,
		Items:        topItems,
		SimilarItems: similarItems,
	}, nil
}

// collectRecommendations fetches the top tracks once and builds the requested
// lists. force bypasses the similar-answer cache, which an explicit refresh
// must do.
func (r *Recommender) collectRecommendations(
	ctx context.Context, wantTop, wantSimilar, force bool,
) (recommendationCollection, error) {
	collected := recommendationCollection{
		status:       StatusOK,
		topItems:     []TopTrackItem{},
		similarItems: []TopTrackItem{},
		wantTop:      wantTop,
		wantSimilar:  wantSimilar,
	}
	connected, username, _ := r.client.GetStatus()
	if !connected || strings.TrimSpace(username) == "" {
		collected.status = StatusNotConnected
		return collected, nil
	}

	tracks, err := r.client.GetTopTracks(ctx, topTrackPeriod, topTrackLimit)
	if err != nil {
		return collected, err
	}

	if wantTop {
		collected.topItems = r.buildTopTrackItems(tracks)
	}
	if wantSimilar {
		// "For you" reuses the same top tracks as seeds: track.getSimilar first,
		// the similar-artist fallback only for the seeds that returned nothing.
		// Local files are deliberately kept — a recommendation the user already
		// owns is playable and shows "in library".
		seeds := usableSeeds(tracks, similarSeedLimit)
		similarTracks, similarPartial := r.collectSimilarRecommendations(ctx, seeds, force)
		collected.similarItems = buildSimilarItems(seeds, similarTracks)
		if similarPartial {
			collected.status = StatusPartial
		}
	}
	collected.sources = r.topTrackSources(ctx)
	return collected, nil
}

// buildTopTrackItems keeps every usable unique Last.fm row.
func (r *Recommender) buildTopTrackItems(tracks []LastFmTrack) []TopTrackItem {
	items := make([]TopTrackItem, 0, len(tracks))
	seen := make(map[string]struct{}, len(tracks))
	for _, track := range tracks {
		if !usableTrack(track) {
			continue
		}
		key := candidateKey(track.Artist, track.Title)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		items = append(items, TopTrackItem{
			Title:    track.Title,
			Artist:   track.Artist,
			Album:    track.Album,
			Duration: track.Duration,
			Artists:  []string{track.Artist},
			CoverURL: track.ImageURL,
			Kind:     domain.LastFmRecommendationTopTracks,
			Matching: true,
			MatchKey: key,
		})
		if len(items) >= topTrackLimit {
			break
		}
	}
	return items
}

// startRecommendationMatching applies cached matches and starts the workers.
// It returns the rows to render, with cached and no-source states already
// applied.
//
// combined uses one run for both lists (the initial page load). A single-list
// refresh passes false so its Done frame names the only list it touched.
func (r *Recommender) startRecommendationMatching(
	ctx context.Context, collected recommendationCollection, combined bool,
) ([]TopTrackItem, []TopTrackItem) {
	_, username, _ := r.client.GetStatus()
	r.rememberTopRows(collected.topItems, collected.similarItems)

	if combined && collected.wantTop && collected.wantSimilar {
		topStates := r.seedTopTrackStates(collected.topItems, collected.sources)
		similarStates := r.seedTopTrackStates(collected.similarItems, collected.sources)
		// Cached hits are resolved before the run starts and would otherwise
		// stay "matching" in the database forever.
		r.persistMatchingStates(topStates)
		r.persistMatchingStates(similarStates)
		// Similar rows go first: the upper section should resolve before the long
		// top-tracks list.
		states := append(similarStates, topStates...)
		if anyTopStatePending(states) {
			r.markMatchingActive(domain.LastFmRecommendationTopTracks, true)
			r.markMatchingActive(domain.LastFmRecommendationSimilar, true)
			r.startTopTrackMatching(username, "", states, collected.sources)
		}
		return topTrackItemsFromStates(topStates), topTrackItemsFromStates(similarStates)
	}

	topItems := []TopTrackItem{}
	similarItems := []TopTrackItem{}
	kinds := make([]string, 0, 2)
	if collected.wantTop {
		kinds = append(kinds, domain.LastFmRecommendationTopTracks)
	}
	if collected.wantSimilar {
		kinds = append(kinds, domain.LastFmRecommendationSimilar)
	}
	for _, kind := range kinds {
		items := collected.topItems
		if kind == domain.LastFmRecommendationSimilar {
			items = collected.similarItems
		}
		states := r.seedTopTrackStates(items, collected.sources)
		r.persistMatchingStates(states)
		if anyTopStatePending(states) {
			r.markMatchingActive(kind, true)
			r.startTopTrackMatching(username, kind, states, collected.sources)
		}
		if kind == domain.LastFmRecommendationTopTracks {
			topItems = topTrackItemsFromStates(states)
		} else {
			similarItems = topTrackItemsFromStates(states)
		}
	}
	return topItems, similarItems
}

// topTrackItemsFromStates returns the rows with the seeded state applied.
func topTrackItemsFromStates(states []*topTrackState) []TopTrackItem {
	items := make([]TopTrackItem, len(states))
	for index, state := range states {
		items[index] = state.item
	}
	return items
}

// anyTopStatePending reports whether any row still needs a source search.
func anyTopStatePending(states []*topTrackState) bool {
	for _, state := range states {
		if !state.done {
			return true
		}
	}
	return false
}

// usableSeeds takes the first usable top tracks, which are the "for you" seeds.
func usableSeeds(tracks []LastFmTrack, limit int) []LastFmTrack {
	seeds := make([]LastFmTrack, 0, limit)
	for _, track := range tracks {
		if !usableTrack(track) {
			continue
		}
		seeds = append(seeds, track)
		if len(seeds) >= limit {
			break
		}
	}
	return seeds
}

// buildSimilarItems turns the collected rows into page rows. Seeds never appear
// as their own recommendation, and the same song (title + artist set) is
// rendered once even when several seeds recommend it.
func buildSimilarItems(seeds []LastFmTrack, tracks []LastFmTrack) []TopTrackItem {
	if len(tracks) == 0 {
		return []TopTrackItem{}
	}
	seedKeys := make(map[string]struct{}, len(seeds))
	for _, seed := range seeds {
		seedKeys[similarRecommendationKey(seed)] = struct{}{}
	}
	seen := make(map[string]struct{}, len(tracks))
	items := make([]TopTrackItem, 0, len(tracks))
	for _, track := range tracks {
		if !usableTrack(track) {
			continue
		}
		if _, isSeed := seedKeys[similarRecommendationKey(track)]; isSeed {
			continue
		}
		key := candidateKey(track.Artist, track.Title)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		items = append(items, TopTrackItem{
			Title:    track.Title,
			Artist:   track.Artist,
			Album:    track.Album,
			Duration: track.Duration,
			Artists:  []string{track.Artist},
			CoverURL: track.ImageURL,
			Kind:     domain.LastFmRecommendationSimilar,
			Matching: true,
			MatchKey: key,
		})
		if len(items) >= similarRecommendationTake {
			break
		}
	}
	return items
}

// collectSimilarRecommendations returns the ranked "for you" rows for a seed
// set. The answer is cached per seed set for recommendationCacheTTL, so a
// refresh does not spend the request budget again.
func (r *Recommender) collectSimilarRecommendations(
	ctx context.Context, seeds []LastFmTrack, force bool,
) ([]LastFmTrack, bool) {
	if len(seeds) == 0 {
		return nil, false
	}
	cacheKey := similarRecommendationCacheKey(seeds)

	r.mu.Lock()
	if r.similarRecs == nil {
		r.similarRecs = make(map[string]cachedSimilarRecommendation)
	}
	cached, ok := r.similarRecs[cacheKey]
	r.mu.Unlock()
	if ok && !force && r.now().Before(cached.expiresAt) {
		return append([]LastFmTrack{}, cached.tracks...), cached.partial
	}

	set := seedSet{all: make([]seed, 0, len(seeds))}
	for _, track := range seeds {
		set.all = append(set.all, seed{track: track, kind: seedTop})
	}
	candidates, failed := r.collectSimilarTracks(ctx, set)
	tracks := rankSimilarRecommendations(candidates)

	r.mu.Lock()
	r.similarRecs[cacheKey] = cachedSimilarRecommendation{
		tracks:    tracks,
		partial:   failed,
		expiresAt: r.now().Add(recommendationCacheTTL),
	}
	r.mu.Unlock()
	return tracks, failed
}

// similarRecommendationCacheKey identifies one seed set.
func similarRecommendationCacheKey(seeds []LastFmTrack) string {
	keys := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		keys = append(keys, candidateKey(seed.Artist, seed.Title))
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\x1e")))
	return hex.EncodeToString(sum[:])
}

// similarRecommendationKey is the dedupe identity of one recommendation: the
// normalised title plus the artist set, so order and separators do not matter.
func similarRecommendationKey(track LastFmTrack) string {
	return domain.NormalizationKey(track.Title) + "\x00" +
		domain.ArtistSetKey([]string{track.Artist})
}

// rankSimilarRecommendations merges duplicates, then orders by how many seeds
// recommended the row, then by Last.fm's own popularity.
func rankSimilarRecommendations(candidates map[string]*candidate) []LastFmTrack {
	type ranked struct {
		track LastFmTrack
		hits  int
	}
	merged := make(map[string]*ranked, len(candidates))
	for _, entry := range candidates {
		key := similarRecommendationKey(entry.track)
		hits := entry.hits[seedTop]
		existing := merged[key]
		if existing == nil {
			merged[key] = &ranked{track: entry.track, hits: hits}
			continue
		}
		existing.hits += hits
		if entry.track.PlayCount > existing.track.PlayCount {
			existing.track = entry.track
		}
	}

	rows := make([]*ranked, 0, len(merged))
	for _, entry := range merged {
		rows = append(rows, entry)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].hits != rows[j].hits {
			return rows[i].hits > rows[j].hits
		}
		if rows[i].track.PlayCount != rows[j].track.PlayCount {
			return rows[i].track.PlayCount > rows[j].track.PlayCount
		}
		if rows[i].track.Title != rows[j].track.Title {
			return rows[i].track.Title < rows[j].track.Title
		}
		return rows[i].track.Artist < rows[j].track.Artist
	})
	if len(rows) > similarRecommendationTake {
		rows = rows[:similarRecommendationTake]
	}

	tracks := make([]LastFmTrack, 0, len(rows))
	for _, entry := range rows {
		tracks = append(tracks, entry.track)
	}
	return tracks
}

// rememberTopRows keeps the Last.fm-side rows of the current page. Failover
// resets a matched row from this copy, and only rows the user can currently see
// are worth keeping. The key includes the list kind because the same track may
// appear in both lists.
func (r *Recommender) rememberTopRows(lists ...[]TopTrackItem) {
	rows := make(map[string]TopTrackItem)
	for _, items := range lists {
		for _, item := range items {
			if item.MatchKey == "" || item.Kind == "" {
				continue
			}
			rows[recommendationRowKey(item.Kind, item.MatchKey)] = item
		}
	}
	r.mu.Lock()
	r.topRows = rows
	r.mu.Unlock()
}

// recommendationRowKey identifies one row across both lists.
func recommendationRowKey(kind, matchKey string) string {
	return kind + "\x00" + matchKey
}

// seedTopTrackStates applies the per-source match cache and returns the rows a
// worker still has to handle.
func (r *Recommender) seedTopTrackStates(items []TopTrackItem, sources []string) []*topTrackState {
	states := make([]*topTrackState, len(items))
	for index, item := range items {
		state := &topTrackState{item: item, tried: make(map[string]bool)}
		failed := r.failedTopSources(item.Kind, item.MatchKey)
		for _, source := range sources {
			if failed[source] {
				// This provider already failed to play this track: a retry would
				// only fail again, so the row moves on to the next source.
				state.tried[source] = true
				continue
			}
			row, ok := r.cachedSourceMatch(item.Kind, item.MatchKey, source)
			if !ok {
				continue
			}
			state.tried[source] = true
			if row != nil {
				r.applyTopTrackMatch(&state.item, row)
				state.item.Matching = false
				state.item.Matched = true
				state.done = true
				break
			}
		}
		if !state.done && len(state.tried) >= len(sources) {
			state.item.Matching = false
			state.item.NoSource = true
			state.done = true
		}
		states[index] = state
	}
	return states
}

// persistMatchingStates writes the rows a cached match already resolved. They
// are never emitted, so without this the database would keep them "matching".
func (r *Recommender) persistMatchingStates(states []*topTrackState) {
	items := make([]TopTrackItem, 0, len(states))
	for _, state := range states {
		if state.done {
			items = append(items, state.item)
		}
	}
	r.persistRecommendationItems(items)
}

// queuedTopTrackRun is one worker queue. A page load runs with finalDone, so the
// last event tells the frontend that every unresolved row has no source. A
// failover rerun sends Done=false: the rest of the page is already settled and
// must not be overwritten. kind is empty for the combined run.
type queuedTopTrackRun struct {
	kind      string
	states    []*topTrackState
	sources   []string
	finalDone bool
}

// startTopTrackMatching runs one job per account. A second page request while
// the job runs joins it through the same event stream.
func (r *Recommender) startTopTrackMatching(
	username, kind string, states []*topTrackState, sources []string,
) {
	r.enqueueTopTrackRun(username, queuedTopTrackRun{
		kind:      kind,
		states:    states,
		sources:   sources,
		finalDone: true,
	})
}

// enqueueTopTrackRun serializes runs per account. A failover arrives while the
// page's queue may still be working; it waits instead of racing the same rows.
func (r *Recommender) enqueueTopTrackRun(username string, run queuedTopTrackRun) {
	r.mu.Lock()
	if r.topRunning[username] {
		r.topQueue[username] = append(r.topQueue[username], run)
		r.mu.Unlock()
		return
	}
	r.topRunning[username] = true
	r.mu.Unlock()

	go r.runTopTrackQueue(username, run)
}

// runTopTrackQueue drains the queued runs one after another.
func (r *Recommender) runTopTrackQueue(username string, run queuedTopTrackRun) {
	for {
		r.runTopTrackMatching(username, run)
		r.mu.Lock()
		queue := r.topQueue[username]
		if len(queue) == 0 {
			delete(r.topQueue, username)
			delete(r.topRunning, username)
			r.mu.Unlock()
			return
		}
		run = queue[0]
		r.topQueue[username] = queue[1:]
		r.mu.Unlock()
	}
}

// runTopTrackMatching is the five-worker queue.
//
// Each worker owns one source. A worker takes any pending row that has not tried
// its source yet; a miss puts the row back for the other workers and records the
// source as tried. A row becomes NoSource after every source failed.
func (r *Recommender) runTopTrackMatching(username string, run queuedTopTrackRun) {
	states, sources := run.states, run.sources
	defer r.markMatchingActive(run.kind, false)

	var (
		bufferMu sync.Mutex
		buffer   []TopTrackItem
	)
	flush := func(done bool) {
		bufferMu.Lock()
		items := append([]TopTrackItem{}, buffer...)
		buffer = nil
		bufferMu.Unlock()
		if len(items) == 0 && !done {
			return
		}
		// Persist before emitting: the page may be closed when the event lands,
		// and the row must still show its final state on the next visit.
		r.persistRecommendationItems(items)
		if r.emitter == nil {
			return
		}
		r.emitter(recommendationsMatchedEvent, TopTrackUpdate{
			Kind:  run.kind,
			Items: items,
			Done:  done,
		})
	}

	if len(sources) == 0 {
		for _, state := range states {
			if state.done {
				continue
			}
			state.item.Matching = false
			state.item.NoSource = true
			state.done = true
			buffer = append(buffer, state.item)
		}
		flush(run.finalDone)
		return
	}

	// Local-first: a file the user already owns wins over every source, and
	// skipping the source calls is the whole point of the check.
	for _, state := range states {
		if state.done {
			continue
		}
		if match := r.localMatch(state.item); match != nil {
			r.applyTopTrackMatch(&state.item, match)
			state.item.Matching = false
			state.item.Matched = true
			state.item.NoSource = false
			state.done = true
			buffer = append(buffer, state.item)
		}
	}

	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(matchEventInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				flush(false)
			case <-stop:
				return
			}
		}
	}()

	var mu sync.Mutex
	cond := sync.NewCond(&mu)
	pending := make([]int, 0, len(states))
	complete := 0
	for index, state := range states {
		if state.done {
			complete++
			continue
		}
		pending = append(pending, index)
	}

	var wg sync.WaitGroup
	for _, source := range sources {
		source := source
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				next := -1
				for _, candidate := range pending {
					if !states[candidate].tried[source] {
						next = candidate
						break
					}
				}
				if next == -1 {
					if complete >= len(states) {
						mu.Unlock()
						return
					}
					cond.Wait()
					mu.Unlock()
					continue
				}
				for index, candidate := range pending {
					if candidate == next {
						pending = append(pending[:index], pending[index+1:]...)
						break
					}
				}
				states[next].active = true
				mu.Unlock()

				match := r.searchTopTrackSource(states[next].item, source)

				mu.Lock()
				states[next].active = false
				states[next].tried[source] = true
				r.storeSourceMatch(states[next].item.Kind, states[next].item.MatchKey, source, match)

				var update *TopTrackItem
				if match != nil {
					r.applyTopTrackMatch(&states[next].item, match)
					states[next].item.Matching = false
					states[next].item.Matched = true
					states[next].item.NoSource = false
					states[next].done = true
					complete++
					item := states[next].item
					update = &item
				} else if len(states[next].tried) >= len(sources) {
					states[next].item.Matching = false
					states[next].item.NoSource = true
					states[next].done = true
					complete++
					item := states[next].item
					update = &item
				} else {
					pending = append(pending, next)
				}
				cond.Broadcast()
				mu.Unlock()

				if update != nil {
					bufferMu.Lock()
					buffer = append(buffer, *update)
					shouldFlush := len(buffer) >= matchEventBatch
					bufferMu.Unlock()
					if shouldFlush {
						flush(false)
					}
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-stopped
	flush(run.finalDone)
}

// HandlePlaybackFailure re-queues the top-track row whose matched source could
// not play.
//
// The failed provider is remembered per row and its cached match is dropped, so
// the next run searches only the remaining sources. The row is pushed back to
// "matching" immediately; when every source has failed the final update carries
// no_source and nothing retries automatically.
func (r *Recommender) HandlePlaybackFailure(provider, externalID string) {
	provider = domain.NormalizeProvider(provider)
	externalID = strings.TrimSpace(externalID)
	if provider == "" || externalID == "" {
		return
	}
	for _, ref := range r.topTrackKeysForIdentity(provider, externalID) {
		r.requeueTopTrack(ref.kind, ref.matchKey, provider)
	}
}

// recommendationRowRef names one row across the two lists.
type recommendationRowRef struct {
	kind     string
	matchKey string
}

// topTrackKeysForIdentity maps a playing provider identity back to every page
// row that was matched from it.
func (r *Recommender) topTrackKeysForIdentity(provider, externalID string) []recommendationRowRef {
	r.mu.Lock()
	defer r.mu.Unlock()

	seen := make(map[recommendationRowRef]struct{})
	keys := make([]recommendationRowRef, 0, 1)
	for cacheKey, entry := range r.sourceMatches {
		if entry.row == nil {
			continue
		}
		if domain.NormalizeProvider(entry.row.Provider) != provider ||
			strings.TrimSpace(entry.row.ExternalID) != externalID {
			continue
		}
		// The cache key is kind + "\x00" + match_key + "|" + source, and match_key
		// itself contains "\x00", so split from the right.
		index := strings.LastIndexByte(cacheKey, '|')
		if index <= 0 {
			continue
		}
		ref := splitRecommendationRowKey(cacheKey[:index])
		if ref.kind == "" || ref.matchKey == "" {
			continue
		}
		if _, duplicate := seen[ref]; duplicate {
			continue
		}
		seen[ref] = struct{}{}
		keys = append(keys, ref)
	}
	return keys
}

// splitRecommendationRowKey splits kind + "\x00" + match_key. The match key keeps
// its own separator because only the first field is the kind.
func splitRecommendationRowKey(key string) recommendationRowRef {
	index := strings.IndexByte(key, '\x00')
	if index <= 0 {
		return recommendationRowRef{}
	}
	return recommendationRowRef{kind: key[:index], matchKey: key[index+1:]}
}

// requeueTopTrack resets one row and searches it again without the failed source.
func (r *Recommender) requeueTopTrack(kind, matchKey, failedProvider string) {
	rowKey := recommendationRowKey(kind, matchKey)
	r.mu.Lock()
	base, ok := r.topRows[rowKey]
	if !ok {
		r.mu.Unlock()
		return
	}
	failed := r.topFailedSources[rowKey]
	if failed == nil {
		failed = make(map[string]bool)
		r.topFailedSources[rowKey] = failed
	}
	failed[failedProvider] = true
	delete(r.sourceMatches, rowKey+"|"+failedProvider)
	tried := make(map[string]bool, len(failed))
	for source := range failed {
		tried[source] = true
	}
	r.mu.Unlock()

	item := resetTopTrackItem(base)
	r.persistRecommendationItems([]TopTrackItem{item})
	r.emitTopTrackUpdate([]TopTrackItem{item})

	connected, username, _ := r.client.GetStatus()
	if !connected || strings.TrimSpace(username) == "" {
		return
	}

	sources := r.topTrackSources(context.Background())
	state := &topTrackState{item: item, tried: tried}
	remaining := 0
	for _, source := range sources {
		if !tried[source] {
			remaining++
		}
	}
	if remaining == 0 {
		item.Matching = false
		item.NoSource = true
		state.item = item
		state.done = true
		r.persistRecommendationItems([]TopTrackItem{item})
		r.emitTopTrackUpdate([]TopTrackItem{item})
		return
	}
	r.markMatchingActive(kind, true)
	r.enqueueTopTrackRun(username, queuedTopTrackRun{
		kind:    kind,
		states:  []*topTrackState{state},
		sources: sources,
	})
}

// resetTopTrackItem clears every matched field, leaving the Last.fm identity the
// next source search needs.
func resetTopTrackItem(item TopTrackItem) TopTrackItem {
	item.Provider = ""
	item.ExternalID = ""
	item.State = ""
	item.LocalTrackID = ""
	item.BlockedReason = ""
	item.Sources = nil
	item.Downloadable = false
	item.CoverURLLarge = ""
	item.Suspicious = false
	item.Matched = false
	item.NoSource = false
	item.Matching = true
	return item
}

// emitTopTrackUpdate sends a failover state change. Done is always false: the
// rest of the page is already resolved and must not be finished as "no source".
// The row's kind scopes the update to one list.
func (r *Recommender) emitTopTrackUpdate(items []TopTrackItem) {
	if r.emitter == nil {
		return
	}
	kind := ""
	if len(items) > 0 {
		kind = items[0].Kind
	}
	r.emitter(recommendationsMatchedEvent, TopTrackUpdate{Kind: kind, Items: items, Done: false})
}

// failedTopSources returns the providers already known to fail this row.
func (r *Recommender) failedTopSources(kind, matchKey string) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	failed := r.topFailedSources[recommendationRowKey(kind, matchKey)]
	if len(failed) == 0 {
		return nil
	}
	copy := make(map[string]bool, len(failed))
	for source := range failed {
		copy[source] = true
	}
	return copy
}

// searchTopTrackSource asks exactly one source, within the per-candidate budget.
func (r *Recommender) searchTopTrackSource(item TopTrackItem, source string) *domain.CatalogSearchResult {
	artist := strings.TrimSpace(item.Artist)
	if r.matcher == nil || artist == "" || strings.TrimSpace(item.Title) == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), matchCandidateTimeout)
	defer cancel()

	rows, err := r.matcher.MatchTrack(ctx, item.Title, artist, item.Duration, []string{source})
	if err != nil {
		r.logger.Debug("last.fm top tracks: source search failed",
			"source", source, "artist", artist, "title", item.Title, "error", err)
		return nil
	}
	return chooseMatch(ctx, r.streams, rows)
}

// localMatch returns a catalogue-shaped row for an on-disk copy, or nil.
//
// It never touches the network. A hit is reported as in_library so the page
// shows the play action without waiting for any source.
func (r *Recommender) localMatch(item TopTrackItem) *domain.CatalogSearchResult {
	if r.matcher == nil {
		return nil
	}
	artists := append([]string{}, item.Artists...)
	if len(artists) == 0 && strings.TrimSpace(item.Artist) != "" {
		artists = []string{strings.TrimSpace(item.Artist)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), matchCandidateTimeout)
	defer cancel()

	local, err := r.matcher.MatchLocalTrack(ctx, item.Title, artists, item.Duration)
	if err != nil {
		r.logger.Debug("last.fm top tracks: local match failed",
			"title", item.Title, "error", err)
		return nil
	}
	if local == nil || local.Track == nil || local.Track.IsRemote() {
		return nil
	}
	track := local.Track

	names := make([]string, 0, len(track.Artists))
	for _, artist := range track.Artists {
		if artist != nil && strings.TrimSpace(artist.Name) != "" {
			names = append(names, strings.TrimSpace(artist.Name))
		}
	}
	album := strings.TrimSpace(track.RawAlbumName)
	if track.Album != nil && strings.TrimSpace(track.Album.Title) != "" {
		album = strings.TrimSpace(track.Album.Title)
	}
	return &domain.CatalogSearchResult{
		Title:        track.Title,
		Artists:      names,
		AlbumName:    album,
		Duration:     track.Duration,
		CoverURL:     localArtworkURL(track.ArtworkKey),
		State:        domain.CatalogRowStateInLibrary,
		LocalTrackID: track.ID,
		Suspicious:   local.Suspicious,
	}
}

// localArtworkURL is the WebView path for a library row's embedded artwork.
// A local row has no remote cover URL, so this is its cover source.
func localArtworkURL(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	return "/artwork/" + key + "?size=sm"
}

// applyTopTrackMatch copies the catalogue identity onto the Last.fm row.
//
// Cover priority: a real Last.fm image wins, then the matched source's cover,
// then the cover cache for the identity. Nothing found leaves the field empty,
// and the row renders its placeholder.
func (r *Recommender) applyTopTrackMatch(item *TopTrackItem, row *domain.CatalogSearchResult) {
	lastFmCover := strings.TrimSpace(item.CoverURL)
	item.Provider = row.Provider
	item.ExternalID = row.ExternalID
	item.Title = row.Title
	item.Album = row.AlbumName
	item.Duration = row.Duration
	item.Artists = append([]string{}, row.Artists...)
	if len(item.Artists) > 0 {
		item.Artist = item.Artists[0]
	}
	if lastFmCover != "" {
		item.CoverURL = lastFmCover
	} else {
		// The source's display cover is its large image when it has one.
		item.CoverURL = strings.TrimSpace(row.CoverURLLarge)
		if item.CoverURL == "" {
			item.CoverURL = strings.TrimSpace(row.CoverURL)
		}
		if item.CoverURL == "" {
			item.CoverURL = r.coverForIdentity(row.Provider, row.ExternalID)
		}
	}
	item.CoverURLLarge = row.CoverURLLarge
	item.Downloadable = row.Downloadable
	item.State = row.State
	item.LocalTrackID = row.LocalTrackID
	item.BlockedReason = row.DownloadBlockedReason
	item.Sources = append([]string{}, row.Sources...)
	item.Suspicious = row.Suspicious
	// A local hit wins over the source it came from: a promoted row keeps its
	// remote identity, and showing "from wy" for a file already in the library
	// is both wrong in the chip and offers a download that already happened.
	if item.State == domain.CatalogRowStateInLibrary || strings.TrimSpace(item.LocalTrackID) != "" {
		item.Provider = ""
		item.ExternalID = ""
		item.Sources = []string{}
	}
}

// topTrackSources returns every available source in the priority order, capped
// at the five workers this MVP starts.
func (r *Recommender) topTrackSources(ctx context.Context) []string {
	sources := r.allSources(ctx)
	if len(sources) > topTrackSourceLimit {
		sources = sources[:topTrackSourceLimit]
	}
	return sources
}

func (r *Recommender) cachedSourceMatch(kind, matchKey, source string) (*domain.CatalogSearchResult, bool) {
	key := recommendationRowKey(kind, matchKey) + "|" + source
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.sourceMatches[key]
	if !ok {
		return nil, false
	}
	if r.now().After(entry.expiresAt) {
		delete(r.sourceMatches, key)
		return nil, false
	}
	return entry.row, true
}

func (r *Recommender) storeSourceMatch(kind, matchKey, source string, row *domain.CatalogSearchResult) {
	if kind == "" || matchKey == "" || source == "" {
		return
	}
	key := recommendationRowKey(kind, matchKey) + "|" + source
	r.mu.Lock()
	defer r.mu.Unlock()
	var copyRow *domain.CatalogSearchResult
	if row != nil {
		value := *row
		copyRow = &value
	}
	r.sourceMatches[key] = cachedSourceMatch{row: copyRow, expiresAt: r.now().Add(matchCacheTTL)}
}
