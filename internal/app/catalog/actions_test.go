package catalog

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"airmedy/internal/domain"
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
		Library:   tracks,
		Favorites: favorites,
		Playlists: playlists,
		Settings:  settings,
		Resolver:  fakeResolver{"wy": true},
		Logger:    quietLogger(),
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
