package download_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"airmedy/internal/app/download"
	"airmedy/internal/domain"
	"airmedy/internal/infra/sqlite"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fixtures ---------------------------------------------------------------

type stubResolver struct {
	mu    sync.Mutex
	calls int
	ref   domain.StreamRef
	err   error
	// refs, when set, is returned in order and then ref.
	refs []domain.StreamRef
}

func (r *stubResolver) Resolve(context.Context, *domain.TrackDTO, string) (domain.StreamRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return domain.StreamRef{}, r.err
	}
	if len(r.refs) >= r.calls {
		return r.refs[r.calls-1], nil
	}
	return r.ref, nil
}

func (r *stubResolver) CallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type stubDownloader struct {
	mu         sync.Mutex
	errs       []error
	bytes      int64
	callsCount int
	// lastRef is the last resolved stream handed to the mover, so tests can
	// assert the plugin's headers survived routing.
	lastRef domain.StreamRef
}

func (d *stubDownloader) Download(_ context.Context, _ *domain.TrackDTO, ref domain.StreamRef, target domain.DownloadTarget, progress domain.DownloadProgressFunc) (domain.DownloadResult, error) {
	d.mu.Lock()
	d.callsCount++
	d.lastRef = ref
	var err error
	if len(d.errs) > 0 {
		err = d.errs[0]
		d.errs = d.errs[1:]
	}
	d.mu.Unlock()
	if progress != nil {
		progress(1, 2)
		progress(2, 2)
	}
	if err != nil {
		return domain.DownloadResult{}, err
	}
	return domain.DownloadResult{Path: filepath.Join(target.Dir, target.Name), Container: "mp3", Bytes: d.bytes}, nil
}

func (d *stubDownloader) PartPath(dir, name string) string {
	return filepath.Join(dir, ".tmp", name+".part")
}
func (d *stubDownloader) TmpDir(dir string) string { return filepath.Join(dir, ".tmp") }

type stubFinalizer struct {
	mu    sync.Mutex
	calls int
	err   error
	// onFinalize, when set, replaces the canned result so a test can hand back a
	// different imported row (the merge regression where the remote row is
	// replaced by the download).
	onFinalize func(in download.FinalizeInput) (download.FinalizeResult, error)
}

func (f *stubFinalizer) Finalize(_ context.Context, in download.FinalizeInput) (download.FinalizeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return download.FinalizeResult{}, f.err
	}
	if f.onFinalize != nil {
		return f.onFinalize(in)
	}
	return download.FinalizeResult{
		TrackID:         in.Track.ID,
		TargetPath:      filepath.Join(in.Root, "artist", "album", "01 track.mp3"),
		DurationSeconds: 200,
		Bitrate:         320,
	}, nil
}

type stubSettings struct {
	dir        string
	autoAdd    bool
	withLyrics bool
	withCover  bool
}

// stubDurationProbe stands in for the ffmpeg container probe. Its default is an
// error, so tests that predate the probe keep exercising the taglib fallback.
type stubDurationProbe struct {
	mu       sync.Mutex
	duration time.Duration
	err      error
	calls    int
}

func (p *stubDurationProbe) Probe(context.Context, string) (time.Duration, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.err != nil {
		return 0, p.err
	}
	return p.duration, nil
}

func (s stubSettings) GetSettings(context.Context) (*domain.AppSettings, error) {
	return &domain.AppSettings{
		DownloadDir:               s.dir,
		DownloadQuality:           domain.Quality320k,
		AutoDownloadOnPlaylistAdd: s.autoAdd,
		DownloadWithLyrics:        s.withLyrics,
		DownloadWithCover:         s.withCover,
	}, nil
}

// stubLyrics records the row it was asked to fetch lyrics for.
type stubLyrics struct {
	mu    sync.Mutex
	calls int
	ids   []string
	err   error
}

func (s *stubLyrics) FetchForTrack(_ context.Context, track *domain.TrackDTO) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if track != nil {
		s.ids = append(s.ids, track.ID)
	}
	return s.err
}

type recordingSink struct {
	mu       sync.Mutex
	progress []*domain.DownloadJob
	done     []*domain.DownloadJob
	failed   []*domain.DownloadJob
}

func (s *recordingSink) DownloadProgress(j *domain.DownloadJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.progress = append(s.progress, j)
}

func (s *recordingSink) DownloadDone(j *domain.DownloadJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = append(s.done, j)
}

func (s *recordingSink) DownloadFailed(j *domain.DownloadJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = append(s.failed, j)
}

func (s *recordingSink) counts() (progress, done, failed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.progress), len(s.done), len(s.failed)
}

type harness struct {
	t          *testing.T
	settings   stubSettings
	svc        *download.Service
	db         *sqlite.DB
	jobs       domain.DownloadJobRepository
	tracks     domain.TrackRepository
	resolver   *stubResolver
	downloader *stubDownloader
	finalizer  *stubFinalizer
	probe      *stubDurationProbe
	sink       *recordingSink
	dir        string
	// resolverOverride/downloaderOverride let integration tests drive the same
	// worker through the composite (plugin) resolver and the real downloader
	// without forking the fixture.
	resolverOverride   domain.StreamResolver
	downloaderOverride domain.StreamDownloader
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, err := sqlite.NewDB(":memory:", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	// ":memory:" gives every pooled connection its own empty database, so the
	// worker's transactions (which run on whichever connection is free) would
	// not see the tables. One connection keeps the test database consistent.
	// Production uses a file and does not have this constraint.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	tracks := sqlite.NewTrackRepository(db)
	dir := t.TempDir()

	h := &harness{
		t:          t,
		db:         db,
		jobs:       sqlite.NewDownloadJobRepository(db),
		tracks:     tracks,
		resolver:   &stubResolver{ref: domain.StreamRef{URL: "http://cdn.example/1.mp3"}},
		downloader: &stubDownloader{bytes: 1024},
		finalizer:  &stubFinalizer{},
		probe:      &stubDurationProbe{err: errors.New("probe unavailable in this fixture")},
		sink:       &recordingSink{},
		dir:        dir,
	}
	// Verbose on purpose while the worker is under test: a swallowed error is
	// indistinguishable from a timeout otherwise.
	h.settings = stubSettings{dir: dir}
	h.rebuild()
	return h
}

// setAutoDownload flips the auto-download switch the service reads.
// seedPlaylist creates a playlist row so a job's playlist_id foreign key holds.
func (h *harness) seedPlaylist(t *testing.T, id string) {
	t.Helper()
	require.NoError(t, sqlite.NewPlaylistRepository(h.db).Save(context.Background(), &domain.Playlist{ID: id, Name: id}))
}

// rebuild constructs the service from the harness state, so a test can change
// the settings between runs.
func (h *harness) rebuild() {
	logger := slog.New(slog.NewTextHandler(testWriter{h.t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	resolver := domain.StreamResolver(h.resolver)
	if h.resolverOverride != nil {
		resolver = h.resolverOverride
	}
	var mover download.AudioDownloader = h.downloader
	if h.downloaderOverride != nil {
		mover = h.downloaderOverride
	}
	h.svc = download.NewService(
		h.jobs, h.tracks, resolver, mover, h.finalizer,
		h.probe,
		download.NewMerger(h.tracks, sqlite.NewDownloadMergeRepository(h.db, h.tracks), sqlite.NewTxManager(h.db), logger),
		h.settings, logger,
	)
	h.svc.SetEventSink(h.sink)
}

// useResolver swaps in a resolver (such as the provider router) and rebuilds the
// service around it.
func (h *harness) useResolver(resolver domain.StreamResolver) {
	h.resolverOverride = resolver
	h.rebuild()
}

// useDownloader swaps in a byte mover (such as the real HTTP downloader).
func (h *harness) useDownloader(downloader domain.StreamDownloader) {
	h.downloaderOverride = downloader
	h.rebuild()
}

func (h *harness) setAutoDownload(enabled bool) {
	h.settings = stubSettings{dir: h.dir, autoAdd: enabled}
	h.rebuild()
}

func (h *harness) seedRemote(t *testing.T, id string) *domain.TrackDTO {
	t.Helper()
	require.NoError(t, h.tracks.Upsert(context.Background(), &domain.Track{
		ID:           id,
		Path:         domain.RemotePath("wy", id),
		Title:        "晴天",
		Duration:     200,
		Availability: domain.AvailabilityRemote,
		Provider:     "wy",
		ExternalID:   id,
	}))
	tr, err := h.tracks.GetByID(context.Background(), id)
	require.NoError(t, err)
	return tr
}

// waitFor polls until cond holds or the deadline passes. Tests drive the real
// worker, so they wait on observable state rather than sleeping.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (h *harness) jobState(t *testing.T, id string) *domain.DownloadJob {
	t.Helper()
	job, err := h.jobs.GetByID(context.Background(), id)
	require.NoError(t, err)
	return job
}

func (h *harness) activeJob(t *testing.T, trackID string) *domain.DownloadJob {
	t.Helper()
	job, err := h.jobs.GetActiveByTrackID(context.Background(), trackID)
	require.NoError(t, err)
	return job
}

// downloadAndImport runs one job whose finalize imports a row that already
// carries storedDuration and reports decodedDuration from the container. It
// returns the surviving row so a test can assert what was persisted.
func (h *harness) downloadAndImport(t *testing.T, storedDuration, decodedDuration int) *domain.TrackDTO {
	t.Helper()
	ctx := context.Background()
	track := h.seedRemote(t, "remote-duration")

	h.finalizer.onFinalize = func(in download.FinalizeInput) (download.FinalizeResult, error) {
		importedPath := filepath.Join(h.dir, "imported-duration.mp3")
		if err := h.tracks.Upsert(ctx, &domain.Track{
			ID: "imported-duration", Path: importedPath, Title: in.Track.Title,
			Duration: storedDuration, Availability: domain.AvailabilityReady,
		}); err != nil {
			return download.FinalizeResult{}, err
		}
		return download.FinalizeResult{
			TrackID: "imported-duration", TargetPath: importedPath,
			DurationSeconds: decodedDuration, Bitrate: 320,
		}, nil
	}

	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", ""))
	require.NoError(t, h.svc.Start(ctx))
	t.Cleanup(func() { _ = h.svc.Stop(context.Background()) })

	waitFor(t, "the job to finish", func() bool {
		jobs, err := h.svc.List(ctx)
		if err != nil || len(jobs) == 0 {
			return false
		}
		return jobs[0].State == domain.DownloadStateDone
	})

	// markDone closes the job before the merge and the duration write, so wait
	// for the post-processing itself: applyDecodedDuration always stores the
	// bitrate, which makes it a reliable completion marker.
	waitFor(t, "the decoded metadata to be applied", func() bool {
		imported, err := h.tracks.GetByID(ctx, "imported-duration")
		return err == nil && imported != nil && imported.Bitrate == 320
	})

	imported, err := h.tracks.GetByID(ctx, "imported-duration")
	require.NoError(t, err)
	require.NotNil(t, imported)
	return imported
}

// --- tests ------------------------------------------------------------------

func TestService_EnqueueRunsToDone(t *testing.T) {
	h := newHarness(t)
	track := h.seedRemote(t, "t1")
	ctx := context.Background()

	// playlist_id is a foreign key: the playlist must exist.
	require.NoError(t, sqlite.NewPlaylistRepository(h.db).Save(ctx, &domain.Playlist{ID: "pl-1", Name: "pl-1"}))
	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "pl-1", domain.Quality320k))
	require.NoError(t, h.svc.Start(ctx))
	t.Cleanup(func() { _ = h.svc.Stop(context.Background()) })

	waitFor(t, "the job to finish", func() bool {
		job := h.activeJob(t, track.ID)
		return job == nil
	})

	jobs, err := h.svc.List(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	job := jobs[0]
	assert.Equal(t, domain.DownloadStateDone, job.State)
	assert.Equal(t, "pl-1", *job.PlaylistID)
	assert.NotEmpty(t, job.TargetPath)
	assert.EqualValues(t, 2, job.BytesDone)

	_, done, failed := h.sink.counts()
	assert.Equal(t, 1, done, "a finished job emits download:done")
	assert.Zero(t, failed)
	assert.Equal(t, 1, h.finalizer.calls)
	assert.Equal(t, 1, h.resolver.CallCount(), "every attempt resolves once")
}

// TestService_ApplyDecodedDurationGuardsImplausibleValues pins the rule that a
// broken container must not replace a plausible catalogue duration. A one-second
// read from an old, malformed FLAC is what turned a 284-second track into "0:01"
// in the lyrics panel; only a row that is itself that short may accept it.
func TestService_ApplyDecodedDurationGuardsImplausibleValues(t *testing.T) {
	t.Run("keeps the stored duration when the decode reports one second", func(t *testing.T) {
		h := newHarness(t)
		imported := h.downloadAndImport(t, 284, 1)
		assert.Equal(t, 284, imported.Duration)
	})

	t.Run("keeps the stored duration when the decode reports nothing", func(t *testing.T) {
		h := newHarness(t)
		imported := h.downloadAndImport(t, 284, 0)
		assert.Equal(t, 284, imported.Duration)
	})

	t.Run("accepts a genuinely short track", func(t *testing.T) {
		h := newHarness(t)
		imported := h.downloadAndImport(t, 1, 1)
		assert.Equal(t, 1, imported.Duration)
	})

	t.Run("accepts a sensible decode", func(t *testing.T) {
		h := newHarness(t)
		imported := h.downloadAndImport(t, 284, 200)
		assert.Equal(t, 200, imported.Duration)
	})

	t.Run("writes the decoded duration when the row has none", func(t *testing.T) {
		h := newHarness(t)
		imported := h.downloadAndImport(t, 0, 200)
		assert.Equal(t, 200, imported.Duration)
	})
}

// TestService_ApplyDecodedDurationPrefersTheProbe pins the 37:15 case: taglib
// reported 2235 seconds for a 271-second raw FLAC. The container probe is the
// authoritative value and must win over the tagged one.
func TestService_ApplyDecodedDurationPrefersTheProbe(t *testing.T) {
	h := newHarness(t)
	h.probe.err = nil
	h.probe.duration = 271 * time.Second

	imported := h.downloadAndImport(t, 271, 2235)
	assert.Equal(t, 271, imported.Duration, "the probe value must replace the taglib value")
}

// TestService_EnqueueWithCoverStoresBeforeEnqueue pins the playlist auto-download
// race: the hook may already have queued the job, so EnqueueWithCover returns
// ErrDownloadAlreadyQueued. The cover must still reach finalize.
func TestService_EnqueueWithCoverStoresBeforeEnqueue(t *testing.T) {
	h := newHarness(t)
	h.settings = stubSettings{dir: h.dir, withCover: true}
	h.rebuild()
	track := h.seedRemote(t, "cover-race")
	ctx := context.Background()

	// The playlist auto-download hook has already queued this track without a
	// cover. The catalog action then calls EnqueueWithCover with the URL.
	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", ""))

	var finalizedCover string
	h.finalizer.onFinalize = func(in download.FinalizeInput) (download.FinalizeResult, error) {
		finalizedCover = in.CoverURL
		return download.FinalizeResult{
			TrackID: in.Track.ID, TargetPath: filepath.Join(h.dir, "cover-race.mp3"),
			DurationSeconds: 200, Bitrate: 320,
		}, nil
	}
	err := h.svc.EnqueueWithCover(ctx, track.ID, "", "", "https://p1.music.126.net/cover.jpg")
	require.ErrorIs(t, err, domain.ErrDownloadAlreadyQueued)

	require.NoError(t, h.svc.Start(ctx))
	t.Cleanup(func() { _ = h.svc.Stop(context.Background()) })
	waitFor(t, "the queued job to finish", func() bool {
		jobs, listErr := h.svc.List(ctx)
		return listErr == nil && len(jobs) == 1 && jobs[0].State == domain.DownloadStateDone
	})

	require.Equal(t, "https://p1.music.126.net/cover.jpg", finalizedCover,
		"the cover must be stored before the already-queued job finishes")
}

// TestService_MergeReplacingTheRemoteRowLeavesTheJobDone pins the P0-1 fix: the
// import replaces the remote track row, and the merge deletes that row. The job
// is closed before the merge, so a finished download is never reported as a
// failure — and the job row survives its track because track_id no longer
// cascades.
func TestService_MergeReplacingTheRemoteRowLeavesTheJobDone(t *testing.T) {
	h := newHarness(t)
	track := h.seedRemote(t, "remote-1")
	ctx := context.Background()

	h.finalizer.onFinalize = func(in download.FinalizeInput) (download.FinalizeResult, error) {
		importedPath := filepath.Join(h.dir, "imported.mp3")
		if err := h.tracks.Upsert(ctx, &domain.Track{
			ID: "imported-1", Path: importedPath, Title: in.Track.Title, Availability: domain.AvailabilityReady,
		}); err != nil {
			return download.FinalizeResult{}, err
		}
		return download.FinalizeResult{TrackID: "imported-1", TargetPath: importedPath, DurationSeconds: 200}, nil
	}

	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", ""))
	require.NoError(t, h.svc.Start(ctx))
	t.Cleanup(func() { _ = h.svc.Stop(context.Background()) })

	waitFor(t, "the job to finish", func() bool {
		jobs, err := h.svc.List(ctx)
		if err != nil || len(jobs) == 0 {
			return false
		}
		return jobs[0].State == domain.DownloadStateDone
	})

	_, done, failed := h.sink.counts()
	assert.Equal(t, 1, done)
	assert.Zero(t, failed, "a merge after a finished download must not fail the job")

	remote, err := h.tracks.GetByID(ctx, track.ID)
	require.NoError(t, err)
	assert.Nil(t, remote, "the merge replaced the remote row")
}

func TestService_FetchesLyricsForTheSurvivorAfterMerge(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	track := h.seedRemote(t, "remote-lyrics")
	h.settings = stubSettings{dir: h.dir, withLyrics: true}
	h.rebuild()
	writer := &stubLyrics{}
	h.svc.SetLyricsWriter(writer)

	h.finalizer.onFinalize = func(in download.FinalizeInput) (download.FinalizeResult, error) {
		importedPath := filepath.Join(h.dir, "with-lyrics.mp3")
		if err := h.tracks.Upsert(ctx, &domain.Track{
			ID: "imported-lyrics", Path: importedPath, Title: in.Track.Title, Availability: domain.AvailabilityReady,
		}); err != nil {
			return download.FinalizeResult{}, err
		}
		return download.FinalizeResult{TrackID: "imported-lyrics", TargetPath: importedPath, DurationSeconds: 200}, nil
	}

	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", ""))
	require.NoError(t, h.svc.Start(ctx))
	t.Cleanup(func() { _ = h.svc.Stop(context.Background()) })

	// markDone closes the job before the merge and the lyric fetch, so wait for
	// the fetch itself rather than the job row: asserting on the job state races
	// the post-processing that this test is about.
	waitFor(t, "lyrics for the survivor", func() bool {
		writer.mu.Lock()
		defer writer.mu.Unlock()
		return writer.calls == 1
	})

	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Equal(t, []string{"imported-lyrics"}, writer.ids,
		"lyrics must attach to the row that owns the file, not the merged-away remote row")
}

func TestService_LyricFailureDoesNotFailTheDownload(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	track := h.seedRemote(t, "remote-lyrics-fail")
	h.settings = stubSettings{dir: h.dir, withLyrics: true}
	h.rebuild()
	writer := &stubLyrics{err: errors.New("lyrics provider down")}
	h.svc.SetLyricsWriter(writer)

	h.finalizer.onFinalize = func(in download.FinalizeInput) (download.FinalizeResult, error) {
		importedPath := filepath.Join(h.dir, "with-lyrics-fail.mp3")
		if err := h.tracks.Upsert(ctx, &domain.Track{
			ID: "imported-lyrics-fail", Path: importedPath, Title: in.Track.Title, Availability: domain.AvailabilityReady,
		}); err != nil {
			return download.FinalizeResult{}, err
		}
		return download.FinalizeResult{TrackID: "imported-lyrics-fail", TargetPath: importedPath, DurationSeconds: 200}, nil
	}

	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", ""))
	require.NoError(t, h.svc.Start(ctx))
	t.Cleanup(func() { _ = h.svc.Stop(context.Background()) })

	// A failed lyric fetch happens after markDone; wait for the sink's done event
	// so the assertion cannot read the counters before post-processing runs.
	waitFor(t, "the job to finish", func() bool {
		_, done, _ := h.sink.counts()
		return done == 1
	})

	_, done, failed := h.sink.counts()
	assert.Equal(t, 1, done)
	assert.Zero(t, failed, "a lyrics outage must not fail a finished download")
}

// stubRemover records the explicit file deletion Redownload performs.
type stubRemover struct {
	mu       sync.Mutex
	ids      []string
	err      error
	onDelete func(trackID string) error
}

func (s *stubRemover) DeleteTrackFile(_ context.Context, trackID string) error {
	s.mu.Lock()
	s.ids = append(s.ids, trackID)
	s.mu.Unlock()
	if s.onDelete != nil {
		return s.onDelete(trackID)
	}
	return s.err
}

func TestService_RedownloadDeletesTheFileAndQueuesAFreshJob(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	require.NoError(t, h.tracks.Upsert(ctx, &domain.Track{
		ID: "broken-1", Path: filepath.Join(h.dir, "broken.flac"), Title: "前前前世",
		RawArtistNames: "RADWIMPS", RawAlbumName: "君の名は。",
		Availability: domain.AvailabilityReady, Provider: "wy", ExternalID: "1001",
	}))
	remover := &stubRemover{onDelete: func(id string) error {
		return h.tracks.Delete(ctx, id)
	}}
	h.svc.SetLocalFileRemover(remover)

	require.NoError(t, h.svc.Redownload(ctx, "broken-1"))

	remover.mu.Lock()
	defer remover.mu.Unlock()
	assert.Equal(t, []string{"broken-1"}, remover.ids, "the damaged file must be removed explicitly")

	jobs, err := h.svc.List(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, domain.DownloadStateQueued, jobs[0].State)
	fresh, err := h.tracks.GetByID(ctx, jobs[0].TrackID)
	require.NoError(t, err)
	require.NotNil(t, fresh)
	assert.Equal(t, "wy", fresh.Provider)
	assert.Equal(t, "1001", fresh.ExternalID, "the fresh job runs on the re-registered identity row")
}

func TestService_RedownloadRejectsATrackWithoutAnIdentity(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.tracks.Upsert(context.Background(), &domain.Track{
		ID: "local-1", Path: filepath.Join(h.dir, "local.flac"), Title: "local",
		Availability: domain.AvailabilityReady,
	}))
	h.svc.SetLocalFileRemover(&stubRemover{})

	err := h.svc.Redownload(context.Background(), "local-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no online identity")
}

func TestService_EnqueueRejectsReadyTrack(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	require.NoError(t, h.tracks.Upsert(ctx, &domain.Track{
		ID: "local-1", Path: filepath.Join(t.TempDir(), "a.mp3"), Title: "a", Availability: domain.AvailabilityReady,
	}))

	err := h.svc.Enqueue(ctx, "local-1", "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already available locally")
}

func TestService_EnqueueIsIdempotentWhileActive(t *testing.T) {
	h := newHarness(t)
	track := h.seedRemote(t, "t1")
	ctx := context.Background()

	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", ""))
	err := h.svc.Enqueue(ctx, track.ID, "", "")
	assert.ErrorIs(t, err, domain.ErrDownloadAlreadyQueued, "the partial unique index is the guard")
}

func TestService_RetriesTransientFailureThenSucceeds(t *testing.T) {
	h := newHarness(t)
	track := h.seedRemote(t, "t1")
	ctx := context.Background()

	h.downloader.errs = []error{errors.New("connection reset")}
	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", ""))
	require.NoError(t, h.svc.Start(ctx))
	t.Cleanup(func() { _ = h.svc.Stop(context.Background()) })

	// Give the first attempt time to fail and schedule the retry.
	waitFor(t, "the first attempt to fail", func() bool {
		job := h.activeJob(t, track.ID)
		return job != nil && job.Attempts == 1 && job.State == domain.DownloadStateQueued
	})

	// The retry is 5s out in real time; move the clock past it and wake the
	// worker instead of sleeping through the backoff.
	h.svc.SetClock(func() time.Time { return time.Now().Add(time.Minute) })
	h.svc.WakeForTest()

	waitFor(t, "the retry to succeed", func() bool {
		jobs, err := h.svc.List(ctx)
		if err != nil || len(jobs) == 0 {
			return false
		}
		return jobs[0].State == domain.DownloadStateDone
	})

	jobs, err := h.svc.List(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, jobs[0].Attempts, 1, "retry reuses the same row and counts the attempt")
	assert.Equal(t, 2, h.downloader.calls())
	assert.GreaterOrEqual(t, h.resolver.CallCount(), 2, "each attempt resolves a fresh URL")
}

func TestService_PermanentFailureDoesNotRetry(t *testing.T) {
	h := newHarness(t)
	track := h.seedRemote(t, "t1")
	ctx := context.Background()

	h.resolver.err = domain.ErrStreamUnavailable // VIP / -110
	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", ""))
	require.NoError(t, h.svc.Start(ctx))
	t.Cleanup(func() { _ = h.svc.Stop(context.Background()) })

	waitFor(t, "the job to fail permanently", func() bool {
		jobs, err := h.svc.List(ctx)
		return err == nil && len(jobs) == 1 && jobs[0].State == domain.DownloadStateFailed
	})

	jobs, err := h.svc.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, jobs[0].Attempts)
	assert.Contains(t, jobs[0].LastError, "stream unavailable")
	assert.Equal(t, 1, h.resolver.CallCount(), "a -110 must not be retried")

	tr, err := h.tracks.GetByID(ctx, track.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.AvailabilityFailed, tr.Availability)

	_, _, failed := h.sink.counts()
	assert.Equal(t, 1, failed)
}

func TestService_ClassifiesAnExhaustedJobAsPermanent(t *testing.T) {
	// The attempt cap is decided by domain.ClassifyDownloadError and covered
	// there; this checks the service honours it: on the last allowed attempt the
	// job stops being requeued and the track is marked unavailable.
	h := newHarness(t)
	track := h.seedRemote(t, "t1")
	ctx := context.Background()

	h.downloader.errs = []error{errors.New("timeout"), errors.New("timeout"), errors.New("timeout"), errors.New("timeout")}
	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", ""))
	// Claim and process by hand: two failures in, the third attempt is the
	// last one, so the service must not schedule a fourth.
	for i := 0; i < domain.DefaultDownloadMaxAttempts-1; i++ {
		job, err := h.jobs.ClaimNext(ctx, time.Now().Add(24*time.Hour))
		require.NoError(t, err)
		require.NotNil(t, job)

		h.svc.ProcessJobForTest(job)

		requeued, err := h.jobs.GetByID(ctx, job.ID)
		require.NoError(t, err)
		require.Equal(t, domain.DownloadStateQueued, requeued.State, "attempt %d/%d is retried",
			i+1, domain.DefaultDownloadMaxAttempts)
		assert.NotNil(t, requeued.NextRetryAt)
	}

	job, err := h.jobs.ClaimNext(ctx, time.Now().Add(48*time.Hour))
	require.NoError(t, err)
	require.NotNil(t, job)
	h.svc.ProcessJobForTest(job)

	final, err := h.jobs.GetByID(ctx, job.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.DownloadStateFailed, final.State, "the max_attempts-th failure is permanent")
	assert.Nil(t, final.NextRetryAt)
	assert.Equal(t, domain.DefaultDownloadMaxAttempts, final.Attempts)

	tr, err := h.tracks.GetByID(ctx, track.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.AvailabilityFailed, tr.Availability)
}

func TestService_RetryRequeuesAFailedJob(t *testing.T) {
	h := newHarness(t)
	track := h.seedRemote(t, "t1")
	ctx := context.Background()

	h.resolver.err = domain.ErrStreamUnavailable
	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", ""))
	require.NoError(t, h.svc.Start(ctx))
	waitFor(t, "the first attempt to fail", func() bool {
		jobs, err := h.svc.List(ctx)
		return err == nil && len(jobs) == 1 && jobs[0].State == domain.DownloadStateFailed
	})
	require.NoError(t, h.svc.Stop(ctx))

	jobs, err := h.svc.List(ctx)
	require.NoError(t, err)
	failedID := jobs[0].ID

	h.resolver.err = nil
	require.NoError(t, h.svc.Retry(ctx, failedID))

	requeued := h.activeJob(t, track.ID)
	require.NotNil(t, requeued, "a manual retry must create a claimable job")
	assert.Equal(t, domain.DownloadStateQueued, requeued.State)

	require.NoError(t, h.svc.Start(ctx))
	waitFor(t, "the retried job to finish", func() bool {
		return h.activeJob(t, track.ID) == nil
	})
	jobs, err = h.svc.List(ctx)
	require.NoError(t, err)
	assert.Len(t, jobs, 2, "the failed row is kept as a record")
}

func TestService_CancelStopsTheJob(t *testing.T) {
	h := newHarness(t)
	track := h.seedRemote(t, "t1")
	ctx := context.Background()

	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", ""))
	job := h.activeJob(t, track.ID)
	require.NotNil(t, job)

	require.NoError(t, h.svc.Cancel(ctx, job.ID))
	stored := h.jobState(t, job.ID)
	assert.Equal(t, domain.DownloadStateCanceled, stored.State)

	// Cancelling again is rejected: the row is no longer active.
	assert.Error(t, h.svc.Cancel(ctx, job.ID))
}

func TestService_StartRecoversInterruptedJobs(t *testing.T) {
	h := newHarness(t)
	track := h.seedRemote(t, "t1")
	ctx := context.Background()

	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", ""))
	// Simulate a crash: claim the job (queued -> active) and then restart,
	// which is exactly what a killed process leaves behind.
	claimed, err := h.jobs.ClaimNext(ctx, time.Now())
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, domain.DownloadStateActive, claimed.State)

	// A new process builds a new service over the same database.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	restarted := download.NewService(
		h.jobs, h.tracks, h.resolver, h.downloader, h.finalizer,
		h.probe,
		download.NewMerger(h.tracks, sqlite.NewDownloadMergeRepository(h.db, h.tracks), sqlite.NewTxManager(h.db), logger),
		stubSettings{dir: h.dir}, logger,
	)
	require.NoError(t, restarted.Start(ctx))
	t.Cleanup(func() { _ = restarted.Stop(context.Background()) })

	waitFor(t, "the recovered job to finish", func() bool {
		jobs, err := h.jobs.List(ctx)
		return err == nil && len(jobs) == 1 && jobs[0].State == domain.DownloadStateDone
	})
}

func TestService_StopExitsTheWorker(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	require.NoError(t, h.svc.Start(ctx))

	done := make(chan error, 1)
	go func() { done <- h.svc.Stop(ctx) }()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return: the worker goroutine is still running")
	}

	// Idempotent, and Start can run again afterwards.
	assert.NoError(t, h.svc.Stop(ctx))
	require.NoError(t, h.svc.Start(ctx))
	assert.NoError(t, h.svc.Stop(ctx))
}

func TestService_WakesImmediatelyOnEnqueue(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// No job yet: the service is started before anything is enqueued, which is
	// the real startup order.
	require.NoError(t, h.svc.Start(ctx))
	t.Cleanup(func() { _ = h.svc.Stop(context.Background()) })
	track := h.seedRemote(t, "t1")

	require.NoError(t, h.svc.Enqueue(ctx, track.ID, "", ""))
	// The poll interval is 30s; without the enqueue wakeup this would only run
	// after that. A 5s deadline fails if the wakeup path is broken.
	waitFor(t, "the enqueued job to be picked up", func() bool {
		jobs, err := h.svc.List(ctx)
		return err == nil && len(jobs) == 1 && jobs[0].State == domain.DownloadStateDone
	})
}

// two-phase Download counters: the retry test asserts the downloader ran twice.
func (d *stubDownloader) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.callsCount
}

// testWriter forwards slog output to the test log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\r\n"))
	return len(p), nil
}
