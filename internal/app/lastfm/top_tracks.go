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
	topTrackPeriod = "7day"
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

// GetTopTrackRecommendations returns the user's top tracks from the last week.
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

// seedTopTrackStates applies the per-source match cache and returns the rows a
// worker still has to handle.
func (r *Recommender) seedTopTrackStates(items []TopTrackItem, sources []string) []*topTrackState {
	states := make([]*topTrackState, len(items))
	for index, item := range items {
		state := &topTrackState{item: item, tried: make(map[string]bool)}
		for _, source := range sources {
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

// startTopTrackMatching runs one job per account. A second page request while
// the job runs joins it through the same event stream.
func (r *Recommender) startTopTrackMatching(
	username string, states []*topTrackState, sources []string,
) {
	r.mu.Lock()
	if r.topRunning[username] {
		r.mu.Unlock()
		return
	}
	r.topRunning[username] = true
	r.mu.Unlock()

	go r.runTopTrackMatching(username, states, sources)
}

// runTopTrackMatching is the five-worker queue.
//
// Each worker owns one source. A worker takes any pending row that has not tried
// its source yet; a miss puts the row back for the other workers and records the
// source as tried. A row becomes NoSource after every source failed.
func (r *Recommender) runTopTrackMatching(
	username string, states []*topTrackState, sources []string,
) {
	defer func() {
		r.mu.Lock()
		delete(r.topRunning, username)
		r.mu.Unlock()
	}()

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
		flush(true)
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
	flush(true)
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
