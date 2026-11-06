package library

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airmedy/internal/domain"
)

type mockTxManager struct{}

func (m *mockTxManager) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

type mockTrackRepo struct {
	domain.TrackRepository
	tracks        map[string]*domain.Track
	deletedPrefix string
}

func (m *mockTrackRepo) Upsert(ctx context.Context, track *domain.Track) error {
	m.tracks[track.Path] = track
	return nil
}

func (m *mockTrackRepo) Count(ctx context.Context) (int, error) {
	return len(m.tracks), nil
}

func (m *mockTrackRepo) GetByPath(ctx context.Context, path string) (*domain.TrackDTO, error) {
	if track, ok := m.tracks[path]; ok {
		return &domain.TrackDTO{Track: *track}, nil
	}
	return nil, nil
}

func (m *mockTrackRepo) GetByPathPrefix(ctx context.Context, prefix string) ([]*domain.TrackDTO, error) {
	var results []*domain.TrackDTO
	for path, track := range m.tracks {
		if strings.HasPrefix(path, prefix) {
			results = append(results, &domain.TrackDTO{Track: *track})
		}
	}
	return results, nil
}

func (m *mockTrackRepo) GetAll(ctx context.Context) ([]*domain.TrackDTO, error) {
	results := make([]*domain.TrackDTO, 0, len(m.tracks))
	for _, track := range m.tracks {
		results = append(results, &domain.TrackDTO{Track: *track})
	}
	return results, nil
}

func (m *mockTrackRepo) DeleteByPathPrefix(ctx context.Context, prefix string) error {
	m.deletedPrefix = prefix
	for path := range m.tracks {
		if strings.HasPrefix(path, prefix) {
			delete(m.tracks, path)
		}
	}
	return nil
}

func (m *mockTrackRepo) Delete(ctx context.Context, id string) error {
	for path, track := range m.tracks {
		if track.ID == id {
			delete(m.tracks, path)
			return nil
		}
	}
	return nil
}

func (m *mockTrackRepo) GetAllArtworkKeys(ctx context.Context) ([]string, error) {
	return nil, nil
}
func (m *mockTrackRepo) SetArtists(ctx context.Context, trackID string, artistIDs []string) error {
	return nil
}
func (m *mockTrackRepo) SetAlbumArtists(ctx context.Context, trackID string, artistIDs []string) error {
	return nil
}
func (m *mockTrackRepo) SetGenres(ctx context.Context, trackID string, genreIDs []string) error {
	return nil
}
func (m *mockTrackRepo) SetComposers(ctx context.Context, trackID string, composerIDs []string) error {
	return nil
}

type mockAlbumRepo struct{ domain.AlbumRepository }

func (m *mockAlbumRepo) Upsert(ctx context.Context, album *domain.Album) error { return nil }
func (m *mockAlbumRepo) DeleteOrphaned(ctx context.Context) ([]string, error)  { return nil, nil }
func (m *mockAlbumRepo) GetByID(ctx context.Context, id string) (*domain.AlbumDTO, error) {
	return nil, nil
}
func (m *mockAlbumRepo) GetByNormalizationKey(ctx context.Context, key string) (*domain.Album, error) {
	return nil, nil
}
func (m *mockAlbumRepo) SetArtists(ctx context.Context, albumID string, artistIDs []string) error {
	return nil
}

type mockArtistRepo struct{ domain.ArtistRepository }

func (m *mockArtistRepo) Upsert(ctx context.Context, artist *domain.Artist) error { return nil }
func (m *mockArtistRepo) DeleteOrphaned(ctx context.Context) ([]string, error)    { return nil, nil }
func (m *mockArtistRepo) GetByNormalizationKey(ctx context.Context, key string) (*domain.Artist, error) {
	return nil, nil
}
func (m *mockArtistRepo) GetAll(ctx context.Context) ([]*domain.Artist, error) { return nil, nil }
func (m *mockArtistRepo) GetAllArtworkKeys(ctx context.Context) ([]string, error) {
	return nil, nil
}

type mockGenreRepo struct{ domain.GenreRepository }

func (m *mockGenreRepo) Upsert(ctx context.Context, genre *domain.Genre) error { return nil }
func (m *mockGenreRepo) DeleteOrphaned(ctx context.Context) ([]string, error)  { return nil, nil }
func (m *mockGenreRepo) GetByNormalizationKey(ctx context.Context, key string) (*domain.Genre, error) {
	return nil, nil
}

type mockComposerRepo struct{ domain.ComposerRepository }

func (m *mockComposerRepo) Upsert(ctx context.Context, composer *domain.Composer) error { return nil }
func (m *mockComposerRepo) DeleteOrphaned(ctx context.Context) ([]string, error)        { return nil, nil }
func (m *mockComposerRepo) GetByNormalizationKey(ctx context.Context, key string) (*domain.Composer, error) {
	return nil, nil
}

type mockPlaylistRepo struct{ domain.PlaylistRepository }

func (m *mockPlaylistRepo) GetAllArtworkKeys(ctx context.Context) ([]string, error) { return nil, nil }

type mockFolderRepo struct {
	domain.WatchedFolderRepository
	folders   []*domain.WatchedFolder
	deletedID string
}

func (m *mockFolderRepo) Save(ctx context.Context, folder *domain.WatchedFolder) error {
	m.folders = append(m.folders, folder)
	return nil
}

func (m *mockFolderRepo) GetAll(ctx context.Context) ([]*domain.WatchedFolder, error) {
	return m.folders, nil
}

func (m *mockFolderRepo) GetByID(ctx context.Context, id string) (*domain.WatchedFolder, error) {
	for _, f := range m.folders {
		if f.ID == id {
			return f, nil
		}
	}
	return nil, nil
}

func (m *mockFolderRepo) Delete(ctx context.Context, id string) error {
	m.deletedID = id
	return nil
}

type mockSettingsRepo struct {
	domain.SettingsRepository
}

func (m *mockSettingsRepo) Load(ctx context.Context) (*domain.AppSettings, error) {
	return &domain.AppSettings{UseOnlineArtistArtwork: false}, nil
}

type mockMetadataExtractor struct {
	domain.MetadataExtractor
	callCount  atomic.Int32
	emptyTitle bool
	duration   int
	// extractFn, when set, replaces the canned extraction so a test can return
	// different values (or an error) per path.
	extractFn func(path string) (*domain.TrackDTO, error)
}

func (m *mockMetadataExtractor) Extract(ctx context.Context, path string) (*domain.TrackDTO, error) {
	m.callCount.Add(1)
	if m.extractFn != nil {
		return m.extractFn(path)
	}
	title := filepath.Base(path)
	if m.emptyTitle {
		title = ""
	}
	return &domain.TrackDTO{
		Track: domain.Track{
			Path:     path,
			Title:    title,
			Duration: m.duration,
		},
		Artists: []*domain.Artist{{Name: "Mock Artist"}},
		Album:   &domain.Album{Title: "Mock Album"},
	}, nil
}
func (m *mockMetadataExtractor) ExtractArtwork(ctx context.Context, path string) ([]byte, string, error) {
	return nil, "", nil
}
func (m *mockMetadataExtractor) ExtractLyrics(ctx context.Context, path string) (string, bool, error) {
	return "", false, nil
}

// stubDurationProbe stands in for the ffmpeg container probe. Its default is an
// error, so existing import tests keep the taglib duration (the fallback path).
type stubDurationProbe struct {
	probeFn func(path string) (time.Duration, error)
	calls   atomic.Int32
}

func (p *stubDurationProbe) Probe(_ context.Context, path string) (time.Duration, error) {
	p.calls.Add(1)
	if p.probeFn != nil {
		return p.probeFn(path)
	}
	return 0, errors.New("probe unavailable in this fixture")
}

type mockSearchService struct {
	domain.SearchService
	deletedTrackIDs []string
}

func (m *mockSearchService) IndexTrack(ctx context.Context, track *domain.TrackDTO) error { return nil }
func (m *mockSearchService) IndexAlbum(ctx context.Context, album *domain.AlbumDTO) error { return nil }
func (m *mockSearchService) IndexArtist(ctx context.Context, artist *domain.Artist) error { return nil }
func (m *mockSearchService) IndexComposer(ctx context.Context, composer *domain.Composer) error {
	return nil
}
func (m *mockSearchService) IndexPlaylist(ctx context.Context, playlist *domain.Playlist) error {
	return nil
}
func (m *mockSearchService) Close() error                                         { return nil }
func (m *mockSearchService) DeleteFromIndex(ctx context.Context, id string) error { return nil }
func (m *mockSearchService) DeleteTracksFromIndex(ctx context.Context, ids []string) error {
	m.deletedTrackIDs = append(m.deletedTrackIDs, ids...)
	return nil
}
func (m *mockSearchService) BatchDeleteFromIndex(ctx context.Context, docIDs []string) error {
	return nil
}
func (m *mockSearchService) DeleteAlbumFromIndex(ctx context.Context, albumID string) error {
	return nil
}
func (m *mockSearchService) DeleteArtistFromIndex(ctx context.Context, artistID string) error {
	return nil
}
func (m *mockSearchService) DeleteComposerFromIndex(ctx context.Context, composerID string) error {
	return nil
}
func (m *mockSearchService) DocCount(ctx context.Context) (uint64, error) { return 0, nil }
func (m *mockSearchService) BeginSyncBatch()                              {}
func (m *mockSearchService) EndSyncBatch() error                          { return nil }

type mockDownloadJobs struct {
	domain.DownloadJobRepository
	deletedTrackIDs []string
}

func (m *mockDownloadJobs) DeleteByTrack(ctx context.Context, trackID string) error {
	m.deletedTrackIDs = append(m.deletedTrackIDs, trackID)
	return nil
}

type mockArtworkCache struct {
	domain.ArtworkCache
	cleanupCalls int
	cleanupErr   error
}

func (m *mockArtworkCache) Save(ctx context.Context, data []byte, mimeType string) (string, error) {
	return "", nil
}
func (m *mockArtworkCache) CleanupOrphaned(ctx context.Context, activeKeys map[string]bool) error {
	m.cleanupCalls++
	return m.cleanupErr
}

type mockMetadataWriter struct{}

func (m *mockMetadataWriter) WriteMetadata(_ context.Context, _ string, _ domain.MetadataUpdate) error {
	return nil
}

type mockSyncStateRepo struct {
	sig     string
	version int
}

func (m *mockSyncStateRepo) GetDelimitersSignature(_ context.Context) (string, error) {
	return m.sig, nil
}
func (m *mockSyncStateRepo) SetDelimitersSignature(_ context.Context, sig string) error {
	m.sig = sig
	return nil
}
func (m *mockSyncStateRepo) GetMetadataSchemaVersion(_ context.Context) (int, error) {
	return m.version, nil
}
func (m *mockSyncStateRepo) SetMetadataSchemaVersion(_ context.Context, version int) error {
	m.version = version
	return nil
}

func TestLibraryService_SyncFolder(t *testing.T) {
	// Create a temporary directory for testing sync
	tempDir, err := os.MkdirTemp("", "airmedy_test_sync")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	// Create a dummy music file
	dummyFile := filepath.Join(tempDir, "test.mp3")
	if err := os.WriteFile(dummyFile, []byte("dummy"), 0644); err != nil {
		t.Fatalf("Failed to create dummy file: %v", err)
	}

	trackRepo := &mockTrackRepo{tracks: make(map[string]*domain.Track)}

	s, err := NewLibraryService(
		trackRepo,
		&mockAlbumRepo{},
		&mockArtistRepo{},
		&mockGenreRepo{},
		&mockComposerRepo{},
		&mockPlaylistRepo{},
		&mockFolderRepo{},
		&mockSettingsRepo{},
		&mockSyncStateRepo{version: currentMetadataSchemaVersion},
		&mockMetadataExtractor{},
		&stubDurationProbe{},
		&mockMetadataWriter{},
		&mockArtworkCache{},
		&mockSearchService{},
		&mockTxManager{},
		&mockDownloadJobs{},
		nil,
		slog.Default(),
	)
	if err != nil {
		t.Fatalf("Failed to create library service: %v", err)
	}
	defer func() { _ = s.Stop(context.Background()) }()

	err = s.SyncFolder(context.Background(), tempDir)
	if err != nil {
		t.Fatalf("SyncFolder failed: %v", err)
	}

	// Verify the track was "imported" into our mock repo
	if len(trackRepo.tracks) != 1 {
		t.Errorf("Expected 1 track in repo, got %d", len(trackRepo.tracks))
	}

	if track, ok := trackRepo.tracks[dummyFile]; !ok {
		t.Errorf("Track with path %s not found in repo", dummyFile)
	} else if track.Title != "test.mp3" {
		t.Errorf("Expected title 'test.mp3', got '%s'", track.Title)
	}

	// Verify optimization: Sync again, Extract should NOT be called
	extractor := s.metadataExtractor.(*mockMetadataExtractor)
	initialCalls := extractor.callCount.Load()
	if initialCalls != 1 {
		t.Errorf("Expected 1 call to Extract, got %d", initialCalls)
	}

	err = s.SyncFolder(context.Background(), tempDir)
	if err != nil {
		t.Fatalf("Second SyncFolder failed: %v", err)
	}

	if extractor.callCount.Load() != initialCalls {
		t.Errorf("Optimization failed: Extract was called during second sync. Initial: %d, Current: %d", initialCalls, extractor.callCount.Load())
	}

	// Verify that modification triggers a re-import
	// Small delay to ensure mtime change
	time.Sleep(1 * time.Second)
	if err := os.WriteFile(dummyFile, []byte("changed"), 0644); err != nil {
		t.Fatalf("Failed to update dummy file: %v", err)
	}

	err = s.SyncFolder(context.Background(), tempDir)
	if err != nil {
		t.Fatalf("Third SyncFolder failed: %v", err)
	}

	if extractor.callCount.Load() != initialCalls+1 {
		t.Errorf("Extract should have been called after file modification. Expected %d, got %d", initialCalls+1, extractor.callCount.Load())
	}
}

func TestLibraryService_SyncFolder_SupportedExtensions(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "airmedy_test_exts")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	extensions := []string{
		"mp3", "flac", "m4a", "wav", "ogg", "opus",
		"aiff", "aif", "ape", "wv", "dsf", "dff",
	}
	for _, ext := range extensions {
		path := filepath.Join(tempDir, "track."+ext)
		if err := os.WriteFile(path, []byte("dummy"), 0644); err != nil {
			t.Fatalf("Failed to create %s file: %v", ext, err)
		}
	}

	// Create a file with an unsupported extension — must be ignored.
	unsupported := filepath.Join(tempDir, "notes.txt")
	if err := os.WriteFile(unsupported, []byte("ignored"), 0644); err != nil {
		t.Fatalf("Failed to create unsupported file: %v", err)
	}

	trackRepo := &mockTrackRepo{tracks: make(map[string]*domain.Track)}
	s, err := NewLibraryService(
		trackRepo,
		&mockAlbumRepo{},
		&mockArtistRepo{},
		&mockGenreRepo{},
		&mockComposerRepo{},
		&mockPlaylistRepo{},
		&mockFolderRepo{},
		&mockSettingsRepo{},
		&mockSyncStateRepo{version: currentMetadataSchemaVersion},
		&mockMetadataExtractor{},
		&stubDurationProbe{},
		&mockMetadataWriter{},
		&mockArtworkCache{},
		&mockSearchService{},
		&mockTxManager{},
		&mockDownloadJobs{},
		nil,
		slog.Default(),
	)
	if err != nil {
		t.Fatalf("Failed to create library service: %v", err)
	}
	defer func() { _ = s.Stop(context.Background()) }()

	if err := s.SyncFolder(context.Background(), tempDir); err != nil {
		t.Fatalf("SyncFolder failed: %v", err)
	}

	if len(trackRepo.tracks) != len(extensions) {
		t.Errorf("Expected %d tracks (one per extension), got %d", len(extensions), len(trackRepo.tracks))
	}
	if _, found := trackRepo.tracks[unsupported]; found {
		t.Error("Unsupported .txt file was imported — should have been ignored")
	}
}

func TestLibraryService_AddWatchedFolder_CoveringExisting(t *testing.T) {
	trackRepo := &mockTrackRepo{tracks: make(map[string]*domain.Track)}
	folderRepo := &mockFolderRepo{
		folders: []*domain.WatchedFolder{
			{ID: "child-id", Path: "/Music/A/B"},
		},
	}

	s, _ := NewLibraryService(
		trackRepo,
		&mockAlbumRepo{},
		&mockArtistRepo{},
		&mockGenreRepo{},
		&mockComposerRepo{},
		&mockPlaylistRepo{},
		folderRepo,
		&mockSettingsRepo{},
		&mockSyncStateRepo{version: currentMetadataSchemaVersion},
		&mockMetadataExtractor{},
		&stubDurationProbe{},
		&mockMetadataWriter{},
		&mockArtworkCache{},
		&mockSearchService{},
		&mockTxManager{},
		&mockDownloadJobs{},
		nil,
		slog.Default(),
	)
	defer func() { _ = s.Stop(context.Background()) }()

	// Add parent folder /Music/A
	err := s.AddWatchedFolder(context.Background(), "/Music/A")
	if err != nil {
		t.Fatalf("AddWatchedFolder failed: %v", err)
	}

	// Verify child folder was deleted from repo
	if folderRepo.deletedID != "child-id" {
		t.Errorf("Expected child folder to be deleted, got deletedID: %s", folderRepo.deletedID)
	}

	// Verify tracks were NOT deleted (deletedPrefix should be empty)
	if trackRepo.deletedPrefix != "" {
		t.Errorf("Expected tracks NOT to be deleted, but DeleteByPathPrefix was called with prefix: %s", trackRepo.deletedPrefix)
	}

	// Verify new parent folder was added
	wantPath := filepath.Clean("/Music/A")
	found := false
	for _, f := range folderRepo.folders {
		if f.Path == wantPath {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Parent folder %s not found in repo after AddWatchedFolder", wantPath)
	}
}

// TestLibraryService_RemoveWatchedFolder_NotifiesTrackDeletedListeners covers
// the fix for tracks/folders getting deleted while the background analysis
// pool still has them queued: RemoveWatchedFolder must tell listeners
// (wired to AnalysisService.Dequeue in production) which track IDs are gone
// before it starts deleting, so a running analysis pass drops them instead
// of continuing to race the deletion for the same DB writer.
func TestLibraryService_RemoveWatchedFolder_NotifiesTrackDeletedListeners(t *testing.T) {
	trackRepo := &mockTrackRepo{tracks: map[string]*domain.Track{
		"/Music/A/one.mp3": {ID: "track-1", Path: "/Music/A/one.mp3"},
		"/Music/A/two.mp3": {ID: "track-2", Path: "/Music/A/two.mp3"},
	}}
	folderRepo := &mockFolderRepo{
		folders: []*domain.WatchedFolder{{ID: "folder-id", Path: "/Music/A"}},
	}

	s, err := NewLibraryService(
		trackRepo,
		&mockAlbumRepo{},
		&mockArtistRepo{},
		&mockGenreRepo{},
		&mockComposerRepo{},
		&mockPlaylistRepo{},
		folderRepo,
		&mockSettingsRepo{},
		&mockSyncStateRepo{version: currentMetadataSchemaVersion},
		&mockMetadataExtractor{},
		&stubDurationProbe{},
		&mockMetadataWriter{},
		&mockArtworkCache{},
		&mockSearchService{},
		&mockTxManager{},
		&mockDownloadJobs{},
		nil,
		slog.Default(),
	)
	if err != nil {
		t.Fatalf("Failed to create library service: %v", err)
	}
	defer func() { _ = s.Stop(context.Background()) }()

	var notified []string
	s.AddTrackDeletedListener(func(ids []string) {
		notified = append(notified, ids...)
	})

	if err := s.RemoveWatchedFolder(context.Background(), "folder-id", false); err != nil {
		t.Fatalf("RemoveWatchedFolder failed: %v", err)
	}

	if len(notified) != 2 {
		t.Fatalf("expected 2 deleted track IDs notified, got %v", notified)
	}
	seen := map[string]bool{}
	for _, id := range notified {
		seen[id] = true
	}
	if !seen["track-1"] || !seen["track-2"] {
		t.Fatalf("expected track-1 and track-2 notified, got %v", notified)
	}
}

// TestLibraryService_SyncFolder_KeepsRemoteTracks pins the invariant that a
// track whose audio is not on disk (virtual remote:// path, see
// domain.RemotePath) must survive a folder rescan. The missing-file pass is
// scoped by GetByPathPrefix(root), so a remote path never falls inside a scanned
// root — this test guards against a future rewrite that switches to "list all
// tracks, stat each one".
func TestLibraryService_SyncFolder_KeepsRemoteTracks(t *testing.T) {
	tempDir := t.TempDir()

	remotePath := domain.RemotePath("wy", "509781655")
	trackRepo := &mockTrackRepo{tracks: map[string]*domain.Track{
		remotePath: {
			ID:           "remote-1",
			Path:         remotePath,
			Title:        "晴天",
			Availability: domain.AvailabilityRemote,
			Provider:     "wy",
			ExternalID:   "509781655",
		},
	}}

	s, err := NewLibraryService(
		trackRepo,
		&mockAlbumRepo{},
		&mockArtistRepo{},
		&mockGenreRepo{},
		&mockComposerRepo{},
		&mockPlaylistRepo{},
		&mockFolderRepo{},
		&mockSettingsRepo{},
		&mockSyncStateRepo{version: currentMetadataSchemaVersion},
		&mockMetadataExtractor{},
		&stubDurationProbe{},
		&mockMetadataWriter{},
		&mockArtworkCache{},
		&mockSearchService{},
		&mockTxManager{},
		&mockDownloadJobs{},
		nil,
		slog.Default(),
	)
	if err != nil {
		t.Fatalf("Failed to create library service: %v", err)
	}
	defer func() { _ = s.Stop(context.Background()) }()

	if err := s.SyncFolder(context.Background(), tempDir); err != nil {
		t.Fatalf("SyncFolder failed: %v", err)
	}

	if _, ok := trackRepo.tracks[remotePath]; !ok {
		t.Fatalf("remote track %q was pruned by the scan; remote tracks must never be treated as missing files", remotePath)
	}
}

// TestLibraryService_RemoveWatchedFolder_KeepsRemoteTracks covers the other
// deletion path: removing a watched folder deletes by path prefix, so a remote
// track must survive while its local sibling is removed.
func TestLibraryService_RemoveWatchedFolder_KeepsRemoteTracks(t *testing.T) {
	const folder = "/Music/A"
	remotePath := domain.RemotePath("wy", "42")

	trackRepo := &mockTrackRepo{tracks: map[string]*domain.Track{
		"/Music/A/local.mp3": {ID: "local-1", Path: "/Music/A/local.mp3", Availability: domain.AvailabilityReady},
		remotePath: {
			ID:           "remote-42",
			Path:         remotePath,
			Availability: domain.AvailabilityRemote,
			Provider:     "wy",
			ExternalID:   "42",
		},
	}}
	folderRepo := &mockFolderRepo{
		folders: []*domain.WatchedFolder{{ID: "folder-id", Path: folder}},
	}

	s, err := NewLibraryService(
		trackRepo,
		&mockAlbumRepo{},
		&mockArtistRepo{},
		&mockGenreRepo{},
		&mockComposerRepo{},
		&mockPlaylistRepo{},
		folderRepo,
		&mockSettingsRepo{},
		&mockSyncStateRepo{version: currentMetadataSchemaVersion},
		&mockMetadataExtractor{},
		&stubDurationProbe{},
		&mockMetadataWriter{},
		&mockArtworkCache{},
		&mockSearchService{},
		&mockTxManager{},
		&mockDownloadJobs{},
		nil,
		slog.Default(),
	)
	if err != nil {
		t.Fatalf("Failed to create library service: %v", err)
	}
	defer func() { _ = s.Stop(context.Background()) }()

	if err := s.RemoveWatchedFolder(context.Background(), "folder-id", false); err != nil {
		t.Fatalf("RemoveWatchedFolder failed: %v", err)
	}

	if _, ok := trackRepo.tracks[remotePath]; !ok {
		t.Fatalf("remote track %q was deleted with its folder; it has no file on disk to remove", remotePath)
	}
	if _, ok := trackRepo.tracks["/Music/A/local.mp3"]; ok {
		t.Fatal("local track should have been deleted with its folder")
	}
}

// newDeleteFixture builds a service whose only meaningful collaborators are the
// track row, the search index and the download jobs.
func newDeleteFixture(t *testing.T, track *domain.Track) (*LibraryService, *mockTrackRepo, *mockSearchService, *mockDownloadJobs) {
	t.Helper()
	trackRepo := &mockTrackRepo{tracks: map[string]*domain.Track{track.Path: track}}
	search := &mockSearchService{}
	jobs := &mockDownloadJobs{}
	s, err := NewLibraryService(
		trackRepo,
		&mockAlbumRepo{},
		&mockArtistRepo{},
		&mockGenreRepo{},
		&mockComposerRepo{},
		&mockPlaylistRepo{},
		&mockFolderRepo{},
		&mockSettingsRepo{},
		&mockSyncStateRepo{version: currentMetadataSchemaVersion},
		&mockMetadataExtractor{},
		&stubDurationProbe{},
		&mockMetadataWriter{},
		&mockArtworkCache{},
		search,
		&mockTxManager{},
		jobs,
		nil,
		slog.Default(),
	)
	if err != nil {
		t.Fatalf("Failed to create library service: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	return s, trackRepo, search, jobs
}

func TestDeleteTrackFile_RemovesFileRowJobsAndIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "song.mp3")
	if err := os.WriteFile(path, []byte("audio"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	track := &domain.Track{ID: "t1", Path: path, Title: "song", Availability: domain.AvailabilityReady}
	s, trackRepo, search, jobs := newDeleteFixture(t, track)

	if err := s.DeleteTrackFile(context.Background(), "t1"); err != nil {
		t.Fatalf("DeleteTrackFile: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still on disk: %v", err)
	}
	if len(trackRepo.tracks) != 0 {
		t.Fatalf("row still in the library: %+v", trackRepo.tracks)
	}
	if len(jobs.deletedTrackIDs) != 1 || jobs.deletedTrackIDs[0] != "t1" {
		t.Fatalf("download jobs deleted = %v, want [t1]", jobs.deletedTrackIDs)
	}
	if len(search.deletedTrackIDs) != 1 || search.deletedTrackIDs[0] != "t1" {
		t.Fatalf("index docs deleted = %v, want [t1]", search.deletedTrackIDs)
	}

	// Deleting again must not silently succeed: the row is already gone.
	if err := s.DeleteTrackFile(context.Background(), "t1"); err == nil {
		t.Fatal("second delete returned nil, want not-found")
	}
}

func TestVerifyImportedTrack_AcceptsTitleAndArtist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "song.mp3")
	if err := os.WriteFile(path, []byte("audio"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	track := &domain.Track{
		ID: "t1", Path: path, Title: "song", RawArtistNames: "artist",
		Availability: domain.AvailabilityReady,
	}
	s, _, _, _ := newDeleteFixture(t, track)

	if err := s.VerifyImportedTrack(context.Background(), "t1"); err != nil {
		t.Fatalf("VerifyImportedTrack: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a valid import was removed: %v", err)
	}
}

func TestVerifyImportedTrack_DiscardsAnUntaggedImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "song.mp3")
	if err := os.WriteFile(path, []byte("audio"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	track := &domain.Track{
		ID: "t1", Path: path, Title: "song", // no artist: the tag write did not land
		Availability: domain.AvailabilityReady,
	}
	s, trackRepo, _, _ := newDeleteFixture(t, track)

	err := s.VerifyImportedTrack(context.Background(), "t1")
	if !errors.Is(err, domain.ErrDownloadPermanent) {
		t.Fatalf("err = %v, want ErrDownloadPermanent", err)
	}
	if _, ok := trackRepo.tracks[path]; ok {
		t.Fatal("the unreadable import should have been removed from the library")
	}
}

func TestParseFile_FallsBackToTheFileNameWhenTheTagsHaveNoTitle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "前前前世 - RADWIMPS.flac")
	if err := os.WriteFile(path, []byte("audio"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	s, err := NewLibraryService(
		&mockTrackRepo{tracks: map[string]*domain.Track{}},
		&mockAlbumRepo{}, &mockArtistRepo{}, &mockGenreRepo{}, &mockComposerRepo{},
		&mockPlaylistRepo{}, &mockFolderRepo{}, &mockSettingsRepo{},
		&mockSyncStateRepo{version: currentMetadataSchemaVersion},
		&mockMetadataExtractor{emptyTitle: true},
		&stubDurationProbe{},
		&mockMetadataWriter{}, &mockArtworkCache{}, &mockSearchService{},
		&mockTxManager{}, &mockDownloadJobs{}, nil,
		slog.Default(),
	)
	if err != nil {
		t.Fatalf("Failed to create library service: %v", err)
	}
	defer func() { _ = s.Stop(context.Background()) }()

	job, err := s.parseFile(context.Background(), path, &domain.AppSettings{})
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if job.dto.Title != "前前前世" {
		t.Fatalf("title = %q, want the title half of the file name", job.dto.Title)
	}
	if job.dto.RawArtistNames != "RADWIMPS" {
		t.Fatalf("artist = %q, want the artist half of the file name", job.dto.RawArtistNames)
	}
	if job.dto.SortTitle == "" {
		t.Fatal("sort_title must follow the repaired title")
	}
}

func TestParseFile_DurationComesFromTheProbe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "track.flac")
	if err := os.WriteFile(path, []byte("audio"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// taglib reports the raw-container misread; the probe must replace it.
	cases := []struct {
		name  string
		probe domain.MediaDurationProbe
		want  int
	}{
		{
			name: "probe wins",
			probe: &stubDurationProbe{probeFn: func(string) (time.Duration, error) {
				return 271 * time.Second, nil
			}},
			want: 271,
		},
		{
			name:  "probe failure keeps the tagged value",
			probe: &stubDurationProbe{},
			want:  2235,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewLibraryService(
				&mockTrackRepo{tracks: map[string]*domain.Track{}},
				&mockAlbumRepo{}, &mockArtistRepo{}, &mockGenreRepo{}, &mockComposerRepo{},
				&mockPlaylistRepo{}, &mockFolderRepo{}, &mockSettingsRepo{},
				&mockSyncStateRepo{version: currentMetadataSchemaVersion},
				&mockMetadataExtractor{duration: 2235},
				tc.probe,
				&mockMetadataWriter{}, &mockArtworkCache{}, &mockSearchService{},
				&mockTxManager{}, &mockDownloadJobs{}, nil,
				slog.Default(),
			)
			if err != nil {
				t.Fatalf("Failed to create library service: %v", err)
			}
			defer func() { _ = s.Stop(context.Background()) }()

			job, err := s.parseFile(context.Background(), path, &domain.AppSettings{})
			if err != nil {
				t.Fatalf("parseFile: %v", err)
			}
			if job.dto.Duration != tc.want {
				t.Fatalf("duration = %d, want %d", job.dto.Duration, tc.want)
			}
		})
	}
}

func TestTitleAndArtistFromPath_HonoursTheNamingOrder(t *testing.T) {
	title, artist := titleAndArtistFromPath("/music/晴天 - 周杰伦.mp3", domain.NamingOrderTitleArtist)
	if title != "晴天" || artist != "周杰伦" {
		t.Fatalf("title-artist split = %q/%q", title, artist)
	}
	title, artist = titleAndArtistFromPath("/music/周杰伦 - 晴天.mp3", domain.NamingOrderArtistTitle)
	if title != "晴天" || artist != "周杰伦" {
		t.Fatalf("artist-title split = %q/%q", title, artist)
	}
	title, artist = titleAndArtistFromPath("/music/Untitled.mp3", domain.DefaultNamingOrder)
	if title != "Untitled" || artist != "" {
		t.Fatalf("no-separator split = %q/%q", title, artist)
	}
}

func TestDeleteTrackFile_RefusesRemoteTracks(t *testing.T) {
	track := &domain.Track{
		ID: "remote-1", Path: "remote://wy/123", Title: "remote",
		Availability: domain.AvailabilityDownloading,
	}
	s, _, _, _ := newDeleteFixture(t, track)

	err := s.DeleteTrackFile(context.Background(), "remote-1")
	if err == nil || !strings.Contains(err.Error(), "no local file") {
		t.Fatalf("DeleteTrackFile = %v, want a no-local-file refusal", err)
	}
}

func TestLibraryService_RepairDurations(t *testing.T) {
	// Paths are labels only: the probe is mocked, so this test needs no real files
	// and stays independent of the machine's temporary-directory rules.
	readablePath := "/music/readable.flac"
	brokenPath := "/music/broken.flac"
	highPath := "/music/too-long.flac"
	healthyPath := "/music/healthy.mp3"
	remotePath := domain.RemotePath("wy", "remote-1")

	trackRepo := &mockTrackRepo{tracks: map[string]*domain.Track{
		readablePath: {
			ID: "readable", Path: readablePath, Title: "Readable",
			Duration: 1, Availability: domain.AvailabilityReady,
		},
		brokenPath: {
			ID: "broken", Path: brokenPath, Title: "Broken",
			Duration: 1, Availability: domain.AvailabilityReady,
		},
		highPath: {
			ID: "high", Path: highPath, Title: "TooLong",
			Duration: 2235, Availability: domain.AvailabilityReady,
		},
		healthyPath: {
			ID: "healthy", Path: healthyPath, Title: "Healthy",
			Duration: 284, Availability: domain.AvailabilityReady,
		},
		remotePath: {
			ID: "remote", Path: remotePath, Title: "Remote", Duration: 1,
			Availability: domain.AvailabilityRemote, Provider: "wy", ExternalID: "remote-1",
		},
	}}

	probe := &stubDurationProbe{probeFn: func(path string) (time.Duration, error) {
		switch path {
		case readablePath:
			return 284 * time.Second, nil
		case highPath:
			return 271 * time.Second, nil
		case healthyPath:
			return 284 * time.Second, nil
		case brokenPath:
			return 0, errors.New("cannot read the container")
		default:
			t.Fatalf("RepairDurations probed an unexpected file: %s", path)
			return 0, nil
		}
	}}

	s, err := NewLibraryService(
		trackRepo,
		&mockAlbumRepo{},
		&mockArtistRepo{},
		&mockGenreRepo{},
		&mockComposerRepo{},
		&mockPlaylistRepo{},
		&mockFolderRepo{},
		&mockSettingsRepo{},
		&mockSyncStateRepo{version: currentMetadataSchemaVersion},
		&mockMetadataExtractor{},
		probe,
		&mockMetadataWriter{},
		&mockArtworkCache{},
		&mockSearchService{},
		&mockTxManager{},
		&mockDownloadJobs{},
		nil,
		slog.Default(),
	)
	if err != nil {
		t.Fatalf("NewLibraryService: %v", err)
	}
	defer func() { _ = s.Stop(context.Background()) }()

	result, err := s.RepairDurations(context.Background())
	if err != nil {
		t.Fatalf("RepairDurations: %v", err)
	}
	if result.Repaired != 2 {
		t.Fatalf("Repaired = %d, want 2 (short and too-long rows)", result.Repaired)
	}
	if len(result.Unreadable) != 1 || result.Unreadable[0].TrackID != "broken" {
		t.Fatalf("Unreadable = %+v, want only the unreadable file", result.Unreadable)
	}
	if got := trackRepo.tracks[readablePath].Duration; got != 284 {
		t.Fatalf("readable duration = %d, want 284", got)
	}
	if got := trackRepo.tracks[highPath].Duration; got != 271 {
		t.Fatalf("too-long duration = %d, want the probed 271", got)
	}
	if got := trackRepo.tracks[brokenPath].Duration; got != 1 {
		t.Fatalf("unreadable duration = %d, want the untouched 1", got)
	}
	if got := trackRepo.tracks[healthyPath].Duration; got != 284 {
		t.Fatalf("healthy duration = %d, want 284", got)
	}
	if probe.calls.Load() != 4 {
		t.Fatalf("probe calls = %d, want every local row", probe.calls.Load())
	}
}
