package sqlite

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"airmedy/internal/domain"
)

func newPendingScrobbleTestRepo(t *testing.T) (*DB, domain.PendingScrobbleRepository) {
	t.Helper()
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"), slog.Default())
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, NewLastFmPendingScrobbleRepository(db)
}

func pending(artist, title string, startedAt int64) *domain.PendingScrobble {
	return &domain.PendingScrobble{
		Artist:    artist,
		Title:     title,
		Album:     "Album",
		StartedAt: startedAt,
		Duration:  200,
	}
}

func TestPendingScrobbleEnqueueIsIdempotent(t *testing.T) {
	_, repo := newPendingScrobbleTestRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()

	entry := pending("Artist", "Song", 1000)
	entry.NextRetryAt = &now
	if err := repo.Enqueue(ctx, entry); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// The same play reaching the listener twice must not queue twice.
	if err := repo.Enqueue(ctx, pending("Artist", "Song", 1000)); err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}

	due, err := repo.Due(ctx, now.Add(time.Minute), 20, 10)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("due = %d, want one row", len(due))
	}
	if due[0].Artist != "Artist" || due[0].Title != "Song" || due[0].Album != "Album" || due[0].StartedAt != 1000 {
		t.Fatalf("row = %+v, want the queued play", due[0])
	}
}

func TestPendingScrobbleDueIsOldestFirstAndCapped(t *testing.T) {
	_, repo := newPendingScrobbleTestRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, startedAt := range []int64{3000, 1000, 2000} {
		entry := pending("Artist", "Song", startedAt)
		entry.NextRetryAt = &now
		if err := repo.Enqueue(ctx, entry); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	due, err := repo.Due(ctx, now.Add(time.Minute), 20, 2)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(due) != 2 || due[0].StartedAt != 1000 || due[1].StartedAt != 2000 {
		t.Fatalf("due = %+v, want the two oldest plays in order", due)
	}
}

func TestPendingScrobbleFailureSchedulesTheNextAttempt(t *testing.T) {
	_, repo := newPendingScrobbleTestRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()

	entry := pending("Artist", "Song", 1000)
	entry.NextRetryAt = &now
	if err := repo.Enqueue(ctx, entry); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	due, _ := repo.Due(ctx, now, 20, 10)
	if len(due) != 1 {
		t.Fatalf("due = %d, want the new row", len(due))
	}
	if err := repo.MarkFailure(ctx, due[0].ID, "network down", now.Add(time.Minute)); err != nil {
		t.Fatalf("MarkFailure: %v", err)
	}

	// Not due yet, then due again with the attempt recorded.
	if rows, err := repo.Due(ctx, now.Add(30*time.Second), 20, 10); err != nil || len(rows) != 0 {
		t.Fatalf("early Due = %+v, %v; want nothing", rows, err)
	}
	rows, err := repo.Due(ctx, now.Add(2*time.Minute), 20, 10)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(rows) != 1 || rows[0].Attempts != 1 || rows[0].LastError != "network down" {
		t.Fatalf("row = %+v, want attempts=1 and the error", rows)
	}
	// maxAttempts is a hard cutoff for the worker.
	if rows, _ := repo.Due(ctx, now.Add(2*time.Minute), 1, 10); len(rows) != 0 {
		t.Fatalf("rows at the attempt limit = %+v, want none", rows)
	}
}

func TestPendingScrobbleIgnoredIsNeverDueAgain(t *testing.T) {
	_, repo := newPendingScrobbleTestRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()

	entry := pending("Artist", "Song", 1000)
	entry.NextRetryAt = &now
	if err := repo.Enqueue(ctx, entry); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	due, _ := repo.Due(ctx, now, 20, 10)
	if len(due) != 1 {
		t.Fatalf("due = %d, want the new row", len(due))
	}
	if err := repo.MarkIgnored(ctx, due[0].ID, "ignored by last.fm"); err != nil {
		t.Fatalf("MarkIgnored: %v", err)
	}
	if rows, err := repo.Due(ctx, now.Add(24*time.Hour), 20, 10); err != nil || len(rows) != 0 {
		t.Fatalf("Due after ignored = %+v, %v; want nothing", rows, err)
	}
}

func TestPendingScrobbleDeleteRemovesAcceptedRows(t *testing.T) {
	_, repo := newPendingScrobbleTestRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()

	first := pending("Artist", "One", 1000)
	first.NextRetryAt = &now
	second := pending("Artist", "Two", 2000)
	second.NextRetryAt = &now
	if err := repo.Enqueue(ctx, first); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := repo.Enqueue(ctx, second); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	due, _ := repo.Due(ctx, now, 20, 10)
	if len(due) != 2 {
		t.Fatalf("due = %d, want both rows", len(due))
	}
	if err := repo.Delete(ctx, []string{due[0].ID}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	left, _ := repo.Due(ctx, now, 20, 10)
	if len(left) != 1 || left[0].StartedAt != 2000 {
		t.Fatalf("left = %+v, want only the second play", left)
	}
}

func TestPendingScrobbleCleanupExpiresAndTrims(t *testing.T) {
	db, repo := newPendingScrobbleTestRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seed := func(title string, startedAt int64) string {
		entry := pending("Artist", title, startedAt)
		entry.NextRetryAt = &now
		if err := repo.Enqueue(ctx, entry); err != nil {
			t.Fatalf("Enqueue %s: %v", title, err)
		}
		return entry.ID
	}
	oldID := seed("Old", 1000)
	exhaustedID := seed("Exhausted", 2000)
	freshID := seed("Fresh", 3000)

	if _, err := db.ExecContext(ctx,
		`UPDATE lastfm_pending_scrobbles SET created_at = datetime('now', '-40 days') WHERE id = ?`, oldID); err != nil {
		t.Fatalf("age the row: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE lastfm_pending_scrobbles SET attempts = 20 WHERE id = ?`, exhaustedID); err != nil {
		t.Fatalf("exhaust the row: %v", err)
	}

	removed, err := repo.Cleanup(ctx, now.Add(-30*24*time.Hour), 20, 5000)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want the old and exhausted rows", removed)
	}
	left, _ := repo.Due(ctx, now.Add(time.Minute), 20, 10)
	if len(left) != 1 || left[0].ID != freshID {
		t.Fatalf("left = %+v, want only the fresh row", left)
	}

	// The hard cap keeps the newest rows, not an arbitrary subset. Give every
	// row a distinct creation time so the trim order is deterministic.
	seed("Newer", 4000)
	seed("Newest", 5000)
	seed("Newest 2", 6000)
	if _, err := db.ExecContext(ctx,
		`UPDATE lastfm_pending_scrobbles
		 SET created_at = datetime('now', '-' || (7000 - started_at) || ' seconds')`); err != nil {
		t.Fatalf("stagger created_at: %v", err)
	}
	if _, err := repo.Cleanup(ctx, now.Add(-30*24*time.Hour), 20, 2); err != nil {
		t.Fatalf("Cleanup trim: %v", err)
	}
	left, _ = repo.Due(ctx, now.Add(time.Minute), 20, 10)
	if len(left) != 2 || left[0].StartedAt != 5000 || left[1].StartedAt != 6000 {
		t.Fatalf("left = %+v, want the two newest plays", left)
	}
}
