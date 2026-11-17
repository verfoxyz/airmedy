package lastfm

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"airmedy/internal/domain"
)

// Seed weights. Local signals outrank Last.fm because an imported library
// often has no scrobbles at all; the Last.fm rows keep the taste map from
// growing stale when the local library is small.
const (
	seedWeightLocalListened = 1.0
	seedWeightLocalPlayed   = 0.9
	seedWeightLocalPlaylist = 0.7
	seedWeightLastFmTracks  = 0.6
	seedWeightLastFmArtists = 0.5
	// seedWeightColdStart ranks the chart/tag fallback, which exists only to
	// keep the page useful when nothing personal is known.
	seedWeightColdStart = 0.2
	// seedTrackLimit / seedArtistLimit are the rendered seed counts.
	seedTrackLimit  = 10
	seedArtistLimit = 10
	// seedLastFmShare is the maximum share of Last.fm-only seeds once local
	// history exists (40%).
	seedLastFmShare = 0.4
	// seedHistoryDays is the local listening window.
	seedHistoryDays = 90
	// seedLocalFetchLimit is how many rows each local source contributes.
	seedLocalFetchLimit = 50
)

// seedTrack is one ranked track seed before it becomes a Last.fm query.
type seedTrack struct {
	track   LastFmTrack
	weight  float64
	local   bool
	trackID string
	album   string
}

// seedArtist is one ranked artist seed.
type seedArtist struct {
	name   string
	weight float64
	local  bool
}

// recommendationSeedSet is the merged weekly seed pool.
type recommendationSeedSet struct {
	tracks  []seedTrack
	artists []seedArtist
	// general marks the chart/tag fallback used when neither a local history
	// nor a connected Last.fm account exists.
	general bool
}

// weeklyRecommendationSeedCache keeps one ISO week's seeds plus the previous
// week's set for the half-and-half rotation.
type weeklyRecommendationSeedCache struct {
	username string
	week     string
	current  recommendationSeedSet
	previous recommendationSeedSet
}

// SetSeedRepository installs the local listening-signal source. Optional:
// without it the seeds are Last.fm-only.
func (r *Recommender) SetSeedRepository(repo domain.RecommendationSeedRepository) {
	r.mu.Lock()
	r.seedRepo = repo
	r.mu.Unlock()
}

func (r *Recommender) seedRepositoryOrNil() domain.RecommendationSeedRepository {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seedRepo
}

// weeklyRecommendationSeeds returns the stable seed set for the current ISO
// week. A new week builds fresh seeds and keeps half of the previous week, so
// recommendations drift instead of jumping.
func (r *Recommender) weeklyRecommendationSeeds(
	ctx context.Context, topTracks []LastFmTrack,
) recommendationSeedSet {
	_, username, _ := r.client.GetStatus()
	year, week := r.now().UTC().ISOWeek()
	weekKey := fmt.Sprintf("%d-%02d", year, week)

	r.mu.Lock()
	cached := r.seedCache
	r.mu.Unlock()
	if cached.username == username && cached.week == weekKey &&
		len(cached.current.tracks) > 0 {
		return cached.current
	}

	fresh := r.buildRecommendationSeeds(ctx, topTracks)
	next := fresh
	if cached.username == username && len(cached.current.tracks) > 0 {
		next = rotateRecommendationSeeds(cached.current, fresh)
	}

	r.mu.Lock()
	r.seedCache = weeklyRecommendationSeedCache{
		username: username,
		week:     weekKey,
		current:  next,
		previous: cached.current,
	}
	r.mu.Unlock()
	return next
}

// buildRecommendationSeeds merges the local and Last.fm signals.
func (r *Recommender) buildRecommendationSeeds(
	ctx context.Context, topTracks []LastFmTrack,
) recommendationSeedSet {
	dislikes := r.currentDislikes(ctx)
	tracks := make(map[string]*seedTrack)
	addTrack := func(track LastFmTrack, weight float64, local bool, trackID, album string) {
		if !usableTrack(track) {
			return
		}
		key := candidateKey(track.Artist, track.Title)
		if dislikes.blocksTrack(key, track.Artist) {
			return
		}
		entry := tracks[key]
		if entry == nil {
			entry = &seedTrack{track: track}
			tracks[key] = entry
		}
		entry.weight += weight
		if local {
			entry.local = true
			if entry.trackID == "" {
				entry.trackID = trackID
			}
			if entry.album == "" {
				entry.album = album
			}
		}
	}

	repo := r.seedRepositoryOrNil()
	if repo != nil {
		since := r.now().UTC().AddDate(0, 0, -seedHistoryDays)
		if rows, err := repo.MostListenedTracks(ctx, since, seedLocalFetchLimit); err == nil {
			for _, row := range rows {
				addTrack(localSeedTrack(row), seedWeightLocalListened, true, row.TrackID, row.AlbumName)
			}
		}
		if rows, err := repo.MostPlayedTracks(ctx, seedLocalFetchLimit); err == nil {
			for _, row := range rows {
				addTrack(localSeedTrack(row), seedWeightLocalPlayed, true, row.TrackID, row.AlbumName)
			}
		}
		if rows, err := repo.PlaylistTracks(ctx, seedLocalFetchLimit); err == nil {
			for _, row := range rows {
				addTrack(localSeedTrack(row), seedWeightLocalPlaylist, true, row.TrackID, row.AlbumName)
			}
		}
	}

	lastFmTracks := usableSeeds(topTracks, seedLocalFetchLimit)
	if len(lastFmTracks) == 0 {
		if rows, err := r.client.GetTopTracks(ctx, topTrackPeriod, seedLocalFetchLimit); err == nil {
			lastFmTracks = usableSeeds(rows, seedLocalFetchLimit)
		}
	}
	for _, track := range lastFmTracks {
		addTrack(track, seedWeightLastFmTracks, false, "", track.Album)
	}

	artists := make(map[string]*seedArtist)
	addArtist := func(name string, weight float64, local bool) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if dislikes.blocksArtist(name) {
			return
		}
		key := domain.ArtistSetKey([]string{name})
		if key == "" {
			return
		}
		entry := artists[key]
		if entry == nil {
			entry = &seedArtist{name: name}
			artists[key] = entry
		}
		entry.weight += weight
		entry.local = entry.local || local
	}
	if rows, err := r.client.GetUserTopArtists(ctx, "6month", seedArtistLimit*2); err == nil {
		for _, artist := range rows {
			addArtist(artist.Name, seedWeightLastFmArtists, false)
		}
	}
	for _, entry := range tracks {
		if entry.local && len(entry.track.Artist) > 0 {
			addArtist(entry.track.Artist, seedWeightLocalListened, true)
		}
	}

	set := recommendationSeedSet{
		tracks:  rankSeedTracks(tracks, seedTrackLimit),
		artists: rankSeedArtists(artists, seedArtistLimit),
	}
	if len(set.tracks) == 0 {
		set = r.coldStartSeeds(ctx)
	}
	return set
}

// coldStartSeeds is the last resort: the site-wide chart plus the library's
// most frequent genres, so a brand-new library still gets "general"
// recommendations instead of an empty page.
func (r *Recommender) coldStartSeeds(ctx context.Context) recommendationSeedSet {
	dislikes := r.currentDislikes(ctx)
	tracks := make(map[string]*seedTrack)
	add := func(track LastFmTrack) {
		if !usableTrack(track) {
			return
		}
		key := candidateKey(track.Artist, track.Title)
		if dislikes.blocksTrack(key, track.Artist) {
			return
		}
		entry := tracks[key]
		if entry == nil {
			entry = &seedTrack{track: track}
			tracks[key] = entry
		}
		entry.weight += seedWeightColdStart
	}

	if repo := r.seedRepositoryOrNil(); repo != nil {
		if genres, err := repo.TopGenres(ctx, 3); err == nil {
			for _, genre := range genres {
				if genre = strings.TrimSpace(genre); genre == "" {
					continue
				}
				if rows, err := r.client.GetTagTopTracks(ctx, genre, 10); err == nil {
					for _, track := range rows {
						add(track)
					}
				}
			}
		}
	}
	if len(tracks) < seedTrackLimit {
		if rows, err := r.client.GetChartTopTracks(ctx, 20); err == nil {
			for _, track := range rows {
				add(track)
			}
		}
	}
	return recommendationSeedSet{
		tracks:  rankSeedTracks(tracks, seedTrackLimit),
		general: true,
	}
}

// localSeedTrack adapts a local row to the Last.fm query shape.
func localSeedTrack(row domain.RecommendationSeedTrack) LastFmTrack {
	artist := ""
	if len(row.Artists) > 0 {
		artist = strings.TrimSpace(row.Artists[0])
	}
	return LastFmTrack{
		Artist: artist,
		Title:  row.Title,
		Album:  row.AlbumName,
	}
}

// rankSeedTracks orders by weight, then enforces the local share once local
// history exists.
func rankSeedTracks(candidates map[string]*seedTrack, limit int) []seedTrack {
	list := make([]seedTrack, 0, len(candidates))
	localPresent := false
	for _, entry := range candidates {
		if entry.local {
			localPresent = true
		}
		list = append(list, *entry)
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].weight != list[j].weight {
			return list[i].weight > list[j].weight
		}
		if list[i].track.Title != list[j].track.Title {
			return strings.ToLower(list[i].track.Title) < strings.ToLower(list[j].track.Title)
		}
		return strings.ToLower(list[i].track.Artist) < strings.ToLower(list[j].track.Artist)
	})
	if !localPresent {
		if len(list) > limit {
			list = list[:limit]
		}
		return list
	}
	maxLastFm := int(float64(limit) * seedLastFmShare)
	selected := make([]seedTrack, 0, limit)
	lastFmCount := 0
	for _, entry := range list {
		if len(selected) >= limit {
			break
		}
		if !entry.local {
			if lastFmCount >= maxLastFm {
				continue
			}
			lastFmCount++
		}
		selected = append(selected, entry)
	}
	return selected
}

func rankSeedArtists(candidates map[string]*seedArtist, limit int) []seedArtist {
	list := make([]seedArtist, 0, len(candidates))
	localPresent := false
	for _, entry := range candidates {
		if entry.local {
			localPresent = true
		}
		list = append(list, *entry)
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].weight != list[j].weight {
			return list[i].weight > list[j].weight
		}
		return strings.ToLower(list[i].name) < strings.ToLower(list[j].name)
	})
	if !localPresent {
		if len(list) > limit {
			list = list[:limit]
		}
		return list
	}
	maxLastFm := int(float64(limit) * seedLastFmShare)
	selected := make([]seedArtist, 0, limit)
	lastFmCount := 0
	for _, entry := range list {
		if len(selected) >= limit {
			break
		}
		if !entry.local {
			if lastFmCount >= maxLastFm {
				continue
			}
			lastFmCount++
		}
		selected = append(selected, entry)
	}
	return selected
}

// rotateRecommendationSeeds keeps half of the previous week and fills the rest
// from the fresh set.
func rotateRecommendationSeeds(
	previous, fresh recommendationSeedSet,
) recommendationSeedSet {
	half := seedTrackLimit / 2
	result := recommendationSeedSet{
		tracks:  make([]seedTrack, 0, seedTrackLimit),
		artists: make([]seedArtist, 0, seedArtistLimit),
		general: fresh.general,
	}
	seenTracks := make(map[string]struct{})
	seenArtists := make(map[string]struct{})

	takeOldTracks := min(half, len(previous.tracks))
	for _, entry := range previous.tracks[:takeOldTracks] {
		key := candidateKey(entry.track.Artist, entry.track.Title)
		if _, duplicate := seenTracks[key]; duplicate {
			continue
		}
		seenTracks[key] = struct{}{}
		result.tracks = append(result.tracks, entry)
	}
	for _, entry := range fresh.tracks {
		if len(result.tracks) >= seedTrackLimit {
			break
		}
		key := candidateKey(entry.track.Artist, entry.track.Title)
		if _, duplicate := seenTracks[key]; duplicate {
			continue
		}
		seenTracks[key] = struct{}{}
		result.tracks = append(result.tracks, entry)
	}
	for _, entry := range previous.tracks[takeOldTracks:] {
		if len(result.tracks) >= seedTrackLimit {
			break
		}
		key := candidateKey(entry.track.Artist, entry.track.Title)
		if _, duplicate := seenTracks[key]; duplicate {
			continue
		}
		seenTracks[key] = struct{}{}
		result.tracks = append(result.tracks, entry)
	}

	takeOldArtists := min(half, len(previous.artists))
	for _, entry := range previous.artists[:takeOldArtists] {
		key := domain.ArtistSetKey([]string{entry.name})
		if _, duplicate := seenArtists[key]; duplicate {
			continue
		}
		seenArtists[key] = struct{}{}
		result.artists = append(result.artists, entry)
	}
	for _, entry := range fresh.artists {
		if len(result.artists) >= seedArtistLimit {
			break
		}
		key := domain.ArtistSetKey([]string{entry.name})
		if _, duplicate := seenArtists[key]; duplicate {
			continue
		}
		seenArtists[key] = struct{}{}
		result.artists = append(result.artists, entry)
	}
	return result
}

// similarListSeeds is the "for you" seed list: the merged weekly seeds, with
// the raw top tracks as the fallback when no seed could be built.
func (r *Recommender) similarListSeeds(ctx context.Context, topTracks []LastFmTrack) []LastFmTrack {
	set := r.weeklyRecommendationSeeds(ctx, topTracks)
	seeds := seedSetTracks(set, similarSeedLimit)
	if len(seeds) == 0 {
		return usableSeeds(topTracks, similarSeedLimit)
	}
	return seeds
}

// seedSetTracks adapts a seed set to the Last.fm row shape the builders use.
func seedSetTracks(set recommendationSeedSet, limit int) []LastFmTrack {
	tracks := make([]LastFmTrack, 0, len(set.tracks))
	for _, entry := range set.tracks {
		tracks = append(tracks, entry.track)
		if len(tracks) >= limit {
			break
		}
	}
	return tracks
}

func seedSetArtists(set recommendationSeedSet) []LastFmArtist {
	artists := make([]LastFmArtist, 0, len(set.artists))
	for _, entry := range set.artists {
		artists = append(artists, LastFmArtist{Name: entry.name})
	}
	return artists
}
