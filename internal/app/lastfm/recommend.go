package lastfm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"airmedy/internal/domain"
)

// Recommendation page status. The frontend words each one, so they are machine
// strings rather than sentences.
const (
	// StatusOK means every stage that could run finished.
	StatusOK = "ok"
	// StatusNotConnected means the page has to send the user to settings.
	StatusNotConnected = "not_connected"
	// StatusPartial means the page is usable but at least one request or match
	// failed; the UI shows a banner above the lists that did arrive.
	StatusPartial = "partial"
)

const (
	// seedListLimit is how many rows each Last.fm seed endpoint is asked for.
	seedListLimit = 50
	// seedTake is how many of those rows become seeds per source.
	seedTake = 20
	// topSeedPeriods are the windows merged into the "most played" seed set.
	topSeedPeriods = "7day,1month"
	// topPeriodLimit is the per-window fetch size.
	topPeriodLimit = 20
	// maxSeeds bounds the merged seed set.
	maxSeeds = 50
	// similarTrackLimit is Last.fm's per-seed recommendation window.
	similarTrackLimit = 10
	// perKindLimit bounds each source-specific list.
	perKindLimit = 40
	// combinedLimit bounds the merged list before matching.
	combinedLimit = 60
	// maxPlayable bounds every list the page renders.
	maxPlayable = 50
	// similarArtistSeedLimit bounds how many seed artists are queried.
	similarArtistSeedLimit = 20
	// similarArtistLimit is the per-artist window.
	similarArtistLimit = 10
	// similarArtistTake bounds the rendered similar-artist grid.
	similarArtistTake = 20
	// recommendationCacheTTL is how long one seed set is reused.
	recommendationCacheTTL = 30 * time.Minute
	// matchCacheTTL is how long one catalogue match is reused. It is shared by
	// all four lists so the same track is never searched twice.
	matchCacheTTL = 30 * time.Minute
	// maxMatchCandidates bounds one page build's catalogue searches. Four lists
	// can overlap heavily; this keeps a cold page from turning into hundreds of
	// source calls.
	maxMatchCandidates = 120
	// matchConcurrency is the number of catalogue searches in flight.
	matchConcurrency = 2
)

// Source weights for the combined list. A recent seed is the strongest signal,
// a loved track is deliberate but older, and a top track is already a favourite
// of the period it came from.
var seedKindWeights = map[seedKind]float64{
	seedRecent: 1.0,
	seedTop:    0.8,
	seedLoved:  0.6,
}

type seedKind string

const (
	seedRecent seedKind = "recent"
	seedTop    seedKind = "top"
	seedLoved  seedKind = "loved"
)

// TrackSource is the read-only Last.fm surface the recommender consumes.
//
// It is an interface so the aggregation, scoring, cache and single-flight rules
// are testable without HTTP; *LastFmService is the production implementation.
type TrackSource interface {
	GetRecentTracks(ctx context.Context, limit int) ([]LastFmTrack, error)
	GetTopTracks(ctx context.Context, period string, limit int) ([]LastFmTrack, error)
	GetLovedTracks(ctx context.Context, limit int) ([]LastFmTrack, error)
	GetSimilarTracks(ctx context.Context, artist, title string, limit int) ([]LastFmTrack, error)
	GetSimilarArtists(ctx context.Context, artist string, limit int) ([]LastFmArtist, error)
	// GetStatus reports whether an account is connected, its username and avatar.
	GetStatus() (connected bool, username string, avatarURL string)
}

// CatalogMatcher is the narrow catalogue surface the recommender consumes.
//
// *catalog.Service implements it. Matching reuses the search pipeline, so the
// recommendation page resolves local files, stream-cache availability and
// downloadable sources exactly like the search page does.
type CatalogMatcher interface {
	MatchTrack(
		ctx context.Context, title, artist string, duration int, sources []string,
	) ([]domain.CatalogSearchResult, error)
	Sources(ctx context.Context) ([]domain.CatalogSearchSource, error)
}

// RecommendedTrack is one playable recommendation.
//
// The fields mirror a catalogue row (provider/external id, state, local track)
// so the frontend can feed it to the same play/save actions as a search row.
type RecommendedTrack struct {
	Provider   string
	ExternalID string
	Title      string
	Artists    []string
	AlbumName  string
	Duration   int
	CoverURL   string
	// CoverURLLarge is the rendition the saved file should embed.
	CoverURLLarge string
	Downloadable  bool
	// DownloadBlockedReason is a machine kind ("no_resolver") the frontend words.
	DownloadBlockedReason string
	// State is the catalogue row state: "", in_library, downloading, failed, saved.
	State string
	// LocalTrackID is an on-disk row that already represents this song.
	LocalTrackID string
	Sources      []string
	// Score is the recommendation score within the list that contains it.
	Score float64
}

// SimilarArtist is one artist from artist.getSimilar.
type SimilarArtist struct {
	Name      string
	ImageURL  string
	Listeners int
	URL       string
}

// RecommendationPage is everything the Last.fm page renders.
type RecommendationPage struct {
	Status string
	// Username is the connected account, shown in the page header.
	Username  string
	AvatarURL string
	Tracks    []RecommendedTrack
	// RecentTracks / LovedTracks / TopTracks are the three per-seed-source lists.
	RecentTracks   []RecommendedTrack
	LovedTracks    []RecommendedTrack
	TopTracks      []RecommendedTrack
	SimilarArtists []SimilarArtist
}

// Recommender builds the personalised page from Last.fm seeds and catalogue
// matches.
type Recommender struct {
	client  TrackSource
	matcher CatalogMatcher
	streams domain.StreamCacheRepository
	logger  *slog.Logger
	now     func() time.Time

	mu       sync.Mutex
	pages    map[string]cachedPage
	matches  map[string]cachedMatch
	inflight map[string]*inflightBuild
}

type cachedPage struct {
	page      RecommendationPage
	expiresAt time.Time
}

type cachedMatch struct {
	row       *domain.CatalogSearchResult
	expiresAt time.Time
}

type inflightBuild struct {
	done chan struct{}
	page RecommendationPage
	err  error
}

// NewRecommender builds the Last.fm recommendation service.
func NewRecommender(
	client TrackSource,
	matcher CatalogMatcher,
	streams domain.StreamCacheRepository,
	logger *slog.Logger,
) *Recommender {
	if logger == nil {
		logger = slog.Default()
	}
	return &Recommender{
		client:   client,
		matcher:  matcher,
		streams:  streams,
		logger:   logger,
		now:      time.Now,
		pages:    make(map[string]cachedPage),
		matches:  make(map[string]cachedMatch),
		inflight: make(map[string]*inflightBuild),
	}
}

// GetRecommendations builds (or reuses) the page for the connected account.
//
// force skips the 30-minute page cache. Concurrent callers for the same seed
// set share one build, so opening the page twice in a burst does not double the
// Last.fm requests.
func (r *Recommender) GetRecommendations(ctx context.Context, force bool) (RecommendationPage, error) {
	connected, username, avatarURL := r.client.GetStatus()
	if !connected || strings.TrimSpace(username) == "" {
		return RecommendationPage{Status: StatusNotConnected}, nil
	}

	seeds, partial := r.gatherSeeds(ctx)
	if len(seeds.all) == 0 && !seeds.connected {
		// The seed calls all failed with "not connected": surface the same state
		// as the initial check instead of an empty "partial" page.
		return RecommendationPage{Status: StatusNotConnected, Username: username, AvatarURL: avatarURL}, nil
	}

	cacheKey := username + "|" + seedHash(seeds.all)
	if !force {
		if page, ok := r.cachedPage(cacheKey); ok {
			return page, nil
		}
	}

	page, err := r.buildOnce(ctx, cacheKey, username, avatarURL, seeds, partial)
	return page, err
}

// seedSet is the merged input of one build.
type seedSet struct {
	recent []LastFmTrack
	top    []LastFmTrack
	loved  []LastFmTrack
	// all is the deduplicated union used for track.getSimilar.
	all []seed
	// connected is false when every seed request failed with the not-connected
	// sentinel, which means the session disappeared mid-page.
	connected bool
}

type seed struct {
	track LastFmTrack
	kind  seedKind
}

// gatherSeeds fetches the three seed sources in parallel and merges them.
//
// A failing source only removes its own list; partial reports whether anything
// failed so the page can say so.
func (r *Recommender) gatherSeeds(ctx context.Context) (seedSet, bool) {
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		recent   []LastFmTrack
		top      []LastFmTrack
		loved    []LastFmTrack
		failures int
		notConn  int
	)
	collect := func(tracks []LastFmTrack, err error, target *[]LastFmTrack) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			failures++
			if errors.Is(err, domain.ErrLastFmNotConnected) {
				notConn++
			}
			r.logger.Warn("last.fm seed request failed", "error", err)
		}
		// A failed top window must not discard the window that answered, so rows
		// are assigned independently of the error.
		if len(tracks) > 0 {
			*target = tracks
		}
	}

	wg.Add(3)
	go func() {
		defer wg.Done()
		tracks, err := r.client.GetRecentTracks(ctx, seedListLimit)
		collect(tracks, err, &recent)
	}()
	go func() {
		defer wg.Done()
		var merged []LastFmTrack
		var firstErr error
		for _, period := range strings.Split(topSeedPeriods, ",") {
			tracks, err := r.client.GetTopTracks(ctx, period, topPeriodLimit)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			merged = append(merged, tracks...)
		}
		collect(merged, firstErr, &top)
	}()
	go func() {
		defer wg.Done()
		tracks, err := r.client.GetLovedTracks(ctx, seedListLimit)
		collect(tracks, err, &loved)
	}()
	wg.Wait()

	set := seedSet{
		recent:    dedupeTracks(recent, seedTake),
		top:       dedupeTracks(top, seedTake),
		loved:     dedupeTracks(loved, seedTake),
		connected: failures == 0 || notConn < failures,
	}
	for _, item := range set.recent {
		set.all = append(set.all, seed{track: item, kind: seedRecent})
	}
	for _, item := range set.top {
		set.all = append(set.all, seed{track: item, kind: seedTop})
	}
	for _, item := range set.loved {
		set.all = append(set.all, seed{track: item, kind: seedLoved})
	}
	set.all = dedupeSeeds(set.all, maxSeeds)
	return set, failures > 0
}

// buildOnce runs one build per seed set, sharing it with concurrent callers.
func (r *Recommender) buildOnce(
	ctx context.Context,
	cacheKey, username, avatarURL string,
	seeds seedSet,
	partial bool,
) (RecommendationPage, error) {
	r.mu.Lock()
	if call, ok := r.inflight[cacheKey]; ok {
		r.mu.Unlock()
		select {
		case <-call.done:
			return call.page, call.err
		case <-ctx.Done():
			return RecommendationPage{Status: StatusPartial, Username: username, AvatarURL: avatarURL}, ctx.Err()
		}
	}
	call := &inflightBuild{done: make(chan struct{})}
	r.inflight[cacheKey] = call
	r.mu.Unlock()

	page, err := r.build(ctx, username, avatarURL, seeds, partial)

	r.mu.Lock()
	call.page, call.err = page, err
	close(call.done)
	delete(r.inflight, cacheKey)
	if err == nil {
		r.pages[cacheKey] = cachedPage{page: page, expiresAt: r.now().Add(recommendationCacheTTL)}
	}
	r.mu.Unlock()
	return page, err
}

// build is the cold path: similar tracks, similar artists, then matching.
func (r *Recommender) build(
	ctx context.Context,
	username, avatarURL string,
	seeds seedSet,
	partial bool,
) (RecommendationPage, error) {
	page := RecommendationPage{
		Status:         StatusOK,
		Username:       username,
		AvatarURL:      avatarURL,
		Tracks:         []RecommendedTrack{},
		RecentTracks:   []RecommendedTrack{},
		LovedTracks:    []RecommendedTrack{},
		TopTracks:      []RecommendedTrack{},
		SimilarArtists: []SimilarArtist{},
	}

	candidates, similarFailed := r.collectSimilarTracks(ctx, seeds)
	if similarFailed {
		partial = true
	}
	artists, artistsFailed := r.collectSimilarArtists(ctx, seeds)
	if artistsFailed {
		partial = true
	}
	page.SimilarArtists = artists

	combined := rankCombined(candidates)
	recentList := rankKind(candidates, seedRecent, perKindLimit)
	lovedList := rankKind(candidates, seedLoved, perKindLimit)
	topList := rankKind(candidates, seedTop, perKindLimit)

	sources := r.searchSources(ctx)
	matches, matchFailed := r.matchCandidates(ctx, orderedCandidates(combined, recentList, lovedList, topList), sources)
	if matchFailed {
		partial = true
	}

	page.Tracks = renderList(combined, matches, maxPlayable, combinedScore)
	page.RecentTracks = renderList(recentList, matches, perKindLimit, func(entry *candidate) float64 {
		return float64(entry.hits[seedRecent])
	})
	page.LovedTracks = renderList(lovedList, matches, perKindLimit, func(entry *candidate) float64 {
		return float64(entry.hits[seedLoved])
	})
	page.TopTracks = renderList(topList, matches, perKindLimit, func(entry *candidate) float64 {
		return float64(entry.hits[seedTop])
	})

	if partial {
		page.Status = StatusPartial
	}
	return page, nil
}

// --- Similar tracks ----------------------------------------------------------

type candidate struct {
	track LastFmTrack
	hits  map[seedKind]int
}

func candidateKey(artist, title string) string {
	return domain.NormalizationKey(artist) + "\x00" + domain.NormalizationKey(title)
}

// usableTrack reports whether both halves of the matching identity are present.
func usableTrack(track LastFmTrack) bool {
	return strings.TrimSpace(track.Artist) != "" && strings.TrimSpace(track.Title) != ""
}

// collectSimilarTracks asks Last.fm for every seed's neighbours and aggregates
// them by kind. It returns whether any request failed.
func (r *Recommender) collectSimilarTracks(ctx context.Context, seeds seedSet) (map[string]*candidate, bool) {
	candidates := make(map[string]*candidate)
	seedKeys := make(map[string]struct{}, len(seeds.all))
	for _, item := range seeds.all {
		seedKeys[candidateKey(item.track.Artist, item.track.Title)] = struct{}{}
	}

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		failures int
	)
	for _, item := range seeds.all {
		item := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracks, err := r.client.GetSimilarTracks(ctx, item.track.Artist, item.track.Title, similarTrackLimit)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures++
				r.logger.Debug("last.fm similar tracks failed",
					"artist", item.track.Artist, "title", item.track.Title, "error", err)
				// A single failed seed is skipped, never fatal.
				return
			}
			seen := make(map[string]struct{}, len(tracks))
			for _, track := range tracks {
				if !usableTrack(track) {
					continue
				}
				key := candidateKey(track.Artist, track.Title)
				if _, isSeed := seedKeys[key]; isSeed {
					// §3.2.5: a seed is never its own recommendation.
					continue
				}
				if _, duplicate := seen[key]; duplicate {
					continue
				}
				seen[key] = struct{}{}
				entry := candidates[key]
				if entry == nil {
					entry = &candidate{track: track, hits: make(map[seedKind]int)}
					candidates[key] = entry
				}
				// One seed counts once, even if Last.fm listed it twice.
				entry.hits[item.kind]++
			}
		}()
	}
	wg.Wait()
	return candidates, failures > 0
}

// rankKind returns one source-specific list: hit count descending, then the
// track title for a stable order across builds.
func rankKind(candidates map[string]*candidate, kind seedKind, limit int) []*candidate {
	list := make([]*candidate, 0, len(candidates))
	for _, entry := range candidates {
		if entry.hits[kind] > 0 {
			list = append(list, entry)
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].hits[kind] != list[j].hits[kind] {
			return list[i].hits[kind] > list[j].hits[kind]
		}
		return strings.ToLower(list[i].track.Title) < strings.ToLower(list[j].track.Title)
	})
	if len(list) > limit {
		list = list[:limit]
	}
	return list
}

// combinedScore weights each source's hit count.
func combinedScore(entry *candidate) float64 {
	score := 0.0
	for kind, weight := range seedKindWeights {
		score += float64(entry.hits[kind]) * weight
	}
	return score
}

func rankCombined(candidates map[string]*candidate) []*candidate {
	list := make([]*candidate, 0, len(candidates))
	for _, entry := range candidates {
		if combinedScore(entry) > 0 {
			list = append(list, entry)
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		left, right := combinedScore(list[i]), combinedScore(list[j])
		if left != right {
			return left > right
		}
		return strings.ToLower(list[i].track.Title) < strings.ToLower(list[j].track.Title)
	})
	if len(list) > combinedLimit {
		list = list[:combinedLimit]
	}
	return list
}

// orderedCandidates flattens the lists for matching, combined first, without
// duplicate keys.
func orderedCandidates(lists ...[]*candidate) []*candidate {
	seen := make(map[string]struct{})
	ordered := make([]*candidate, 0)
	for _, list := range lists {
		for _, entry := range list {
			key := candidateKey(entry.track.Artist, entry.track.Title)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			ordered = append(ordered, entry)
			if len(ordered) >= maxMatchCandidates {
				return ordered
			}
		}
	}
	return ordered
}

// --- Similar artists ---------------------------------------------------------

func (r *Recommender) collectSimilarArtists(ctx context.Context, seeds seedSet) ([]SimilarArtist, bool) {
	seen := make(map[string]struct{})
	names := make([]string, 0, similarArtistSeedLimit)
	for _, item := range seeds.all {
		artist := strings.TrimSpace(item.track.Artist)
		if artist == "" {
			continue
		}
		key := domain.NormalizationKey(artist)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		names = append(names, artist)
		if len(names) >= similarArtistSeedLimit {
			break
		}
	}
	if len(names) == 0 {
		return []SimilarArtist{}, false
	}

	type aggregate struct {
		artist SimilarArtist
		count  int
	}
	type artistAnswer struct {
		items []LastFmArtist
		err   error
	}

	answers := make([]artistAnswer, len(names))
	var (
		wg       sync.WaitGroup
		failures int
		failMu   sync.Mutex
	)
	for index, artist := range names {
		index, artist := index, artist
		wg.Add(1)
		go func() {
			defer wg.Done()
			similar, err := r.client.GetSimilarArtists(ctx, artist, similarArtistLimit)
			if err != nil {
				failMu.Lock()
				failures++
				failMu.Unlock()
				r.logger.Debug("last.fm similar artists failed", "artist", artist, "error", err)
			}
			answers[index] = artistAnswer{items: similar, err: err}
		}()
	}
	wg.Wait()

	// Merge in seed order so the image and listener count shown for an artist
	// that several seeds agreed on do not depend on which request finished first.
	counts := make(map[string]*aggregate)
	for _, answer := range answers {
		for _, item := range answer.items {
			key := domain.NormalizationKey(item.Name)
			if key == "" {
				continue
			}
			entry := counts[key]
			if entry == nil {
				entry = &aggregate{artist: SimilarArtist{
					Name:      item.Name,
					ImageURL:  item.ImageURL,
					Listeners: item.Listeners,
					URL:       item.URL,
				}}
				counts[key] = entry
			}
			entry.count++
			if entry.artist.ImageURL == "" {
				entry.artist.ImageURL = item.ImageURL
			}
			if entry.artist.Listeners == 0 {
				entry.artist.Listeners = item.Listeners
			}
			if entry.artist.URL == "" {
				entry.artist.URL = item.URL
			}
		}
	}

	merged := make([]*aggregate, 0, len(counts))
	for _, entry := range counts {
		merged = append(merged, entry)
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].count != merged[j].count {
			return merged[i].count > merged[j].count
		}
		return strings.ToLower(merged[i].artist.Name) < strings.ToLower(merged[j].artist.Name)
	})
	if len(merged) > similarArtistTake {
		merged = merged[:similarArtistTake]
	}

	result := make([]SimilarArtist, 0, len(merged))
	for _, entry := range merged {
		result = append(result, entry.artist)
	}
	return result, failures > 0
}

// --- Catalogue matching ------------------------------------------------------

// searchSources resolves the source filter used for matching. The page asks at
// most two sources per track, which is what the specification budgets.
func (r *Recommender) searchSources(ctx context.Context) []string {
	if r.matcher == nil {
		return nil
	}
	sources, err := r.matcher.Sources(ctx)
	if err != nil {
		r.logger.Warn("last.fm recommendations: cannot list catalogue sources", "error", err)
		return nil
	}
	ids := make([]string, 0, len(sources))
	for _, source := range sources {
		if id := strings.TrimSpace(source.ID); id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) > 2 {
		ids = ids[:2]
	}
	return ids
}

// matchCandidates matches every candidate once and returns the chosen row per
// key. The per-track result is cached for 30 minutes, so the four lists and a
// later page build reuse the same search.
func (r *Recommender) matchCandidates(
	ctx context.Context, candidates []*candidate, sources []string,
) (map[string]*domain.CatalogSearchResult, bool) {
	matches := make(map[string]*domain.CatalogSearchResult, len(candidates))
	if r.matcher == nil || len(sources) == 0 {
		return matches, false
	}

	matchCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		failures int
		slots    = make(chan struct{}, matchConcurrency)
	)
	for _, entry := range candidates {
		entry := entry
		key := candidateKey(entry.track.Artist, entry.track.Title)
		if row, ok := r.cachedMatch(key, sources); ok {
			if row != nil {
				matches[key] = row
			}
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-matchCtx.Done():
				mu.Lock()
				failures++
				mu.Unlock()
				return
			}
			defer func() { <-slots }()

			row, err := r.matchOne(matchCtx, entry.track, sources)
			mu.Lock()
			defer mu.Unlock()
			// A partial search failure still leaves usable rows; keep them and
			// report the page as partial.
			if row != nil {
				r.storeMatch(key, sources, row)
				matches[key] = row
			} else if err == nil {
				r.storeMatch(key, sources, nil)
			}
			if err != nil {
				failures++
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					r.logger.Debug("last.fm recommendations: match failed",
						"artist", entry.track.Artist, "title", entry.track.Title, "error", err)
				}
			}
		}()
	}
	wg.Wait()
	return matches, failures > 0
}

// matchOne picks the first row across the priority documented in §4: an
// on-disk file, then a cached stream, then a downloadable source hit.
func (r *Recommender) matchOne(
	ctx context.Context, track LastFmTrack, sources []string,
) (*domain.CatalogSearchResult, error) {
	rows, err := r.matcher.MatchTrack(ctx, track.Title, track.Artist, track.Duration, sources)
	if err != nil && len(rows) == 0 {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, err
	}

	best := chooseMatch(ctx, r.streams, rows)
	return best, err
}

func chooseMatch(ctx context.Context, streams domain.StreamCacheRepository, rows []domain.CatalogSearchResult) *domain.CatalogSearchResult {
	// 1. A local file always wins, whether it is an imported file or a finished
	//    download of this very identity.
	for index := range rows {
		if rows[index].State == domain.CatalogRowStateInLibrary {
			row := rows[index]
			return &row
		}
	}
	// 2. A stream-cache copy is instantly playable.
	for index := range rows {
		if streamCached(ctx, streams, rows[index]) {
			row := rows[index]
			return &row
		}
	}
	// 3. A downloadable search hit.
	for index := range rows {
		if rows[index].Downloadable {
			row := rows[index]
			return &row
		}
	}
	return nil
}

func streamCached(ctx context.Context, streams domain.StreamCacheRepository, row domain.CatalogSearchResult) bool {
	if streams == nil {
		return false
	}
	if entry, err := streams.GetByIdentity(ctx, row.Provider, row.ExternalID); err == nil && entry != nil {
		return true
	}
	titleKey := domain.NormalizationKey(row.Title)
	artist := ""
	if len(row.Artists) > 0 {
		artist = domain.NormalizationKey(row.Artists[0])
	}
	if titleKey == "" || artist == "" {
		return false
	}
	entry, err := streams.MatchByTitleArtist(ctx, titleKey, artist, int64(row.Duration)*1000, 5000)
	return err == nil && entry != nil
}

// renderList maps matched rows onto one list, preserving its order.
func renderList(
	list []*candidate,
	matches map[string]*domain.CatalogSearchResult,
	limit int,
	score func(*candidate) float64,
) []RecommendedTrack {
	result := make([]RecommendedTrack, 0, len(list))
	for _, entry := range list {
		row := matches[candidateKey(entry.track.Artist, entry.track.Title)]
		if row == nil {
			continue
		}
		result = append(result, toRecommended(row, score(entry)))
		if len(result) >= limit {
			break
		}
	}
	return result
}

func toRecommended(row *domain.CatalogSearchResult, score float64) RecommendedTrack {
	return RecommendedTrack{
		Provider:              row.Provider,
		ExternalID:            row.ExternalID,
		Title:                 row.Title,
		Artists:               append([]string{}, row.Artists...),
		AlbumName:             row.AlbumName,
		Duration:              row.Duration,
		CoverURL:              row.CoverURL,
		CoverURLLarge:         row.CoverURLLarge,
		Downloadable:          row.Downloadable,
		DownloadBlockedReason: row.DownloadBlockedReason,
		State:                 row.State,
		LocalTrackID:          row.LocalTrackID,
		Sources:               append([]string{}, row.Sources...),
		Score:                 score,
	}
}

// --- Caches ------------------------------------------------------------------

func (r *Recommender) cachedPage(key string) (RecommendationPage, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.pages[key]
	if !ok {
		return RecommendationPage{}, false
	}
	if r.now().After(entry.expiresAt) {
		delete(r.pages, key)
		return RecommendationPage{}, false
	}
	return entry.page, true
}

func (r *Recommender) cachedMatch(key string, sources []string) (*domain.CatalogSearchResult, bool) {
	cacheKey := matchCacheKey(key, sources)
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.matches[cacheKey]
	if !ok {
		return nil, false
	}
	if r.now().After(entry.expiresAt) {
		delete(r.matches, cacheKey)
		return nil, false
	}
	return entry.row, true
}

func (r *Recommender) storeMatch(key string, sources []string, row *domain.CatalogSearchResult) {
	cacheKey := matchCacheKey(key, sources)
	r.mu.Lock()
	defer r.mu.Unlock()
	var copyRow *domain.CatalogSearchResult
	if row != nil {
		value := *row
		copyRow = &value
	}
	r.matches[cacheKey] = cachedMatch{row: copyRow, expiresAt: r.now().Add(matchCacheTTL)}
}

func matchCacheKey(key string, sources []string) string {
	return key + "|" + strings.Join(sources, ",")
}

// seedHash identifies one seed set for the page cache.
func seedHash(seeds []seed) string {
	parts := make([]string, 0, len(seeds))
	for _, item := range seeds {
		parts = append(parts, string(item.kind)+"|"+
			domain.NormalizationKey(item.track.Artist)+"|"+
			domain.NormalizationKey(item.track.Title))
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:16])
}

// dedupeTracks keeps the first occurrence of each artist+title and caps the
// list.
func dedupeTracks(tracks []LastFmTrack, limit int) []LastFmTrack {
	seen := make(map[string]struct{}, len(tracks))
	result := make([]LastFmTrack, 0, len(tracks))
	for _, track := range tracks {
		if !usableTrack(track) {
			continue
		}
		key := candidateKey(track.Artist, track.Title)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, track)
		if len(result) >= limit {
			break
		}
	}
	return result
}

// dedupeSeeds keeps the first occurrence of each artist+title across the three
// sources, so a track that is both recent and loved is one query.
func dedupeSeeds(seeds []seed, limit int) []seed {
	seen := make(map[string]struct{}, len(seeds))
	result := make([]seed, 0, len(seeds))
	for _, item := range seeds {
		if !usableTrack(item.track) {
			continue
		}
		key := candidateKey(item.track.Artist, item.track.Title)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, item)
		if len(result) >= limit {
			break
		}
	}
	return result
}
