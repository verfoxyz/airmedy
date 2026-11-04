package sqlite

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"

	"airmedy/internal/domain"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

func TestSqliteRepositories(t *testing.T) {
	dbPath := "test.db"
	defer func() { _ = os.Remove(dbPath) }()

	db, err := NewDB(dbPath, slog.Default())
	if err != nil {
		t.Fatalf("Failed to create test db: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	trackRepo := NewTrackRepository(db)

	track := &domain.Track{
		ID:            "test-1",
		Path:          "/path/to/test.mp3",
		Title:         "Test Track",
		SortTitle:     "Test Track",
		Copyright:     "Test Copyright",
		OtherMetadata: `{"test":"meta"}`,
		Format:        "mp3",
		AlbumID:       "",
	}

	err = trackRepo.Save(ctx, track)
	if err != nil {
		t.Fatalf("Failed to save track: %v", err)
	}

	savedTrackDTO, err := trackRepo.GetByID(ctx, "test-1")
	if err != nil {
		t.Fatalf("Failed to get track: %v", err)
	}
	if savedTrackDTO.Title != "Test Track" {
		t.Errorf("Expected title 'Test Track', got '%s'", savedTrackDTO.Title)
	}
	if savedTrackDTO.Copyright != "Test Copyright" {
		t.Errorf("Expected copyright 'Test Copyright', got '%s'", savedTrackDTO.Copyright)
	}
	if savedTrackDTO.OtherMetadata != `{"test":"meta"}` {
		t.Errorf("Expected other_metadata '{\"test\":\"meta\"}', got '%s'", savedTrackDTO.OtherMetadata)
	}

	// Test Upsert
	track.Title = "Updated Track"
	err = trackRepo.Upsert(ctx, track)
	if err != nil {
		t.Fatalf("Failed to upsert track: %v", err)
	}

	updatedTrackDTO, _ := trackRepo.GetByID(ctx, "test-1")
	if updatedTrackDTO.Title != "Updated Track" {
		t.Errorf("Expected title 'Updated Track', got '%s'", updatedTrackDTO.Title)
	}

	// Test Album Copyright
	albumRepo := NewAlbumRepository(db)
	album := &domain.Album{
		ID:        "test-album-1",
		Title:     "Test Album",
		SortTitle: "Test Album",
		Copyright: "Album Copyright",
	}
	err = albumRepo.Save(ctx, album)
	if err != nil {
		t.Fatalf("Failed to save album: %v", err)
	}

	savedAlbumDTO, err := albumRepo.GetByID(ctx, "test-album-1")
	if err != nil {
		t.Fatalf("Failed to get album: %v", err)
	}
	if savedAlbumDTO.Copyright != "Album Copyright" {
		t.Errorf("Expected album copyright 'Album Copyright', got '%s'", savedAlbumDTO.Copyright)
	}

	// Test SettingsRepository
	settingsRepo := NewSettingsRepository(db)
	settings := &domain.AppSettings{
		Language:     "fr",
		Theme:        "dark",
		PrimaryColor: "#3B82F6",
		EQEnabled:    false,
		EQPreamp:     -3.5,
	}
	err = settingsRepo.Save(ctx, settings)
	if err != nil {
		t.Fatalf("Failed to save settings: %v", err)
	}

	savedSettings, err := settingsRepo.Load(ctx)
	if err != nil {
		t.Fatalf("Failed to load settings: %v", err)
	}
	if savedSettings.Language != "fr" {
		t.Errorf("Expected language 'fr', got '%s'", savedSettings.Language)
	}
	if savedSettings.Theme != "dark" {
		t.Errorf("Expected theme 'dark', got '%s'", savedSettings.Theme)
	}
	if savedSettings.PrimaryColor != "#3B82F6" {
		t.Errorf("Expected primary color #3B82F6, got %q", savedSettings.PrimaryColor)
	}
	if savedSettings.EQEnabled != false {
		t.Errorf("Expected EQEnabled false, got %v", savedSettings.EQEnabled)
	}
	if savedSettings.EQPreamp != -3.5 {
		t.Errorf("Expected EQPreamp -3.5, got %v", savedSettings.EQPreamp)
	}

	var profilePreampColumns int
	if err := db.Get(&profilePreampColumns, `SELECT COUNT(*) FROM pragma_table_info('eq_profiles') WHERE name = 'preamp_gain'`); err != nil {
		t.Fatalf("failed to inspect eq_profiles schema: %v", err)
	}
	if profilePreampColumns != 0 {
		t.Errorf("expected eq_profiles to contain only profile metadata and bands, found preamp_gain")
	}
	var profileKeyColumns int
	if err := db.Get(&profileKeyColumns, `SELECT COUNT(*) FROM pragma_table_info('eq_profiles') WHERE name = 'preset_key'`); err != nil {
		t.Fatalf("failed to inspect eq_profiles key schema: %v", err)
	}
	if profileKeyColumns != 1 {
		t.Errorf("expected eq_profiles.preset_key column, got %d", profileKeyColumns)
	}
}

func TestTrackFeaturesMigration(t *testing.T) {
	dbPath := "test_features.db"
	defer func() { _ = os.Remove(dbPath) }()

	db, err := NewDB(dbPath, slog.Default())
	if err != nil {
		t.Fatalf("Failed to create test db: %v", err)
	}
	defer func() { _ = db.Close() }()

	// track_features table exists with expected analyzer columns.
	var count int
	if err := db.Get(&count, `SELECT COUNT(*) FROM pragma_table_info('track_features') WHERE name IN ('loudness_lufs','spectral_centroid','tempo','onset_variance')`); err != nil {
		t.Fatalf("track_features table not created: %v", err)
	}
	if count != 4 {
		t.Errorf("Expected 4 known track_features columns, got %d", count)
	}

	// tracks.analyzed_version pending marker exists.
	if err := db.Get(&count, `SELECT COUNT(*) FROM pragma_table_info('tracks') WHERE name = 'analyzed_version'`); err != nil || count != 1 {
		t.Errorf("tracks.analyzed_version missing (count=%d, err=%v)", count, err)
	}
}

func TestAnalysisComponentVersionsMigration(t *testing.T) {
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer func() { _ = db.Close() }()
	var count int
	if err := db.Get(&count, `SELECT COUNT(*) FROM pragma_table_info('track_analysis_components') WHERE name IN ('track_id', 'component', 'version', 'status')`); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("component analysis table columns: got %d want 4", count)
	}
	if err := db.Get(&count, `SELECT COUNT(*) FROM pragma_index_list('tracks') WHERE name = 'idx_tracks_analysis_pending_backfill'`); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("pending analysis backfill index missing (count=%d)", count)
	}
}

func TestAnalysisComponentVersionsMigrationBackfillsOnlyV4(t *testing.T) {
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	tracks := NewTrackRepository(db)
	for _, id := range []string{"current", "failed", "old"} {
		if err := tracks.Save(ctx, &domain.Track{ID: id, Path: "/m/" + id, Title: id, Format: "mp3"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE tracks SET analyzed_version = 4 WHERE id IN ('current', 'failed')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tracks SET analyzed_version = 3 WHERE id = 'old'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO track_features (track_id, analyzer_version) VALUES ('current', 4)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE track_analysis_components`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("migrations/000051_analysis_component_versions.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	var currentComplete, failedRows, oldRows int
	if err := db.Get(&currentComplete, `SELECT COUNT(*) FROM track_analysis_components WHERE track_id = 'current' AND version = 1 AND status = 'complete'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Get(&failedRows, `SELECT COUNT(*) FROM track_analysis_components WHERE track_id = 'failed' AND version = 1 AND status = 'failed'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Get(&oldRows, `SELECT COUNT(*) FROM track_analysis_components WHERE track_id = 'old'`); err != nil {
		t.Fatal(err)
	}
	if currentComplete != 2 || failedRows != 2 || oldRows != 0 {
		t.Fatalf("unexpected backfill: current=%d failed=%d old=%d", currentComplete, failedRows, oldRows)
	}
}

func TestNormalizationSettingsRoundTrip(t *testing.T) {
	dbPath := "test_norm.db"
	defer func() { _ = os.Remove(dbPath) }()

	db, err := NewDB(dbPath, slog.Default())
	if err != nil {
		t.Fatalf("Failed to create test db: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	repo := NewSettingsRepository(db)

	// Empty table => normalization defaults.
	def, err := repo.Load(ctx)
	if err != nil {
		t.Fatalf("Failed to load default settings: %v", err)
	}
	if def.NormalizationEnabled != false || def.NormalizationMode != "track" ||
		def.NormalizationTargetLUFS != domain.DefaultTargetLUFS || def.NormalizationPreventClip != true {
		t.Errorf("Unexpected normalization defaults: %+v", def)
	}
	if def.LibraryAnalysisEnabled != false {
		t.Errorf("Expected LibraryAnalysisEnabled default false, got %+v", def)
	}
	if def.HighContrastLyrics != true {
		t.Errorf("Expected HighContrastLyrics default true, got %+v", def)
	}
	if def.LyricsGlow {
		t.Errorf("Expected LyricsGlow default false, got %+v", def)
	}
	if def.LivingArtworkBackground != true {
		t.Errorf("Expected LivingArtworkBackground default true, got %+v", def)
	}
	if def.RomanizationEnabled != true {
		t.Errorf("Expected RomanizationEnabled default true, got %+v", def)
	}
	if def.AutoAdvanceNotificationsEnabled != true {
		t.Errorf("Expected AutoAdvanceNotificationsEnabled default true, got %+v", def)
	}

	// Non-default values round-trip.
	in := &domain.AppSettings{
		Language:                        "en",
		Theme:                           "dark",
		LibraryAnalysisEnabled:          true,
		NormalizationEnabled:            true,
		NormalizationMode:               "album",
		NormalizationTargetLUFS:         -18,
		NormalizationPreventClip:        false,
		HighContrastLyrics:              false,
		LyricsGlow:                      true,
		LivingArtworkBackground:         false,
		RomanizationEnabled:             false,
		AutoAdvanceNotificationsEnabled: false,
	}
	if err := repo.Save(ctx, in); err != nil {
		t.Fatalf("Failed to save settings: %v", err)
	}
	out, err := repo.Load(ctx)
	if err != nil {
		t.Fatalf("Failed to load settings: %v", err)
	}
	if out.NormalizationEnabled != true || out.NormalizationMode != "album" ||
		out.NormalizationTargetLUFS != -18 || out.NormalizationPreventClip != false {
		t.Errorf("Normalization round-trip mismatch: %+v", out)
	}
	if out.LibraryAnalysisEnabled != true {
		t.Errorf("LibraryAnalysisEnabled round-trip mismatch: %+v", out)
	}
	if out.HighContrastLyrics != false {
		t.Errorf("HighContrastLyrics round-trip mismatch: %+v", out)
	}
	if !out.LyricsGlow {
		t.Errorf("LyricsGlow round-trip mismatch: %+v", out)
	}
	if out.LivingArtworkBackground != false {
		t.Errorf("LivingArtworkBackground round-trip mismatch: %+v", out)
	}
	if out.RomanizationEnabled != false {
		t.Errorf("RomanizationEnabled round-trip mismatch: %+v", out)
	}
	if out.AutoAdvanceNotificationsEnabled != false {
		t.Errorf("AutoAdvanceNotificationsEnabled round-trip mismatch: %+v", out)
	}
}

// TestRemoteTracksMigration_UpDownUp verifies 000077 is reversible: the down
// path must drop the three columns and their indexes, and re-applying up must
// restore them. Migrations are otherwise only ever exercised upwards.
func TestRemoteTracksMigration_UpDownUp(t *testing.T) {
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("Failed to create test db: %v", err)
	}
	defer func() { _ = db.Close() }()

	countRemoteCols := func() int {
		var n int
		if err := db.Get(&n, `SELECT COUNT(*) FROM pragma_table_info('tracks') WHERE name IN ('availability','provider','external_id')`); err != nil {
			t.Fatalf("failed to inspect tracks columns: %v", err)
		}
		return n
	}

	driver, err := sqlite3.WithInstance(db.DB.DB, &sqlite3.Config{})
	if err != nil {
		t.Fatalf("failed to create migrate driver: %v", err)
	}
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("failed to create migration source: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite3", driver)
	if err != nil {
		t.Fatalf("failed to create migrator: %v", err)
	}

	// Pin the schema to 000077 instead of assuming it is the latest version:
	// later migrations must not make this test silently step down the wrong one.
	const remoteTracksVersion = 77
	if err := m.Migrate(remoteTracksVersion); err != nil {
		t.Fatalf("failed to migrate to %d: %v", remoteTracksVersion, err)
	}
	if got := countRemoteCols(); got != 3 {
		t.Fatalf("expected 3 remote columns after up, got %d", got)
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("down migration failed: %v", err)
	}
	if got := countRemoteCols(); got != 0 {
		t.Fatalf("expected 0 remote columns after down, got %d", got)
	}

	// Two steps: back to 79, then through 81 (lx sources) and 82 (plugin KV) to
	// 83, whose columns the repository reads.
	if err := m.Steps(2); err != nil {
		t.Fatalf("re-applying up migration failed: %v", err)
	}
	if got := countRemoteCols(); got != 3 {
		t.Fatalf("expected 3 remote columns after re-up, got %d", got)
	}
}

// TestDownloadSettingsMigration_UpDownUp verifies 000079 is reversible and that
// removing the columns really rolls back: a settings row written under 79 must
// still load after the down/up cycle, with the download columns reset to their
// defaults rather than taking the rest of the row with them.
func TestDownloadSettingsMigration_UpDownUp(t *testing.T) {
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("Failed to create test db: %v", err)
	}
	defer func() { _ = db.Close() }()

	countDownloadCols := func() int {
		var n int
		if err := db.Get(&n, `SELECT COUNT(*) FROM pragma_table_info('app_settings')
			WHERE name IN ('download_dir','download_quality','auto_download_on_playlist_add')`); err != nil {
			t.Fatalf("failed to inspect app_settings columns: %v", err)
		}
		return n
	}

	ctx := context.Background()
	repo := NewSettingsRepository(db)

	driver, err := sqlite3.WithInstance(db.DB.DB, &sqlite3.Config{})
	if err != nil {
		t.Fatalf("failed to create migrate driver: %v", err)
	}
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("failed to create migration source: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite3", driver)
	if err != nil {
		t.Fatalf("failed to create migrator: %v", err)
	}

	const downloadSettingsVersion = 79
	// A fresh database starts at the newest version, so this walks *down* to 79.
	// The repository queries the newest columns, so it is only exercised again
	// once the schema is back at the top (see the last block).
	if err := m.Migrate(downloadSettingsVersion); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("failed to migrate to %d: %v", downloadSettingsVersion, err)
	}
	if got := countDownloadCols(); got != 3 {
		t.Fatalf("expected 3 download columns after up, got %d", got)
	}

	// Write the settings row at plain SQL level while the 79 columns exist.
	// The repository cannot be used here: it queries the newest schema, which
	// this database is deliberately rolled back to 79 below.
	if _, err := db.ExecContext(ctx, `INSERT INTO app_settings (id, download_dir, download_quality, auto_download_on_playlist_add)
		VALUES (1, '/tmp/a', 'flac', 1)`); err != nil {
		t.Fatalf("failed to seed the settings row: %v", err)
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("down migration failed: %v", err)
	}
	if got := countDownloadCols(); got != 0 {
		t.Fatalf("expected 0 download columns after down, got %d", got)
	}
	// The repository queries the newest schema, so it cannot be used to read a
	// rolled-back database; the point here is that DROP COLUMN removes the three
	// columns and leaves the rest of the single settings row intact.
	var language string
	if err := db.Get(&language, `SELECT language FROM app_settings WHERE id = 1`); err != nil {
		t.Fatalf("settings row did not survive the down migration: %v", err)
	}

	if err := m.Steps(1); err != nil {
		t.Fatalf("re-applying up migration failed: %v", err)
	}
	if got := countDownloadCols(); got != 3 {
		t.Fatalf("expected 3 download columns after re-up, got %d", got)
	}

	// Back to the newest schema: 000082 (plugin KV) and 000083 (download content
	// switches) both follow, and the repository reads the new columns.
	if version, _, versionErr := m.Version(); versionErr != nil {
		t.Fatalf("failed to read the schema version: %v", versionErr)
	} else if version < 83 {
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			t.Fatalf("failed to migrate from %d to the newest schema: %v", version, err)
		}
	}

	if version, _, versionErr := m.Version(); versionErr != nil || version < 83 {
		t.Fatalf("before repo.Load: version %d (err %v), want >= 83", version, versionErr)
	}
	var colCount int
	if err := db.Get(&colCount, `SELECT COUNT(*) FROM pragma_table_info('app_settings')`); err != nil {
		t.Fatalf("count app_settings columns: %v", err)
	}
	t.Logf("app_settings columns: %d", colCount)

	settings, err := repo.Load(ctx)
	if err != nil {
		t.Fatalf("failed to load defaults: %v", err)
	}
	settings.DownloadDir = t.TempDir()
	settings.DownloadQuality = domain.QualityFlac
	settings.AutoDownloadOnPlaylistAdd = true
	settings.DownloadWithLyrics = true
	settings.DownloadWithLyricTranslation = true
	settings.DownloadWithCover = false
	if err := repo.Save(ctx, settings); err != nil {
		t.Fatalf("failed to save download settings: %v", err)
	}

	reloaded, err := repo.Load(ctx)
	if err != nil {
		t.Fatalf("failed to reload settings: %v", err)
	}
	if !reloaded.DownloadWithLyrics || !reloaded.DownloadWithLyricTranslation {
		t.Fatalf("lyric switches did not round-trip: %+v", reloaded)
	}
	if reloaded.DownloadWithCover {
		t.Fatalf("DownloadWithCover = true after saving false")
	}

	// The defaults a fresh install sees: no lyrics, no translation, cover on.
	fresh, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("failed to create fresh db: %v", err)
	}
	defer func() { _ = fresh.Close() }()
	freshSettings, err := NewSettingsRepository(fresh).Load(ctx)
	if err != nil {
		t.Fatalf("failed to load fresh defaults: %v", err)
	}
	if freshSettings.DownloadWithLyrics || freshSettings.DownloadWithLyricTranslation {
		t.Fatalf("lyric switches default to off, got %+v", freshSettings)
	}
	if !freshSettings.DownloadWithCover {
		t.Fatalf("DownloadWithCover defaults to on, got %+v", freshSettings)
	}
}

// TestPathPrefixQueries_RejectEmptyPrefix guards the latent "LIKE '%'" bug:
// with an empty prefix both queries stop being scoped to a folder —
// GetByPathPrefix would return the whole library (including virtual remote://
// rows) and DeleteByPathPrefix would delete every track.
func TestPathPrefixQueries_RejectEmptyPrefix(t *testing.T) {
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("Failed to create test db: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	repo := NewTrackRepository(db)

	localPath := "/music/a.mp3"
	remotePath := domain.RemotePath("wy", "1")
	for _, tr := range []*domain.Track{
		{ID: "local-1", Path: localPath, Title: "local", SortTitle: "local"},
		{
			ID: "remote-1", Path: remotePath, Title: "remote", SortTitle: "remote",
			Availability: domain.AvailabilityRemote, Provider: "wy", ExternalID: "1",
		},
	} {
		if err := repo.Upsert(ctx, tr); err != nil {
			t.Fatalf("failed to seed track %s: %v", tr.ID, err)
		}
	}

	got, err := repo.GetByPathPrefix(ctx, "")
	if err != nil {
		t.Fatalf("GetByPathPrefix(\"\") returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("GetByPathPrefix(\"\") returned %d tracks, want 0", len(got))
	}

	if err := repo.DeleteByPathPrefix(ctx, ""); err == nil {
		t.Fatal("DeleteByPathPrefix(\"\") must fail instead of deleting the whole library")
	}

	for _, id := range []string{"local-1", "remote-1"} {
		if tr, err := repo.GetByID(ctx, id); err != nil {
			t.Fatalf("failed to read track %s: %v", id, err)
		} else if tr == nil {
			t.Fatalf("track %s was deleted by an empty-prefix delete", id)
		}
	}
}

// TestRepairRemoteReadyRowsMigration verifies 000084 repairs a library where a
// finished download left the remote row in place with 'ready' but still pointing
// at the virtual remote:// path.
//
// That row is the one behind "ma_player_load failed: -10": the identity and the
// user's associations (favorite, play count, playlist entry) end up on the row
// that owns the file, and the leftover is gone.
func TestRepairRemoteReadyRowsMigration(t *testing.T) {
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("Failed to create test db: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	files := NewTrackRepository(db)

	const (
		remoteID = "remote-1"
		localID  = "local-1"
		filePath = `D:\Music\屋顶.mp3`
	)

	// The downloaded file, imported without an identity.
	if err := files.Upsert(ctx, &domain.Track{
		ID: localID, Path: filePath, Title: "屋顶", SortTitle: "屋顶",
		Duration: 319, Availability: domain.AvailabilityReady,
	}); err != nil {
		t.Fatalf("seed the imported row: %v", err)
	}
	// The leftover remote row: 'ready' but still the virtual path, carrying the
	// user's favorite and a completed job pointing at the real file.
	if err := files.Upsert(ctx, &domain.Track{
		ID: remoteID, Path: domain.RemotePath("wy", "5257138"), Title: "屋顶",
		SortTitle: "屋顶", Duration: 319, Availability: domain.AvailabilityReady,
		Provider: "wy", ExternalID: "5257138",
	}); err != nil {
		t.Fatalf("seed the remote row: %v", err)
	}
	if err := files.SetFavorite(ctx, remoteID, true); err != nil {
		t.Fatalf("favorite the remote row: %v", err)
	}
	if err := files.IncrementPlayCount(ctx, remoteID); err != nil {
		t.Fatalf("play the remote row: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO download_jobs (id, track_id, state, quality, target_path)
		VALUES ('job-1', ?, 'done', '320k', ?)`, remoteID, filePath); err != nil {
		t.Fatalf("seed the finished job: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO playlists (id, name) VALUES ('pl-1', 'pl-1')`); err != nil {
		t.Fatalf("seed the playlist: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO playlist_tracks (playlist_id, track_id, position)
		VALUES ('pl-1', ?, '0|hzzzzz:')`, remoteID); err != nil {
		t.Fatalf("seed the playlist entry: %v", err)
	}

	// Apply the repair exactly as the migration runner would.
	repair, err := os.ReadFile("migrations/000084_repair_remote_ready_rows.up.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(repair)); err != nil {
		t.Fatalf("apply the repair: %v", err)
	}

	// The leftover row is gone and nothing points at a virtual path any more.
	left, err := files.GetByID(ctx, remoteID)
	if err != nil {
		t.Fatalf("look up the leftover row: %v", err)
	}
	if left != nil {
		t.Fatalf("the leftover remote row still exists: %+v", left)
	}
	rows, err := files.GetAll(ctx)
	if err != nil {
		t.Fatalf("list tracks: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("tracks = %d, want exactly one row for the song", len(rows))
	}
	row := rows[0]
	if row.IsRemote() {
		t.Fatalf("the surviving row is still remote: path=%q availability=%q", row.Path, row.Availability)
	}
	if row.Provider != "wy" || row.ExternalID != "5257138" {
		t.Fatalf("identity = %s/%s, want wy/5257138", row.Provider, row.ExternalID)
	}
	if !row.IsFavorite {
		t.Fatal("the favorite did not move onto the surviving row")
	}
	if row.PlayCount != 1 {
		t.Fatalf("play count = %d, want 1", row.PlayCount)
	}

	// The playlist entry follows the song.
	var members []string
	if err := db.SelectContext(ctx, &members,
		`SELECT track_id FROM playlist_tracks WHERE playlist_id = 'pl-1'`); err != nil {
		t.Fatalf("read the playlist: %v", err)
	}
	if len(members) != 1 || members[0] != localID {
		t.Fatalf("playlist members = %v, want [%s]", members, localID)
	}
}
