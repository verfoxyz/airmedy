package mediarequest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airmedy/internal/domain"
	"airmedy/internal/infra/sqlite"
)

type stubExecutor struct {
	mu         sync.Mutex
	track      *domain.TrackDTO
	locateErr  error
	executeErr error
	statusFail string
	phase      string
	calls      int
}

func (s *stubExecutor) LocateTrack(context.Context, string, string) (*domain.TrackDTO, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.track, s.locateErr
}

func (s *stubExecutor) Execute(
	context.Context, *domain.TrackDTO, string, string,
) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.executeErr != nil {
		return "", s.executeErr
	}
	if s.phase != "" {
		return s.phase, nil
	}
	return domain.MediaRequestPhaseDownloading, nil
}

func (s *stubExecutor) Status(context.Context, *domain.TrackDTO, time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusFail, nil
}

func (s *stubExecutor) setTrack(track *domain.TrackDTO) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.track = track
}

func (s *stubExecutor) setStatus(failure string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusFail = failure
}

func (s *stubExecutor) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func remoteTrack() *domain.TrackDTO {
	return &domain.TrackDTO{Track: domain.Track{
		ID: "track-remote", Path: domain.RemotePath("wy", "42"), Title: "Song",
		Availability: domain.AvailabilityRemote, Provider: "wy", ExternalID: "42",
	}}
}

func localTrack() *domain.TrackDTO {
	return &domain.TrackDTO{Track: domain.Track{
		ID: "track-local", Path: "/music/song.flac", Title: "Song",
		Availability: domain.AvailabilityReady, Provider: "wy", ExternalID: "42",
	}}
}

func seedRequest(t *testing.T, repo domain.MediaRequestRepository) *domain.MediaRequest {
	t.Helper()
	svc := NewService(repo, testLogger())
	request, err := svc.Upsert(context.Background(), UpsertInput{
		Provider: "wy", ExternalID: "42", TrackID: "track-remote",
		Desired: domain.MediaRequestDesiredPromote, Quality: "320k",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	return request
}

func startWorker(
	t *testing.T, repo domain.MediaRequestRepository, executor *stubExecutor,
	execute bool, clock func() time.Time,
) *Worker {
	t.Helper()
	worker := NewWorker(repo, executor, testLogger())
	worker.SetExecutionEnabled(execute)
	worker.SetSweepInterval(time.Hour)
	if clock != nil {
		worker.SetClock(clock)
	}
	if err := worker.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Stop(ctx)
	})
	return worker
}

func readRequest(t *testing.T, repo domain.MediaRequestRepository, id string) *domain.MediaRequest {
	t.Helper()
	request, err := repo.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if request == nil {
		t.Fatalf("request %s disappeared", id)
	}
	return request
}

func TestWorkerMarksDoneWhenTheIdentityIsLocal(t *testing.T) {
	repo := newTestRepo(t)
	request := seedRequest(t, repo)
	executor := &stubExecutor{track: localTrack()}
	startWorker(t, repo, executor, false, nil)

	waitFor(t, "the request to become done", func() bool {
		return readRequest(t, repo, request.ID).State == domain.MediaRequestStateDone
	})
	done := readRequest(t, repo, request.ID)
	if done.TrackID != "track-local" {
		t.Fatalf("track_id = %q, want the local row", done.TrackID)
	}
	if executor.callCount() != 0 {
		t.Fatalf("executor ran %d times, want 0 for a local identity", executor.callCount())
	}
}

func TestWorkerNotifiesTheTasksPageWhenTheTrackIDChanges(t *testing.T) {
	repo := newTestRepo(t)
	request := seedRequest(t, repo)
	executor := &stubExecutor{track: localTrack()}

	worker := NewWorker(repo, executor, testLogger())
	worker.SetExecutionEnabled(false)
	worker.SetSweepInterval(time.Hour)
	var updates int64
	worker.SetUpdateHook(func() { atomic.AddInt64(&updates, 1) })
	if err := worker.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Stop(ctx)
	})

	waitFor(t, "the request to become done", func() bool {
		return readRequest(t, repo, request.ID).State == domain.MediaRequestStateDone
	})
	if atomic.LoadInt64(&updates) == 0 {
		t.Fatal("the tasks page was never told the request's track id changed")
	}
}

func TestWorkerLeavesRequestsAloneWhileExecutionIsDisabled(t *testing.T) {
	repo := newTestRepo(t)
	request := seedRequest(t, repo)
	executor := &stubExecutor{track: remoteTrack()}
	startWorker(t, repo, executor, false, nil)

	time.Sleep(60 * time.Millisecond)
	stored := readRequest(t, repo, request.ID)
	if stored.State != domain.MediaRequestStateWanted {
		t.Fatalf("state = %s, want wanted in the gray phase", stored.State)
	}
	if executor.callCount() != 0 {
		t.Fatalf("executor ran %d times with execution disabled", executor.callCount())
	}
}

func TestWorkerExecutesOnceAndWaitsForTheLocalRow(t *testing.T) {
	repo := newTestRepo(t)
	request := seedRequest(t, repo)
	executor := &stubExecutor{track: remoteTrack(), phase: domain.MediaRequestPhasePromoting}
	worker := startWorker(t, repo, executor, true, nil)

	waitFor(t, "the request to start running", func() bool {
		return readRequest(t, repo, request.ID).State == domain.MediaRequestStateRunning
	})
	running := readRequest(t, repo, request.ID)
	if running.Phase != domain.MediaRequestPhasePromoting {
		t.Fatalf("phase = %q, want promoting", running.Phase)
	}
	if executor.callCount() != 1 {
		t.Fatalf("executor ran %d times, want 1", executor.callCount())
	}

	// A running request belongs to the executor; another sweep must not start
	// the work twice.
	worker.Wake()
	time.Sleep(60 * time.Millisecond)
	if executor.callCount() != 1 {
		t.Fatalf("executor ran %d times after a second sweep", executor.callCount())
	}

	// The executor finishes asynchronously (merge), which shows up as the local
	// row; the next sweep closes the request.
	executor.setTrack(localTrack())
	worker.Wake()
	waitFor(t, "the request to become done", func() bool {
		return readRequest(t, repo, request.ID).State == domain.MediaRequestStateDone
	})
}

func TestWorkerFailsARunningRequestWhenTheExecutorReportsFailure(t *testing.T) {
	repo := newTestRepo(t)
	request := seedRequest(t, repo)
	executor := &stubExecutor{track: remoteTrack(), phase: domain.MediaRequestPhaseDownloading}
	worker := startWorker(t, repo, executor, true, nil)

	waitFor(t, "the request to start running", func() bool {
		return readRequest(t, repo, request.ID).State == domain.MediaRequestStateRunning
	})

	executor.setStatus("403 forbidden")
	worker.Wake()
	waitFor(t, "the delegated failure", func() bool {
		return readRequest(t, repo, request.ID).Attempts == 1
	})
	failed := readRequest(t, repo, request.ID)
	if failed.State != domain.MediaRequestStateFailed || failed.LastError != "403 forbidden" {
		t.Fatalf("failed row = %+v", failed)
	}
	if failed.NextAttemptAt == nil {
		t.Fatalf("a reported failure must schedule a retry: %+v", failed)
	}
}

func TestWorkerLeavesARequestWantedWhenTheExecutorDefers(t *testing.T) {
	repo := newTestRepo(t)
	request := seedRequest(t, repo)
	executor := &stubExecutor{track: remoteTrack(), executeErr: domain.ErrMediaRequestDeferred}
	worker := startWorker(t, repo, executor, true, nil)

	waitFor(t, "the executor to be consulted", func() bool { return executor.callCount() > 0 })
	worker.Wake()
	time.Sleep(60 * time.Millisecond)

	stored := readRequest(t, repo, request.ID)
	if stored.State != domain.MediaRequestStateWanted {
		t.Fatalf("state = %s, want wanted while playback owns the cache", stored.State)
	}
	if stored.Attempts != 0 || stored.NextAttemptAt != nil {
		t.Fatalf("a deferral must not count as a failure: %+v", stored)
	}
}

func TestWorkerBacksOffAfterAFailure(t *testing.T) {
	repo := newTestRepo(t)
	request := seedRequest(t, repo)
	executor := &stubExecutor{track: remoteTrack(), executeErr: errors.New("boom")}

	now := time.Now()
	worker := startWorker(t, repo, executor, true, func() time.Time { return now })

	waitFor(t, "the first failure", func() bool {
		return readRequest(t, repo, request.ID).Attempts == 1
	})
	failed := readRequest(t, repo, request.ID)
	if failed.State != domain.MediaRequestStateFailed || failed.LastError != "boom" {
		t.Fatalf("failed row = %+v", failed)
	}
	if failed.NextAttemptAt == nil || failed.FinishedAt != nil {
		t.Fatalf("retryable failure must keep a next attempt: %+v", failed)
	}

	// Before the backoff elapses the worker must not retry.
	worker.Wake()
	time.Sleep(60 * time.Millisecond)
	if attempts := readRequest(t, repo, request.ID).Attempts; attempts != 1 {
		t.Fatalf("attempts = %d before the retry window", attempts)
	}

	now = now.Add(2 * time.Minute)
	worker.Wake()
	waitFor(t, "the retry", func() bool {
		return readRequest(t, repo, request.ID).Attempts == 2
	})
}

func TestWorkerResolvesAMissingTrackRowThroughTheLocalMatch(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewMediaRequestRepository(db)
	svc := NewService(repo, testLogger())
	request, err := svc.Upsert(context.Background(), UpsertInput{
		Provider: "kg", ExternalID: "91a6ce", TrackID: "track-remote",
		Title: "新·与天齐", Artist: "指尖笑",
		Desired: domain.MediaRequestDesiredPromote, Quality: "320k",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	executor := &stubExecutor{track: nil}
	worker := NewWorker(repo, executor, testLogger())
	worker.SetLocalTrackChecker(stubLocalTrack{match: localTrack()})

	worker.sweep(context.Background())

	got := readRequest(t, repo, request.ID)
	if got.State != domain.MediaRequestStateDone {
		t.Fatalf("state = %s, want done", got.State)
	}
	if got.TrackID != "track-local" {
		t.Fatalf("track id = %q, want the local row the match found", got.TrackID)
	}
	if executor.callCount() != 0 {
		t.Fatalf("executor calls = %d, want the missing row resolved locally", executor.callCount())
	}
	var details []string
	if err := db.Select(&details,
		`SELECT detail FROM media_request_events WHERE request_id = ? AND state = ?`,
		request.ID, domain.MediaRequestStateDone,
	); err != nil {
		t.Fatalf("query events: %v", err)
	}
	if len(details) != 1 || details[0] != "resolved: local row" {
		t.Fatalf("done details = %v, want the auto-done record", details)
	}
}

func TestWorkerCancelsAMissingTrackRowWithoutALocalCopy(t *testing.T) {
	repo := newTestRepo(t)
	svc := NewService(repo, testLogger())
	request, err := svc.Upsert(context.Background(), UpsertInput{
		Provider: "kg", ExternalID: "91a6ce", TrackID: "track-remote",
		Title: "新·与天齐", Artist: "指尖笑",
		Desired: domain.MediaRequestDesiredPromote, Quality: "320k",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	executor := &stubExecutor{track: nil}
	worker := NewWorker(repo, executor, testLogger())
	worker.SetLocalTrackChecker(stubLocalTrack{})

	worker.sweep(context.Background())

	got := readRequest(t, repo, request.ID)
	if got.State != domain.MediaRequestStateCanceled {
		t.Fatalf("state = %s, want canceled because the row is gone", got.State)
	}
	if got.Attempts != 0 || got.NextAttemptAt != nil {
		t.Fatalf("attempts = %d, next = %v, want no retry", got.Attempts, got.NextAttemptAt)
	}
	if executor.callCount() != 0 {
		t.Fatalf("executor calls = %d, want none", executor.callCount())
	}
}

func TestWorkerStopsRetryingAtTheAttemptLimit(t *testing.T) {
	repo := newTestRepo(t)
	request := seedRequest(t, repo)
	now := time.Now()
	if err := repo.MarkFailed(context.Background(), request.ID, "source down", maxAttempts-1, &now, now); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	executor := &stubExecutor{track: remoteTrack(), executeErr: errors.New("source down")}
	worker := NewWorker(repo, executor, testLogger())
	worker.SetClock(func() time.Time { return now.Add(time.Minute) })

	worker.sweep(context.Background())

	got := readRequest(t, repo, request.ID)
	if got.Attempts != maxAttempts {
		t.Fatalf("attempts = %d, want %d", got.Attempts, maxAttempts)
	}
	if got.NextAttemptAt != nil {
		t.Fatalf("next attempt = %v, want nil at the limit", got.NextAttemptAt)
	}
	if got.State != domain.MediaRequestStateFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
}

func TestWorkerStartupSettlesAnOrphanFailedRowWithoutWaitingForBackoff(t *testing.T) {
	repo := newTestRepo(t)
	svc := NewService(repo, testLogger())
	ctx := context.Background()
	request, err := svc.Upsert(ctx, UpsertInput{
		Provider: "kg", ExternalID: "91a6ce", TrackID: "track-gone",
		Title: "新·与天齐", Artist: "指尖笑",
		Desired: domain.MediaRequestDesiredPromote, Quality: "320k",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	next := time.Now().Add(6 * time.Hour)
	if err := repo.MarkFailed(ctx, request.ID, "track row removed", 5, &next, time.Now()); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	executor := &stubExecutor{}
	worker := NewWorker(repo, executor, testLogger())
	worker.SetSweepInterval(time.Hour)
	worker.SetLocalTrackChecker(stubLocalTrack{match: localTrack()})
	if err := worker.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Stop(stopCtx)
	})

	waitFor(t, "the orphan row to settle at startup", func() bool {
		return readRequest(t, repo, request.ID).State == domain.MediaRequestStateDone
	})
	if executor.callCount() != 0 {
		t.Fatalf("executor calls = %d, want none", executor.callCount())
	}
}

func TestWorkerStartupCancelsAnOrphanFailedRowWithoutALocalRow(t *testing.T) {
	repo := newTestRepo(t)
	svc := NewService(repo, testLogger())
	ctx := context.Background()
	request, err := svc.Upsert(ctx, UpsertInput{
		Provider: "kg", ExternalID: "91a6ce", TrackID: "track-gone",
		Title: "新·与天齐", Artist: "指尖笑",
		Desired: domain.MediaRequestDesiredPromote, Quality: "320k",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	next := time.Now().Add(6 * time.Hour)
	if err := repo.MarkFailed(ctx, request.ID, "track row removed", 5, &next, time.Now()); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	worker := NewWorker(repo, &stubExecutor{}, testLogger())
	worker.SetSweepInterval(time.Hour)
	worker.SetLocalTrackChecker(stubLocalTrack{})
	if err := worker.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Stop(stopCtx)
	})

	waitFor(t, "the orphan row to be canceled at startup", func() bool {
		return readRequest(t, repo, request.ID).State == domain.MediaRequestStateCanceled
	})
	if got := readRequest(t, repo, request.ID); got.NextAttemptAt != nil {
		t.Fatalf("next attempt = %v, want none", got.NextAttemptAt)
	}
}
