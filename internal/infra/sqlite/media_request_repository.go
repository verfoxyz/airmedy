package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"airmedy/internal/domain"
)

type mediaRequestRepository struct {
	db *DB
}

// NewMediaRequestRepository returns the SQLite-backed media request store.
func NewMediaRequestRepository(db *DB) domain.MediaRequestRepository {
	return &mediaRequestRepository{db: db}
}

const mediaRequestColumns = `id, provider, external_id, COALESCE(track_id, '') AS track_id,
	title, artist, desired, quality, state, phase, attempts, last_error, next_attempt_at,
	created_at, updated_at, finished_at`

func (r *mediaRequestRepository) Ensure(ctx context.Context, request *domain.MediaRequest) error {
	if request == nil {
		return fmt.Errorf("media request: nil request")
	}
	if request.State == "" {
		request.State = domain.MediaRequestStateWanted
	}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO media_requests
		 (id, provider, external_id, track_id, title, artist, desired, quality, state, phase,
		  attempts, last_error, next_attempt_at, created_at, updated_at, finished_at)
		 VALUES (?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(provider, external_id) DO NOTHING`,
		request.ID, request.Provider, request.ExternalID, request.TrackID,
		request.Title, request.Artist,
		request.Desired, request.Quality, request.State, request.Phase,
		request.Attempts, request.LastError, request.NextAttemptAt,
		request.CreatedAt, request.UpdatedAt, request.FinishedAt,
	); err != nil {
		return fmt.Errorf("media request: insert %s/%s: %w", request.Provider, request.ExternalID, err)
	}
	return nil
}

func (r *mediaRequestRepository) GetByID(ctx context.Context, id string) (*domain.MediaRequest, error) {
	var request domain.MediaRequest
	err := r.db.GetContext(ctx, &request,
		`SELECT `+mediaRequestColumns+` FROM media_requests WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("media request: get %s: %w", id, err)
	}
	return &request, nil
}

func (r *mediaRequestRepository) GetByIdentity(
	ctx context.Context, provider, externalID string,
) (*domain.MediaRequest, error) {
	var request domain.MediaRequest
	err := r.db.GetContext(ctx, &request,
		`SELECT `+mediaRequestColumns+`
		 FROM media_requests WHERE provider = ? AND external_id = ?`,
		provider, externalID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("media request: get %s/%s: %w", provider, externalID, err)
	}
	return &request, nil
}

func (r *mediaRequestRepository) List(ctx context.Context) ([]*domain.MediaRequest, error) {
	var requests []*domain.MediaRequest
	if err := r.db.SelectContext(ctx, &requests,
		`SELECT `+mediaRequestColumns+` FROM media_requests ORDER BY updated_at DESC`,
	); err != nil {
		return nil, fmt.Errorf("media request: list: %w", err)
	}
	return requests, nil
}

func (r *mediaRequestRepository) ListActive(
	ctx context.Context, now time.Time,
) ([]*domain.MediaRequest, error) {
	var requests []*domain.MediaRequest
	if err := r.db.SelectContext(ctx, &requests,
		`SELECT `+mediaRequestColumns+`
		 FROM media_requests
		 WHERE state IN (?, ?, ?)
		    OR (state = ? AND next_attempt_at IS NOT NULL AND next_attempt_at <= ?)
		 ORDER BY updated_at`,
		domain.MediaRequestStateWanted, domain.MediaRequestStateQueued,
		domain.MediaRequestStateRunning,
		domain.MediaRequestStateFailed, now,
	); err != nil {
		return nil, fmt.Errorf("media request: list active: %w", err)
	}
	return requests, nil
}

func (r *mediaRequestRepository) UpdateIntent(
	ctx context.Context, id, desired, quality, trackID string, reset bool, at time.Time,
) error {
	var err error
	if reset {
		_, err = r.db.ExecContext(ctx,
			`UPDATE media_requests
			 SET desired = ?, quality = ?, track_id = COALESCE(NULLIF(?, ''), track_id),
			     state = ?, phase = '', attempts = 0, last_error = '',
			     next_attempt_at = NULL, finished_at = NULL, updated_at = ?
			 WHERE id = ?`,
			desired, quality, trackID, domain.MediaRequestStateWanted, at, id,
		)
	} else {
		_, err = r.db.ExecContext(ctx,
			`UPDATE media_requests
			 SET desired = ?, quality = ?, track_id = COALESCE(NULLIF(?, ''), track_id),
			     updated_at = ?
			 WHERE id = ?`,
			desired, quality, trackID, at, id,
		)
	}
	if err != nil {
		return fmt.Errorf("media request: update intent %s: %w", id, err)
	}
	return nil
}

func (r *mediaRequestRepository) ReopenDone(ctx context.Context, id string, at time.Time) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE media_requests
		 SET state = ?, phase = '', attempts = 0, last_error = '',
		     next_attempt_at = NULL, finished_at = NULL, updated_at = ?
		 WHERE id = ? AND state = ?`,
		domain.MediaRequestStateWanted, at, id, domain.MediaRequestStateDone,
	); err != nil {
		return fmt.Errorf("media request: reopen %s: %w", id, err)
	}
	return nil
}

func (r *mediaRequestRepository) MarkRunning(
	ctx context.Context, id, phase, trackID string, at time.Time,
) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE media_requests
		 SET state = ?, phase = ?, track_id = COALESCE(NULLIF(?, ''), track_id),
		     next_attempt_at = NULL, updated_at = ?
		 WHERE id = ?`,
		domain.MediaRequestStateRunning, phase, trackID, at, id,
	); err != nil {
		return fmt.Errorf("media request: mark running %s: %w", id, err)
	}
	return nil
}

func (r *mediaRequestRepository) MarkDone(ctx context.Context, id, trackID string, at time.Time) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE media_requests
		 SET state = ?, phase = '', track_id = COALESCE(NULLIF(?, ''), track_id),
		     last_error = '', next_attempt_at = NULL, finished_at = ?, updated_at = ?
		 WHERE id = ?`,
		domain.MediaRequestStateDone, trackID, at, at, id,
	); err != nil {
		return fmt.Errorf("media request: mark done %s: %w", id, err)
	}
	return nil
}

func (r *mediaRequestRepository) MarkFailed(
	ctx context.Context, id, message string, attempts int, next *time.Time, at time.Time,
) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE media_requests
		 SET state = ?, phase = '', last_error = ?, attempts = ?, next_attempt_at = ?,
		     finished_at = CASE WHEN ? IS NULL THEN ? ELSE NULL END, updated_at = ?
		 WHERE id = ?`,
		domain.MediaRequestStateFailed, message, attempts, next, next, at, at, id,
	); err != nil {
		return fmt.Errorf("media request: mark failed %s: %w", id, err)
	}
	return nil
}

func (r *mediaRequestRepository) MarkCanceled(ctx context.Context, id string, at time.Time) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE media_requests
		 SET state = ?, phase = '', next_attempt_at = NULL, finished_at = ?, updated_at = ?
		 WHERE id = ?`,
		domain.MediaRequestStateCanceled, at, at, id,
	); err != nil {
		return fmt.Errorf("media request: cancel %s: %w", id, err)
	}
	return nil
}

func (r *mediaRequestRepository) RecordEvent(
	ctx context.Context, event *domain.MediaRequestEvent,
) error {
	if event == nil {
		return nil
	}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO media_request_events (id, request_id, state, phase, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		event.ID, event.RequestID, event.State, event.Phase, event.Detail, event.CreatedAt,
	); err != nil {
		return fmt.Errorf("media request: record event %s: %w", event.RequestID, err)
	}
	return nil
}

// TrimFinished keeps the newest finished rows and drops the events of the rows
// it deleted. Live states are never touched, so a save can always find its row.
func (r *mediaRequestRepository) TrimFinished(ctx context.Context, keep int) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM media_requests
		 WHERE state IN (?, ?, ?)
		   AND id NOT IN (
		       SELECT id FROM media_requests
		       WHERE state IN (?, ?, ?)
		       ORDER BY COALESCE(finished_at, updated_at) DESC
		       LIMIT ?
		   )`,
		domain.MediaRequestStateDone, domain.MediaRequestStateFailed,
		domain.MediaRequestStateCanceled,
		domain.MediaRequestStateDone, domain.MediaRequestStateFailed,
		domain.MediaRequestStateCanceled, keep,
	)
	if err != nil {
		return 0, fmt.Errorf("media request: trim history: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("media request: trim history: %w", err)
	}
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM media_request_events
		 WHERE request_id NOT IN (SELECT id FROM media_requests)`,
	); err != nil {
		return deleted, fmt.Errorf("media request: trim events: %w", err)
	}
	return deleted, nil
}
