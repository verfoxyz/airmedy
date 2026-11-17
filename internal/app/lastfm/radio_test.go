package lastfm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"airmedy/internal/app/catalog"
	"airmedy/internal/domain"
)

type fakeRadioQueue struct {
	mu      sync.Mutex
	queue   []*domain.TrackDTO
	current *domain.TrackDTO
	appends [][]*domain.TrackDTO
}

func (q *fakeRadioQueue) GetQueue() []*domain.TrackDTO {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]*domain.TrackDTO{}, q.queue...)
}

func (q *fakeRadioQueue) GetCurrentTrack() *domain.TrackDTO {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.current
}

func (q *fakeRadioQueue) AppendTracks(tracks []*domain.TrackDTO) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.appends = append(q.appends, append([]*domain.TrackDTO{}, tracks...))
	q.queue = append(q.queue, tracks...)
}

func (q *fakeRadioQueue) AddQueueListener(func([]*domain.TrackDTO)) {}

func (q *fakeRadioQueue) AddTrackLoadListener(func(*domain.TrackDTO)) {}

func (q *fakeRadioQueue) setQueue(tracks []*domain.TrackDTO, current *domain.TrackDTO) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queue = tracks
	q.current = current
}

func (q *fakeRadioQueue) appendCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.appends)
}

type fakeStreamableCatalog struct {
	mu        sync.Mutex
	failCalls int
	block     chan struct{}
	calls     int
}

func (c *fakeStreamableCatalog) EnsureStreamableTrack(
	_ context.Context, request catalog.ActionRequest,
) (*domain.TrackDTO, error) {
	c.mu.Lock()
	block := c.block
	c.calls++
	if c.failCalls > 0 {
		c.failCalls--
		c.mu.Unlock()
		return nil, errors.New("resolver down")
	}
	c.mu.Unlock()
	if block != nil {
		<-block
	}
	id := "track-" + request.Provider + "-" + request.ExternalID
	artists := make([]*domain.Artist, 0, len(request.Artists))
	for _, name := range request.Artists {
		artists = append(artists, &domain.Artist{Name: name})
	}
	return &domain.TrackDTO{
		Track: domain.Track{
			ID: id, Title: request.Title, Duration: request.Duration,
			Path: "remote://" + id, Provider: request.Provider, ExternalID: request.ExternalID,
		},
		Artists: artists,
	}, nil
}

func (c *fakeStreamableCatalog) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type fakeRadioTrackLookup struct {
	domain.TrackRepository
	tracks map[string]*domain.TrackDTO
}

func (f *fakeRadioTrackLookup) GetByID(_ context.Context, id string) (*domain.TrackDTO, error) {
	return f.tracks[id], nil
}

type radioEventCapture struct {
	mu       sync.Mutex
	states   []bool
	appended []int
}

func newRadioEventCapture() *radioEventCapture {
	return &radioEventCapture{}
}

func (c *radioEventCapture) emit(name string, payload any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch name {
	case radioStateEvent:
		state, _ := payload.(bool)
		c.states = append(c.states, state)
	case radioQueueAppendedEvent:
		event, _ := payload.(RadioQueueAppended)
		c.appended = append(c.appended, event.Count)
	}
}

func (c *radioEventCapture) appendedBatches() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int{}, c.appended...)
}

func radioSeedTrack() *domain.TrackDTO {
	return &domain.TrackDTO{
		Track: domain.Track{
			ID: "seed", Title: "Seed Song", Duration: 200,
			Path: "/music/seed.flac", RawArtistNames: "Seed Artist",
		},
		Artists: []*domain.Artist{{Name: "Seed Artist"}},
	}
}

// radioCandidatesFor answers track.getSimilar for the key with n distinct
// artists and albums.
func radioCandidatesFor(source *fakeTrackSource, artist, title, prefix string, n int) []LastFmTrack {
	tracks := make([]LastFmTrack, 0, n)
	for index := 0; index < n; index++ {
		tracks = append(tracks, LastFmTrack{
			Artist:   fmt.Sprintf("%s Artist %d", prefix, index),
			Title:    fmt.Sprintf("%s Song %d", prefix, index),
			Album:    fmt.Sprintf("%s Album %d", prefix, index),
			Duration: 200,
		})
	}
	source.similarTracks[artist+"\x00"+title] = tracks
	return tracks
}

func newRadioForTest(
	t *testing.T, source *fakeTrackSource, matcher *fakeMatcher, seedRepo domain.RecommendationSeedRepository,
) (*Radio, *fakeRadioQueue, *fakeRadioTrackLookup, *fakeStreamableCatalog, *radioEventCapture) {
	t.Helper()
	recommender := newTestRecommender(source, matcher, &fakeStreamRepo{})
	lookup := &fakeRadioTrackLookup{tracks: map[string]*domain.TrackDTO{"seed": radioSeedTrack()}}
	catalogResolver := &fakeStreamableCatalog{}
	queue := &fakeRadioQueue{}
	radio := NewRadio(recommender, lookup, seedRepo, catalogResolver, queue, slog.Default())
	capture := newRadioEventCapture()
	radio.emit = capture.emit
	radio.now = fixedClock(1_800_000_000)
	return radio, queue, lookup, catalogResolver, capture
}

func radioMatcherFor(candidates []LastFmTrack) *fakeMatcher {
	rows := make(map[string][]domain.CatalogSearchResult, len(candidates))
	for index, candidate := range candidates {
		key := candidate.Artist + "\x00" + candidate.Title
		rows[key] = []domain.CatalogSearchResult{{
			Provider: "wy", ExternalID: strconv.Itoa(index), Title: candidate.Title,
			Artists: []string{candidate.Artist}, AlbumName: candidate.Album,
			Duration: candidate.Duration, Downloadable: true,
		}}
	}
	return &fakeMatcher{
		sources: []domain.CatalogSearchSource{{ID: "wy"}},
		rows:    rows,
	}
}

func TestRadioStartBuildsTheFirstBatch(t *testing.T) {
	source := connectedSource(nil)
	candidates := radioCandidatesFor(source, "Seed Artist", "Seed Song", "Radio", 30)
	matcher := radioMatcherFor(candidates)
	radio, queue, _, resolver, events := newRadioForTest(t, source, matcher, &fakeSeedRepository{})

	result, err := radio.StartRadio(context.Background(), RadioSeedRequest{TrackID: "seed"})
	if err != nil {
		t.Fatalf("StartRadio: %v", err)
	}
	if result.Seed == nil || result.Seed.ID != "seed" {
		t.Fatalf("seed = %+v, want the library row", result.Seed)
	}
	if len(result.Tracks) != radioInitialBatch {
		t.Fatalf("tracks = %d, want %d", len(result.Tracks), radioInitialBatch)
	}
	for index, track := range result.Tracks {
		if track == nil || track.ID == "" || track.Provider != "wy" {
			t.Fatalf("track %d = %+v, want a streamable source row", index, track)
		}
	}
	if !radio.GetRadioActive() {
		t.Fatal("the radio must be active after StartRadio")
	}
	if queue.appendCount() != 0 {
		t.Fatal("StartRadio must not append; the frontend starts the returned queue")
	}
	if resolver.callCount() < radioInitialBatch {
		t.Fatalf("resolver calls = %d, want the matched batch", resolver.callCount())
	}
	if batches := events.appendedBatches(); len(batches) != 1 || batches[0] != radioInitialBatch {
		t.Fatalf("appended events = %v, want one batch of %d", batches, radioInitialBatch)
	}
}

func TestRadioStartFiltersOwnedRecentAndQueuedTracks(t *testing.T) {
	source := connectedSource(nil)
	candidates := radioCandidatesFor(source, "Seed Artist", "Seed Song", "Radio", 30)
	recent := candidates[0]
	owned := candidates[1]
	queued := candidates[2]
	source.similarTracks["Seed Artist\x00Seed Song"] = candidates

	matcher := radioMatcherFor(candidates)
	matcher.localRows = map[string]*domain.TrackDTO{
		owned.Artist + "\x00" + owned.Title: {
			Track: domain.Track{ID: "local-owned", Title: owned.Title, Path: "/music/owned.flac"},
		},
	}
	seedRepo := &fakeSeedRepository{recent: []domain.RecommendationSeedTrack{{
		TrackID: "recent-track", Title: recent.Title, Artists: []string{recent.Artist},
	}}}
	radio, queue, _, _, _ := newRadioForTest(t, source, matcher, seedRepo)
	queue.setQueue([]*domain.TrackDTO{{
		Track:   domain.Track{ID: "queued", Title: queued.Title},
		Artists: []*domain.Artist{{Name: queued.Artist}},
	}}, nil)

	result, err := radio.StartRadio(context.Background(), RadioSeedRequest{TrackID: "seed"})
	if err != nil {
		t.Fatalf("StartRadio: %v", err)
	}
	excluded := map[string]struct{}{
		recent.Title: {}, owned.Title: {}, queued.Title: {},
	}
	for _, track := range result.Tracks {
		if _, skip := excluded[track.Title]; skip {
			t.Fatalf("result contains %q, which should have been filtered", track.Title)
		}
	}
	if len(result.Tracks) != radioInitialBatch {
		t.Fatalf("tracks = %d, want %d after filtering", len(result.Tracks), radioInitialBatch)
	}
}

func TestRadioRefillsWhenFewTracksRemain(t *testing.T) {
	source := connectedSource(nil)
	candidates := radioCandidatesFor(source, "Seed Artist", "Seed Song", "Radio", 30)
	matcher := radioMatcherFor(candidates)
	radio, queue, _, _, events := newRadioForTest(t, source, matcher, &fakeSeedRepository{})

	result, err := radio.StartRadio(context.Background(), RadioSeedRequest{TrackID: "seed"})
	if err != nil {
		t.Fatalf("StartRadio: %v", err)
	}
	current := result.Tracks[15]
	refillCandidates := radioCandidatesFor(source, firstArtist(trackArtists(current)), current.Title, "Refill", 15)
	for index, candidate := range refillCandidates {
		matcher.rows[candidate.Artist+"\x00"+candidate.Title] = []domain.CatalogSearchResult{{
			Provider: "wy", ExternalID: fmt.Sprintf("refill-%d", index), Title: candidate.Title,
			Artists: []string{candidate.Artist}, Duration: candidate.Duration, Downloadable: true,
		}}
	}
	all := append([]*domain.TrackDTO{result.Seed}, result.Tracks...)
	queue.setQueue(all, current)

	radio.refillIfNeeded(context.Background())

	if queue.appendCount() != 1 {
		t.Fatalf("appends = %d, want one refill batch", queue.appendCount())
	}
	queue.mu.Lock()
	appended := queue.appends[0]
	queue.mu.Unlock()
	if len(appended) != radioRefillBatch {
		t.Fatalf("refilled = %d tracks, want %d", len(appended), radioRefillBatch)
	}
	batches := events.appendedBatches()
	if len(batches) != 2 || batches[1] != radioRefillBatch {
		t.Fatalf("appended events = %v, want the initial batch then %d", batches, radioRefillBatch)
	}
}

func TestRadioRefillIsSingleFlight(t *testing.T) {
	source := connectedSource(nil)
	candidates := radioCandidatesFor(source, "Seed Artist", "Seed Song", "Radio", 30)
	matcher := radioMatcherFor(candidates)
	radio, queue, _, resolver, _ := newRadioForTest(t, source, matcher, &fakeSeedRepository{})

	result, err := radio.StartRadio(context.Background(), RadioSeedRequest{TrackID: "seed"})
	if err != nil {
		t.Fatalf("StartRadio: %v", err)
	}
	current := result.Tracks[15]
	refillCandidates := radioCandidatesFor(source, firstArtist(trackArtists(current)), current.Title, "Refill", 15)
	for index, candidate := range refillCandidates {
		matcher.rows[candidate.Artist+"\x00"+candidate.Title] = []domain.CatalogSearchResult{{
			Provider: "wy", ExternalID: fmt.Sprintf("refill-%d", index), Title: candidate.Title,
			Artists: []string{candidate.Artist}, Downloadable: true,
		}}
	}
	all := append([]*domain.TrackDTO{result.Seed}, result.Tracks...)
	queue.setQueue(all, current)

	callsBefore := resolver.callCount()
	block := make(chan struct{})
	resolver.mu.Lock()
	resolver.block = block
	resolver.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		radio.refillIfNeeded(context.Background())
	}()
	deadline := time.Now().Add(2 * time.Second)
	for resolver.callCount() == callsBefore && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// The second evaluation arrives while the first one is still matching.
	radio.refillIfNeeded(context.Background())
	close(block)
	<-done

	if queue.appendCount() != 1 {
		t.Fatalf("appends = %d, want exactly one refill", queue.appendCount())
	}
}

func TestRadioRefillRetriesOnceAfterAFailure(t *testing.T) {
	source := connectedSource(nil)
	candidates := radioCandidatesFor(source, "Seed Artist", "Seed Song", "Radio", 30)
	matcher := radioMatcherFor(candidates)
	radio, queue, _, resolver, _ := newRadioForTest(t, source, matcher, &fakeSeedRepository{})

	result, err := radio.StartRadio(context.Background(), RadioSeedRequest{TrackID: "seed"})
	if err != nil {
		t.Fatalf("StartRadio: %v", err)
	}
	current := result.Tracks[15]
	refillCandidates := radioCandidatesFor(
		source, firstArtist(trackArtists(current)), current.Title, "Refill", 5)
	for index, candidate := range refillCandidates {
		matcher.rows[candidate.Artist+"\x00"+candidate.Title] = []domain.CatalogSearchResult{{
			Provider: "wy", ExternalID: fmt.Sprintf("retry-%d", index), Title: candidate.Title,
			Artists: []string{candidate.Artist}, Downloadable: true,
		}}
	}
	all := append([]*domain.TrackDTO{result.Seed}, result.Tracks...)
	queue.setQueue(all, current)

	// Every resolver call of the first attempt fails; the retry succeeds.
	resolver.mu.Lock()
	resolver.failCalls = len(refillCandidates)
	resolver.mu.Unlock()

	radio.refillIfNeeded(context.Background())

	if queue.appendCount() != 1 {
		t.Fatalf("appends = %d, want the retry to append once", queue.appendCount())
	}
}

func TestRadioStopsOnOrdinaryPlayback(t *testing.T) {
	source := connectedSource(nil)
	candidates := radioCandidatesFor(source, "Seed Artist", "Seed Song", "Radio", 30)
	radio, queue, _, _, events := newRadioForTest(t, source, radioMatcherFor(candidates), &fakeSeedRepository{})
	if _, err := radio.StartRadio(context.Background(), RadioSeedRequest{TrackID: "seed"}); err != nil {
		t.Fatalf("StartRadio: %v", err)
	}

	other := &domain.TrackDTO{Track: domain.Track{ID: "other", Title: "Other Song"}}
	queue.setQueue([]*domain.TrackDTO{other}, other)
	radio.refillIfNeeded(context.Background())

	if radio.GetRadioActive() {
		t.Fatal("ordinary playback must end the radio session")
	}
	if queue.appendCount() != 0 {
		t.Fatal("a stopped session must not append tracks")
	}
	states := events.states
	if len(states) < 2 || states[len(states)-1] != false {
		t.Fatalf("states = %v, want a final inactive event", states)
	}
}

func TestRadioStopEndsAutomaticRefills(t *testing.T) {
	source := connectedSource(nil)
	candidates := radioCandidatesFor(source, "Seed Artist", "Seed Song", "Radio", 30)
	radio, queue, _, _, events := newRadioForTest(t, source, radioMatcherFor(candidates), &fakeSeedRepository{})
	result, err := radio.StartRadio(context.Background(), RadioSeedRequest{TrackID: "seed"})
	if err != nil {
		t.Fatalf("StartRadio: %v", err)
	}
	current := result.Tracks[15]
	queue.setQueue(append([]*domain.TrackDTO{result.Seed}, result.Tracks...), current)

	radio.StopRadio()
	radio.refillIfNeeded(context.Background())

	if radio.GetRadioActive() {
		t.Fatal("the session is still active after StopRadio")
	}
	if queue.appendCount() != 0 {
		t.Fatal("a stopped session appended a refill")
	}
	if len(events.states) != 2 || events.states[0] != true || events.states[1] != false {
		t.Fatalf("states = %v, want true then false", events.states)
	}
}
