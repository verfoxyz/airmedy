package lastfm

import (
	"context"
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
	// MatchKey is artist + "\x00" + title, the stable key the frontend uses to
	// update a row in place.
	MatchKey string `json:"match_key"`
}

// TopTrackPage is the immediate response: every Last.fm top track, plus the
// status chips already known from the match cache.
type TopTrackPage struct {
	Status string         `json:"status"`
	Items  []TopTrackItem `json:"items"`
}

// TopTrackUpdate is the payload of lastfm:recommendations-matched for this MVP.
type TopTrackUpdate struct {
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

// GetTopTrackRecommendations returns the user's all-time most played tracks.
//
// It makes exactly one Last.fm request and returns every row before any source
// search runs. Cached per-source outcomes are applied first, so a refresh shows
// the previous matches immediately; the remaining rows are matched by five
// source workers.
func (r *Recommender) GetTopTrackRecommendations(ctx context.Context) (TopTrackPage, error) {
	connected, username, _ := r.client.GetStatus()
	if !connected || strings.TrimSpace(username) == "" {
		return TopTrackPage{Status: StatusNotConnected, Items: []TopTrackItem{}}, nil
	}

	tracks, err := r.client.GetTopTracks(ctx, topTrackPeriod, topTrackLimit)
	if err != nil {
		return TopTrackPage{Status: StatusOK, Items: []TopTrackItem{}}, err
	}

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
			Matching: true,
			MatchKey: key,
		})
		if len(items) >= topTrackLimit {
			break
		}
	}

	sources := r.topTrackSources(ctx)
	r.rememberTopRows(items)
	states := r.seedTopTrackStates(items, sources)
	pageItems := make([]TopTrackItem, len(states))
	pending := false
	for index, state := range states {
		pageItems[index] = state.item
		if !state.done {
			pending = true
		}
	}

	if pending {
		r.startTopTrackMatching(username, states, sources)
	}
	return TopTrackPage{Status: StatusOK, Items: pageItems}, nil
}

// rememberTopRows keeps the Last.fm-side rows of the current page. Failover
// resets a matched row from this copy, and only rows the user can currently see
// are worth keeping.
func (r *Recommender) rememberTopRows(items []TopTrackItem) {
	rows := make(map[string]TopTrackItem, len(items))
	for _, item := range items {
		if item.MatchKey == "" {
			continue
		}
		rows[item.MatchKey] = item
	}
	r.mu.Lock()
	r.topRows = rows
	r.mu.Unlock()
}

// seedTopTrackStates applies the per-source match cache and returns the rows a
// worker still has to handle.
func (r *Recommender) seedTopTrackStates(items []TopTrackItem, sources []string) []*topTrackState {
	states := make([]*topTrackState, len(items))
	for index, item := range items {
		state := &topTrackState{item: item, tried: make(map[string]bool)}
		failed := r.failedTopSources(item.MatchKey)
		for _, source := range sources {
			if failed[source] {
				// This provider already failed to play this track: a retry would
				// only fail again, so the row moves on to the next source.
				state.tried[source] = true
				continue
			}
			row, ok := r.cachedSourceMatch(item.MatchKey, source)
			if !ok {
				continue
			}
			state.tried[source] = true
			if row != nil {
				applyTopTrackMatch(&state.item, row)
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

// queuedTopTrackRun is one worker queue. A page load runs with finalDone, so the
// last event tells the frontend that every unresolved row has no source. A
// failover rerun sends Done=false: the rest of the page is already settled and
// must not be overwritten.
type queuedTopTrackRun struct {
	states    []*topTrackState
	sources   []string
	finalDone bool
}

// startTopTrackMatching runs one job per account. A second page request while
// the job runs joins it through the same event stream.
func (r *Recommender) startTopTrackMatching(
	username string, states []*topTrackState, sources []string,
) {
	r.enqueueTopTrackRun(username, queuedTopTrackRun{
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
		if r.emitter == nil {
			return
		}
		r.emitter(recommendationsMatchedEvent, TopTrackUpdate{Items: items, Done: done})
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
				r.storeSourceMatch(states[next].item.MatchKey, source, match)

				var update *TopTrackItem
				if match != nil {
					applyTopTrackMatch(&states[next].item, match)
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
	for _, matchKey := range r.topTrackKeysForIdentity(provider, externalID) {
		r.requeueTopTrack(matchKey, provider)
	}
}

// topTrackKeysForIdentity maps a playing provider identity back to every page
// row that was matched from it.
func (r *Recommender) topTrackKeysForIdentity(provider, externalID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	seen := make(map[string]struct{})
	keys := make([]string, 0, 1)
	for cacheKey, entry := range r.sourceMatches {
		if entry.row == nil {
			continue
		}
		if domain.NormalizeProvider(entry.row.Provider) != provider ||
			strings.TrimSpace(entry.row.ExternalID) != externalID {
			continue
		}
		index := strings.LastIndexByte(cacheKey, '|')
		if index <= 0 {
			continue
		}
		matchKey := cacheKey[:index]
		if _, duplicate := seen[matchKey]; duplicate {
			continue
		}
		seen[matchKey] = struct{}{}
		keys = append(keys, matchKey)
	}
	return keys
}

// requeueTopTrack resets one row and searches it again without the failed source.
func (r *Recommender) requeueTopTrack(matchKey, failedProvider string) {
	r.mu.Lock()
	base, ok := r.topRows[matchKey]
	if !ok {
		r.mu.Unlock()
		return
	}
	failed := r.topFailedSources[matchKey]
	if failed == nil {
		failed = make(map[string]bool)
		r.topFailedSources[matchKey] = failed
	}
	failed[failedProvider] = true
	delete(r.sourceMatches, matchKey+"|"+failedProvider)
	tried := make(map[string]bool, len(failed))
	for source := range failed {
		tried[source] = true
	}
	r.mu.Unlock()

	item := resetTopTrackItem(base)
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
		r.emitTopTrackUpdate([]TopTrackItem{item})
		return
	}
	r.enqueueTopTrackRun(username, queuedTopTrackRun{
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
	item.Matched = false
	item.NoSource = false
	item.Matching = true
	return item
}

// emitTopTrackUpdate sends a failover state change. Done is always false: the
// rest of the page is already resolved and must not be finished as "no source".
func (r *Recommender) emitTopTrackUpdate(items []TopTrackItem) {
	if r.emitter == nil {
		return
	}
	r.emitter(recommendationsMatchedEvent, TopTrackUpdate{Items: items, Done: false})
}

// failedTopSources returns the providers already known to fail this row.
func (r *Recommender) failedTopSources(matchKey string) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	failed := r.topFailedSources[matchKey]
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

// applyTopTrackMatch copies the catalogue identity onto the Last.fm row.
func applyTopTrackMatch(item *TopTrackItem, row *domain.CatalogSearchResult) {
	item.Provider = row.Provider
	item.ExternalID = row.ExternalID
	item.Title = row.Title
	item.Album = row.AlbumName
	item.Duration = row.Duration
	item.Artists = append([]string{}, row.Artists...)
	if len(item.Artists) > 0 {
		item.Artist = item.Artists[0]
	}
	item.CoverURL = row.CoverURL
	item.CoverURLLarge = row.CoverURLLarge
	item.Downloadable = row.Downloadable
	item.State = row.State
	item.LocalTrackID = row.LocalTrackID
	item.BlockedReason = row.DownloadBlockedReason
	item.Sources = append([]string{}, row.Sources...)
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

func (r *Recommender) cachedSourceMatch(matchKey, source string) (*domain.CatalogSearchResult, bool) {
	key := matchKey + "|" + source
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

func (r *Recommender) storeSourceMatch(matchKey, source string, row *domain.CatalogSearchResult) {
	if matchKey == "" || source == "" {
		return
	}
	key := matchKey + "|" + source
	r.mu.Lock()
	defer r.mu.Unlock()
	var copyRow *domain.CatalogSearchResult
	if row != nil {
		value := *row
		copyRow = &value
	}
	r.sourceMatches[key] = cachedSourceMatch{row: copyRow, expiresAt: r.now().Add(matchCacheTTL)}
}
