package sqlite

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"airmedy/internal/domain"
)

func newDownloadTestDB(t *testing.T) (*DB, domain.DownloadJobRepository, domain.TrackRepository) {
	t.Helper()
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("Failed to create test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, NewDownloadJobRepository(db), NewTrackRepository(db)
}

// seedRemoteTrack inserts a remote (not downloaded) track and returns its ID.
func seedRemoteTrack(t *testing.T, repo domain.TrackRepository, id, externalID string) string {
	t.Helper()
	tr := &domain.Track{
		ID:           id,
		Path:         domain.RemotePath("wy", externalID),
		Title:        "track " + externalID,
		SortTitle:    "track " + externalID,
		Availability: domain.AvailabilityRemote,
		Provider:     "wy",
		ExternalID:   externalID,
	}
	if err := repo.Upsert(context.Background(), tr); err != nil {
		t.Fatalf("failed to seed track: %v", err)
	}
	return tr.ID
}

func TestDownloadJobRepository_CreateIsIdempotentWhileActive(t *testing.T) {
	_, repo, tracks := newDownloadTestDB(t)
	ctx := context.Background()
	trackID := seedRemoteTrack(t, tracks, "t1", "1")

	first := &domain.DownloadJob{ID: "job-1", TrackID: trackID, State: domain.DownloadStateQueued}
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("first enqueue failed: %v", err)
	}
	if first.Quality != "320k" || first.MaxAttempts != domain.DefaultDownloadMaxAttempts {
		t.Fatalf("defaults not applied: quality=%q maxAttempts=%d", first.Quality, first.MaxAttempts)
	}

	second := &domain.DownloadJob{ID: "job-2", TrackID: trackID, State: domain.DownloadStateQueued}
	if err := repo.Create(ctx, second); !errors.Is(err, domain.ErrDownloadAlreadyQueued) {
		t.Fatalf("expected ErrDownloadAlreadyQueued, got %v", err)
	}

	// Once the first job is done, the track can be enqueued again.
	// MarkDone is only valid from 'active', so claim the job first.
	if _, err := repo.ClaimNext(ctx, time.Now()); err != nil {
		t.Fatalf("claim failed: %v", err)
	}
	if err := repo.MarkDone(ctx, "job-1", "/music/a.mp3"); err != nil {
		t.Fatalf("mark done failed: %v", err)
	}
	third := &domain.DownloadJob{ID: "job-3", TrackID: trackID, State: domain.DownloadStateQueued}
	if err := repo.Create(ctx, third); err != nil {
		t.Fatalf("re-enqueue after done failed: %v", err)
	}
}

func TestDownloadJobRepository_MarkDoneIsIdempotentWhenTheJobIsGone(t *testing.T) {
	_, repo, tracks := newDownloadTestDB(t)
	ctx := context.Background()
	trackID := seedRemoteTrack(t, tracks, "t1", "1")

	job := &domain.DownloadJob{ID: "job-1", TrackID: trackID, State: domain.DownloadStateQueued}
	if err := repo.Create(ctx, job); err != nil {
		t.Fatalf("create job failed: %v", err)
	}
	// Deleting the track must keep its download history: track_id is a plain
	// reference, not ON DELETE CASCADE.
	if err := tracks.Delete(ctx, trackID); err != nil {
		t.Fatalf("delete track failed: %v", err)
	}
	kept, err := repo.GetByID(ctx, job.ID)
	if err != nil {
		t.Fatalf("load job after track delete failed: %v", err)
	}
	if kept == nil {
		t.Fatalf("the job row was deleted with its track")
	}

	// Closing a job that no longer exists (or is already closed) is not an
	// error: the caller is done either way.
	if err := repo.MarkDone(ctx, "job-missing", "/music/a.mp3"); err != nil {
		t.Fatalf("MarkDone on a missing job returned %v", err)
	}
}

func TestDownloadJobRepository_ClaimNextRespectsOrderAndRetryTime(t *testing.T) {
	db, repo, tracks := newDownloadTestDB(t)
	ctx := context.Background()
	now := time.Now()

	late := seedRemoteTrack(t, tracks, "t-late", "late")
	early := seedRemoteTrack(t, tracks, "t-early", "early")
	notDue := seedRemoteTrack(t, tracks, "t-notdue", "notdue")

	// Insert out of chronological order to prove ORDER BY created_at decides.
	for _, j := range []*domain.DownloadJob{
		{ID: "job-notdue", TrackID: notDue, State: domain.DownloadStateQueued, NextRetryAt: ptrTime(now.Add(time.Hour))},
		{ID: "job-early", TrackID: early, State: domain.DownloadStateQueued},
		{ID: "job-late", TrackID: late, State: domain.DownloadStateQueued},
	} {
		if err := repo.Create(ctx, j); err != nil {
			t.Fatalf("failed to create %s: %v", j.ID, err)
		}
	}
	// Force deterministic created_at ordering (same-timestamp inserts tie).
	if _, err := db.ExecContext(ctx, `UPDATE download_jobs SET created_at = ? WHERE id = ?`, now.Add(-2*time.Minute), "job-early"); err != nil {
		t.Fatalf("failed to set created_at: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE download_jobs SET created_at = ? WHERE id = ?`, now.Add(-time.Minute), "job-late"); err != nil {
		t.Fatalf("failed to set created_at: %v", err)
	}

	got, err := repo.ClaimNext(ctx, now)
	if err != nil {
		t.Fatalf("ClaimNext failed: %v", err)
	}
	if got == nil || got.ID != "job-early" {
		t.Fatalf("expected job-early to be claimed first, got %#v", got)
	}
	if got.State != domain.DownloadStateActive || got.StartedAt == nil {
		t.Fatalf("claimed job not active: state=%q startedAt=%v", got.State, got.StartedAt)
	}

	got, err = repo.ClaimNext(ctx, now)
	if err != nil || got == nil || got.ID != "job-late" {
		t.Fatalf("expected job-late second, got %#v (err=%v)", got, err)
	}

	// job-notdue is still in the future.
	got, err = repo.ClaimNext(ctx, now)
	if err != nil || got != nil {
		t.Fatalf("expected no due job, got %#v (err=%v)", got, err)
	}
	// ... but it becomes claimable once its retry time passes.
	got, err = repo.ClaimNext(ctx, now.Add(2*time.Hour))
	if err != nil || got == nil || got.ID != "job-notdue" {
		t.Fatalf("expected job-notdue after its retry time, got %#v (err=%v)", got, err)
	}
}

func TestDownloadJobRepository_RetryReusesSameRow(t *testing.T) {
	_, repo, tracks := newDownloadTestDB(t)
	ctx := context.Background()
	trackID := seedRemoteTrack(t, tracks, "t1", "1")

	if err := repo.Create(ctx, &domain.DownloadJob{ID: "job-1", TrackID: trackID, State: domain.DownloadStateQueued}); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}
	if _, err := repo.ClaimNext(ctx, time.Now()); err != nil {
		t.Fatalf("claim failed: %v", err)
	}

	retryAt := time.Now().Add(30 * time.Second)
	if err := repo.MarkFailed(ctx, "job-1", "请求过速", &retryAt); err != nil {
		t.Fatalf("mark failed (retryable) failed: %v", err)
	}

	job, err := repo.GetByID(ctx, "job-1")
	if err != nil || job == nil {
		t.Fatalf("job vanished after retry: %v", err)
	}
	if job.State != domain.DownloadStateQueued {
		t.Fatalf("retryable failure must return to queued, got %q", job.State)
	}
	if job.Attempts != 1 {
		t.Fatalf("attempts must be 1 after one failure, got %d", job.Attempts)
	}
	if job.LastError != "请求过速" {
		t.Fatalf("last_error not recorded, got %q", job.LastError)
	}
	if job.FinishedAt != nil {
		t.Fatalf("retryable failure must not set finished_at")
	}

	// Permanent failure keeps it out of the queue.
	if _, err := repo.ClaimNext(ctx, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("claim failed: %v", err)
	}
	if err := repo.MarkFailed(ctx, "job-1", "鉴权失败", nil); err != nil {
		t.Fatalf("mark failed (permanent) failed: %v", err)
	}
	job, _ = repo.GetByID(ctx, "job-1")
	if job.State != domain.DownloadStateFailed || job.FinishedAt == nil {
		t.Fatalf("permanent failure must be failed+finished, got state=%q finished=%v", job.State, job.FinishedAt)
	}
	if claimed, _ := repo.ClaimNext(ctx, time.Now().Add(time.Hour)); claimed != nil {
		t.Fatalf("failed job must not be claimable, got %#v", claimed)
	}
}

func TestDownloadJobRepository_ResetActiveToQueuedOnRestart(t *testing.T) {
	_, repo, tracks := newDownloadTestDB(t)
	ctx := context.Background()
	id1 := seedRemoteTrack(t, tracks, "t1", "1")
	id2 := seedRemoteTrack(t, tracks, "t2", "2")

	for _, j := range []*domain.DownloadJob{
		{ID: "job-1", TrackID: id1, State: domain.DownloadStateQueued},
		{ID: "job-2", TrackID: id2, State: domain.DownloadStateQueued},
	} {
		if err := repo.Create(ctx, j); err != nil {
			t.Fatalf("enqueue failed: %v", err)
		}
	}
	if _, err := repo.ClaimNext(ctx, time.Now()); err != nil {
		t.Fatalf("claim failed: %v", err)
	}

	n, err := repo.ResetActiveToQueued(ctx)
	if err != nil {
		t.Fatalf("reset failed: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 active job reset, got %d", n)
	}
	job, _ := repo.GetByID(ctx, "job-1")
	if job.State != domain.DownloadStateQueued {
		t.Fatalf("expected job-1 back to queued, got %q", job.State)
	}
}

func TestDownloadJobRepository_PlaylistDeleteNullsSetOnly(t *testing.T) {
	db, repo, tracks := newDownloadTestDB(t)
	ctx := context.Background()
	trackID := seedRemoteTrack(t, tracks, "t1", "1")

	playlistRepo := NewPlaylistRepository(db)
	if err := playlistRepo.Save(ctx, &domain.Playlist{ID: "p1", Name: "P"}); err != nil {
		t.Fatalf("failed to seed playlist: %v", err)
	}
	playlistID := "p1"
	if err := repo.Create(ctx, &domain.DownloadJob{
		ID: "job-1", TrackID: trackID, PlaylistID: &playlistID, State: domain.DownloadStateQueued,
	}); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	if err := playlistRepo.Delete(ctx, "p1"); err != nil {
		t.Fatalf("failed to delete playlist: %v", err)
	}

	job, err := repo.GetByID(ctx, "job-1")
	if err != nil || job == nil {
		t.Fatalf("job must survive playlist deletion: %v", err)
	}
	if job.PlaylistID != nil {
		t.Fatalf("playlist_id should be NULL after playlist delete, got %q", *job.PlaylistID)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
