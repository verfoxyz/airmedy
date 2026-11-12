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

	"github.com/wailsapp/wails/v3/pkg/application"
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
	// maxMatchCandidates bounds one page's background matching. It matches the
	// four lists after de-duplication, not 60 per list.
	maxMatchCandidates = 60
	// matchConcurrency is the number of catalogue searches in flight.
	matchConcurrency = 4
	// matchCandidateTimeout bounds one candidate's attempt against one source.
	matchCandidateTimeout = 15 * time.Second
	// matchEventBatch flushes the event buffer once this many rows are ready.
	matchEventBatch = 3
	// matchEventInterval is the periodic flush for a slow trickle of matches.
	matchEventInterval = 500 * time.Millisecond
	// fallbackSeedLimit bounds how many seeds run the neighbour fallback. A cold
	// account's seeds all miss track.getSimilar, and the fallback costs four
	// requests per seed.
	fallbackSeedLimit = 10
	// fallbackSimilarArtistLimit is how many neighbours one seed may inspect.
	fallbackSimilarArtistLimit = 3
	// fallbackArtistTrackLimit is the per-neighbour track window.
	fallbackArtistTrackLimit = 5
	// fallbackTagLimit bounds the aggregated tags used by the global fallback.
	fallbackTagLimit = 5
	// fallbackTagTrackLimit is the per-tag track window.
	fallbackTagTrackLimit = 10
	// fallbackChartLimit is the global chart window of the last resort.
	fallbackChartLimit = 20
)

// sourcePriority is the matching order: fast sources first, then anything else
// a plugin reports. One candidate tries at most two of them.
var sourcePriority = map[string]int{
	"wy": 0,
	"kg": 1,
	"tx": 2,
	"kw": 3,
	"mg": 4,
}

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
	GetArtistTopTracks(ctx context.Context, artist string, limit int) ([]LastFmTrack, error)
	GetUserTopTags(ctx context.Context, limit int) ([]LastFmTag, error)
	GetArtistTopTags(ctx context.Context, artist string, limit int) ([]LastFmTag, error)
	GetTagTopTracks(ctx context.Context, tag string, limit int) ([]LastFmTrack, error)
	GetChartTopTracks(ctx context.Context, limit int) ([]LastFmTrack, error)
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
	// MatchLocalTrack answers "is this song already on disk?" without a network
	// call, so a library copy is reused before any source is searched.
	MatchLocalTrack(
		ctx context.Context, title string, artists []string, duration int,
	) (*domain.TrackDTO, error)
	Sources(ctx context.Context) ([]domain.CatalogSearchSource, error)
}

// RecommendedTrack is one recommendation, before or after source matching.
//
// The first page ships Last.fm's own rows with Matching=true and no provider.
// The background matcher then sends the same row (same MatchKey) with provider
// fields filled in and Matching=false. A row that finished without a provider
// is the frontend's "no playable source" state.
type RecommendedTrack struct {
	Provider   string   `json:"provider"`
	ExternalID string   `json:"external_id"`
	Title      string   `json:"title"`
	Artists    []string `json:"artists"`
	AlbumName  string   `json:"album_name"`
	Duration   int      `json:"duration"`
	CoverURL   string   `json:"cover_url"`
	// CoverURLLarge is the rendition the saved file should embed.
	CoverURLLarge string `json:"cover_url_large"`
	Downloadable  bool   `json:"downloadable"`
	// DownloadBlockedReason is a machine kind ("no_resolver") the frontend words.
	DownloadBlockedReason string `json:"blocked_reason"`
	// State is the catalogue row state: "", in_library, downloading, failed, saved.
	State string `json:"state"`
	// LocalTrackID is an on-disk row that already represents this song.
	LocalTrackID string   `json:"local_track_id"`
	Sources      []string `json:"sources"`
	// Score is the recommendation score within the list that contains it.
	Score float64 `json:"score"`
	// Source is "similar" for a direct track.getSimilar hit and "fallback" for a
	// row produced by the neighbour/tag/chart fallback chain. The page renders
	// both as ordinary recommendations.
	Source string `json:"source"`
	// MatchKey is the stable artist+title identity shared by the initial row and
	// every event update. The frontend replaces rows by this key.
	MatchKey string `json:"match_key"`
	// Matching is true while the background matcher has not finished this row.
	Matching bool `json:"matching"`
}

// RecommendationMatchUpdate is the payload of lastfm:recommendations-matched.
//
// The backend sends 1–3 matched rows at a time, coalesced every 500 ms. Done is
// the final frame: rows still marked Matching on the page have no playable
// source. It is emitted from the app layer, so the JSON tags are the frontend
// contract.
type RecommendationMatchUpdate struct {
	Username string             `json:"username"`
	Items    []RecommendedTrack `json:"items"`
	Done     bool               `json:"done"`
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
	// Matching reports whether any row still awaits source matching.
	Matching bool
	Tracks   []RecommendedTrack
	// RecentTracks / LovedTracks / TopTracks are the three per-seed-source lists.
	RecentTracks   []RecommendedTrack
	LovedTracks    []RecommendedTrack
	TopTracks      []RecommendedTrack
	SimilarArtists []SimilarArtist
}

// EventEmitter delivers one frontend event. It is a seam so the matcher can be
// tested without a running Wails application.
type EventEmitter func(name string, payload any)

// recommendationsMatchedEvent is emitted as matching progresses.
const recommendationsMatchedEvent = "lastfm:recommendations-matched"

// Recommender builds the personalised page from Last.fm seeds and catalogue
// matches.
type Recommender struct {
	client  TrackSource
	matcher CatalogMatcher
	streams domain.StreamCacheRepository
	logger  *slog.Logger
	now     func() time.Time
	// emitter delivers match progress. nil disables events (tests that do not
	// inspect them).
	emitter EventEmitter

	mu           sync.Mutex
	pages        map[string]cachedPage
	matches      map[string]cachedMatch
	inflight     map[string]*inflightBuild
	matchRunning map[string]bool
	// sourceMatches is the MVP per (artist+title, source) cache; topRunning
	// keeps one worker queue per account.
	sourceMatches map[string]cachedSourceMatch
	topRunning    map[string]bool
	// topQueue holds follow-up runs (a re-queued row after a playback failure)
	// while a page's worker queue is still running.
	topQueue map[string][]queuedTopTrackRun
	// topRows is the original Last.fm row of every track on the current page. A
	// playback failure resets the row from this copy, because the matched item
	// has already replaced the title/artist with the provider's version.
	topRows map[string]TopTrackItem
	// topFailedSources remembers, per row, which providers could not play it.
	// Every later run skips them, so a page refresh cannot send the user back to
	// the same broken source.
	topFailedSources map[string]map[string]bool
	// coverAssets resolves a cached cover by identity when neither Last.fm nor
	// the matched source reported one. Optional.
	coverAssets domain.CoverIdentityAssets
	// similarRecs caches the "for you" answer per seed set for 30 minutes, so a
	// refresh does not spend the track.getSimilar request budget again.
	similarRecs map[string]cachedSimilarRecommendation
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
		client:           client,
		matcher:          matcher,
		streams:          streams,
		logger:           logger,
		now:              time.Now,
		emitter:          emitToWails,
		pages:            make(map[string]cachedPage),
		matches:          make(map[string]cachedMatch),
		inflight:         make(map[string]*inflightBuild),
		matchRunning:     make(map[string]bool),
		sourceMatches:    make(map[string]cachedSourceMatch),
		topRunning:       make(map[string]bool),
		topQueue:         make(map[string][]queuedTopTrackRun),
		topRows:          make(map[string]TopTrackItem),
		topFailedSources: make(map[string]map[string]bool),
		similarRecs:      make(map[string]cachedSimilarRecommendation),
	}
}

// SetCoverAssets installs the identity cover resolver. Optional: without it a
// row that has no Last.fm or source cover keeps the placeholder.
func (r *Recommender) SetCoverAssets(assets domain.CoverIdentityAssets) {
	r.mu.Lock()
	r.coverAssets = assets
	r.mu.Unlock()
}

// coverForIdentity returns the cached cover URL for a provider identity, or "".
func (r *Recommender) coverForIdentity(provider, externalID string) string {
	r.mu.Lock()
	assets := r.coverAssets
	r.mu.Unlock()
	if assets == nil {
		return ""
	}
	provider = domain.NormalizeProvider(provider)
	externalID = strings.TrimSpace(externalID)
	if provider == "" || externalID == "" {
		return ""
	}
	assetURL, err := assets.AssetForIdentity(context.Background(), provider, externalID)
	if err != nil {
		return ""
	}
	return assetURL
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
			return r.decorateAndStart(ctx, cacheKey, username, page), nil
		}
	}

	page, err := r.buildOnce(ctx, cacheKey, username, avatarURL, seeds, partial)
	if err != nil {
		return page, err
	}
	return r.decorateAndStart(ctx, cacheKey, username, page), nil
}

// decorateAndStart applies cached matches to a fresh page and starts the
// background matcher for the rest.
//
// The page is returned before any source search runs: Last.fm rows are visible
// immediately, and the browser fills in provider identities through
// lastfm:recommendations-matched. Rows whose match is already cached come back
// complete, so reopening the page does not flash "matching".
func (r *Recommender) decorateAndStart(
	ctx context.Context, cacheKey, username string, page RecommendationPage,
) RecommendationPage {
	page, pending := r.decoratePage(page)
	if len(pending) == 0 {
		return page
	}
	sources := r.matchSources(ctx)
	if len(sources) == 0 {
		// Nothing can be searched: finish immediately instead of leaving the page
		// waiting forever.
		page.Matching = true
		r.startMatching(cacheKey, username, pending, nil)
		return page
	}
	r.startMatching(cacheKey, username, pending, sources)
	return page
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

	// A cold account can produce no candidate at all. The global fallback keeps
	// the page useful by deriving tags from the seed artists and, as a last
	// resort, the site-wide chart.
	var globalFallback []*candidate
	if len(combined) == 0 && len(recentList) == 0 && len(lovedList) == 0 && len(topList) == 0 {
		fallback, fallbackFailed := r.collectGlobalFallback(ctx, seeds)
		globalFallback = fallback
		if fallbackFailed {
			partial = true
		}
	}

	page.Tracks = renderCandidates(combined, maxPlayable, combinedScore)
	if len(page.Tracks) == 0 && len(globalFallback) > 0 {
		page.Tracks = renderCandidates(globalFallback, maxPlayable, func(entry *candidate) float64 {
			return entry.score
		})
	}
	page.RecentTracks = renderCandidates(recentList, perKindLimit, func(entry *candidate) float64 {
		return float64(entry.hits[seedRecent])
	})
	page.LovedTracks = renderCandidates(lovedList, perKindLimit, func(entry *candidate) float64 {
		return float64(entry.hits[seedLoved])
	})
	page.TopTracks = renderCandidates(topList, perKindLimit, func(entry *candidate) float64 {
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
	// fromSimilar / fromFallback record how the row was found. A row seen by
	// both keeps fromSimilar, so the source says "fallback" only when nothing
	// else produced it.
	fromSimilar  bool
	fromFallback bool
	// score orders global-fallback rows; normal rows rank by their weighted hits.
	score float64
}

// source is the value the page shows for how the row was found.
func (c *candidate) source() string {
	if c.fromFallback && !c.fromSimilar {
		return "fallback"
	}
	return "similar"
}

// fallbackHit is one track a seed's neighbour artists produced, with how many
// neighbours listed it.
type fallbackHit struct {
	track LastFmTrack
	count int
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
//
// A seed whose similar list is empty gets one neighbour fallback (only the
// first fallbackSeedLimit seeds, to bound the request burst): similar artists,
// then their top tracks. The result keeps the same per-kind hit accounting, so
// the four lists continue to rank by how many seeds support a row.
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
	for index, item := range seeds.all {
		index, item := index, item
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracks, err := r.client.GetSimilarTracks(ctx, item.track.Artist, item.track.Title, similarTrackLimit)

			var (
				fallback       []fallbackHit
				fallbackFailed bool
			)
			if err == nil && len(tracks) == 0 && index < fallbackSeedLimit {
				fallback, fallbackFailed = r.fallbackForSeed(ctx, item.track)
			}

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures++
				r.logger.Debug("last.fm similar tracks failed",
					"artist", item.track.Artist, "title", item.track.Title, "error", err)
				// A single failed seed is skipped, never fatal.
				return
			}
			if fallbackFailed {
				failures++
			}

			if len(fallback) > 0 {
				for _, hit := range fallback {
					if !usableTrack(hit.track) {
						continue
					}
					key := candidateKey(hit.track.Artist, hit.track.Title)
					if _, isSeed := seedKeys[key]; isSeed {
						continue
					}
					entry := candidates[key]
					if entry == nil {
						entry = &candidate{track: hit.track, hits: make(map[seedKind]int)}
						candidates[key] = entry
					}
					entry.fromFallback = true
					entry.hits[item.kind] += hit.count
				}
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
				entry.fromSimilar = true
				entry.hits[item.kind]++
			}
		}()
	}
	wg.Wait()
	return candidates, failures > 0
}

// fallbackForSeed replaces one empty track.getSimilar answer with the top tracks
// of its similar artists.
//
// Budget: one artist.getSimilar plus at most fallbackSimilarArtistLimit
// artist.getTopTracks calls. The caller only invokes it for the first
// fallbackSeedLimit seeds. Request errors are reported to the caller; an empty
// answer is simply "this seed has nothing".
func (r *Recommender) fallbackForSeed(ctx context.Context, seed LastFmTrack) ([]fallbackHit, bool) {
	artists, err := r.client.GetSimilarArtists(ctx, seed.Artist, fallbackSimilarArtistLimit)
	if err != nil {
		r.logger.Debug("last.fm fallback: similar artists failed",
			"artist", seed.Artist, "error", err)
		return nil, true
	}
	if len(artists) > fallbackSimilarArtistLimit {
		artists = artists[:fallbackSimilarArtistLimit]
	}

	seedKey := candidateKey(seed.Artist, seed.Title)
	counts := make(map[string]int)
	tracks := make(map[string]LastFmTrack)
	failed := false
	for _, artist := range artists {
		name := strings.TrimSpace(artist.Name)
		if name == "" {
			continue
		}
		items, err := r.client.GetArtistTopTracks(ctx, name, fallbackArtistTrackLimit)
		if err != nil {
			failed = true
			r.logger.Debug("last.fm fallback: artist top tracks failed",
				"artist", name, "error", err)
			continue
		}
		for _, track := range items {
			if !usableTrack(track) {
				continue
			}
			key := candidateKey(track.Artist, track.Title)
			if key == seedKey {
				continue
			}
			counts[key]++
			if _, exists := tracks[key]; !exists {
				tracks[key] = track
			}
		}
	}

	hits := make([]fallbackHit, 0, len(counts))
	for key, count := range counts {
		hits = append(hits, fallbackHit{track: tracks[key], count: count})
	}
	// Order by how many neighbours listed the track, then by title so the page
	// is stable across builds.
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].count != hits[j].count {
			return hits[i].count > hits[j].count
		}
		return strings.ToLower(hits[i].track.Title) < strings.ToLower(hits[j].track.Title)
	})
	return hits, failed
}

// collectGlobalFallback guarantees a cold account still sees some music.
//
// Tags come from the user's own top tags plus every seed artist's tags (a user
// with no scrobble weight has no tags of their own). The site-wide chart is the
// last resort. Every row is marked as a fallback recommendation.
func (r *Recommender) collectGlobalFallback(ctx context.Context, seeds seedSet) ([]*candidate, bool) {
	failed := false
	tags := r.collectTags(ctx, seeds, &failed)

	var tracks []LastFmTrack
	for _, tag := range tags {
		items, err := r.client.GetTagTopTracks(ctx, tag, fallbackTagTrackLimit)
		if err != nil {
			failed = true
			r.logger.Debug("last.fm fallback: tag top tracks failed", "tag", tag, "error", err)
			continue
		}
		tracks = append(tracks, items...)
	}
	if len(tracks) == 0 {
		items, err := r.client.GetChartTopTracks(ctx, fallbackChartLimit)
		if err != nil {
			failed = true
			r.logger.Debug("last.fm fallback: chart failed", "error", err)
		} else {
			tracks = items
		}
	}

	seedKeys := make(map[string]struct{}, len(seeds.all))
	for _, item := range seeds.all {
		seedKeys[candidateKey(item.track.Artist, item.track.Title)] = struct{}{}
	}

	candidates := make([]*candidate, 0, len(tracks))
	seen := make(map[string]struct{}, len(tracks))
	score := float64(len(tracks))
	for _, track := range tracks {
		if !usableTrack(track) {
			continue
		}
		key := candidateKey(track.Artist, track.Title)
		if _, isSeed := seedKeys[key]; isSeed {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		candidates = append(candidates, &candidate{
			track:        track,
			hits:         make(map[seedKind]int),
			fromFallback: true,
			score:        score,
		})
		score--
	}
	if len(candidates) > combinedLimit {
		candidates = candidates[:combinedLimit]
	}
	return candidates, failed
}

// collectTags merges the user's top tags with the seed artists' tags.
func (r *Recommender) collectTags(ctx context.Context, seeds seedSet, failed *bool) []string {
	counts := make(map[string]int)
	names := make(map[string]string)
	add := func(tags []LastFmTag) {
		for _, tag := range tags {
			name := strings.TrimSpace(tag.Name)
			if name == "" {
				continue
			}
			key := domain.NormalizationKey(name)
			if key == "" {
				continue
			}
			counts[key]++
			if _, exists := names[key]; !exists {
				names[key] = name
			}
		}
	}

	userTags, err := r.client.GetUserTopTags(ctx, fallbackTagLimit*2)
	if err != nil {
		*failed = true
		r.logger.Debug("last.fm fallback: user top tags failed", "error", err)
	} else {
		add(userTags)
	}

	seenArtists := make(map[string]struct{})
	for _, item := range seeds.all {
		artist := strings.TrimSpace(item.track.Artist)
		if artist == "" {
			continue
		}
		key := domain.NormalizationKey(artist)
		if _, duplicate := seenArtists[key]; duplicate {
			continue
		}
		seenArtists[key] = struct{}{}
		tags, err := r.client.GetArtistTopTags(ctx, artist, 5)
		if err != nil {
			*failed = true
			r.logger.Debug("last.fm fallback: artist top tags failed", "artist", artist, "error", err)
			continue
		}
		add(tags)
		if len(seenArtists) >= fallbackSeedLimit {
			break
		}
	}

	type tagScore struct {
		name  string
		count int
	}
	list := make([]tagScore, 0, len(counts))
	for key, count := range counts {
		list = append(list, tagScore{name: names[key], count: count})
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].count != list[j].count {
			return list[i].count > list[j].count
		}
		return strings.ToLower(list[i].name) < strings.ToLower(list[j].name)
	})
	if len(list) > fallbackTagLimit {
		list = list[:fallbackTagLimit]
	}

	result := make([]string, 0, len(list))
	for _, item := range list {
		result = append(result, item.name)
	}
	return result
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

// matchSources returns the sources in matching order, capped at the two a
// candidate may try.
func (r *Recommender) matchSources(ctx context.Context) []string {
	sources := r.allSources(ctx)
	if len(sources) > 2 {
		sources = sources[:2]
	}
	return sources
}

// allSources returns every available source in the priority order. Fast sources
// come first; any other plugin keeps an alphabetical order after the known ones.
func (r *Recommender) allSources(ctx context.Context) []string {
	if r.matcher == nil {
		return nil
	}
	sources, err := r.matcher.Sources(ctx)
	if err != nil {
		r.logger.Warn("last.fm recommendations: cannot list catalogue sources", "error", err)
		return nil
	}
	ids := make([]string, 0, len(sources))
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		id := strings.TrimSpace(source.ID)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool {
		left, right := sourceRank(ids[i]), sourceRank(ids[j])
		if left != right {
			return left < right
		}
		return ids[i] < ids[j]
	})
	return ids
}

func sourceRank(id string) int {
	if rank, ok := sourcePriority[strings.ToLower(id)]; ok {
		return rank
	}
	return len(sourcePriority)
}

// decoratePage applies already-known matches to the raw Last.fm rows and
// returns the rows that still need a search.
func (r *Recommender) decoratePage(page RecommendationPage) (RecommendationPage, []RecommendedTrack) {
	matched := make(map[string]RecommendedTrack)
	failed := make(map[string]struct{})
	pendingSeen := make(map[string]struct{})
	pending := make([]RecommendedTrack, 0)

	for _, list := range [][]RecommendedTrack{page.Tracks, page.RecentTracks, page.LovedTracks, page.TopTracks} {
		for _, row := range list {
			key := row.MatchKey
			if key == "" {
				continue
			}
			if _, known := matched[key]; known {
				continue
			}
			if _, known := failed[key]; known {
				continue
			}
			if _, queued := pendingSeen[key]; queued {
				continue
			}
			cached, ok := r.cachedMatch(key)
			if !ok {
				pendingSeen[key] = struct{}{}
				pending = append(pending, row)
				continue
			}
			if cached == nil {
				// A cached "no match" is definitive for the cache window; the row
				// renders as "no playable source" without another search.
				failed[key] = struct{}{}
				continue
			}
			matched[key] = r.matchedRecommended(row, cached)
		}
	}
	if len(pending) > maxMatchCandidates {
		pending = pending[:maxMatchCandidates]
	}

	page.Tracks = applyMatches(page.Tracks, matched, failed)
	page.RecentTracks = applyMatches(page.RecentTracks, matched, failed)
	page.LovedTracks = applyMatches(page.LovedTracks, matched, failed)
	page.TopTracks = applyMatches(page.TopTracks, matched, failed)
	page.Matching = len(pending) > 0
	return page, pending
}

func applyMatches(
	rows []RecommendedTrack, matched map[string]RecommendedTrack, failed map[string]struct{},
) []RecommendedTrack {
	if len(rows) == 0 {
		return rows
	}
	result := make([]RecommendedTrack, len(rows))
	for index, row := range rows {
		if match, ok := matched[row.MatchKey]; ok {
			result[index] = match
			continue
		}
		if _, ok := failed[row.MatchKey]; ok {
			row.Matching = false
		}
		result[index] = row
	}
	return result
}

// startMatching launches one background matcher per page key. A second request
// while the job runs joins it instead of doubling the source calls.
func (r *Recommender) startMatching(
	cacheKey, username string, pending []RecommendedTrack, sources []string,
) {
	r.mu.Lock()
	if r.matchRunning[cacheKey] {
		r.mu.Unlock()
		return
	}
	r.matchRunning[cacheKey] = true
	r.mu.Unlock()

	go r.runMatching(cacheKey, username, pending, sources)
}

// runMatching searches the pending rows and emits matches as they arrive.
//
// Matches are coalesced into batches of matchEventBatch or one event per
// matchEventInterval, whichever comes first. The final event always carries
// Done=true, even when nothing matched.
func (r *Recommender) runMatching(
	cacheKey, username string, pending []RecommendedTrack, sources []string,
) {
	defer func() {
		r.mu.Lock()
		delete(r.matchRunning, cacheKey)
		r.mu.Unlock()
	}()

	if len(sources) == 0 || r.matcher == nil {
		r.emitMatchUpdate(username, nil, true)
		return
	}

	var (
		bufferMu sync.Mutex
		buffer   []RecommendedTrack
	)
	flush := func(done bool) {
		bufferMu.Lock()
		items := append([]RecommendedTrack{}, buffer...)
		buffer = nil
		bufferMu.Unlock()
		if len(items) == 0 && !done {
			return
		}
		r.emitMatchUpdate(username, items, done)
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

	var wg sync.WaitGroup
	slots := make(chan struct{}, matchConcurrency)
	for _, row := range pending {
		row := row
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()

			match, err := r.matchSlow(row, sources)
			// A failed search is cached as "no match" too: the page must not
			// hammer a dead source on every refresh.
			r.storeMatch(row.MatchKey, match)
			if err != nil && match == nil {
				r.logger.Debug("last.fm recommendations: match failed",
					"artist", firstArtist(row.Artists), "title", row.Title, "error", err)
			}
			if match == nil {
				return
			}

			item := r.matchedRecommended(row, match)
			bufferMu.Lock()
			buffer = append(buffer, item)
			shouldFlush := len(buffer) >= matchEventBatch
			bufferMu.Unlock()
			if shouldFlush {
				flush(false)
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-stopped
	flush(true)
}

// matchSlow tries the selected sources one at a time within one candidate
// budget. The first source that yields a playable row wins; a failed source
// moves to the next, up to two.
func (r *Recommender) matchSlow(
	track RecommendedTrack, sources []string,
) (*domain.CatalogSearchResult, error) {
	artist := firstArtist(track.Artists)
	if r.matcher == nil || artist == "" || strings.TrimSpace(track.Title) == "" {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), matchCandidateTimeout)
	defer cancel()

	var lastErr error
	for index, source := range sources {
		if index >= 2 {
			break
		}
		rows, err := r.matcher.MatchTrack(ctx, track.Title, artist, track.Duration, []string{source})
		best := chooseMatch(ctx, r.streams, rows)
		if best != nil {
			return best, nil
		}
		if err != nil {
			lastErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

func (r *Recommender) emitMatchUpdate(username string, items []RecommendedTrack, done bool) {
	if r.emitter == nil {
		return
	}
	if items == nil {
		items = []RecommendedTrack{}
	}
	r.emitter(recommendationsMatchedEvent, RecommendationMatchUpdate{
		Username: username,
		Items:    items,
		Done:     done,
	})
}

// emitToWails is the production emitter. A missing application (unit tests)
// simply drops the event.
func emitToWails(name string, payload any) {
	app := application.Get()
	if app == nil || app.Event == nil {
		return
	}
	app.Event.Emit(name, payload)
}

func firstArtist(artists []string) string {
	if len(artists) > 0 {
		return strings.TrimSpace(artists[0])
	}
	return ""
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
	artistKey := domain.ArtistSetKey(row.Artists)
	if titleKey == "" || artistKey == "" {
		return false
	}
	entry, err := streams.MatchByTitleArtist(ctx, titleKey, artistKey, int64(row.Duration)*1000, 5000)
	return err == nil && entry != nil
}

// renderCandidates renders the Last.fm-side rows. No provider is known yet;
// the background matcher fills it in through events.
func renderCandidates(
	list []*candidate, limit int, score func(*candidate) float64,
) []RecommendedTrack {
	result := make([]RecommendedTrack, 0, len(list))
	for _, entry := range list {
		if len(result) >= limit {
			break
		}
		result = append(result, rawRecommended(entry, score(entry)))
	}
	return result
}

func rawRecommended(entry *candidate, score float64) RecommendedTrack {
	return RecommendedTrack{
		Title:     entry.track.Title,
		Artists:   []string{entry.track.Artist},
		AlbumName: entry.track.Album,
		Duration:  entry.track.Duration,
		CoverURL:  entry.track.ImageURL,
		Score:     score,
		Source:    entry.source(),
		MatchKey:  candidateKey(entry.track.Artist, entry.track.Title),
		Matching:  true,
	}
}

// matchedRecommended copies the catalogue identity onto the Last.fm row while
// preserving the recommendation-only fields (score, source, match key).
//
// Cover priority: a real Last.fm image wins, then the matched source's cover,
// then whatever the cover cache has for the identity. Nothing found leaves the
// field empty, and the page renders its placeholder.
func (r *Recommender) matchedRecommended(base RecommendedTrack, row *domain.CatalogSearchResult) RecommendedTrack {
	cover := strings.TrimSpace(base.CoverURL)
	large := strings.TrimSpace(base.CoverURLLarge)
	if cover == "" {
		// The source's display cover is its large image when it has one.
		cover = strings.TrimSpace(row.CoverURLLarge)
		large = cover
		if cover == "" {
			cover = strings.TrimSpace(row.CoverURL)
		}
		if cover == "" {
			cover = r.coverForIdentity(row.Provider, row.ExternalID)
		}
	}
	return RecommendedTrack{
		Provider:              row.Provider,
		ExternalID:            row.ExternalID,
		Title:                 row.Title,
		Artists:               append([]string{}, row.Artists...),
		AlbumName:             row.AlbumName,
		Duration:              row.Duration,
		CoverURL:              cover,
		CoverURLLarge:         large,
		Downloadable:          row.Downloadable,
		DownloadBlockedReason: row.DownloadBlockedReason,
		State:                 row.State,
		LocalTrackID:          row.LocalTrackID,
		Sources:               append([]string{}, row.Sources...),
		Score:                 base.Score,
		Source:                base.Source,
		MatchKey:              base.MatchKey,
		Matching:              false,
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

func (r *Recommender) cachedMatch(key string) (*domain.CatalogSearchResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.matches[key]
	if !ok {
		return nil, false
	}
	if r.now().After(entry.expiresAt) {
		delete(r.matches, key)
		return nil, false
	}
	return entry.row, true
}

func (r *Recommender) storeMatch(key string, row *domain.CatalogSearchResult) {
	if key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var copyRow *domain.CatalogSearchResult
	if row != nil {
		value := *row
		copyRow = &value
	}
	r.matches[key] = cachedMatch{row: copyRow, expiresAt: r.now().Add(matchCacheTTL)}
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
