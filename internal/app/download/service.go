package download

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"airmedy/internal/domain"

	"github.com/google/uuid"
)

// defaultPollInterval is how often an idle worker looks for a due job. A new
// enqueue wakes it immediately, so this only bounds how late a retry that comes
// due while the queue is otherwise empty is picked up.
const defaultPollInterval = 30 * time.Second

// Duration sanity thresholds for applyDecodedDuration. A decoded value shorter
// than minimumDecodedDurationSeconds is only plausible when the row already
// expects a track that short (minimumStoredDurationSeconds). Anything below two
// seconds on a row that stored at least five means the container was read
// wrong, not that the song is a one-second clip.
const (
	minimumDecodedDurationSeconds = 2
	minimumStoredDurationSeconds  = 5
)

// SettingsReader is the slice of the settings service the download service
// needs. Declared here so the service does not depend on the settings package
// (and so a test can hand it a literal).
type SettingsReader interface {
	GetSettings(ctx context.Context) (*domain.AppSettings, error)
}

// AudioDownloader is the byte mover the service drives. It is
// domain.StreamDownloader with the single-method convenience of a value that
// can also discard a partial file; the downloader adapter satisfies it.
type AudioDownloader interface {
	domain.StreamDownloader
}

// LibraryIngest is the library side of a finished download: finalize writes the
// file into place and imports it.
type LibraryIngest interface {
	Finalize(ctx context.Context, in FinalizeInput) (FinalizeResult, error)
}

// EventSink publishes progress to the UI. Implementations must not block; the
// service calls it from the worker goroutine.
type EventSink interface {
	DownloadProgress(job *domain.DownloadJob)
	DownloadDone(job *domain.DownloadJob)
	DownloadFailed(job *domain.DownloadJob)
}

// Service runs the download queue: it claims due jobs, resolves a fresh stream,
// downloads, finalizes and merges, and records progress. One job at a time,
// which is enough for a background feature and keeps the SQLite writer and the
// network budget to itself.
type Service struct {
	jobs          domain.DownloadJobRepository
	tracks        domain.TrackRepository
	resolver      domain.StreamResolver
	downloader    AudioDownloader
	finalizer     LibraryIngest
	durationProbe domain.MediaDurationProbe
	merger        *Merger
	settings      SettingsReader
	lyrics        LyricsWriter
	remover       LocalFileRemover
	logger        *slog.Logger

	// pollInterval and now are test seams: a test drives the clock instead of
	// sleeping through the 5s/30s/5min backoff.
	pollInterval time.Duration
	now          func() time.Time

	wake chan struct{}

	mu      sync.Mutex
	running bool
	stop    chan struct{}
	done    chan struct{}

	// sink is replaced when the Wails layer installs itself; nil until then.
	sinkMu sync.RWMutex
	sink   EventSink

	// coverURLs remembers the cover a track was enqueued with. It is not
	// persisted: a cached cover is a nicety, and the row already carries
	// everything needed to re-download the file without one.
	coverMu   sync.RWMutex
	coverURLs map[string]string
}

// NewService wires the download service. A nil sink means events are dropped
// until SetEventSink is called (the Wails layer owns the window lifecycle).
func NewService(
	jobs domain.DownloadJobRepository,
	tracks domain.TrackRepository,
	resolver domain.StreamResolver,
	downloader AudioDownloader,
	finalizer LibraryIngest,
	durationProbe domain.MediaDurationProbe,
	merger *Merger,
	settings SettingsReader,
	logger *slog.Logger,
) *Service {
	return &Service{
		jobs:          jobs,
		tracks:        tracks,
		resolver:      resolver,
		downloader:    downloader,
		finalizer:     finalizer,
		durationProbe: durationProbe,
		merger:        merger,
		settings:      settings,
		logger:        logger,
		pollInterval:  defaultPollInterval,
		now:           time.Now,
		wake:          make(chan struct{}, 1),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
		coverURLs:     make(map[string]string),
	}
}

// SetClock replaces the clock the worker and the retry backoff read. It exists
// so tests can make a 5s/30s/5m backoff due immediately instead of sleeping;
// production never calls it.
func (s *Service) SetClock(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

// SetPollInterval replaces the idle poll period. Tests set it long so the only
// thing that can wake the worker is what they control.
func (s *Service) SetPollInterval(d time.Duration) {
	if d > 0 {
		s.pollInterval = d
	}
}

// ProcessJobForTest runs one already-claimed job to completion (or to its next
// retry) synchronously. Tests use it to exercise the attempt cap without racing
// the worker's poll timer.
func (s *Service) ProcessJobForTest(job *domain.DownloadJob) bool {
	stop := make(chan struct{})
	return s.processJob(stop, job)
}

// WakeForTest nudges the worker without enqueueing anything. Tests use it after
// moving the clock past a retry's due time.
func (s *Service) WakeForTest() { s.signal() }

// LocalFileRemover deletes a track's file and its library row. It is used only
// by Redownload, after the user explicitly asked to fetch the file again.
type LocalFileRemover interface {
	DeleteTrackFile(ctx context.Context, trackID string) error
}

// SetLocalFileRemover installs the optional remover used by Redownload.
func (s *Service) SetLocalFileRemover(remover LocalFileRemover) {
	s.remover = remover
}

// SetLyricsWriter installs the optional lyric fetcher. It runs after the merge
// so lyrics attach to the row that owns the file, and is safe to call before or
// after Start.
func (s *Service) SetLyricsWriter(writer LyricsWriter) {
	s.lyrics = writer
}

// SetEventSink installs the UI publisher. Safe to call before or after Start.
func (s *Service) SetEventSink(sink EventSink) {
	s.sinkMu.Lock()
	defer s.sinkMu.Unlock()
	s.sink = sink
}

func (s *Service) emit(fn func(EventSink, *domain.DownloadJob), job *domain.DownloadJob) {
	if job == nil {
		return
	}
	s.sinkMu.RLock()
	sink := s.sink
	s.sinkMu.RUnlock()
	if sink != nil {
		fn(sink, job)
	}
}

// providerAvailability is the resolver capability that answers "could any
// installed source fetch this provider?" without attempting the download.
type providerAvailability interface {
	CanResolve(provider string) bool
}

// Enqueue queues one track for download.
//
// The track must be a remote track: a ready row is already on disk. The playlist
// id may be nil (a favorites add, or a job whose playlist was deleted) and the
// job simply does not add the result to a playlist.
//
// Enqueueing a track that already has a queued/active job returns
// domain.ErrDownloadAlreadyQueued; the caller can treat that as success.
func (s *Service) Enqueue(ctx context.Context, trackID, playlistID, quality string) error {
	track, err := s.tracks.GetByID(ctx, trackID)
	if err != nil {
		return fmt.Errorf("enqueue download: load track %s: %w", trackID, err)
	}
	if track == nil {
		return fmt.Errorf("enqueue download: track %s not found", trackID)
	}
	if !track.IsRemote() {
		return fmt.Errorf("enqueue download: track %s is already available locally", trackID)
	}
	// Availability is an optional capability on the resolver: the plugin-backed
	// implementation has it, a bare test double may not. Checking it here blocks
	// every entry (playlist auto-download, direct Wails call, retry) from queueing
	// a job no installed source can serve.
	if availability, ok := s.resolver.(providerAvailability); ok {
		provider := domain.NormalizeProvider(track.Provider)
		if !availability.CanResolve(provider) {
			return fmt.Errorf(
				"%w: no enabled source can download provider %q (install one that declares %q under \"providers\")",
				domain.ErrStreamUnavailable, provider, provider)
		}
	}

	job := &domain.DownloadJob{
		ID:      uuid.New().String(),
		TrackID: trackID,
		Quality: domain.ResolveDownloadQuality(quality),
		State:   domain.DownloadStateQueued,
	}
	if playlistID != "" {
		id := playlistID
		job.PlaylistID = &id
	}
	if err := s.jobs.Create(ctx, job); err != nil {
		return err
	}

	s.logger.Info("download: enqueued", "job_id", job.ID, "track_id", trackID, "quality", job.Quality)
	s.signal()
	return nil
}

// DefaultQuality is the quality a caller should ask for when it has no
// preference of its own: the user's download-quality setting, or the package
// default when settings cannot be read. Search-row actions use it so "save" and
// the auto-download hook queue the same quality.
func (s *Service) DefaultQuality(ctx context.Context) string {
	if s.settings == nil {
		return domain.DefaultDownloadQuality
	}
	settings, err := s.settings.GetSettings(ctx)
	if err != nil || settings == nil {
		return domain.DefaultDownloadQuality
	}
	return domain.ResolveDownloadQuality(settings.DownloadQuality)
}

// EnqueueTrack is Enqueue without a playlist, for callers that only have the
// track (the queue-play action, a manual retry from the track list).
func (s *Service) EnqueueTrack(ctx context.Context, trackID string) error {
	return s.Enqueue(ctx, trackID, "", "")
}

// EnqueueWithCover is Enqueue plus the cover art URL the search result carried.
// The URL is kept in memory only: if the app restarts the file is downloaded
// without a cover instead of the job failing.
func (s *Service) EnqueueWithCover(ctx context.Context, trackID, playlistID, quality, coverURL string) error {
	// Store the cover before enqueueing: the playlist auto-download hook may have
	// already created the job, in which case Enqueue returns
	// ErrDownloadAlreadyQueued (success for the user) and a later store would be
	// skipped. finalize reads the cover only after the download finishes, so
	// writing it first is safe for both paths.
	if trimmed := strings.TrimSpace(coverURL); trimmed != "" {
		s.coverMu.Lock()
		s.coverURLs[trackID] = trimmed
		s.coverMu.Unlock()
	}
	return s.Enqueue(ctx, trackID, playlistID, quality)
}

func (s *Service) coverURL(trackID string) string {
	s.coverMu.RLock()
	defer s.coverMu.RUnlock()
	return s.coverURLs[trackID]
}

// forgetCover drops a cover URL once its track has been downloaded, so the map
// does not grow for the lifetime of the process.
func (s *Service) forgetCover(trackID string) {
	s.coverMu.Lock()
	delete(s.coverURLs, trackID)
	s.coverMu.Unlock()
}

// Cancel stops a queued or active job. The partial file is left in place so a
// later Retry can resume it.
func (s *Service) Cancel(ctx context.Context, jobID string) error {
	job, err := s.jobs.GetByID(ctx, jobID)
	if err != nil {
		return fmt.Errorf("cancel download: load job %s: %w", jobID, err)
	}
	if job == nil {
		return fmt.Errorf("cancel download: job %s not found", jobID)
	}
	if !domain.IsActiveState(job.State) {
		return fmt.Errorf("cancel download: job %s is already %s", jobID, job.State)
	}
	if err := s.jobs.Cancel(ctx, jobID); err != nil {
		return err
	}
	job.State = domain.DownloadStateCanceled
	s.emit(func(sink EventSink, j *domain.DownloadJob) { sink.DownloadFailed(j) }, job)
	return nil
}

// Retry re-runs a failed or canceled job.
//
// The repository has no transition back from failed/canceled, and the partial
// index only covers queued/active, so the supported way to re-run is a fresh
// job; this keeps the old row as the record of what happened.
func (s *Service) Retry(ctx context.Context, jobID string) error {
	old, err := s.jobs.GetByID(ctx, jobID)
	if err != nil {
		return fmt.Errorf("retry download: load job %s: %w", jobID, err)
	}
	if old == nil {
		return fmt.Errorf("retry download: job %s not found", jobID)
	}
	if domain.IsActiveState(old.State) {
		return fmt.Errorf("retry download: job %s is still %s", jobID, old.State)
	}
	if old.State == domain.DownloadStateDone {
		return fmt.Errorf("retry download: job %s already finished", jobID)
	}
	return s.Enqueue(ctx, old.TrackID, derefString(old.PlaylistID), old.Quality)
}

// Redownload replaces a finished file by fetching it again. It exists because a
// file damaged by an older build cannot be repaired in place: the user deletes
// it explicitly and the identity drives a fresh job.
//
// A remote row (a failed job) is simply enqueued. A ready row is deleted first
// (file, row, playlist memberships, index), then the identity is registered
// again with a zero duration so a stale estimate cannot fail the download
// validation.
func (s *Service) Redownload(ctx context.Context, trackID string) error {
	track, err := s.tracks.GetByID(ctx, trackID)
	if err != nil {
		return fmt.Errorf("redownload: load track %s: %w", trackID, err)
	}
	if track == nil {
		return fmt.Errorf("redownload: track %s not found", trackID)
	}
	if track.IsRemote() {
		return s.Enqueue(ctx, trackID, "", s.DefaultQuality(ctx))
	}
	if s.remover == nil {
		return fmt.Errorf("redownload: no file remover is available")
	}
	if track.Provider == "" || track.ExternalID == "" {
		return fmt.Errorf("redownload: track %s has no online identity", trackID)
	}

	spec := domain.RemoteTrackSpec{
		Provider:   track.Provider,
		ExternalID: track.ExternalID,
		Title:      track.Title,
		Artists:    domain.SplitNames(track.RawArtistNames, []string{domain.RawTagSeparator}),
		AlbumName:  track.RawAlbumName,
	}
	if err := s.remover.DeleteTrackFile(ctx, trackID); err != nil {
		return fmt.Errorf("redownload: remove %s: %w", trackID, err)
	}
	fresh, err := s.EnsureRemoteTrack(ctx, spec)
	if err != nil {
		return fmt.Errorf("redownload: register %s: %w", track.Provider, err)
	}
	return s.Enqueue(ctx, fresh.ID, "", s.DefaultQuality(ctx))
}

// List returns the queue for the UI.
func (s *Service) List(ctx context.Context) ([]*domain.DownloadJob, error) {
	return s.jobs.List(ctx)
}

// Start recovers interrupted jobs and starts the worker loop.
//
// Jobs left in 'active' by a crash are moved back to 'queued' first: without
// that they would sit outside the claimable set forever, because the partial
// unique index does not let a new job for the same track be created either.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("download service already running")
	}
	reset, err := s.jobs.ResetActiveToQueued(ctx)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("start download service: recover interrupted jobs: %w", err)
	}
	if reset > 0 {
		s.logger.Info("download: recovered interrupted jobs", "count", reset)
	}
	s.running = true
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	stop, done := s.stop, s.done
	s.mu.Unlock()

	go s.loop(stop, done)
	s.signal()
	return nil
}

// Stop ends the worker. A job that is mid-download is abandoned: the worker
// context is cancelled, the partial file stays for the next run, and the job
// remains 'active' until the next Start resets it to 'queued'.
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	s.running = false
	stop, done := s.stop, s.done
	s.mu.Unlock()

	close(stop)
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stop download service: %w", ctx.Err())
	}
}

func (s *Service) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// signal wakes the worker without blocking: a pending wake already covers this
// request.
func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// loop is the worker. It exits when stop is closed, which is what makes Stop
// leak-free: every path out of the select checks stop before doing work.
func (s *Service) loop(stop, done chan struct{}) {
	defer close(done)
	timer := time.NewTimer(s.pollInterval)
	defer timer.Stop()

	for {
		select {
		case <-stop:
			return
		default:
		}

		job, err := s.jobs.ClaimNext(context.Background(), s.now())
		if err != nil {
			s.logger.Error("download: failed to claim a job", "error", err)
			if !s.sleep(stop, timer, s.pollInterval) {
				return
			}
			continue
		}
		if job == nil {
			// Nothing due. A wake that arrived while we were busy (an enqueue
			// between the claim and here) must not be lost, otherwise the job
			// waits a full poll interval; take a non-blocking look first.
			select {
			case <-s.wake:
				continue
			default:
			}
			select {
			case <-stop:
				return
			case <-s.wake:
			case <-timer.C:
			}
			timer.Reset(s.pollInterval)
			continue
		}

		if !s.processJob(stop, job) {
			return
		}
		// Drain any wake queued while working, then go straight back to the
		// claim so a burst of enqueues does not wait a poll interval each.
		select {
		case <-s.wake:
		default:
		}
	}
}

// sleep waits for interval, a wake signal, or Stop. It reports false when the
// worker must exit.
func (s *Service) sleep(stop chan struct{}, timer *time.Timer, interval time.Duration) bool {
	timer.Reset(interval)
	defer timer.Stop()
	select {
	case <-stop:
		return false
	case <-s.wake:
		return true
	case <-timer.C:
		return true
	}
}

// processJob runs one claimed job to a terminal state. It returns false when the
// worker was stopped while working.
func (s *Service) processJob(stop chan struct{}, job *domain.DownloadJob) bool {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A Stop while the job is in flight cancels it; the partial file survives
	// and the next Start resets the row back to queued.
	jobDone := make(chan struct{})
	go func() {
		select {
		case <-stop:
			cancel()
		case <-jobDone:
		}
	}()
	defer close(jobDone)

	s.emit(func(sink EventSink, j *domain.DownloadJob) { sink.DownloadProgress(j) }, job)

	err := s.execute(ctx, job)
	// A cancelled context is a shutdown, not a verdict on the job: leave the
	// row active so recovery treats it as interrupted.
	if errors.Is(err, context.Canceled) && !s.isRunning() {
		s.logger.Info("download: worker stopped mid-job, leaving it for recovery", "job_id", job.ID)
		return false
	}
	if err == nil {
		return true
	}
	s.fail(ctx, job, err)
	return true
}

// fail records a failed attempt with its retry decision.
func (s *Service) fail(ctx context.Context, job *domain.DownloadJob, cause error) {
	policy := domain.ClassifyDownloadError(cause, job.Attempts, job.MaxAttempts)
	message := cause.Error()

	if !policy.Retryable {
		if err := s.jobs.MarkFailed(ctx, job.ID, message, nil); err != nil {
			s.logger.Error("download: failed to record a permanent failure", "job_id", job.ID, "error", err)
		}
		job.State = domain.DownloadStateFailed
		job.LastError = message
		s.logger.Warn("download: permanently failed", "job_id", job.ID, "track_id", job.TrackID, "error", message)
		if err := s.setTrackAvailability(ctx, job.TrackID, domain.AvailabilityFailed); err != nil {
			s.logger.Warn("download: failed to mark the track unavailable", "track_id", job.TrackID, "error", err)
		}
		s.emit(func(sink EventSink, j *domain.DownloadJob) { sink.DownloadFailed(j) }, job)
		return
	}

	next := s.now().Add(policy.Delay)
	if err := s.jobs.MarkFailed(ctx, job.ID, message, &next); err != nil {
		s.logger.Error("download: failed to schedule a retry", "job_id", job.ID, "error", err)
	}
	s.logger.Warn("download: attempt failed, retrying later",
		"job_id", job.ID, "track_id", job.TrackID, "attempts", job.Attempts, "retry_in", policy.Delay, "error", message)
	job.State = domain.DownloadStateQueued
	job.LastError = message
	job.NextRetryAt = &next
	job.Attempts++
	s.emit(func(sink EventSink, j *domain.DownloadJob) { sink.DownloadFailed(j) }, job)
}

// execute performs one attempt: resolve, download, finalize, merge, mark done.
// Every attempt re-resolves: a cached URL from a previous attempt is either
// expired or rate-limited, and replaying it is the reason retries "do nothing".
func (s *Service) execute(ctx context.Context, job *domain.DownloadJob) error {
	track, err := s.tracks.GetByID(ctx, job.TrackID)
	if err != nil {
		return fmt.Errorf("load track %s: %w", job.TrackID, err)
	}
	if track == nil {
		return fmt.Errorf("track %s no longer exists: %w", job.TrackID, domain.ErrStreamUnavailable)
	}
	if !track.IsRemote() {
		// Already downloaded (by an earlier attempt that crashed between the
		// import and the job bookkeeping): just close the job out.
		return s.markDone(ctx, job, track.Path)
	}

	settings, err := s.settings.GetSettings(ctx)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	root, err := ResolveDownloadDir(settings.DownloadDir)
	if err != nil {
		return fmt.Errorf("resolve download directory: %w", err)
	}
	if err := s.setTrackAvailability(ctx, track.ID, domain.AvailabilityDownloading); err != nil {
		return err
	}

	ref, err := s.resolver.Resolve(ctx, track, job.Quality)
	if err != nil {
		return fmt.Errorf("resolve stream for %s: %w", track.ID, err)
	}
	if strings.TrimSpace(ref.URL) == "" {
		return fmt.Errorf("resolve stream for %s returned an empty url: %w", track.ID, domain.ErrStreamUnavailable)
	}

	// The part file is named after the job, not the track, so a retry of the
	// same job resumes while a fresh job starts clean.
	name := job.ID + ".mp3"
	target := domain.DownloadTarget{
		Dir:              root,
		Name:             name,
		Quality:          job.Quality,
		ExpectedDuration: time.Duration(track.Duration) * time.Second,
	}

	// Progress is throttled by the downloader already; each callback writes one
	// row, which is the cheapest thing that lets the UI show a bar.
	var lastEmit time.Time
	res, err := s.downloader.Download(ctx, track, ref, target, func(done, total int64) {
		if err := s.jobs.UpdateProgress(ctx, job.ID, done, total); err != nil {
			s.logger.Debug("download: failed to record progress", "job_id", job.ID, "error", err)
			return
		}
		job.BytesDone, job.BytesTotal = done, total
		if time.Since(lastEmit) >= time.Second {
			lastEmit = time.Now()
			s.emit(func(sink EventSink, j *domain.DownloadJob) { sink.DownloadProgress(j) }, job)
		}
	})
	if err != nil {
		s.discardUnusablePart(ctx, job, err)
		return err
	}

	// What to fetch and write beside the audio file is a per-download setting;
	// it is resolved here, once, so the finalizer never re-reads settings.
	content := domain.ResolveDownloadContent(settings)
	coverURL := ""
	if content.Cover {
		coverURL = s.coverURL(track.ID)
	}

	finalized, err := s.finalizer.Finalize(ctx, FinalizeInput{
		Path:        res.Path,
		Root:        root,
		NamingOrder: settings.DownloadNamingOrder,
		Track:       track,
		Quality:     job.Quality,
		PlaylistID:  job.PlaylistID,
		CoverURL:    coverURL,
		Content:     content,
	})
	if err != nil {
		return fmt.Errorf("finalize %s: %w", track.ID, err)
	}

	// The download is complete as soon as the file is on disk and imported,
	// so the job is closed before the merge. The merge deletes the remote row
	// this job points at; closing the job afterwards would find no row to
	// update and turn a successful download into a permanent failure.
	if err := s.markDone(ctx, job, finalized.TargetPath); err != nil {
		return err
	}

	// Merge after the import: the imported row now exists, and the remote row
	// can be folded into it (or retired onto it). It is bookkeeping on top of a
	// finished download: a failure only leaves a redundant remote row behind,
	// which must never be reported as a failed download.
	merged, mergeErr := s.merger.Merge(ctx, MergeInput{
		RemotePath:    track.Path,
		Provider:      track.Provider,
		ExternalID:    track.ExternalID,
		TargetPath:    finalized.TargetPath,
		TargetTrackID: finalized.TrackID,
	})
	if mergeErr != nil {
		s.logger.Warn("download: merge after a finished download failed",
			"job_id", job.ID, "track_id", track.ID, "error", mergeErr)
	}
	survivorID := finalized.TrackID
	if mergeErr == nil && merged.SurvivorTrackID != "" {
		survivorID = merged.SurvivorTrackID
	}

	// Lyrics are attached after the merge: the remote row the job started from
	// is gone by now, and a lyric saved on it would have been cascade-deleted.
	if content.Lyrics && s.lyrics != nil {
		survivor, err := s.tracks.GetByID(ctx, survivorID)
		if err != nil {
			s.logger.Warn("download: cannot load the row for lyrics", "track_id", survivorID, "error", err)
		} else if survivor != nil {
			if err := s.lyrics.FetchForTrack(ctx, survivor); err != nil {
				s.logger.Warn("download: lyrics unavailable", "track_id", survivorID, "error", err)
			}
		}
	}

	if err := s.applyDecodedDuration(ctx, finalized.TrackID, finalized); err != nil {
		s.logger.Warn("download: failed to store the decoded duration", "track_id", finalized.TrackID, "error", err)
	}

	return nil
}

// applyDecodedDuration corrects the row's duration and bitrate after a finished
// download. trackID is the row that owns the downloaded file, which is not
// necessarily the remote row the job started from.
//
// The duration comes from MediaDurationProbe (the container header) and only
// falls back to the taglib value when the probe fails. A tag library misreads
// raw or malformed containers: an old build's FLAC with ID3 frames inside the
// audio stream reports about one second, and a raw FLAC with no standard
// metadata chain has reported eight times the real length. Both directions are
// guarded here; below minimumDecodedDurationSeconds a value is only trusted
// when the row already expects a track that short.
func (s *Service) applyDecodedDuration(ctx context.Context, trackID string, res FinalizeResult) error {
	updated, err := s.tracks.GetByID(ctx, trackID)
	if err != nil {
		return err
	}
	if updated == nil {
		return nil
	}
	tr := updated.Track

	seconds := res.DurationSeconds
	if s.durationProbe != nil {
		if probed, probeErr := s.durationProbe.Probe(ctx, res.TargetPath); probeErr == nil {
			seconds = int(probed.Seconds())
		} else {
			s.logger.Debug("download: duration probe failed, using the tagged value",
				"track_id", trackID, "path", res.TargetPath, "error", probeErr)
		}
	}
	if seconds > 0 {
		if seconds < minimumDecodedDurationSeconds && tr.Duration >= minimumStoredDurationSeconds {
			s.logger.Warn("download: decoded duration looks implausible, keeping the stored value",
				"track_id", trackID,
				"decoded_duration", seconds,
				"stored_duration", tr.Duration,
			)
		} else {
			tr.Duration = seconds
		}
	}
	if res.Bitrate > 0 {
		tr.Bitrate = res.Bitrate
	}
	return s.tracks.Upsert(ctx, &tr)
}

// markDone closes the job out and makes sure the track row reflects a file.
func (s *Service) markDone(ctx context.Context, job *domain.DownloadJob, targetPath string) error {
	if err := s.jobs.MarkDone(ctx, job.ID, targetPath); err != nil {
		return fmt.Errorf("mark job %s done: %w", job.ID, err)
	}
	s.forgetCover(job.TrackID)
	job.State = domain.DownloadStateDone
	job.TargetPath = targetPath
	s.logger.Info("download: finished", "job_id", job.ID, "track_id", job.TrackID, "path", targetPath)
	s.emit(func(sink EventSink, j *domain.DownloadJob) { sink.DownloadDone(j) }, job)
	return nil
}

// discardUnusablePart deletes the partial file when resuming cannot help: a
// rejected container or a bad response would only be re-fetched onto a broken
// prefix.
func (s *Service) discardUnusablePart(ctx context.Context, job *domain.DownloadJob, cause error) {
	if !errors.Is(cause, domain.ErrCorruptDownload) {
		return
	}
	settings, err := s.settings.GetSettings(ctx)
	if err != nil {
		return
	}
	root, err := ResolveDownloadDir(settings.DownloadDir)
	if err != nil {
		return
	}
	part := s.downloader.PartPath(root, job.ID+".mp3")
	if err := os.Remove(part); err != nil && !os.IsNotExist(err) {
		s.logger.Warn("download: failed to discard an invalid partial file", "path", part, "error", err)
	}
}

// setTrackAvailability flips a remote row's availability, leaving the path
// (which is the identity's unique key) alone.
func (s *Service) setTrackAvailability(ctx context.Context, trackID, availability string) error {
	track, err := s.tracks.GetByID(ctx, trackID)
	if err != nil {
		return fmt.Errorf("set availability: load %s: %w", trackID, err)
	}
	if track == nil {
		return nil
	}
	if !track.IsRemote() {
		// Already ready: availability must stay 'ready'.
		return nil
	}
	tr := track.Track
	tr.Availability = availability
	if err := s.tracks.Upsert(ctx, &tr); err != nil {
		return fmt.Errorf("set availability of %s to %s: %w", trackID, availability, err)
	}
	return nil
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ResolveDownloadDir turns the configured directory into an absolute path,
// falling back to <user music>/Airmedy. The default is resolved here rather than
// stored, so moving the music folder does not leave a stale path behind.
func ResolveDownloadDir(configured string) (string, error) {
	if dir := strings.TrimSpace(configured); dir != "" {
		return filepath.Clean(dir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate the user home directory: %w", err)
	}
	return filepath.Join(home, "Music", DefaultDownloadDirName), nil
}
