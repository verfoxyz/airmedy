package mediarequest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"airmedy/internal/domain"

	"github.com/google/uuid"
)

const (
	// defaultSweepInterval bounds how long a request waits when nothing wakes
	// the worker. Saves and retries wake it immediately.
	defaultSweepInterval = 60 * time.Second
	// maxAttempts ends the retry chain; the row stays failed until a new save.
	maxAttempts = 20
	// taskHistoryLimit mirrors the tasks page's cap for finished requests.
	taskHistoryLimit = 500
)

// retryDelays is the shared exponential backoff the tasks page documents:
// 1m, 5m, 30m, 2h, then 6h for every later attempt.
var retryDelays = []time.Duration{
	time.Minute,
	5 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	6 * time.Hour,
}

// Worker is the only thing that advances media requests.
//
// It sweeps serially, so one identity is never processed twice at the same
// time; every decision is delegated to domain.MediaRequestExecutor.
type Worker struct {
	repo     domain.MediaRequestRepository
	executor domain.MediaRequestExecutor
	logger   *slog.Logger

	// execute enables delegation. It is on now that every save entry point
	// upserts a request; the legacy tables are only read by the task page.
	execute  bool
	interval time.Duration
	now      func() time.Time

	wake chan struct{}
	// sweepMu makes a manual wake and the timer sweep single-flight.
	sweepMu sync.Mutex

	mu      sync.Mutex
	running bool
	stop    chan struct{}
	done    chan struct{}
	// localTrack is set by the composition root. At startup it re-opens finished
	// requests whose local file was deleted while the app was closed.
	localTrack LocalTrackChecker
	// updateHook tells the tasks page a row changed (state, phase or track id).
	// The composition root wires it to the download service's event sink.
	updateHook func()
}

func NewWorker(
	repo domain.MediaRequestRepository,
	executor domain.MediaRequestExecutor,
	logger *slog.Logger,
) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		repo:     repo,
		executor: executor,
		logger:   logger,
		execute:  true,
		interval: defaultSweepInterval,
		now:      time.Now,
		wake:     make(chan struct{}, 1),
	}
}

// SetExecutionEnabled turns delegation on or off. It exists for tests and for
// rolling back to the legacy trigger paths without removing the worker.
func (w *Worker) SetExecutionEnabled(enabled bool) {
	w.execute = enabled
}

// SetClock replaces the clock used for retry scheduling. Tests use it.
func (w *Worker) SetClock(now func() time.Time) {
	if now != nil {
		w.now = now
	}
}

// SetSweepInterval replaces the idle period. Tests set it long so only their
// explicit wake runs a sweep.
func (w *Worker) SetSweepInterval(d time.Duration) {
	if d > 0 {
		w.interval = d
	}
}

// SetUpdateHook installs the tasks-page notification. Optional: a worker
// without it still advances requests, the page just waits for its next load.
func (w *Worker) SetUpdateHook(fn func()) {
	w.mu.Lock()
	w.updateHook = fn
	w.mu.Unlock()
}

// notifyUpdate fires the tasks-page notification outside the worker lock.
func (w *Worker) notifyUpdate() {
	w.mu.Lock()
	fn := w.updateHook
	w.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// Wake makes the worker look for work now. It never blocks.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// SetLocalTrackChecker installs the local-file probe used by the startup
// reconciliation. Optional: without it finished rows are left alone.
func (w *Worker) SetLocalTrackChecker(checker LocalTrackChecker) {
	w.localTrack = checker
}

// reconcileDoneRequests re-opens finished rows that no longer have a playable
// local row. A file deleted outside the app leaves such a row behind; without
// this the next save would look like a no-op (fix-delete-resets-request.md).
func (w *Worker) reconcileDoneRequests(ctx context.Context) {
	if w.localTrack == nil {
		return
	}
	requests, err := w.repo.List(ctx)
	if err != nil {
		w.logger.Warn("media request: cannot list finished rows for reconciliation", "error", err)
		return
	}
	reopened := 0
	for _, request := range requests {
		if request == nil || request.State != domain.MediaRequestStateDone {
			continue
		}
		local, checkErr := w.localTrack.HasLocalTrack(ctx, request.Provider, request.ExternalID)
		if checkErr != nil || local {
			continue
		}
		if reopenErr := w.repo.ReopenDone(ctx, request.ID, w.now()); reopenErr != nil {
			w.logger.Warn("media request: cannot re-open a finished row",
				"request_id", request.ID, "error", reopenErr)
			continue
		}
		w.record(ctx, request.ID, domain.MediaRequestStateWanted, "", "reopened: local file missing")
		reopened++
	}
	if reopened > 0 {
		w.logger.Info("media request: re-opened finished rows without a local file", "count", reopened)
	}
}

// reconcileOrphanRequests settles failed rows whose track row was deleted
// while the app was closed. Their retry backoff is irrelevant: the work can
// never succeed, so waiting hours under "waiting to retry" would only look
// like a stuck task. A local match closes the request as done, otherwise it is
// canceled for good.
func (w *Worker) reconcileOrphanRequests(ctx context.Context) {
	requests, err := w.repo.ListOrphans(ctx)
	if err != nil {
		w.logger.Warn("media request: cannot list orphan rows", "error", err)
		return
	}
	for _, request := range requests {
		if request == nil {
			continue
		}
		if local := w.matchLocal(ctx, request); local != nil {
			w.finish(ctx, request, local.ID, "resolved: local row")
		} else {
			w.cancel(ctx, request, "canceled: track row removed")
		}
	}
	if len(requests) > 0 {
		w.logger.Info("media request: settled orphan rows at startup", "count", len(requests))
	}
}

// Start sweeps, then waits for wakes and ticks. The old tables are migrated by
// the SQL migration, so there is no startup mirror to run.
func (w *Worker) Start(ctx context.Context) error {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return nil
	}
	w.running = true
	w.stop = make(chan struct{})
	w.done = make(chan struct{})
	stop, done := w.stop, w.done
	w.mu.Unlock()

	w.reconcileDoneRequests(ctx)
	w.reconcileOrphanRequests(ctx)
	go w.loop(stop, done)
	w.Wake()
	return nil
}

func (w *Worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return nil
	}
	w.running = false
	stop, done := w.stop, w.done
	w.mu.Unlock()

	close(stop)
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stop media request worker: %w", ctx.Err())
	}
}

func (w *Worker) loop(stop, done chan struct{}) {
	defer close(done)
	timer := time.NewTimer(w.interval)
	defer timer.Stop()

	for {
		w.sweep(context.Background())
		timer.Reset(w.interval)
		select {
		case <-stop:
			return
		case <-w.wake:
		case <-timer.C:
		}
	}
}

// sweep processes every active request once, oldest update first. It is
// single-flight: a wake during a sweep is a no-op because the sweep already
// sees the new state.
func (w *Worker) sweep(ctx context.Context) {
	if !w.sweepMu.TryLock() {
		return
	}
	defer w.sweepMu.Unlock()

	requests, err := w.repo.ListActive(ctx, w.now())
	if err != nil {
		w.logger.Warn("media request worker: cannot list active requests", "error", err)
		return
	}
	for _, request := range requests {
		if ctx.Err() != nil {
			return
		}
		w.process(ctx, request)
	}
}

func (w *Worker) process(ctx context.Context, request *domain.MediaRequest) {
	if request == nil {
		return
	}
	track, err := w.executor.LocateTrack(ctx, request.Provider, request.ExternalID)
	if err != nil {
		w.fail(ctx, request, err)
		return
	}
	// Step 1: the identity already owns a local file, so the request is done.
	if track != nil && !track.IsRemote() {
		w.finish(ctx, request, track.ID, "")
		return
	}
	if !w.execute {
		return
	}
	// Step 2: the identity's row is gone. A merge or a delete invalidated the
	// id the request was created against; retrying cannot bring it back, so
	// resolve the song locally and finish, or close the request for good.
	if track == nil {
		if local := w.matchLocal(ctx, request); local != nil {
			w.finish(ctx, request, local.ID, "resolved: local row")
			return
		}
		w.cancel(ctx, request, "canceled: track row removed")
		return
	}
	// A running request is owned by the executor (a download job). A local row
	// means success; a permanent failure is reported by the executor's status.
	if request.State == domain.MediaRequestStateRunning {
		failure, statusErr := w.executor.Status(ctx, track, request.UpdatedAt)
		if statusErr != nil {
			w.logger.Warn("media request worker: cannot inspect delegated work",
				"request_id", request.ID, "error", statusErr)
			return
		}
		if failure != "" {
			w.fail(ctx, request, errors.New(failure))
		}
		return
	}
	// Capture the start before Execute creates a job: Status compares against
	// updated_at, and the delegated row must be newer than it.
	started := w.now()
	phase, err := w.executor.Execute(ctx, track, request.Desired, request.Quality)
	if err != nil {
		if errors.Is(err, domain.ErrMediaRequestDeferred) {
			// Playback still owns the cache file. The request stays wanted; the
			// session-close wake retries it without counting a failure.
			return
		}
		if errors.Is(err, domain.ErrDownloadAlreadyQueued) {
			w.markRunning(ctx, request, track.ID, domain.MediaRequestPhaseDownloading, started)
			return
		}
		w.fail(ctx, request, err)
		return
	}
	w.markRunning(ctx, request, track.ID, phase, started)
}

func (w *Worker) markRunning(
	ctx context.Context, request *domain.MediaRequest, trackID, phase string, at time.Time,
) {
	if err := w.repo.MarkRunning(ctx, request.ID, phase, trackID, at); err != nil {
		w.logger.Warn("media request worker: cannot mark running",
			"request_id", request.ID, "error", err)
		return
	}
	w.record(ctx, request.ID, domain.MediaRequestStateRunning, phase, "")
	w.notifyUpdate()
}

func (w *Worker) finish(
	ctx context.Context, request *domain.MediaRequest, trackID, detail string,
) {
	if err := w.repo.MarkDone(ctx, request.ID, trackID, w.now()); err != nil {
		w.logger.Warn("media request worker: cannot mark done",
			"request_id", request.ID, "error", err)
		return
	}
	w.record(ctx, request.ID, domain.MediaRequestStateDone, "", detail)
	// track_id may now name the merged local row; the page must not keep the old
	// id it was showing.
	w.notifyUpdate()
	w.trimHistory(ctx)
}

func (w *Worker) fail(ctx context.Context, request *domain.MediaRequest, cause error) {
	if cause == nil {
		return
	}
	attempts := request.Attempts + 1
	var next *time.Time
	if attempts < maxAttempts {
		at := w.now().Add(retryDelay(attempts))
		next = &at
	}
	if err := w.repo.MarkFailed(ctx, request.ID, cause.Error(), attempts, next, w.now()); err != nil {
		w.logger.Warn("media request worker: cannot record a failure",
			"request_id", request.ID, "error", err)
		return
	}
	w.record(ctx, request.ID, domain.MediaRequestStateFailed, "", cause.Error())
	w.notifyUpdate()
	if next == nil {
		w.trimHistory(ctx)
	}
}

// matchLocal finds a playable row for the request's song after its identity
// row disappeared. The snapshot on the request is the only identity left, so
// the lookup is title + artist, the same rule the local matcher uses.
func (w *Worker) matchLocal(ctx context.Context, request *domain.MediaRequest) *domain.TrackDTO {
	if w.localTrack == nil {
		return nil
	}
	track, err := w.localTrack.FindLocalMatch(ctx, request.Title, request.Artist)
	if err != nil {
		w.logger.Warn("media request worker: cannot match the song locally",
			"request_id", request.ID, "error", err)
		return nil
	}
	return track
}

// cancel closes a request that can never succeed. It is terminal until a new
// save, and it must not schedule a retry: the row the work needed is gone.
func (w *Worker) cancel(ctx context.Context, request *domain.MediaRequest, detail string) {
	if err := w.repo.MarkCanceled(ctx, request.ID, w.now()); err != nil {
		w.logger.Warn("media request worker: cannot cancel",
			"request_id", request.ID, "error", err)
		return
	}
	w.record(ctx, request.ID, domain.MediaRequestStateCanceled, "", detail)
	w.notifyUpdate()
	w.trimHistory(ctx)
}

// trimHistory caps the finished rows the tasks page keeps. A failure only leaves
// a few extra rows, so it is logged rather than surfaced.
func (w *Worker) trimHistory(ctx context.Context) {
	if _, err := w.repo.TrimFinished(ctx, taskHistoryLimit); err != nil {
		w.logger.Warn("media request worker: cannot trim history", "error", err)
	}
}

func (w *Worker) record(ctx context.Context, requestID, state, phase, detail string) {
	event := &domain.MediaRequestEvent{
		ID:        uuid.NewString(),
		RequestID: requestID,
		State:     state,
		Phase:     phase,
		Detail:    detail,
		CreatedAt: w.now(),
	}
	// History is best effort: a failed insert must not block the transition.
	if err := w.repo.RecordEvent(ctx, event); err != nil {
		w.logger.Debug("media request worker: cannot record an event",
			"request_id", requestID, "error", err)
	}
}

// retryDelay returns the delay after the given attempt (1-based).
func retryDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return retryDelays[0]
	}
	if attempt > len(retryDelays) {
		return retryDelays[len(retryDelays)-1]
	}
	return retryDelays[attempt-1]
}
