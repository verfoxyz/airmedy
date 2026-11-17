package lastfm

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"airmedy/internal/app/catalog"
	"airmedy/internal/domain"
)

const (
	// radioInitialBatch is the first queue the radio builds.
	radioInitialBatch = 20
	// radioRefillBatch is how many tracks one automatic refill adds.
	radioRefillBatch = 10
	// radioRefillThreshold triggers a refill when fewer tracks remain.
	radioRefillThreshold = 5
	// radioRecentDays is the "already heard" window the radio skips.
	radioRecentDays = 30
	// radioCandidateWindow bounds the diversity pool before matching.
	radioCandidateWindow = 40
	// radioMatchConcurrency bounds simultaneous source searches.
	radioMatchConcurrency = 4
	// radioBatchTimeout bounds building one batch (Last.fm plus matching).
	radioBatchTimeout = 2 * time.Minute
)

// radioStateEvent carries the mode on/off state. The payload is a boolean, the
// same shape Mood Radio uses.
const radioStateEvent = "radio:state"

// radioQueueAppendedEvent is emitted after one automatic refill.
const radioQueueAppendedEvent = "radio:queue-appended"

// RadioQueue is the player surface the radio drives.
type RadioQueue interface {
	GetQueue() []*domain.TrackDTO
	GetCurrentTrack() *domain.TrackDTO
	AppendTracks(tracks []*domain.TrackDTO)
	AddQueueListener(f func([]*domain.TrackDTO))
	AddTrackLoadListener(f func(*domain.TrackDTO))
}

// StreamableResolver turns a matched source identity into a playable library
// row. *catalog.Service implements it.
type StreamableResolver interface {
	EnsureStreamableTrack(ctx context.Context, request catalog.ActionRequest) (*domain.TrackDTO, error)
}

// RadioSeedRequest names the starting point. TrackID wins when set; otherwise
// the Last.fm identity is used (a recommendation row that is not in the library
// yet).
type RadioSeedRequest struct {
	TrackID string `json:"track_id"`
	// Provider / ExternalID are set when the seed is an already-matched
	// recommendation row; the radio then skips its own source search.
	Provider   string `json:"provider"`
	ExternalID string `json:"external_id"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Album      string `json:"album"`
	Duration   int    `json:"duration"`
}

// RadioStartResult is the queue the frontend should play. Seed is the resolved
// starting row, Tracks the rest of the first batch.
type RadioStartResult struct {
	Seed   *domain.TrackDTO   `json:"seed"`
	Tracks []*domain.TrackDTO `json:"tracks"`
}

// RadioQueueAppended is the payload of radio:queue-appended.
type RadioQueueAppended struct {
	Count int `json:"count"`
}

type radioSession struct {
	active     bool
	generating bool
	scheduled  bool
	trackIDs   map[string]struct{}
	keys       map[string]struct{}
}

// Radio builds a Last.fm-seeded playback queue: track.getSimilar, then similar
// artists, then tags. It keeps one in-memory session and refills automatically
// while the user stays inside the generated queue.
type Radio struct {
	recommender *Recommender
	tracks      domain.TrackRepository
	listening   domain.RecommendationSeedRepository
	catalog     StreamableResolver
	queue       RadioQueue
	logger      *slog.Logger
	now         func() time.Time
	emit        func(name string, payload any)

	mu      sync.Mutex
	session radioSession
}

// NewRadio builds the Last.fm radio service.
func NewRadio(
	recommender *Recommender,
	tracks domain.TrackRepository,
	listening domain.RecommendationSeedRepository,
	catalogService StreamableResolver,
	queue RadioQueue,
	logger *slog.Logger,
) *Radio {
	if logger == nil {
		logger = slog.Default()
	}
	return &Radio{
		recommender: recommender,
		tracks:      tracks,
		listening:   listening,
		catalog:     catalogService,
		queue:       queue,
		logger:      logger,
		now:         time.Now,
		emit:        emitToWails,
		session: radioSession{
			trackIDs: make(map[string]struct{}),
			keys:     make(map[string]struct{}),
		},
	}
}

// WireListeners subscribes the radio to queue changes. One listener covers
// appends and track changes because both emit the queue.
func (r *Radio) WireListeners(queue RadioQueue) {
	queue.AddQueueListener(func([]*domain.TrackDTO) { r.ScheduleRefill() })
	queue.AddTrackLoadListener(func(*domain.TrackDTO) { r.ScheduleRefill() })
}

// GetRadioActive reports whether the session is on.
func (r *Radio) GetRadioActive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.session.active
}

// StartRadio builds the first batch from seed and marks the session active.
func (r *Radio) StartRadio(ctx context.Context, request RadioSeedRequest) (RadioStartResult, error) {
	connected, _, _ := r.recommender.client.GetStatus()
	if !connected {
		return RadioStartResult{}, fmt.Errorf("lastfm radio: %w", domain.ErrLastFmNotConnected)
	}

	seedTrack, seed, err := r.resolveSeed(ctx, request)
	if err != nil {
		return RadioStartResult{}, err
	}
	excludes := r.exclusionKeys(ctx, seed)

	// Build before activating: a queue event during the build (which can take a
	// while) must not see an active session whose queue is not playing yet.
	batch, err := r.buildBatchWithRetry(ctx, seed, excludes, radioInitialBatch)
	if err != nil && len(batch) == 0 {
		return RadioStartResult{}, err
	}

	r.mu.Lock()
	r.session = radioSession{
		active:   true,
		trackIDs: map[string]struct{}{seedTrack.ID: {}},
		keys:     map[string]struct{}{candidateKey(seed.Artist, seed.Title): {}},
	}
	for _, track := range batch {
		r.session.trackIDs[track.ID] = struct{}{}
		r.session.keys[candidateKey(firstArtist(trackArtists(track)), track.Title)] = struct{}{}
	}
	r.mu.Unlock()
	r.emitRadioState(true)
	if len(batch) > 0 {
		r.emit(radioQueueAppendedEvent, RadioQueueAppended{Count: len(batch)})
	}
	return RadioStartResult{Seed: seedTrack, Tracks: batch}, nil
}

// StopRadio ends the session; no further batches are built.
func (r *Radio) StopRadio() {
	r.mu.Lock()
	active := r.session.active
	r.session = radioSession{
		trackIDs: make(map[string]struct{}),
		keys:     make(map[string]struct{}),
	}
	r.mu.Unlock()
	if active {
		r.emitRadioState(false)
	}
}

// ScheduleRefill evaluates the queue without blocking the caller. Only one
// evaluation runs at a time; extra queue events collapse into it.
func (r *Radio) ScheduleRefill() {
	r.mu.Lock()
	if !r.session.active || r.session.scheduled {
		r.mu.Unlock()
		return
	}
	r.session.scheduled = true
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			r.session.scheduled = false
			r.mu.Unlock()
		}()
		r.refillIfNeeded(context.Background())
	}()
}

// refillIfNeeded appends one batch when fewer than radioRefillThreshold tracks
// remain after the current one.
func (r *Radio) refillIfNeeded(ctx context.Context) {
	r.mu.Lock()
	if !r.session.active || r.session.generating {
		r.mu.Unlock()
		return
	}
	current := r.queue.GetCurrentTrack()
	queue := r.queue.GetQueue()
	if current == nil {
		r.stopLocked()
		r.mu.Unlock()
		return
	}
	if _, member := r.session.trackIDs[current.ID]; !member {
		// Ordinary playback replaced the radio queue.
		r.stopLocked()
		r.mu.Unlock()
		return
	}
	remaining := len(queue) - 1 - indexOfTrack(queue, current.ID)
	if remaining < 0 || remaining >= radioRefillThreshold {
		r.mu.Unlock()
		return
	}
	r.session.generating = true
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.session.generating = false
		r.mu.Unlock()
	}()

	seed := trackSeed(current)
	excludes := r.exclusionKeys(ctx, seed)
	batch, _ := r.buildBatchWithRetry(ctx, seed, excludes, radioRefillBatch)
	if len(batch) == 0 {
		return
	}

	// Add to the session before appending: the queue listener fires during
	// AppendTracks and must already recognise these rows as radio members.
	r.mu.Lock()
	for _, track := range batch {
		r.session.trackIDs[track.ID] = struct{}{}
		r.session.keys[candidateKey(firstArtist(trackArtists(track)), track.Title)] = struct{}{}
	}
	r.mu.Unlock()
	r.queue.AppendTracks(batch)
	r.emit(radioQueueAppendedEvent, RadioQueueAppended{Count: len(batch)})
}

func (r *Radio) stopLocked() {
	if r.session.active {
		r.emitRadioState(false)
	}
	r.session = radioSession{
		trackIDs: make(map[string]struct{}),
		keys:     make(map[string]struct{}),
	}
}

func (r *Radio) emitRadioState(active bool) {
	r.emit(radioStateEvent, active)
}

// resolveSeed loads the library row when an ID is given and resolves a remote
// identity to a playable row otherwise.
func (r *Radio) resolveSeed(
	ctx context.Context, request RadioSeedRequest,
) (*domain.TrackDTO, LastFmTrack, error) {
	if trackID := strings.TrimSpace(request.TrackID); trackID != "" {
		track, err := r.tracks.GetByID(ctx, trackID)
		if err != nil {
			return nil, LastFmTrack{}, fmt.Errorf("lastfm radio: load seed: %w", err)
		}
		if track == nil {
			return nil, LastFmTrack{}, fmt.Errorf("lastfm radio: unknown seed %q", trackID)
		}
		return track, trackSeed(track), nil
	}
	identity := LastFmTrack{
		Artist:   strings.TrimSpace(request.Artist),
		Title:    strings.TrimSpace(request.Title),
		Album:    strings.TrimSpace(request.Album),
		Duration: request.Duration,
	}
	if !usableTrack(identity) {
		return nil, LastFmTrack{}, fmt.Errorf("lastfm radio: seed has no artist or title")
	}
	var resolved *domain.TrackDTO
	if provider := strings.TrimSpace(request.Provider); provider != "" &&
		strings.TrimSpace(request.ExternalID) != "" {
		resolved = r.ensureStreamable(ctx, domain.CatalogSearchResult{
			Provider: provider, ExternalID: strings.TrimSpace(request.ExternalID),
			Title: identity.Title, Artists: []string{identity.Artist},
			AlbumName: identity.Album, Duration: identity.Duration,
		})
	}
	if resolved == nil {
		resolved = r.resolveIdentity(ctx, identity)
	}
	if resolved == nil {
		return nil, LastFmTrack{}, fmt.Errorf("lastfm radio: seed has no playable source")
	}
	return resolved, identity, nil
}

// buildBatchWithRetry retries one failed batch once, then gives up so the
// existing queue stays usable.
func (r *Radio) buildBatchWithRetry(
	ctx context.Context, seed LastFmTrack, excludes map[string]struct{}, limit int,
) ([]*domain.TrackDTO, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		batch, err := r.buildBatch(ctx, seed, excludes, limit)
		if err == nil && len(batch) > 0 {
			return batch, nil
		}
		lastErr = err
		if len(batch) > 0 {
			return batch, nil
		}
	}
	return nil, lastErr
}

func (r *Radio) buildBatch(
	ctx context.Context, seed LastFmTrack, excludes map[string]struct{}, limit int,
) ([]*domain.TrackDTO, error) {
	ctx, cancel := context.WithTimeout(ctx, radioBatchTimeout)
	defer cancel()

	candidates := r.radioCandidates(ctx, seed, excludes, radioCandidateWindow)
	if len(candidates) == 0 {
		return nil, nil
	}
	// Pick more than one batch's worth so failed source matches can be skipped
	// without breaking the diversity order.
	selected := selectDiverseTracks(candidates, limit*2, nil)
	return r.resolveRadioTracks(ctx, selected, limit, excludes), nil
}

// radioCandidates walks the suggestion chain: direct neighbours, then the
// seed's similar artists, then the seed artist's tags.
func (r *Radio) radioCandidates(
	ctx context.Context, seed LastFmTrack, excludes map[string]struct{}, limit int,
) []LastFmTrack {
	result := make([]LastFmTrack, 0, limit)
	seen := make(map[string]struct{}, limit*2)
	dislikes := r.recommender.currentDislikes(ctx)
	add := func(track LastFmTrack) {
		if len(result) >= limit || !usableTrack(track) {
			return
		}
		key := candidateKey(track.Artist, track.Title)
		if dislikes.blocksTrack(key, track.Artist) {
			return
		}
		if key == seedKey(seed) {
			return
		}
		if _, skip := excludes[key]; skip {
			return
		}
		if _, duplicate := seen[key]; duplicate {
			return
		}
		seen[key] = struct{}{}
		result = append(result, track)
	}

	if tracks, err := r.recommender.client.GetSimilarTracks(
		ctx, seed.Artist, seed.Title, similarTrackLimit); err == nil {
		for _, track := range tracks {
			add(track)
		}
	}

	if len(result) == 0 {
		hits, _ := r.recommender.fallbackForSeed(ctx, seed)
		for _, hit := range hits {
			add(hit.track)
		}
	}

	if len(result) < limit {
		if tracks, err := r.recommender.client.GetArtistTopTracks(
			ctx, seed.Artist, similarTrackLimit); err == nil {
			for _, track := range tracks {
				add(track)
			}
		}
	}

	if len(result) < limit {
		tags, err := r.recommender.client.GetArtistTopTags(ctx, seed.Artist, 3)
		if err == nil {
			for _, tag := range tags {
				if len(result) >= limit {
					break
				}
				tracks, err := r.recommender.client.GetTagTopTracks(ctx, tag.Name, similarTrackLimit)
				if err != nil {
					continue
				}
				for _, track := range tracks {
					add(track)
				}
			}
		}
	}
	return result
}

// resolveRadioTracks matches candidates to playable sources in parallel, then
// returns them in the selected order.
func (r *Radio) resolveRadioTracks(
	ctx context.Context, candidates []LastFmTrack, limit int, excludes map[string]struct{},
) []*domain.TrackDTO {
	if limit <= 0 || len(candidates) == 0 {
		return nil
	}
	resolved := make([]*domain.TrackDTO, len(candidates))
	var wg sync.WaitGroup
	slots := make(chan struct{}, radioMatchConcurrency)
	for index, track := range candidates {
		index, track := index, track
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			if r.recommender.localTrackExists(ctx, track) {
				return
			}
			resolved[index] = r.resolveIdentity(ctx, track)
		}()
	}
	wg.Wait()

	batch := make([]*domain.TrackDTO, 0, limit)
	seen := make(map[string]struct{}, limit)
	for _, track := range resolved {
		if len(batch) >= limit {
			break
		}
		if track == nil {
			continue
		}
		if _, duplicate := seen[track.ID]; duplicate {
			continue
		}
		key := candidateKey(firstArtist(trackArtists(track)), track.Title)
		if _, skip := excludes[key]; skip {
			continue
		}
		seen[track.ID] = struct{}{}
		batch = append(batch, track)
	}
	return batch
}

// resolveIdentity searches the sources for one Last.fm track and registers the
// winner as a streamable row. It shares the recommender's match cache.
func (r *Radio) resolveIdentity(ctx context.Context, track LastFmTrack) *domain.TrackDTO {
	key := candidateKey(track.Artist, track.Title)
	if entry, ok := r.recommender.liveMatchCache(ctx, []string{key})[key]; ok {
		if entry.NoSource {
			return nil
		}
		if entry.Provider != "" && entry.ExternalID != "" {
			return r.ensureStreamable(ctx, domain.CatalogSearchResult{
				Provider: entry.Provider, ExternalID: entry.ExternalID,
				Title: track.Title, Artists: []string{track.Artist},
				AlbumName: track.Album, Duration: track.Duration,
				CoverURL: entry.CoverURL,
			})
		}
		if entry.LocalTrackID != "" {
			// Local rows are filtered before matching; this is only a safety
			// net for a cache write that raced the library import.
			return nil
		}
	}

	sources := r.recommender.matchSources(ctx)
	if len(sources) == 0 {
		return nil
	}
	row := RecommendedTrack{
		Title:    track.Title,
		Artists:  []string{track.Artist},
		Duration: track.Duration,
		MatchKey: key,
	}
	match, searchErr := r.recommender.matchSlow(row, sources)
	now := r.now().UTC()
	if match == nil {
		if searchErr == nil {
			r.recommender.storeMatchCacheEntries([]domain.LastFmMatchCacheEntry{{
				MatchKey: key, NoSource: true, UpdatedAt: now,
				ExpiresAt: now.Add(persistentMatchTTLNoSource),
			}})
		}
		return nil
	}
	local := match.LocalTrackID != "" || match.State == domain.CatalogRowStateInLibrary
	if !local {
		r.recommender.storeMatchCacheEntries([]domain.LastFmMatchCacheEntry{{
			MatchKey:   key,
			Provider:   match.Provider,
			ExternalID: match.ExternalID,
			Suspicious: match.Suspicious,
			CoverURL:   match.CoverURLLarge,
			Duration:   match.Duration,
			UpdatedAt:  now,
			ExpiresAt:  now.Add(persistentMatchTTL(local, false, match.Suspicious)),
		}})
	}
	return r.ensureStreamable(ctx, *match)
}

func (r *Radio) ensureStreamable(
	ctx context.Context, match domain.CatalogSearchResult,
) *domain.TrackDTO {
	track, err := r.catalog.EnsureStreamableTrack(ctx, catalog.ActionRequest{
		Provider:     match.Provider,
		ExternalID:   match.ExternalID,
		Title:        match.Title,
		Artists:      match.Artists,
		AlbumName:    match.AlbumName,
		Duration:     match.Duration,
		CoverURL:     match.CoverURLLarge,
		LocalTrackID: match.LocalTrackID,
	})
	if err != nil {
		r.logger.Debug("last.fm radio: register streamable row failed", "error", err)
		return nil
	}
	return track
}

// exclusionKeys is the set of identities the next batch must skip: recently
// heard tracks, rows already queued, and the seed itself.
func (r *Radio) exclusionKeys(ctx context.Context, seed LastFmTrack) map[string]struct{} {
	keys := make(map[string]struct{})
	keys[seedKey(seed)] = struct{}{}

	r.mu.Lock()
	for key := range r.session.keys {
		keys[key] = struct{}{}
	}
	r.mu.Unlock()

	for _, track := range r.queue.GetQueue() {
		if track == nil {
			continue
		}
		keys[candidateKey(firstArtist(trackArtists(track)), track.Title)] = struct{}{}
	}
	if r.listening != nil {
		since := r.now().UTC().AddDate(0, 0, -radioRecentDays)
		if recent, err := r.listening.RecentlyListenedTracks(ctx, since, 1000); err == nil {
			for _, row := range recent {
				track := localSeedTrack(row)
				if usableTrack(track) {
					keys[candidateKey(track.Artist, track.Title)] = struct{}{}
				}
			}
		}
	}
	return keys
}

func seedKey(track LastFmTrack) string {
	return candidateKey(track.Artist, track.Title)
}

func indexOfTrack(tracks []*domain.TrackDTO, trackID string) int {
	for index, track := range tracks {
		if track != nil && track.ID == trackID {
			return index
		}
	}
	return -1
}

func trackArtists(track *domain.TrackDTO) []string {
	if track == nil {
		return nil
	}
	names := make([]string, 0, len(track.Artists))
	for _, artist := range track.Artists {
		if artist != nil && strings.TrimSpace(artist.Name) != "" {
			names = append(names, strings.TrimSpace(artist.Name))
		}
	}
	if len(names) == 0 && strings.TrimSpace(track.RawArtistNames) != "" {
		names = append(names, strings.TrimSpace(track.RawArtistNames))
	}
	return names
}

func trackSeed(track *domain.TrackDTO) LastFmTrack {
	if track == nil {
		return LastFmTrack{}
	}
	album := strings.TrimSpace(track.RawAlbumName)
	if track.Album != nil && strings.TrimSpace(track.Album.Title) != "" {
		album = strings.TrimSpace(track.Album.Title)
	}
	return LastFmTrack{
		Artist:   firstArtist(trackArtists(track)),
		Title:    track.Title,
		Album:    album,
		Duration: track.Duration,
	}
}
