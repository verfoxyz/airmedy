package sqlite

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"airmedy/internal/domain"
)

func newMediaRequestRepo(t *testing.T) domain.MediaRequestRepository {
	t.Helper()
	db, err := NewDB(":memory:", slog.Default())
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return NewMediaRequestRepository(db)
}

func mediaRequest(id, externalID, desired string, now time.Time) *domain.MediaRequest {
	return &domain.MediaRequest{
		ID: id, Provider: "wy", ExternalID: externalID, TrackID: "track-" + externalID,
		Desired: desired, Quality: "320k", State: domain.MediaRequestStateWanted,
		CreatedAt: now, UpdatedAt: now,
	}
}

func TestMediaRequestRepository_EnsureKeepsTheFirstIntent(t *testing.T) {
	repo := newMediaRequestRepo(t)
	ctx := context.Background()
	now := time.Now()

	if err := repo.Ensure(ctx, mediaRequest("r1", "42", domain.MediaRequestDesiredPromote, now)); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	second := mediaRequest("r2", "42", domain.MediaRequestDesiredDownload, now)
	second.Quality = "flac"
	if err := repo.Ensure(ctx, second); err != nil {
		t.Fatalf("Ensure again: %v", err)
	}

	requests, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1 (unique identity)", len(requests))
	}
	if requests[0].ID != "r1" || requests[0].Desired != domain.MediaRequestDesiredPromote ||
		requests[0].Quality != "320k" {
		t.Fatalf("existing row was rewritten: %+v", requests[0])
	}
}

func TestMediaRequestRepository_KeepsTheTitleSnapshot(t *testing.T) {
	repo := newMediaRequestRepo(t)
	ctx := context.Background()
	now := time.Now()

	request := mediaRequest("r1", "42", domain.MediaRequestDesiredPromote, now)
	request.Title = "All Eyes On Me"
	request.Artist = "Artist"
	if err := repo.Ensure(ctx, request); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	got, err := repo.GetByID(ctx, "r1")
	if err != nil || got == nil {
		t.Fatalf("GetByID = %+v, %v", got, err)
	}
	if got.Title != request.Title || got.Artist != request.Artist {
		t.Fatalf("snapshot = %q / %q, want the stored title and artist", got.Title, got.Artist)
	}
}

func TestMediaRequestRepository_ListActiveSkipsTerminalAndFutureRetries(t *testing.T) {
	repo := newMediaRequestRepo(t)
	ctx := context.Background()
	now := time.Now()
	later := now.Add(time.Hour)

	for _, request := range []*domain.MediaRequest{
		mediaRequest("wanted", "1", domain.MediaRequestDesiredPromote, now),
		{ID: "running", Provider: "wy", ExternalID: "2", Desired: domain.MediaRequestDesiredDownload,
			State: domain.MediaRequestStateRunning, CreatedAt: now, UpdatedAt: now},
		{ID: "due", Provider: "wy", ExternalID: "3", Desired: domain.MediaRequestDesiredDownload,
			State: domain.MediaRequestStateFailed, Attempts: 1, NextAttemptAt: &now,
			CreatedAt: now, UpdatedAt: now},
		{ID: "future", Provider: "wy", ExternalID: "4", Desired: domain.MediaRequestDesiredDownload,
			State: domain.MediaRequestStateFailed, Attempts: 1, NextAttemptAt: &later,
			CreatedAt: now, UpdatedAt: now},
		{ID: "done", Provider: "wy", ExternalID: "5", Desired: domain.MediaRequestDesiredPromote,
			State: domain.MediaRequestStateDone, CreatedAt: now, UpdatedAt: now, FinishedAt: &now},
	} {
		if err := repo.Ensure(ctx, request); err != nil {
			t.Fatalf("Ensure %s: %v", request.ID, err)
		}
	}

	active, err := repo.ListActive(ctx, now)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	ids := make([]string, 0, len(active))
	for _, request := range active {
		ids = append(ids, request.ID)
	}
	if len(ids) != 3 {
		t.Fatalf("active = %v, want wanted, running and the due retry", ids)
	}
	for _, want := range []string{"wanted", "running", "due"} {
		found := false
		for _, id := range ids {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("active = %v, missing %s", ids, want)
		}
	}
}

// TestMediaRequestRepository_ReopenDoneKeepsTheIntentAndOnlyTouchesDone:
// the delete hook re-opens a finished row without changing what the user asked
// for, and a live row is left alone.
func TestMediaRequestRepository_ReopenDoneKeepsTheIntentAndOnlyTouchesDone(t *testing.T) {
	repo := newMediaRequestRepo(t)
	ctx := context.Background()
	now := time.Now()

	if err := repo.Ensure(ctx, mediaRequest("r1", "42", domain.MediaRequestDesiredPromote, now)); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := repo.MarkRunning(ctx, "r1", domain.MediaRequestPhaseDownloading, "track-42", now); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := repo.ReopenDone(ctx, "r1", now); err != nil {
		t.Fatalf("ReopenDone on a live row: %v", err)
	}
	live, err := repo.GetByID(ctx, "r1")
	if err != nil || live == nil {
		t.Fatalf("GetByID: %v", err)
	}
	if live.State != domain.MediaRequestStateRunning {
		t.Fatalf("state = %s, want a live row untouched", live.State)
	}

	if err := repo.MarkDone(ctx, "r1", "track-42", now); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	if err := repo.ReopenDone(ctx, "r1", now.Add(time.Second)); err != nil {
		t.Fatalf("ReopenDone: %v", err)
	}
	got, err := repo.GetByID(ctx, "r1")
	if err != nil || got == nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.State != domain.MediaRequestStateWanted || got.FinishedAt != nil {
		t.Fatalf("reopened row = %+v, want wanted without a finish time", got)
	}
	if got.Desired != domain.MediaRequestDesiredPromote || got.Quality != "320k" {
		t.Fatalf("reopen changed the intent: %+v", got)
	}
}

func TestMediaRequestRepository_Transitions(t *testing.T) {
	repo := newMediaRequestRepo(t)
	ctx := context.Background()
	now := time.Now()
	if err := repo.Ensure(ctx, mediaRequest("r1", "42", domain.MediaRequestDesiredPromote, now)); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if err := repo.MarkRunning(ctx, "r1", domain.MediaRequestPhasePromoting, "track-new", now); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	running, err := repo.GetByID(ctx, "r1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if running.State != domain.MediaRequestStateRunning ||
		running.Phase != domain.MediaRequestPhasePromoting || running.TrackID != "track-new" {
		t.Fatalf("running row = %+v", running)
	}

	next := now.Add(time.Minute)
	if err := repo.MarkFailed(ctx, "r1", "boom", 1, &next, now); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	failed, err := repo.GetByID(ctx, "r1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if failed.State != domain.MediaRequestStateFailed || failed.Attempts != 1 ||
		failed.LastError != "boom" || failed.NextAttemptAt == nil || failed.FinishedAt != nil {
		t.Fatalf("retryable failed row = %+v", failed)
	}

	if err := repo.MarkFailed(ctx, "r1", "fatal", 20, nil, now); err != nil {
		t.Fatalf("MarkFailed terminal: %v", err)
	}
	terminal, err := repo.GetByID(ctx, "r1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if terminal.FinishedAt == nil || terminal.NextAttemptAt != nil {
		t.Fatalf("terminal failed row = %+v", terminal)
	}

	if err := repo.MarkDone(ctx, "r1", "track-done", now); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	done, err := repo.GetByID(ctx, "r1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if done.State != domain.MediaRequestStateDone || done.Phase != "" ||
		done.TrackID != "track-done" || done.NextAttemptAt != nil || done.FinishedAt == nil {
		t.Fatalf("done row = %+v", done)
	}
}

func TestMediaRequestRepository_UpdateIntentResetsTerminalRows(t *testing.T) {
	repo := newMediaRequestRepo(t)
	ctx := context.Background()
	now := time.Now()
	if err := repo.Ensure(ctx, mediaRequest("r1", "42", domain.MediaRequestDesiredPromote, now)); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	next := now.Add(time.Minute)
	if err := repo.MarkFailed(ctx, "r1", "boom", 3, &next, now); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	if err := repo.UpdateIntent(ctx, "r1", domain.MediaRequestDesiredDownload, "flac",
		"track-flac", true, now); err != nil {
		t.Fatalf("UpdateIntent: %v", err)
	}
	updated, err := repo.GetByID(ctx, "r1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if updated.State != domain.MediaRequestStateWanted || updated.Desired != domain.MediaRequestDesiredDownload ||
		updated.Quality != "flac" || updated.TrackID != "track-flac" {
		t.Fatalf("reopened row = %+v", updated)
	}
	if updated.Attempts != 0 || updated.LastError != "" ||
		updated.NextAttemptAt != nil || updated.FinishedAt != nil {
		t.Fatalf("reopened row kept stale failure state: %+v", updated)
	}
}

func TestMediaRequestRepository_TrimFinishedKeepsLiveWorkAndNewestHistory(t *testing.T) {
	repo := newMediaRequestRepo(t)
	ctx := context.Background()
	now := time.Now()

	if err := repo.Ensure(ctx, mediaRequest("live", "live", domain.MediaRequestDesiredPromote, now)); err != nil {
		t.Fatalf("Ensure live: %v", err)
	}
	for i := 0; i < 3; i++ {
		id := "done-" + string(rune('a'+i))
		finished := now.Add(time.Duration(i) * time.Minute)
		request := mediaRequest(id, id, domain.MediaRequestDesiredPromote, now)
		request.State = domain.MediaRequestStateDone
		request.FinishedAt = &finished
		if err := repo.Ensure(ctx, request); err != nil {
			t.Fatalf("Ensure %s: %v", id, err)
		}
	}

	deleted, err := repo.TrimFinished(ctx, 1)
	if err != nil {
		t.Fatalf("TrimFinished: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want the two older finished rows", deleted)
	}
	requests, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := map[string]bool{}
	for _, request := range requests {
		ids[request.ID] = true
	}
	if !ids["live"] || !ids["done-c"] || len(ids) != 2 {
		t.Fatalf("remaining = %v, want live and the newest finished row", ids)
	}

	if _, err := repo.TrimFinished(ctx, 0); err != nil {
		t.Fatalf("TrimFinished all: %v", err)
	}
	requests, err = repo.List(ctx)
	if err != nil {
		t.Fatalf("List after clear: %v", err)
	}
	if len(requests) != 1 || requests[0].ID != "live" {
		t.Fatalf("remaining = %+v, want only the live request", requests)
	}
}
