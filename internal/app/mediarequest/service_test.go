package mediarequest

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"airmedy/internal/domain"
	"airmedy/internal/infra/sqlite"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(nopWriter{}, nil))
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func newTestRepo(t *testing.T) domain.MediaRequestRepository {
	t.Helper()
	return sqlite.NewMediaRequestRepository(newTestDB(t))
}

func newTestDB(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.NewDB(":memory:", testLogger())
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestServiceUpsertConvergesToOneRequest(t *testing.T) {
	repo := newTestRepo(t)
	svc := NewService(repo, testLogger())
	ctx := context.Background()

	var last *domain.MediaRequest
	for i := 0; i < 10; i++ {
		updated, err := svc.Upsert(ctx, UpsertInput{
			Provider: "wy", ExternalID: "42", TrackID: "track-1",
			Desired: domain.MediaRequestDesiredPromote, Quality: "320k",
		})
		if err != nil {
			t.Fatalf("Upsert %d: %v", i, err)
		}
		last = updated
	}

	requests, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	if last == nil || last.ID != requests[0].ID {
		t.Fatalf("Upsert returned %v, stored %v", last, requests[0])
	}
	if requests[0].State != domain.MediaRequestStateWanted {
		t.Fatalf("state = %s, want wanted", requests[0].State)
	}
}

func TestServiceUpsertKeepsDoneForTheSameIntentAndReopensItOtherwise(t *testing.T) {
	repo := newTestRepo(t)
	svc := NewService(repo, testLogger())
	ctx := context.Background()

	request, err := svc.Upsert(ctx, UpsertInput{
		Provider: "wy", ExternalID: "42", TrackID: "track-1",
		Desired: domain.MediaRequestDesiredPromote, Quality: "320k",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := repo.MarkDone(ctx, request.ID, "track-local", time.Now()); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}

	again, err := svc.Upsert(ctx, UpsertInput{
		Provider: "wy", ExternalID: "42", TrackID: "track-1",
		Desired: domain.MediaRequestDesiredPromote, Quality: "320k",
	})
	if err != nil {
		t.Fatalf("Upsert again: %v", err)
	}
	if again.State != domain.MediaRequestStateDone {
		t.Fatalf("re-saving the same intent reopened a done request: %s", again.State)
	}

	changed, err := svc.Upsert(ctx, UpsertInput{
		Provider: "wy", ExternalID: "42", TrackID: "track-1",
		Desired: domain.MediaRequestDesiredDownload, Quality: "flac",
	})
	if err != nil {
		t.Fatalf("Upsert changed: %v", err)
	}
	if changed.ID != request.ID {
		t.Fatalf("a changed intent created a new row: %s vs %s", changed.ID, request.ID)
	}
	if changed.State != domain.MediaRequestStateWanted || changed.Desired != domain.MediaRequestDesiredDownload {
		t.Fatalf("changed intent = %+v, want wanted/flac", changed)
	}
}

func TestServiceRetryReopensAFailedRequestWithTheSameIntent(t *testing.T) {
	repo := newTestRepo(t)
	svc := NewService(repo, testLogger())
	ctx := context.Background()

	request, err := svc.Upsert(ctx, UpsertInput{
		Provider: "wy", ExternalID: "42", TrackID: "track-1",
		Desired: domain.MediaRequestDesiredDownload, Quality: "flac",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := repo.MarkFailed(ctx, request.ID, "403", 3, nil, time.Now()); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	retried, err := svc.Retry(ctx, request.ID)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if retried.State != domain.MediaRequestStateWanted ||
		retried.Desired != domain.MediaRequestDesiredDownload ||
		retried.Quality != "flac" || retried.Attempts != 0 || retried.LastError != "" {
		t.Fatalf("retried row = %+v", retried)
	}

	// A live request is returned unchanged, so a double click cannot reset it.
	unchanged, err := svc.Retry(ctx, request.ID)
	if err != nil {
		t.Fatalf("Retry live: %v", err)
	}
	if unchanged.State != domain.MediaRequestStateWanted {
		t.Fatalf("live retry changed the state: %+v", unchanged)
	}
}

func TestServiceCancelIsTerminalUntilTheNextSave(t *testing.T) {
	repo := newTestRepo(t)
	svc := NewService(repo, testLogger())
	ctx := context.Background()

	request, err := svc.Upsert(ctx, UpsertInput{
		Provider: "wy", ExternalID: "42", Desired: domain.MediaRequestDesiredDownload,
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	canceled, err := svc.Cancel(ctx, request.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if canceled.State != domain.MediaRequestStateCanceled || canceled.FinishedAt == nil {
		t.Fatalf("canceled row = %+v", canceled)
	}

	reopened, err := svc.Upsert(ctx, UpsertInput{
		Provider: "wy", ExternalID: "42", Desired: domain.MediaRequestDesiredDownload,
	})
	if err != nil {
		t.Fatalf("Upsert after cancel: %v", err)
	}
	if reopened.ID != request.ID || reopened.State != domain.MediaRequestStateWanted {
		t.Fatalf("reopened row = %+v", reopened)
	}
}

// stubLocalTrack is a LocalTrackChecker with fixed answers.
type stubLocalTrack struct {
	local    bool
	resolved *domain.TrackDTO
	match    *domain.TrackDTO
}

func (s stubLocalTrack) HasLocalTrack(context.Context, string, string) (bool, error) {
	return s.local, nil
}

func (s stubLocalTrack) ResolveLocalTrack(context.Context, string, string) (*domain.TrackDTO, error) {
	return s.resolved, nil
}

func (s stubLocalTrack) FindLocalMatch(context.Context, string, string) (*domain.TrackDTO, error) {
	return s.match, nil
}

// TestServiceUpsertReopensDoneWhenTheLocalFileIsGone is the delete/re-save
// regression: a finished row whose file no longer exists must run again.
func TestServiceUpsertReopensDoneWhenTheLocalFileIsGone(t *testing.T) {
	repo := newTestRepo(t)
	svc := NewService(repo, testLogger())
	svc.SetLocalTrackChecker(stubLocalTrack{local: false})
	ctx := context.Background()

	request, err := svc.Upsert(ctx, UpsertInput{
		Provider: "wy", ExternalID: "42", TrackID: "track-1",
		Desired: domain.MediaRequestDesiredPromote, Quality: "flac",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := repo.MarkDone(ctx, request.ID, "track-local", time.Now()); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}

	again, err := svc.Upsert(ctx, UpsertInput{
		Provider: "wy", ExternalID: "42", TrackID: "track-1",
		Desired: domain.MediaRequestDesiredPromote, Quality: "flac",
	})
	if err != nil {
		t.Fatalf("Upsert after delete: %v", err)
	}
	if again.State != domain.MediaRequestStateWanted {
		t.Fatalf("state = %s, want wanted after the local file disappeared", again.State)
	}
	if again.Desired != domain.MediaRequestDesiredPromote || again.Quality != "flac" {
		t.Fatalf("reopened intent = %+v, want the original promote/flac", again)
	}
}

// TestServiceUpsertKeepsDoneWhenTheLocalFileExists: the idempotent path stays
// intact for a healthy library row.
func TestServiceUpsertKeepsDoneWhenTheLocalFileExists(t *testing.T) {
	repo := newTestRepo(t)
	svc := NewService(repo, testLogger())
	svc.SetLocalTrackChecker(stubLocalTrack{local: true})
	ctx := context.Background()

	request, err := svc.Upsert(ctx, UpsertInput{
		Provider: "wy", ExternalID: "42", Desired: domain.MediaRequestDesiredPromote, Quality: "flac",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := repo.MarkDone(ctx, request.ID, "track-local", time.Now()); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}

	again, err := svc.Upsert(ctx, UpsertInput{
		Provider: "wy", ExternalID: "42", Desired: domain.MediaRequestDesiredPromote, Quality: "flac",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if again.State != domain.MediaRequestStateDone {
		t.Fatalf("state = %s, want the finished row to stay done", again.State)
	}
}

// TestServiceInvalidateLocalFileReopensOnlyDoneRows covers the library's delete
// hook: a finished row becomes wanted, a live row is untouched.
func TestServiceInvalidateLocalFileReopensOnlyDoneRows(t *testing.T) {
	repo := newTestRepo(t)
	svc := NewService(repo, testLogger())
	ctx := context.Background()

	request, err := svc.Upsert(ctx, UpsertInput{
		Provider: "wy", ExternalID: "42", Desired: domain.MediaRequestDesiredPromote,
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	changed, err := svc.InvalidateLocalFile(ctx, "wy", "42")
	if err != nil {
		t.Fatalf("InvalidateLocalFile (wanted): %v", err)
	}
	if changed {
		t.Fatal("a live request must not be reopened")
	}
	if err := repo.MarkDone(ctx, request.ID, "track-local", time.Now()); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	changed, err = svc.InvalidateLocalFile(ctx, "wy", "42")
	if err != nil || !changed {
		t.Fatalf("InvalidateLocalFile (done) = %v/%v, want true", changed, err)
	}
	got, err := repo.GetByIdentity(ctx, "wy", "42")
	if err != nil {
		t.Fatalf("GetByIdentity: %v", err)
	}
	if got == nil || got.State != domain.MediaRequestStateWanted || got.FinishedAt != nil {
		t.Fatalf("reopened row = %+v, want wanted with no finish time", got)
	}
}

// TestWorkerReconcileDoneRequestsReopensMissingFiles is the startup sweep: a
// file deleted while the app was closed must not leave a done row behind.
func TestWorkerReconcileDoneRequestsReopensMissingFiles(t *testing.T) {
	repo := newTestRepo(t)
	svc := NewService(repo, testLogger())
	ctx := context.Background()

	request, err := svc.Upsert(ctx, UpsertInput{
		Provider: "wy", ExternalID: "42", Desired: domain.MediaRequestDesiredPromote,
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := repo.MarkDone(ctx, request.ID, "track-local", time.Now()); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}

	worker := NewWorker(repo, nil, testLogger())
	worker.SetLocalTrackChecker(stubLocalTrack{local: false})
	worker.reconcileDoneRequests(ctx)

	got, err := repo.GetByIdentity(ctx, "wy", "42")
	if err != nil {
		t.Fatalf("GetByIdentity: %v", err)
	}
	if got == nil || got.State != domain.MediaRequestStateWanted {
		t.Fatalf("reconciled row = %+v, want wanted", got)
	}
}

func seedOrphanSong(t *testing.T, svc *Service, provider, externalID string) *domain.MediaRequest {
	t.Helper()
	request, err := svc.Upsert(context.Background(), UpsertInput{
		Provider: provider, ExternalID: externalID, TrackID: "track-" + provider,
		Title: "新·与天齐", Artist: "指尖笑",
		Desired: domain.MediaRequestDesiredPromote, Quality: "320k",
	})
	if err != nil {
		t.Fatalf("Upsert %s: %v", provider, err)
	}
	return request
}

func TestServiceConvergesDuplicateIdentitiesAfterAMerge(t *testing.T) {
	repo := newTestRepo(t)
	svc := NewService(repo, testLogger())
	svc.SetLocalTrackChecker(stubLocalTrack{resolved: localTrack()})
	ctx := context.Background()

	kg := seedOrphanSong(t, svc, "kg", "91a6ce")
	tx := seedOrphanSong(t, svc, "tx", "004Kao")
	survivor := &domain.TrackDTO{
		Track:   domain.Track{ID: "track-local", Title: "新·与天齐"},
		Artists: []*domain.Artist{{Name: "指尖笑"}},
	}

	changed, err := svc.ConvergeForTrack(ctx, survivor)
	if err != nil {
		t.Fatalf("ConvergeForTrack: %v", err)
	}
	if changed != 2 {
		t.Fatalf("changed = %d, want both identities", changed)
	}
	for _, id := range []string{kg.ID, tx.ID} {
		got, err := repo.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.State != domain.MediaRequestStateDone {
			t.Fatalf("%s state = %s, want done", id, got.State)
		}
		if got.TrackID != "track-local" {
			t.Fatalf("%s track id = %q, want the local row", id, got.TrackID)
		}
	}
}

func TestServiceCancelsDuplicateIdentitiesWithoutALocalCopy(t *testing.T) {
	repo := newTestRepo(t)
	svc := NewService(repo, testLogger())
	svc.SetLocalTrackChecker(stubLocalTrack{})
	ctx := context.Background()

	kg := seedOrphanSong(t, svc, "kg", "91a6ce")
	tx := seedOrphanSong(t, svc, "tx", "004Kao")

	changed, err := svc.ConvergeForTrack(ctx, &domain.TrackDTO{
		Track:   domain.Track{Title: "新·与天齐"},
		Artists: []*domain.Artist{{Name: "指尖笑"}},
	})
	if err != nil {
		t.Fatalf("ConvergeForTrack: %v", err)
	}
	if changed != 2 {
		t.Fatalf("changed = %d, want both identities", changed)
	}
	for _, id := range []string{kg.ID, tx.ID} {
		got, err := repo.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.State != domain.MediaRequestStateCanceled {
			t.Fatalf("%s state = %s, want canceled", id, got.State)
		}
		if got.NextAttemptAt != nil {
			t.Fatalf("%s next attempt = %v, want none", id, got.NextAttemptAt)
		}
	}
}

func TestServiceInvalidateLocalFileConvergesTheOtherIdentities(t *testing.T) {
	repo := newTestRepo(t)
	svc := NewService(repo, testLogger())
	svc.SetLocalTrackChecker(stubLocalTrack{})
	ctx := context.Background()

	wy := seedOrphanSong(t, svc, "wy", "42")
	if err := repo.MarkDone(ctx, wy.ID, "track-local", time.Now()); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	kg := seedOrphanSong(t, svc, "kg", "91a6ce")

	changed, err := svc.InvalidateLocalFile(ctx, "wy", "42")
	if err != nil {
		t.Fatalf("InvalidateLocalFile: %v", err)
	}
	if !changed {
		t.Fatal("InvalidateLocalFile = false, want the finished row reopened")
	}

	reopened, err := repo.GetByID(ctx, wy.ID)
	if err != nil {
		t.Fatalf("GetByID wy: %v", err)
	}
	if reopened.State != domain.MediaRequestStateWanted {
		t.Fatalf("wy state = %s, want wanted", reopened.State)
	}
	other, err := repo.GetByID(ctx, kg.ID)
	if err != nil {
		t.Fatalf("GetByID kg: %v", err)
	}
	if other.State != domain.MediaRequestStateCanceled {
		t.Fatalf("kg state = %s, want canceled", other.State)
	}
}
