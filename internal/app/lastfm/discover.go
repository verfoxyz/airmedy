package lastfm

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"airmedy/internal/domain"
)

const (
	// discoverTTL is how long a built discover list is reused before the page
	// checks whether it should be rebuilt.
	discoverTTL = 12 * time.Hour
	// discoverRequestBudget bounds one expansion. Seeds, recent scrobbles and
	// every fallback share it.
	discoverRequestBudget = 30
	discoverSeedArtists   = 10
	discoverSeedTracks    = 10
	discoverSimilarLimit  = 10
	discoverArtistLimit   = 5
	discoverArtistTracks  = 5
	discoverTagLimit      = 5
	discoverTagTracks     = 10
	discoverChartLimit    = 20
	// discoverRecentPages / discoverRecentPageSize bound the "heard recently"
	// exclusion set: at most 500 scrobbles from the last month.
	discoverRecentPages    = 3
	discoverRecentPageSize = 200
	// discoverInitialTake is the first page; every "load more" adds another.
	discoverInitialTake = 20
	discoverPageSize    = 20
	// discoverTake caps the list, so a rebuild cannot grow without bound.
	discoverTake = 100
	// discoverPerArtist bounds one artist's share of the list.
	discoverPerArtist = 2
)

// discoverSeedSet is the weekly-rotating input of one expansion.
type discoverSeedSet struct {
	tracks  []LastFmTrack
	artists []LastFmArtist
}

// cachedDiscoverSeeds keeps one ISO week's seeds stable across builds.
type cachedDiscoverSeeds struct {
	username string
	week     string
	seeds    discoverSeedSet
}

// discoverBudget counts the Last.fm requests one expansion may spend.
type discoverBudget struct {
	remaining int
}

func (b *discoverBudget) take() bool {
	if b.remaining <= 0 {
		return false
	}
	b.remaining--
	return true
}

// discoverCandidate is one proposed track before the novelty filter.
type discoverCandidate struct {
	track LastFmTrack
	score float64
}

// discoverSeeds returns this week's seeds. The artist window alternates between
// all-time and six-month so consecutive weeks do not repeat the same input.
func (r *Recommender) discoverSeeds(
	ctx context.Context, topTracks []LastFmTrack, budget *discoverBudget,
) discoverSeedSet {
	_, username, _ := r.client.GetStatus()
	year, week := r.now().UTC().ISOWeek()
	weekKey := fmt.Sprintf("%d-%02d", year, week)

	r.mu.Lock()
	cached := r.discoverCache
	r.mu.Unlock()
	if cached.username == username && cached.week == weekKey &&
		(len(cached.seeds.tracks) > 0 || len(cached.seeds.artists) > 0) {
		return cached.seeds
	}

	period := "overall"
	if week%2 == 1 {
		period = "6month"
	}

	var artists []LastFmArtist
	if budget.take() {
		items, err := r.client.GetUserTopArtists(ctx, period, discoverSeedArtists)
		if err != nil {
			r.logger.Debug("last.fm discover: top artists failed", "error", err)
		} else {
			artists = items
		}
	}

	tracks := usableSeeds(topTracks, discoverSeedTracks)
	if len(tracks) == 0 && budget.take() {
		items, err := r.client.GetTopTracks(ctx, topTrackPeriod, discoverSeedTracks)
		if err != nil {
			r.logger.Debug("last.fm discover: top tracks failed", "error", err)
		} else {
			tracks = usableSeeds(items, discoverSeedTracks)
		}
	}

	seeds := discoverSeedSet{
		tracks:  dedupeTracks(tracks, discoverSeedTracks),
		artists: dedupeArtists(artists, discoverSeedArtists),
	}
	r.mu.Lock()
	r.discoverCache = cachedDiscoverSeeds{username: username, week: weekKey, seeds: seeds}
	r.mu.Unlock()
	return seeds
}

func dedupeArtists(artists []LastFmArtist, limit int) []LastFmArtist {
	seen := make(map[string]struct{}, len(artists))
	result := make([]LastFmArtist, 0, len(artists))
	for _, artist := range artists {
		name := strings.TrimSpace(artist.Name)
		if name == "" {
			continue
		}
		key := domain.NormalizationKey(name)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		artist.Name = name
		result = append(result, artist)
		if len(result) >= limit {
			break
		}
	}
	return result
}

// expandDiscover walks the expansion priority and returns ranked candidates.
//
// Every request is drawn from one budget; a failed request is skipped. The
// ranking keeps the order stable: weighted source hits first, then Last.fm
// popularity, then artist and title.
func (r *Recommender) expandDiscover(
	ctx context.Context, seeds discoverSeedSet, budget *discoverBudget,
) []discoverCandidate {
	candidates := make(map[string]*discoverCandidate)
	add := func(track LastFmTrack, weight float64) {
		if !usableTrack(track) {
			return
		}
		key := candidateKey(track.Artist, track.Title)
		entry := candidates[key]
		if entry == nil {
			entry = &discoverCandidate{track: track}
			candidates[key] = entry
		}
		entry.score += weight
		if track.PlayCount > entry.track.PlayCount {
			entry.track = track
		}
	}

	for _, seed := range seeds.tracks {
		if !budget.take() {
			break
		}
		tracks, err := r.client.GetSimilarTracks(ctx, seed.Artist, seed.Title, discoverSimilarLimit)
		if err != nil {
			r.logger.Debug("last.fm discover: similar tracks failed",
				"artist", seed.Artist, "title", seed.Title, "error", err)
			continue
		}
		for _, track := range tracks {
			add(track, 1)
		}
	}

	for _, seed := range seeds.artists {
		if !budget.take() {
			break
		}
		neighbours, err := r.client.GetSimilarArtists(ctx, seed.Name, discoverArtistLimit)
		if err != nil {
			r.logger.Debug("last.fm discover: similar artists failed",
				"artist", seed.Name, "error", err)
			continue
		}
		for _, neighbour := range neighbours {
			if !budget.take() {
				break
			}
			tracks, err := r.client.GetArtistTopTracks(ctx, neighbour.Name, discoverArtistTracks)
			if err != nil {
				continue
			}
			for _, track := range tracks {
				add(track, 0.5)
			}
		}
	}

	for _, seed := range seeds.artists {
		if len(seed.Name) == 0 {
			continue
		}
		if !budget.take() {
			break
		}
		tags, err := r.client.GetArtistTopTags(ctx, seed.Name, discoverTagLimit)
		if err != nil {
			continue
		}
		for _, tag := range tags {
			if !budget.take() {
				break
			}
			tracks, err := r.client.GetTagTopTracks(ctx, tag.Name, discoverTagTracks)
			if err != nil {
				continue
			}
			for _, track := range tracks {
				add(track, 0.3)
			}
		}
	}

	if len(candidates) < discoverInitialTake && budget.take() {
		tracks, err := r.client.GetChartTopTracks(ctx, discoverChartLimit)
		if err != nil {
			r.logger.Debug("last.fm discover: chart failed", "error", err)
		}
		for _, track := range tracks {
			add(track, 0.2)
		}
	}

	ranked := make([]discoverCandidate, 0, len(candidates))
	for _, entry := range candidates {
		entry.score += math.Log1p(float64(entry.track.PlayCount)) / 10
		ranked = append(ranked, *entry)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		left := strings.ToLower(ranked[i].track.Artist)
		right := strings.ToLower(ranked[j].track.Artist)
		if left != right {
			return left < right
		}
		return strings.ToLower(ranked[i].track.Title) < strings.ToLower(ranked[j].track.Title)
	})
	return ranked
}

// recentScrobbleKeys builds the "heard in the last 30 days" exclusion set.
// A failed page only removes its own rows; the answer is empty when the first
// page fails.
func (r *Recommender) recentScrobbleKeys(ctx context.Context, budget *discoverBudget) map[string]struct{} {
	keys := make(map[string]struct{})
	for page := 1; page <= discoverRecentPages; page++ {
		if !budget.take() {
			break
		}
		tracks, err := r.client.GetRecentTracksPage(ctx, page, discoverRecentPageSize)
		if err != nil {
			r.logger.Debug("last.fm discover: recent tracks failed", "page", page, "error", err)
			break
		}
		if len(tracks) == 0 {
			break
		}
		for _, track := range tracks {
			if !usableTrack(track) {
				continue
			}
			keys[candidateKey(track.Artist, track.Title)] = struct{}{}
		}
	}
	return keys
}

// discoverItems turns the ranked candidates into list rows, applying novelty
// rules until the page (or the whole list) is full. initial carries the
// per-artist counts of an existing list so "load more" keeps the limit global.
func (r *Recommender) discoverItems(
	ctx context.Context,
	candidates []discoverCandidate,
	seeds discoverSeedSet,
	recent map[string]struct{},
	excluded map[string]struct{},
	initial map[string]int,
	limit int,
) []TopTrackItem {
	seedKeys := make(map[string]struct{}, len(seeds.tracks))
	for _, seed := range seeds.tracks {
		seedKeys[candidateKey(seed.Artist, seed.Title)] = struct{}{}
	}

	perArtist := make(map[string]int, len(initial))
	for key, count := range initial {
		perArtist[key] = count
	}
	items := make([]TopTrackItem, 0, limit)
	for _, entry := range candidates {
		if len(items) >= limit {
			break
		}
		track := entry.track
		key := candidateKey(track.Artist, track.Title)
		if _, isSeed := seedKeys[key]; isSeed {
			continue
		}
		if _, heard := recent[key]; heard {
			continue
		}
		if _, skip := excluded[key]; skip {
			continue
		}
		artistKey := domain.NormalizationKey(track.Artist)
		if perArtist[artistKey] >= discoverPerArtist {
			continue
		}
		if r.localTrackExists(ctx, track) {
			continue
		}
		perArtist[artistKey]++
		items = append(items, TopTrackItem{
			Title:    track.Title,
			Artist:   track.Artist,
			Album:    track.Album,
			Duration: track.Duration,
			Artists:  []string{track.Artist},
			CoverURL: track.ImageURL,
			Kind:     domain.LastFmRecommendationDiscover,
			Matching: true,
			MatchKey: key,
		})
	}
	return items
}

// localTrackExists reports whether the library already owns the song. It never
// touches the network.
func (r *Recommender) localTrackExists(ctx context.Context, track LastFmTrack) bool {
	if r.matcher == nil {
		return false
	}
	callCtx, cancel := context.WithTimeout(ctx, matchCandidateTimeout)
	defer cancel()
	local, err := r.matcher.MatchLocalTrack(
		callCtx, track.Title, []string{track.Artist}, track.Duration)
	if err != nil || local == nil || local.Track == nil {
		return false
	}
	return !local.Track.IsRemote()
}

// buildDiscoverList builds the first page and stores the remaining candidates.
func (r *Recommender) buildDiscoverList(ctx context.Context, topTracks []LastFmTrack) error {
	budget := &discoverBudget{remaining: discoverRequestBudget}
	seeds := r.discoverSeeds(ctx, topTracks, budget)
	// The exclusion set runs first: a full expansion must not starve it.
	recent := r.recentScrobbleKeys(ctx, budget)
	candidates := r.expandDiscover(ctx, seeds, budget)

	// Filter the whole list once, then split it: the per-artist limit has to hold
	// across the first page and every later page.
	ranked := r.discoverItems(
		ctx, candidates, seeds, recent, map[string]struct{}{}, nil, discoverTake)
	firstPage := discoverPageSize
	if firstPage > len(ranked) {
		firstPage = len(ranked)
	}
	items := ranked[:firstPage]
	pool := ranked[firstPage:]

	if err := r.saveRecommendationList(ctx, domain.LastFmRecommendationDiscover, items); err != nil {
		return err
	}
	if err := r.saveDiscoverCandidates(ctx, pool); err != nil {
		return err
	}
	r.startKindMatching(ctx, domain.LastFmRecommendationDiscover, items)
	return nil
}

// refreshDiscover rebuilds the list and its pool.
func (r *Recommender) refreshDiscover(ctx context.Context) error {
	tracks, err := r.client.GetTopTracks(ctx, topTrackPeriod, topTrackLimit)
	if err != nil {
		return err
	}
	return r.buildDiscoverList(ctx, tracks)
}

// discoverList returns the stored list with its HasMore flag.
func (r *Recommender) discoverList(ctx context.Context) (RecommendationList, error) {
	result := RecommendationList{
		Kind:  domain.LastFmRecommendationDiscover,
		Title: recommendationTitle(domain.LastFmRecommendationDiscover),
		Items: []TopTrackItem{},
	}
	store := r.recommendationStoreOrNil()
	if store == nil {
		return result, nil
	}
	list, err := store.GetRecommendationList(ctx, domain.LastFmRecommendationDiscover)
	if err != nil {
		return result, err
	}
	if list == nil {
		if buildErr := r.refreshDiscover(ctx); buildErr != nil {
			return result, buildErr
		}
		list, err = store.GetRecommendationList(ctx, domain.LastFmRecommendationDiscover)
		if err != nil {
			return result, err
		}
		if list == nil {
			return result, nil
		}
	}
	pool, err := store.GetRecommendationCandidates(ctx, domain.LastFmRecommendationDiscover)
	if err != nil {
		return result, err
	}
	loaded := recommendationListFromDomain(*list)
	loaded.HasMore = len(pool) > 0
	r.resumeRecommendationMatching(ctx, domain.LastFmRecommendationDiscover)
	return loaded, nil
}

// LoadMoreRecommendationItems appends the next page of discover rows.
//
// The stored candidate pool is consumed first; once it is empty one fresh
// expansion looks for rows the list does not already show.
func (r *Recommender) LoadMoreRecommendationItems(
	ctx context.Context, kind string,
) (RecommendationList, error) {
	if recommendationBaseKind(kind) != domain.LastFmRecommendationDiscover {
		return RecommendationList{}, fmt.Errorf("lastfm recommendations: unknown list %q", kind)
	}
	connected, username, _ := r.client.GetStatus()
	if !connected || strings.TrimSpace(username) == "" {
		return RecommendationList{}, nil
	}
	store := r.recommendationStoreOrNil()
	if store == nil {
		return RecommendationList{}, nil
	}

	list, err := store.GetRecommendationList(ctx, domain.LastFmRecommendationDiscover)
	if err != nil {
		return RecommendationList{}, err
	}
	if list == nil {
		if err := r.refreshDiscover(ctx); err != nil {
			return RecommendationList{}, err
		}
		list, err = store.GetRecommendationList(ctx, domain.LastFmRecommendationDiscover)
		if err != nil || list == nil {
			return RecommendationList{}, err
		}
	}

	pool, err := store.GetRecommendationCandidates(ctx, domain.LastFmRecommendationDiscover)
	if err != nil {
		return RecommendationList{}, err
	}
	if len(pool) == 0 {
		pool, err = r.expandMoreDiscover(ctx, list.Items)
		if err != nil {
			return RecommendationList{}, err
		}
	}

	room := discoverTake - len(list.Items)
	if room < 0 {
		room = 0
	}
	take := discoverPageSize
	if take > len(pool) {
		take = len(pool)
	}
	if take > room {
		take = room
	}

	appended := make([]TopTrackItem, 0, take)
	items := recommendationItemsFromDomain(list.Items)
	for _, row := range pool[:take] {
		item := recommendationItemFromDomain(row)
		items = append(items, item)
		appended = append(appended, item)
	}
	remaining := pool[take:]

	if len(appended) > 0 {
		if err := r.saveRecommendationList(ctx, domain.LastFmRecommendationDiscover, items); err != nil {
			return RecommendationList{}, err
		}
		r.startKindMatching(ctx, domain.LastFmRecommendationDiscover, appended)
		r.emitRecommendationItemsAdded(domain.LastFmRecommendationDiscover, appended)
	}
	if err := store.SaveRecommendationCandidates(
		ctx, domain.LastFmRecommendationDiscover, remaining); err != nil {
		return RecommendationList{}, err
	}

	result := recommendationListFromDomain(*list)
	result.Kind = domain.LastFmRecommendationDiscover
	result.Title = recommendationTitle(domain.LastFmRecommendationDiscover)
	result.Items = items
	result.UpdatedAt = r.now().UTC()
	result.HasMore = len(remaining) > 0
	return result, nil
}

// expandMoreDiscover asks Last.fm for rows the current list does not show.
func (r *Recommender) expandMoreDiscover(
	ctx context.Context, existing []domain.RecommendationItem,
) ([]domain.RecommendationItem, error) {
	budget := &discoverBudget{remaining: discoverRequestBudget}
	seeds := r.discoverSeeds(ctx, nil, budget)
	recent := r.recentScrobbleKeys(ctx, budget)
	candidates := r.expandDiscover(ctx, seeds, budget)

	excluded := make(map[string]struct{}, len(existing))
	artistCounts := make(map[string]int, len(existing))
	for _, row := range existing {
		excluded[row.MatchKey] = struct{}{}
		artistCounts[domain.NormalizationKey(row.Artist)]++
	}
	room := discoverTake - len(existing)
	if room <= 0 {
		return []domain.RecommendationItem{}, nil
	}
	items := r.discoverItems(ctx, candidates, seeds, recent, excluded, artistCounts, room)
	return toDomainRecommendationItems(items), nil
}

func (r *Recommender) saveDiscoverCandidates(ctx context.Context, items []TopTrackItem) error {
	store := r.recommendationStoreOrNil()
	if store == nil {
		return nil
	}
	return store.SaveRecommendationCandidates(
		ctx, domain.LastFmRecommendationDiscover, toDomainRecommendationItems(items))
}

// recommendationItemsFromDomain converts rows for a rewritten list.
func recommendationItemsFromDomain(rows []domain.RecommendationItem) []TopTrackItem {
	items := make([]TopTrackItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, recommendationItemFromDomain(row))
	}
	return items
}
