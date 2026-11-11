package sqlite

import (
	"context"
	"fmt"
	"time"

	"airmedy/internal/domain"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type lastfmPendingScrobbleRepository struct {
	db *DB
}

// NewLastFmPendingScrobbleRepository returns the durable Last.fm scrobble queue.
func NewLastFmPendingScrobbleRepository(db *DB) domain.PendingScrobbleRepository {
	return &lastfmPendingScrobbleRepository{db: db}
}

const pendingScrobbleColumns = `id, artist, title, album, album_artist,
	track_number, duration, started_at, attempts, last_error, next_retry_at, created_at`

// Enqueue writes the play once. The unique (artist, title, started_at) key makes
// a repeated listener callback a no-op.
func (r *lastfmPendingScrobbleRepository) Enqueue(ctx context.Context, scrobble *domain.PendingScrobble) error {
	if scrobble == nil {
		return fmt.Errorf("lastfm queue: nil scrobble")
	}
	if scrobble.ID == "" {
		scrobble.ID = uuid.NewString()
	}
	nextRetry := scrobble.NextRetryAt
	if nextRetry == nil {
		now := time.Now().UTC()
		nextRetry = &now
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO lastfm_pending_scrobbles
		 (id, artist, title, album, album_artist, track_number, duration, started_at, attempts, last_error, next_retry_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(artist, title, started_at) DO NOTHING`,
		scrobble.ID, scrobble.Artist, scrobble.Title, scrobble.Album, scrobble.AlbumArtist,
		scrobble.TrackNumber, scrobble.Duration, scrobble.StartedAt, scrobble.Attempts,
		scrobble.LastError, nextRetry.UTC(),
	)
	if err != nil {
		return fmt.Errorf("lastfm queue: enqueue %q: %w", scrobble.Title, err)
	}
	return nil
}

// Due returns the next batch in play order. A row with a NULL retry time was
// permanently refused and is never due again.
func (r *lastfmPendingScrobbleRepository) Due(
	ctx context.Context, now time.Time, maxAttempts, limit int,
) ([]domain.PendingScrobble, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows := make([]domain.PendingScrobble, 0, limit)
	err := r.db.SelectContext(ctx, &rows,
		`SELECT `+pendingScrobbleColumns+` FROM lastfm_pending_scrobbles
		 WHERE attempts < ?
		   AND next_retry_at IS NOT NULL
		   AND datetime(next_retry_at) <= datetime(?)
		 ORDER BY started_at ASC, created_at ASC
		 LIMIT ?`,
		maxAttempts, now.UTC(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("lastfm queue: list due scrobbles: %w", err)
	}
	return rows, nil
}

func (r *lastfmPendingScrobbleRepository) Delete(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	query, args, err := sqlx.In(`DELETE FROM lastfm_pending_scrobbles WHERE id IN (?)`, ids)
	if err != nil {
		return fmt.Errorf("lastfm queue: build delete: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, r.db.Rebind(query), args...); err != nil {
		return fmt.Errorf("lastfm queue: delete accepted scrobbles: %w", err)
	}
	return nil
}

func (r *lastfmPendingScrobbleRepository) MarkFailure(
	ctx context.Context, id, message string, nextRetry time.Time,
) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE lastfm_pending_scrobbles
		 SET attempts = attempts + 1, last_error = ?, next_retry_at = ?
		 WHERE id = ?`,
		message, nextRetry.UTC(), id,
	); err != nil {
		return fmt.Errorf("lastfm queue: mark failure %s: %w", id, err)
	}
	return nil
}

func (r *lastfmPendingScrobbleRepository) MarkIgnored(ctx context.Context, id, message string) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE lastfm_pending_scrobbles
		 SET last_error = ?, next_retry_at = NULL
		 WHERE id = ?`,
		message, id,
	); err != nil {
		return fmt.Errorf("lastfm queue: mark ignored %s: %w", id, err)
	}
	return nil
}

// Cleanup enforces both retention rules in one pass: age/attempts expiry, then
// the hard cap that drops the oldest rows of a queue that grew without network.
func (r *lastfmPendingScrobbleRepository) Cleanup(
	ctx context.Context, olderThan time.Time, maxAttempts, keep int,
) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM lastfm_pending_scrobbles
		 WHERE datetime(created_at) < datetime(?) OR attempts >= ?`,
		olderThan.UTC(), maxAttempts,
	)
	if err != nil {
		return 0, fmt.Errorf("lastfm queue: expire scrobbles: %w", err)
	}
	removed, _ := result.RowsAffected()

	if keep > 0 {
		trim, err := r.db.ExecContext(ctx,
			`DELETE FROM lastfm_pending_scrobbles
			 WHERE id NOT IN (
			   SELECT id FROM lastfm_pending_scrobbles
			   ORDER BY datetime(created_at) DESC, id DESC
			   LIMIT ?
			 )`, keep,
		)
		if err != nil {
			return removed, fmt.Errorf("lastfm queue: trim scrobbles: %w", err)
		}
		trimmed, _ := trim.RowsAffected()
		removed += trimmed
	}
	return removed, nil
}
