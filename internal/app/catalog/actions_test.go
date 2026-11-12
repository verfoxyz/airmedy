package catalog

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"airmedy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRemoteTracks records what an action asked the download pipeline to do.
type fakeRemoteTracks struct {
	track      *domain.TrackDTO
	ensureErr  error
	enqueueErr error
	quality    string

	mu       sync.Mutex
	specs    []domain.RemoteTrackSpec
	enqueues []enqueueCall
}

type enqueueCall struct {
	trackID    string
	playlistID string
	quality    string
	coverURL   string
}

func (f *fakeRemoteTracks) EnsureRemoteTrack(_ context.Context, spec domain.RemoteTrackSpec) (*domain.TrackDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, spec)
	if f.ensureErr != nil {
		return nil, f.ensureErr
	}
	return f.track, nil
}

func (f *fakeRemoteTracks) EnqueueWithCover(_ context.Context, trackID, playlistID, quality, coverURL string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueues = append(f.enqueues, enqueueCall{
		trackID: trackID, playlistID: playlistID, quality: quality, coverURL: coverURL,
	})
	return f.enqueueErr
}

// fakeSettings is the "download when adding to a playlist" switch.
type fakeSettings struct {
	enabled bool
	err     error
}

func (f fakeSettings) GetSettings(context.Context) (*domain.AppSettings, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &domain.AppSettings{AutoDownloadOnPlaylistAdd: f.enabled}, nil
}

func (f *fakeRemoteTracks) DefaultQuality(context.Context) string {
	if f.quality == "" {
		return domain.Quality320k
	}
	return f.quality
}

func (f *fakeRemoteTracks) recorded() ([]domain.RemoteTrackSpec, []enqueueCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.RemoteTrackSpec(nil), f.specs...), append([]enqueueCall(nil), f.enqueues...)
}

// fakeSavedTracks stands in for the download coordinator. The catalog only
// reports the save; whether it becomes a promotion or a download is the
// coordinator's decision. This fake keeps the old assertions meaningful by
// translating the event into the same enqueue the pipeline would perform.
type fakeSavedTracks struct {
	tracks *fakeRemoteTracks

	mu     sync.Mutex
	events []domain.RemoteTrackSaved
}

func (f *fakeSavedTracks) OnRemoteTrackSaved(ctx context.Context, ev domain.RemoteTrackSaved) (string, error) {
	f.mu.Lock()
	f.events = append(f.events, ev)
	f.mu.Unlock()

	if ev.Track == nil {
		return "", nil
	}
	if err := f.tracks.EnqueueWithCover(ctx, ev.Track.ID, ev.PlaylistID, f.tracks.DefaultQuality(ctx), ev.CoverURL); err != nil {
		return "", err
	}
	return domain.CatalogRowStateDownloading, nil
}

func (f *fakeSavedTracks) recorded() []domain.RemoteTrackSaved {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.RemoteTrackSaved(nil), f.events...)
}

// fakeFavorites records favorite writes.
type fakeFavorites struct {
	err  error
	mu   sync.Mutex
	sets []string
}

func (f *fakeFavorites) SetFavorite(_ context.Context, trackID string, favorite bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if favorite {
		f.sets = append(f.sets, trackID)
	}
	return f.err
}

// fakePlaylists records playlist membership writes.
type fakePlaylists struct {
	err  error
	mu   sync.Mutex
	args [][2]string
}

func (f *fakePlaylists) AddTrack(_ context.Context, playlistID, trackID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.args = append(f.args, [2]string{playlistID, trackID})
	return f.err
}

// newActionService builds a service whose search side is inert: these tests are
// about the save action only. Without an explicit settings reader the playlist
// switch is on, which is the default a new install gets.
func newActionService(tracks *fakeRemoteTracks, favorites *fakeFavorites, playlists *fakePlaylists, settings SettingsReader) *Service {
	if settings == nil {
		settings = fakeSettings{enabled: true}
	}
	return NewService(Deps{
		Library:     tracks,
		SavedTracks: &fakeSavedTracks{tracks: tracks},
		Favorites:   favorites,
		Playlists:   playlists,
		Settings:    settings,
		Resolver:    fakeResolver{"wy": true},
		Logger:      quietLogger(),
	})
}

// fakeLocalTracks answers the "row already on disk" lookup.
type fakeLocalTracks struct {
	track *domain.TrackDTO
	err   error
}

func (f fakeLocalTracks) GetTrack(context.Context, string) (*domain.TrackDTO, error) {
	return f.track, f.err
}

func TestAct_ReusesALocalCandidateInsteadOfDownloadingAgain(t *testing.T) {
	tracks := &fakeRemoteTracks{track: remoteTrack("remote-1")}
	favorites := &fakeFavorites{}
	playlists := &fakePlaylists{}
	service := newActionService(tracks, favorites, playlists, nil)
	service.localTracks = fakeLocalTracks{track: &domain.TrackDTO{Track: domain.Track{
		ID: "local-1", Path: "C:/Music/Song.flac", Title: "晴天", Availability: domain.AvailabilityReady,
	}}}

	outcome, err := service.Act(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "509781655", Title: "晴天", Duration: 239,
		PlaylistID: "pl-1", Favorite: true, LocalTrackID: "local-1",
	})
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if outcome.TrackID != "local-1" || outcome.State != domain.CatalogRowStateInLibrary {
		t.Fatalf("outcome = %+v, want the local row reported as in_library", outcome)
	}
	if specs, enqueues := tracks.recorded(); len(specs) != 0 || len(enqueues) != 0 {
		t.Fatalf("remote registration = %d/%d, want none for a local candidate", len(specs), len(enqueues))
	}
	if len(favorites.sets) != 1 || favorites.sets[0] != "local-1" {
		t.Fatalf("favorites = %v, want local-1", favorites.sets)
	}
	if len(playlists.args) != 1 || playlists.args[0] != [2]string{"pl-1", "local-1"} {
		t.Fatalf("playlist writes = %v, want the local row", playlists.args)
	}
}

func TestAct_ForceDownloadIgnoresTheLocalCandidate(t *testing.T) {
	tracks := &fakeRemoteTracks{track: remoteTrack("remote-1")}
	service := newActionService(tracks, &fakeFavorites{}, &fakePlaylists{}, nil)
	service.localTracks = fakeLocalTracks{track: &domain.TrackDTO{Track: domain.Track{
		ID: "local-1", Path: "C:/Music/Song.flac", Title: "晴天", Availability: domain.AvailabilityReady,
	}}}

	outcome, err := service.Act(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "509781655", Title: "晴天",
		LocalTrackID: "local-1", ForceDownload: true,
	})
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if outcome.TrackID != "remote-1" || outcome.State != domain.CatalogRowStateDownloading {
		t.Fatalf("outcome = %+v, want a fresh download of remote-1", outcome)
	}
	if specs, enqueues := tracks.recorded(); len(specs) != 1 || len(enqueues) != 1 {
		t.Fatalf("remote registration = %d/%d, want one each", len(specs), len(enqueues))
	}
}

func TestAct_VanishedOrRemoteLocalCandidateFallsBackToDownload(t *testing.T) {
	tracks := &fakeRemoteTracks{track: remoteTrack("remote-1")}
	service := newActionService(tracks, &fakeFavorites{}, &fakePlaylists{}, nil)
	service.localTracks = fakeLocalTracks{track: nil}

	outcome, err := service.Act(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "509781655", Title: "晴天", LocalTrackID: "gone",
	})
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if outcome.State != domain.CatalogRowStateDownloading {
		t.Fatalf("outcome = %+v, want the normal download path", outcome)
	}
}

// remoteTrack returns the row EnsureRemoteTrack answers with.
func remoteTrack(id string) *domain.TrackDTO {
	return &domain.TrackDTO{Track: domain.Track{
		ID:           id,
		Path:         domain.RemotePath("wy", id),
		Title:        "晴天",
		Availability: domain.AvailabilityRemote,
		Provider:     "wy",
		ExternalID:   id,
	}}
}

func TestAct_RegistersQueuesAndReportsDownloading(t *testing.T) {
	tracks := &fakeRemoteTracks{track: remoteTrack("t1")}
	service := newActionService(tracks, &fakeFavorites{}, &fakePlaylists{}, nil)

	outcome, err := service.Act(context.Background(), ActionRequest{
		Provider:   "WY",
		ExternalID: " 509781655 ",
		Title:      " 晴天 ",
		Artists:    []string{"周杰伦", " ", ""},
		AlbumName:  "叶惠美",
		Duration:   239,
		CoverURL:   "https://p1.music.126.net/cover.jpg",
	})
	if err != nil {
		t.Fatalf("Act: %v", err)
	}

	specs, enqueues := tracks.recorded()
	if len(specs) != 1 {
		t.Fatalf("EnsureRemoteTrack calls = %d, want 1", len(specs))
	}
	spec := specs[0]
	if spec.Provider != "wy" || spec.ExternalID != "509781655" {
		t.Fatalf("spec identity = %s/%s, want the normalised wy/509781655", spec.Provider, spec.ExternalID)
	}
	if spec.Title != "晴天" {
		t.Fatalf("title = %q, want it trimmed", spec.Title)
	}
	if len(spec.Artists) != 1 || spec.Artists[0] != "周杰伦" {
		t.Fatalf("artists = %v, want blanks dropped", spec.Artists)
	}
	if len(enqueues) != 1 {
		t.Fatalf("Enqueue calls = %d, want 1", len(enqueues))
	}
	if enqueues[0].trackID != "t1" || enqueues[0].playlistID != "" {
		t.Fatalf("enqueue = %+v, want the registered track and no playlist", enqueues[0])
	}
	if enqueues[0].quality != domain.Quality320k {
		t.Fatalf("quality = %q, want the user's default", enqueues[0].quality)
	}
	if outcome.TrackID != "t1" || outcome.State != domain.CatalogRowStateDownloading {
		t.Fatalf("outcome = %+v, want t1/downloading", outcome)
	}
}

func TestAct_FavoriteSetsTheFlag(t *testing.T) {
	tracks := &fakeRemoteTracks{track: remoteTrack("t1")}
	favorites := &fakeFavorites{}
	service := newActionService(tracks, favorites, &fakePlaylists{}, nil)

	if _, err := service.Act(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "t1", Favorite: true,
	}); err != nil {
		t.Fatalf("Act: %v", err)
	}
	if len(favorites.sets) != 1 || favorites.sets[0] != "t1" {
		t.Fatalf("favorite writes = %v, want t1 once", favorites.sets)
	}
	if _, enqueues := tracks.recorded(); len(enqueues) != 1 {
		t.Fatalf("enqueues = %d, want the favorite to download too", len(enqueues))
	}
}

func TestAct_PlaylistAddsMembershipAndPassesThePlaylist(t *testing.T) {
	tracks := &fakeRemoteTracks{track: remoteTrack("t1")}
	playlists := &fakePlaylists{}
	service := newActionService(tracks, &fakeFavorites{}, playlists, nil)

	if _, err := service.Act(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "t1", PlaylistID: "pl-1",
	}); err != nil {
		t.Fatalf("Act: %v", err)
	}
	if len(playlists.args) != 1 || playlists.args[0] != [2]string{"pl-1", "t1"} {
		t.Fatalf("playlist writes = %v, want pl-1 <- t1", playlists.args)
	}
	_, enqueues := tracks.recorded()
	if len(enqueues) != 1 || enqueues[0].playlistID != "pl-1" {
		t.Fatalf("enqueue = %+v, want the playlist carried to finalize", enqueues)
	}
}

// TestAct_PlaylistFailureDoesNotQueue: an invalid playlist (a smart one) is the
// user's mistake; downloading anyway would leave an orphan the user never asked
// for.
func TestAct_PlaylistFailureDoesNotQueue(t *testing.T) {
	tracks := &fakeRemoteTracks{track: remoteTrack("t1")}
	playlists := &fakePlaylists{err: errors.New("cannot edit a smart playlist")}
	service := newActionService(tracks, &fakeFavorites{}, playlists, nil)

	_, err := service.Act(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "t1", PlaylistID: "smart",
	})
	if err == nil {
		t.Fatal("Act = nil error, want the playlist failure")
	}
	if _, enqueues := tracks.recorded(); len(enqueues) != 0 {
		t.Fatalf("enqueues = %d, want none after a playlist failure", len(enqueues))
	}
}

// TestAct_AlreadyInLibraryDoesNotQueueAgain: a finished download is not a
// download.
func TestAct_AlreadyInLibraryDoesNotQueueAgain(t *testing.T) {
	ready := &domain.TrackDTO{Track: domain.Track{
		ID:           "t1",
		Path:         "/library/晴天.mp3",
		Availability: domain.AvailabilityReady,
		Provider:     "wy",
		ExternalID:   "t1",
	}}
	tracks := &fakeRemoteTracks{track: ready}
	service := newActionService(tracks, &fakeFavorites{}, &fakePlaylists{}, nil)

	outcome, err := service.Act(context.Background(), ActionRequest{Provider: "wy", ExternalID: "t1"})
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if outcome.State != domain.CatalogRowStateInLibrary {
		t.Fatalf("state = %q, want in_library", outcome.State)
	}
	if _, enqueues := tracks.recorded(); len(enqueues) != 0 {
		t.Fatalf("enqueues = %d, want none for an available track", len(enqueues))
	}
}

// TestAct_AlreadyQueuedIsSuccess: the auto-download hook may have queued the
// same track between the row render and the click.
func TestAct_AlreadyQueuedIsSuccess(t *testing.T) {
	tracks := &fakeRemoteTracks{track: remoteTrack("t1"), enqueueErr: domain.ErrDownloadAlreadyQueued}
	service := newActionService(tracks, &fakeFavorites{}, &fakePlaylists{}, nil)

	outcome, err := service.Act(context.Background(), ActionRequest{Provider: "wy", ExternalID: "t1"})
	if err != nil {
		t.Fatalf("Act: %v, want already-queued treated as success", err)
	}
	if outcome.State != domain.CatalogRowStateDownloading {
		t.Fatalf("state = %q, want downloading", outcome.State)
	}
}

func TestAct_RejectsAnIncompleteIdentity(t *testing.T) {
	service := newActionService(&fakeRemoteTracks{}, &fakeFavorites{}, &fakePlaylists{}, nil)

	for _, request := range []ActionRequest{
		{ExternalID: "t1"},
		{Provider: "wy"},
		{Provider: "   ", ExternalID: "t1"},
		{Provider: "wy", ExternalID: "  "},
	} {
		if _, err := service.Act(context.Background(), request); !errors.Is(err, domain.ErrCatalogActionInvalid) {
			t.Fatalf("Act(%+v) = %v, want ErrCatalogActionInvalid", request, err)
		}
	}
}

// TestAct_NoResolverDoesNotQueue: a provider no enabled plugin can resolve is
// refused before registration, so the queue never receives a job that cannot
// succeed.
func TestAct_NoResolverDoesNotQueue(t *testing.T) {
	tracks := &fakeRemoteTracks{track: remoteTrack("t1")}
	service := NewService(Deps{
		Library:  tracks,
		Resolver: fakeResolver{},
		Logger:   quietLogger(),
	})

	_, err := service.Act(context.Background(), ActionRequest{Provider: "wy", ExternalID: "t1"})
	if !errors.Is(err, domain.ErrCatalogActionInvalid) {
		t.Fatalf("Act = %v, want ErrCatalogActionInvalid naming the missing source", err)
	}
	if !strings.Contains(err.Error(), "wy") || !strings.Contains(err.Error(), "providers") {
		t.Fatalf("error should name the provider and the fix, got: %v", err)
	}
	specs, enqueues := tracks.recorded()
	if len(specs) != 0 {
		t.Fatalf("EnsureRemoteTrack calls = %d, want none without a resolver", len(specs))
	}
	if len(enqueues) != 0 {
		t.Fatalf("Enqueue calls = %d, want none without a resolver", len(enqueues))
	}
}

// TestAct_PlaylistWithAutoDownloadOffSavesWithoutQueueing pins plan A: the
// playlist switch is respected, the track is still registered and added, and the
// row reports "saved" rather than pretending a download started.
func TestAct_PlaylistWithAutoDownloadOffSavesWithoutQueueing(t *testing.T) {
	tracks := &fakeRemoteTracks{track: remoteTrack("t1")}
	playlists := &fakePlaylists{}
	service := newActionService(tracks, &fakeFavorites{}, playlists, fakeSettings{enabled: false})

	outcome, err := service.Act(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "t1", PlaylistID: "pl-1",
	})
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if outcome.State != domain.CatalogRowStateSaved {
		t.Fatalf("state = %q, want %q", outcome.State, domain.CatalogRowStateSaved)
	}
	if len(playlists.args) != 1 {
		t.Fatalf("playlist writes = %v, want the membership", playlists.args)
	}
	if _, enqueues := tracks.recorded(); len(enqueues) != 0 {
		t.Fatalf("enqueues = %+v, want none while the switch is off", enqueues)
	}
}

// TestAct_FavoriteDownloadsRegardlessOfThePlaylistSetting: a favorite is an
// explicit "I want this song", so it always downloads.
func TestAct_FavoriteDownloadsRegardlessOfThePlaylistSetting(t *testing.T) {
	tracks := &fakeRemoteTracks{track: remoteTrack("t1")}
	service := newActionService(tracks, &fakeFavorites{}, &fakePlaylists{}, fakeSettings{enabled: false})

	outcome, err := service.Act(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "t1", Favorite: true,
	})
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if outcome.State != domain.CatalogRowStateDownloading {
		t.Fatalf("state = %q, want downloading", outcome.State)
	}
	if _, enqueues := tracks.recorded(); len(enqueues) != 1 {
		t.Fatalf("enqueues = %d, want the favorite to download", len(enqueues))
	}
}

// TestAct_CarriesTheCoverURL: the search result's artwork must reach the queue,
// or the finished file has no embedded cover.
func TestAct_CarriesTheCoverURL(t *testing.T) {
	tracks := &fakeRemoteTracks{track: remoteTrack("t1")}
	service := newActionService(tracks, &fakeFavorites{}, &fakePlaylists{}, nil)

	if _, err := service.Act(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "t1", CoverURL: " https://img.example/cover.jpg ",
	}); err != nil {
		t.Fatalf("Act: %v", err)
	}
	_, enqueues := tracks.recorded()
	if len(enqueues) != 1 || enqueues[0].coverURL != "https://img.example/cover.jpg" {
		t.Fatalf("enqueue = %+v, want the trimmed cover URL", enqueues)
	}
}

// TestAct_HandsTheSaveToTheCoordinator: the catalog registers and saves, then
// reports the save to the download coordinator with the explicit flag and the
// resolved cover. Promotion-versus-download is the coordinator's decision.
func TestAct_HandsTheSaveToTheCoordinator(t *testing.T) {
	tracks := &fakeRemoteTracks{track: remoteTrack("t1")}
	saved := &fakeSavedTracks{tracks: tracks}
	service := NewService(Deps{
		Library:     tracks,
		SavedTracks: saved,
		Favorites:   &fakeFavorites{},
		Playlists:   &fakePlaylists{},
		Settings:    fakeSettings{enabled: true},
		Resolver:    fakeResolver{"wy": true},
		Logger:      quietLogger(),
	})

	if _, err := service.Act(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "t1", PlaylistID: "pl-1",
		CoverURL: " https://img.example/cover.jpg ",
	}); err != nil {
		t.Fatalf("Act: %v", err)
	}
	events := saved.recorded()
	if len(events) != 1 {
		t.Fatalf("coordinator events = %d, want 1", len(events))
	}
	ev := events[0]
	if !ev.Explicit || ev.PlaylistID != "pl-1" || ev.CoverURL != "https://img.example/cover.jpg" {
		t.Fatalf("event = %+v, want explicit with the trimmed playlist and cover", ev)
	}
	if ev.Track == nil || ev.Track.ID != "t1" {
		t.Fatalf("event track = %+v, want the registered row", ev.Track)
	}
}

// TestAct_SettingsFailureDoesNotMutate: a settings read that fails must stop the
// action before the row is registered or the playlist is touched.
func TestAct_SettingsFailureDoesNotMutate(t *testing.T) {
	tracks := &fakeRemoteTracks{track: remoteTrack("t1")}
	playlists := &fakePlaylists{}
	service := newActionService(tracks, &fakeFavorites{}, playlists, fakeSettings{err: errors.New("no settings")})

	_, err := service.Act(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "t1", PlaylistID: "pl-1",
	})
	if err == nil {
		t.Fatal("Act = nil, want the settings failure")
	}
	if specs, _ := tracks.recorded(); len(specs) != 0 {
		t.Fatalf("EnsureRemoteTrack calls = %d, want none after a settings failure", len(specs))
	}
	if len(playlists.args) != 0 {
		t.Fatalf("playlist writes = %d, want none after a settings failure", len(playlists.args))
	}
}

func TestAct_WithoutDownloadPipelineFails(t *testing.T) {
	service := NewService(Deps{Logger: quietLogger()})

	if _, err := service.Act(context.Background(), ActionRequest{Provider: "wy", ExternalID: "t1"}); err == nil {
		t.Fatal("Act without a download pipeline = nil error")
	}
}

func TestAct_FavoriteWithoutWriterFails(t *testing.T) {
	tracks := &fakeRemoteTracks{track: remoteTrack("t1")}
	service := NewService(Deps{Library: tracks, Playlists: &fakePlaylists{}, Logger: quietLogger()})

	if _, err := service.Act(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "t1", Favorite: true,
	}); err == nil {
		t.Fatal("Act with Favorite and no favorites writer = nil error")
	}
}

func TestEnsureStreamableTrack_RegistersWithoutQueueingOrSaving(t *testing.T) {
	tracks := &fakeRemoteTracks{track: remoteTrack("remote-1")}
	favorites := &fakeFavorites{}
	playlists := &fakePlaylists{}
	service := newActionService(tracks, favorites, playlists, nil)

	track, err := service.EnsureStreamableTrack(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "42", Title: "晴天", Artists: []string{"周杰伦"},
	})
	if err != nil {
		t.Fatalf("EnsureStreamableTrack: %v", err)
	}
	if track.ID != "remote-1" {
		t.Fatalf("track = %+v, want the registered row", track)
	}

	specs, enqueues := tracks.recorded()
	if len(specs) != 1 || specs[0].Provider != "wy" || specs[0].ExternalID != "42" {
		t.Fatalf("registered specs = %+v, want wy/42", specs)
	}
	// Playing is not a save: no download, favorite or playlist membership.
	if len(enqueues) != 0 {
		t.Fatalf("enqueue calls = %+v, want none", enqueues)
	}
	if len(favorites.sets) != 0 || len(playlists.args) != 0 {
		t.Fatalf("favorite=%v playlist=%v, want no writes", favorites.sets, playlists.args)
	}
}

func TestEnsureStreamableTrack_ReusesALocalFile(t *testing.T) {
	tracks := &fakeRemoteTracks{track: remoteTrack("remote-1")}
	service := newActionService(tracks, &fakeFavorites{}, &fakePlaylists{}, nil)
	service.localTracks = fakeLocalTracks{track: &domain.TrackDTO{Track: domain.Track{
		ID: "local-1", Path: `C:\Music\晴天.flac`, Title: "晴天",
	}}}

	track, err := service.EnsureStreamableTrack(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "42", LocalTrackID: "local-1",
	})
	if err != nil {
		t.Fatalf("EnsureStreamableTrack: %v", err)
	}
	if track.ID != "local-1" {
		t.Fatalf("track = %+v, want the local file row", track)
	}
	if specs, _ := tracks.recorded(); len(specs) != 0 {
		t.Fatalf("registered %+v, want the on-disk row without a new remote row", specs)
	}
}

// fakeLocalMatcher answers the local-first lookup and records it.
type fakeLocalMatcher struct {
	track *domain.TrackDTO
	err   error

	mu    sync.Mutex
	calls int
}

func (f *fakeLocalMatcher) MatchLocalTrack(
	context.Context, string, []string, int,
) (*domain.LocalTrackMatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.track == nil {
		return nil, f.err
	}
	return &domain.LocalTrackMatch{Track: f.track}, f.err
}

func (f *fakeLocalMatcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestEnsureStreamableTrack_PrefersALocalMatch: when the library already has the
// song, playing it must reuse that row and register nothing.
func TestEnsureStreamableTrack_PrefersALocalMatch(t *testing.T) {
	tracks := &fakeRemoteTracks{track: remoteTrack("remote-1")}
	service := newActionService(tracks, &fakeFavorites{}, &fakePlaylists{}, nil)
	matcher := &fakeLocalMatcher{track: &domain.TrackDTO{Track: domain.Track{
		ID: "local-1", Path: `C:\Music\Synthetic Fracture.flac`, Title: "Synthetic Fracture",
	}}}
	service.localMatcher = matcher

	track, err := service.EnsureStreamableTrack(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "1941192069", Title: "Synthetic Fracture",
		Artists: []string{"Sad Keyboard Guy", "Gardens"}, Duration: 200,
	})
	if err != nil {
		t.Fatalf("EnsureStreamableTrack: %v", err)
	}
	if track.ID != "local-1" {
		t.Fatalf("track = %+v, want the on-disk row", track)
	}
	if specs, _ := tracks.recorded(); len(specs) != 0 {
		t.Fatalf("registered %+v, want no remote row when a local file exists", specs)
	}
	if matcher.callCount() == 0 {
		t.Fatal("the local matcher was never asked")
	}
}

// TestEnsureStreamableTrack_FallsBackToRegistration: no local match means the
// provider row is registered as before.
func TestEnsureStreamableTrack_FallsBackToRegistration(t *testing.T) {
	tracks := &fakeRemoteTracks{track: remoteTrack("remote-1")}
	service := newActionService(tracks, &fakeFavorites{}, &fakePlaylists{}, nil)
	service.localMatcher = &fakeLocalMatcher{}

	track, err := service.EnsureStreamableTrack(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "42", Title: "Song", Duration: 200,
	})
	if err != nil {
		t.Fatalf("EnsureStreamableTrack: %v", err)
	}
	if track.ID != "remote-1" {
		t.Fatalf("track = %+v, want the registered remote row", track)
	}
	if specs, _ := tracks.recorded(); len(specs) != 1 {
		t.Fatalf("registered %+v, want one remote row", specs)
	}
}

// TestEnsureStreamableTrack_WarmsTheCoverCache: the player and the info drawer
// resolve artwork by identity, so pressing play must populate the cache.
func TestEnsureStreamableTrack_WarmsTheCoverCache(t *testing.T) {
	covers := &fakeCoverCache{asset: "/cover-cache/a.jpg"}
	service := NewService(Deps{
		Library:  &fakeRemoteTracks{track: remoteTrack("remote-1")},
		Resolver: fakeResolver{"wy": true},
		Covers:   covers,
		Logger:   quietLogger(),
	})

	_, err := service.EnsureStreamableTrack(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "42", Title: "Song",
		CoverURL: " https://cdn.example/a.jpg ",
	})
	if err != nil {
		t.Fatalf("EnsureStreamableTrack: %v", err)
	}

	require.Len(t, covers.calls, 1)
	assert.Equal(t, "https://cdn.example/a.jpg", covers.calls[0].url)
	assert.Equal(t, "wy", covers.calls[0].provider)
	assert.Equal(t, "42", covers.calls[0].externalID)
}

func TestEnsureStreamableTrack_SkipsTheCoverWarmWithoutAURL(t *testing.T) {
	covers := &fakeCoverCache{asset: "/cover-cache/a.jpg"}
	service := NewService(Deps{
		Library:  &fakeRemoteTracks{track: remoteTrack("remote-1")},
		Resolver: fakeResolver{"wy": true},
		Covers:   covers,
		Logger:   quietLogger(),
	})

	if _, err := service.EnsureStreamableTrack(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "42", Title: "Song",
	}); err != nil {
		t.Fatalf("EnsureStreamableTrack: %v", err)
	}
	assert.Empty(t, covers.calls)
}

func TestEnsureStreamableTrack_CoverWarmFailureDoesNotFailPlayback(t *testing.T) {
	covers := &fakeCoverCache{err: errors.New("network down")}
	service := NewService(Deps{
		Library:  &fakeRemoteTracks{track: remoteTrack("remote-1")},
		Resolver: fakeResolver{"wy": true},
		Covers:   covers,
		Logger:   quietLogger(),
	})

	track, err := service.EnsureStreamableTrack(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "42", Title: "Song", CoverURL: "https://cdn.example/a.jpg",
	})
	if err != nil {
		t.Fatalf("EnsureStreamableTrack must not fail when the cover cannot be cached: %v", err)
	}
	if track.ID != "remote-1" {
		t.Fatalf("track = %+v, want the registered row", track)
	}
}

func TestEnsureStreamableTrack_RequiresAnEnabledSource(t *testing.T) {
	service := NewService(Deps{
		Library:  &fakeRemoteTracks{track: remoteTrack("remote-1")},
		Resolver: fakeResolver{"wy": false},
		Logger:   quietLogger(),
	})

	_, err := service.EnsureStreamableTrack(context.Background(), ActionRequest{
		Provider: "wy", ExternalID: "42",
	})
	if !errors.Is(err, domain.ErrCatalogActionInvalid) {
		t.Fatalf("error = %v, want ErrCatalogActionInvalid", err)
	}
}
