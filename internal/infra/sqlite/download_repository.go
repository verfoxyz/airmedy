package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"airmedy/internal/domain"

	sqlite3 "github.com/mattn/go-sqlite3"
)

type downloadRepository struct {
	db *DB
}

// NewDownloadJobRepository returns the SQLite-backed download queue.
func NewDownloadJobRepository(db *DB) domain.DownloadJobRepository {
	return &downloadRepository{db: db}
}

const downloadJobSelectFields = `
	j.id, j.track_id, j.playlist_id, j.quality, j.state, j.attempts, j.max_attempts,
	j.next_retry_at, j.last_error, j.bytes_done, j.bytes_total, j.target_path,
	j.created_at, j.updated_at, j.started_at, j.finished_at
`

func (r *downloadRepository) Create(ctx context.Context, job *domain.DownloadJob) error {
	if job.Quality == "" {
		job.Quality = "320k"
	}
	if job.MaxAttempts <= 0 {
		job.MaxAttempts = domain.DefaultDownloadMaxAttempts
	}
	now := time.Now()
	job.CreatedAt, job.UpdatedAt = now, now

	_, err := r.db.Ext(ctx).ExecContext(ctx, `
		INSERT INTO download_jobs (
			id, track_id, playlist_id, quality, state, attempts, max_attempts,
			next_retry_at, last_error, bytes_done, bytes_total, target_path,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.TrackID, job.PlaylistID, job.Quality, job.State, job.Attempts, job.MaxAttempts,
		job.NextRetryAt, job.LastError, job.BytesDone, job.BytesTotal, job.TargetPath,
		job.CreatedAt, job.UpdatedAt,
	)
	if err != nil {
		// The partial unique index is the source of truth for "one active job
		// per track"; translate it into a typed error for callers.
		var sqliteErr sqlite3.Error
		if errors.As(err, &sqliteErr) && sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique {
			return domain.ErrDownloadAlreadyQueued
		}
		return fmt.Errorf("failed to create download job: %w", err)
	}
	return nil
}

func (r *downloadRepository) GetByID(ctx context.Context, id string) (*domain.DownloadJob, error) {
	var job domain.DownloadJob
	query := fmt.Sprintf(`SELECT %s FROM download_jobs j WHERE j.id = ?`, downloadJobSelectFields)
	if err := r.db.GetContext(ctx, &job, query, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get download job: %w", err)
	}
	return &job, nil
}

func (r *downloadRepository) GetActiveByTrackID(ctx context.Context, trackID string) (*domain.DownloadJob, error) {
	var job domain.DownloadJob
	query := fmt.Sprintf(`SELECT %s FROM download_jobs j
		WHERE j.track_id = ? AND j.state IN (?, ?)
		ORDER BY j.created_at LIMIT 1`, downloadJobSelectFields)
	if err := r.db.GetContext(ctx, &job, query, trackID, domain.DownloadStateQueued, domain.DownloadStateActive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get active download job: %w", err)
	}
	return &job, nil
}

func (r *downloadRepository) List(ctx context.Context) ([]*domain.DownloadJob, error) {
	var jobs []*domain.DownloadJob
	query := fmt.Sprintf(`SELECT %s FROM download_jobs j ORDER BY j.created_at`, downloadJobSelectFields)
	if err := r.db.SelectContext(ctx, &jobs, query); err != nil {
		return nil, fmt.Errorf("failed to list download jobs: %w", err)
	}
	return jobs, nil
}

// ClaimNext moves the oldest due queued job to active inside one transaction, so
// two workers (or a restart racing a worker) can never take the same job.
func (r *downloadRepository) ClaimNext(ctx context.Context, now time.Time) (*domain.DownloadJob, error) {
	var claimed *domain.DownloadJob
	err := r.db.RunTx(ctx, func(ctx context.Context) error {
		ex := r.db.Ext(ctx)

		var id string
		err := ex.GetContext(ctx, &id, `
			SELECT id FROM download_jobs
			WHERE state = ? AND (next_retry_at IS NULL OR next_retry_at <= ?)
			ORDER BY created_at LIMIT 1`, domain.DownloadStateQueued, now)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to pick a queued download: %w", err)
		}

		res, err := ex.ExecContext(ctx, `
			UPDATE download_jobs
			SET state = ?, started_at = COALESCE(started_at, ?), updated_at = ?
			WHERE id = ? AND state = ?`,
			domain.DownloadStateActive, now, now, id, domain.DownloadStateQueued)
		if err != nil {
			return fmt.Errorf("failed to claim download job: %w", err)
		}
		if affected, err := res.RowsAffected(); err != nil || affected == 0 {
			// Lost the race; another worker owns it now.
			return nil
		}

		query := fmt.Sprintf(`SELECT %s FROM download_jobs j WHERE j.id = ?`, downloadJobSelectFields)
		var job domain.DownloadJob
		if err := ex.GetContext(ctx, &job, query, id); err != nil {
			return fmt.Errorf("failed to read claimed download job: %w", err)
		}
		claimed = &job
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

func (r *downloadRepository) UpdateProgress(ctx context.Context, id string, bytesDone, bytesTotal int64) error {
	if _, err := r.db.Ext(ctx).ExecContext(ctx, `
		UPDATE download_jobs
		SET bytes_done = ?, bytes_total = ?, updated_at = ?
		WHERE id = ?`, bytesDone, bytesTotal, time.Now(), id); err != nil {
		return fmt.Errorf("failed to update download progress: %w", err)
	}
	return nil
}

func (r *downloadRepository) MarkDone(ctx context.Context, id, targetPath string) error {
	now := time.Now()
	res, err := r.db.Ext(ctx).ExecContext(ctx, `
		UPDATE download_jobs
		SET state = ?, target_path = ?, finished_at = ?, updated_at = ?, last_error = ''
		WHERE id = ? AND state = ?`,
		domain.DownloadStateDone, targetPath, now, now, id, domain.DownloadStateActive)
	if err != nil {
		return fmt.Errorf("failed to mark download done: %w", err)
	}
	// Zero rows means the job is already closed (a retry that lost a race) or
	// the row is gone (its track was deleted while the job ran). Either way the
	// caller has nothing left to close, and failing here would report a
	// finished download as a failure, so this is an idempotent success.
	if _, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("failed to mark download done: %w", err)
	}
	return nil
}

func (r *downloadRepository) MarkFailed(ctx context.Context, id, message string, nextRetryAt *time.Time) error {
	now := time.Now()
	state := domain.DownloadStateFailed
	finished := &now
	if nextRetryAt != nil {
		// Retry reuses the same row: back to queued with a due time.
		state = domain.DownloadStateQueued
		finished = nil
	}
	_, err := r.db.Ext(ctx).ExecContext(ctx, `
		UPDATE download_jobs
		SET state = ?, last_error = ?, next_retry_at = ?, finished_at = ?,
		    attempts = attempts + 1, updated_at = ?
		WHERE id = ?`,
		state, message, nextRetryAt, finished, now, id)
	if err != nil {
		return fmt.Errorf("failed to mark download failed: %w", err)
	}
	return nil
}

func (r *downloadRepository) Cancel(ctx context.Context, id string) error {
	now := time.Now()
	_, err := r.db.Ext(ctx).ExecContext(ctx, `
		UPDATE download_jobs
		SET state = ?, finished_at = ?, updated_at = ?
		WHERE id = ? AND state IN (?, ?)`,
		domain.DownloadStateCanceled, now, now, id,
		domain.DownloadStateQueued, domain.DownloadStateActive)
	if err != nil {
		return fmt.Errorf("failed to cancel download job: %w", err)
	}
	return nil
}

func (r *downloadRepository) DeleteByTrack(ctx context.Context, trackID string) error {
	if _, err := r.db.Ext(ctx).ExecContext(ctx, `DELETE FROM download_jobs WHERE track_id = ?`, trackID); err != nil {
		return fmt.Errorf("failed to delete download jobs for track %s: %w", trackID, err)
	}
	return nil
}

func (r *downloadRepository) ResetActiveToQueued(ctx context.Context) (int64, error) {
	res, err := r.db.Ext(ctx).ExecContext(ctx, `
		UPDATE download_jobs SET state = ?, updated_at = ? WHERE state = ?`,
		domain.DownloadStateQueued, time.Now(), domain.DownloadStateActive)
	if err != nil {
		return 0, fmt.Errorf("failed to reset active downloads: %w", err)
	}
	return res.RowsAffected()
}
