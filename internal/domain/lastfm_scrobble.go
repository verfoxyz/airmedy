package domain

import (
	"context"
	"time"
)

// PendingScrobble is one qualified playback waiting for Last.fm.
//
// A play is written here before the network request, so a temporary failure
// only delays it. StartedAt is the scrobble timestamp (Unix seconds). Attempts
// counts failed sends; NextRetryAt is the earliest time the retry worker may
// pick the row up again (nil means "never", used for rows Last.fm ignored).
type PendingScrobble struct {
	ID          string     `json:"id" db:"id"`
	Artist      string     `json:"artist" db:"artist"`
	Title       string     `json:"title" db:"title"`
	Album       string     `json:"album" db:"album"`
	AlbumArtist string     `json:"album_artist" db:"album_artist"`
	TrackNumber int        `json:"track_number" db:"track_number"`
	Duration    int        `json:"duration" db:"duration"`
	StartedAt   int64      `json:"started_at" db:"started_at"`
	Attempts    int        `json:"attempts" db:"attempts"`
	LastError   string     `json:"last_error" db:"last_error"`
	NextRetryAt *time.Time `json:"next_retry_at" db:"next_retry_at"`
	CreatedAt   time.Time  `json:"created_at" db:"created_at"`
}

// PendingScrobbleRepository persists the durable Last.fm scrobble queue.
//
// Enqueue is idempotent for one play: (artist, title, started_at) is unique, so
// a retried listener callback cannot double-count a play.
type PendingScrobbleRepository interface {
	// Enqueue inserts the row unless this play is already queued.
	Enqueue(ctx context.Context, scrobble *PendingScrobble) error
	// Due returns rows whose retry time has passed, oldest play first, capped
	// at limit. Rows that already used maxAttempts are skipped.
	Due(ctx context.Context, now time.Time, maxAttempts, limit int) ([]PendingScrobble, error)
	// Delete removes rows that were accepted by Last.fm.
	Delete(ctx context.Context, ids []string) error
	// MarkFailure records one failed attempt and when to try again.
	MarkFailure(ctx context.Context, id, message string, nextRetry time.Time) error
	// MarkIgnored stops retrying a row Last.fm permanently refused (nil next
	// retry means it is no longer due).
	MarkIgnored(ctx context.Context, id, message string) error
	// Cleanup drops rows older than olderThan or with attempts >= maxAttempts,
	// then trims the queue to keep newest rows. It returns how many were removed.
	Cleanup(ctx context.Context, olderThan time.Time, maxAttempts, keep int) (int64, error)
}
