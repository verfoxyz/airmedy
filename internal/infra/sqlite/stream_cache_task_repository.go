package sqlite

import (
	"context"
	"fmt"
	"time"

	"airmedy/internal/domain"
)

type streamCacheTaskRepository struct {
	db *DB
}

// NewStreamCacheTaskRepository returns the SQLite-backed stream cache history.
func NewStreamCacheTaskRepository(db *DB) domain.StreamCacheTaskRepository {
	return &streamCacheTaskRepository{db: db}
}

func (r *streamCacheTaskRepository) Start(ctx context.Context, task *domain.StreamCacheTask) error {
	if task == nil {
		return fmt.Errorf("stream cache task: nil task")
	}
	if task.State == "" {
		task.State = domain.StreamCacheTaskStateActive
	}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO stream_cache_tasks
		 (id, provider, external_id, title, artist, quality, state,
		  bytes_total, bytes_done, error, started_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?)`,
		task.ID, task.Provider, task.ExternalID, task.Title, task.Artist, task.Quality,
		task.State, task.BytesTotal, task.BytesDone, task.StartedAt,
	); err != nil {
		return fmt.Errorf("stream cache task: insert %s: %w", task.ID, err)
	}
	return nil
}

func (r *streamCacheTaskRepository) Finish(
	ctx context.Context, id string, totalBytes, doneBytes int64, at time.Time,
) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE stream_cache_tasks
		 SET state = ?, bytes_total = ?, bytes_done = ?, error = '', finished_at = ?
		 WHERE id = ?`,
		domain.StreamCacheTaskStateDone, totalBytes, doneBytes, at, id,
	); err != nil {
		return fmt.Errorf("stream cache task: finish %s: %w", id, err)
	}
	return nil
}

func (r *streamCacheTaskRepository) Fail(
	ctx context.Context, id, message string, totalBytes, doneBytes int64, at time.Time,
) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE stream_cache_tasks
		 SET state = ?, error = ?, bytes_total = ?, bytes_done = ?, finished_at = ?
		 WHERE id = ?`,
		domain.StreamCacheTaskStateFailed, message, totalBytes, doneBytes, at, id,
	); err != nil {
		return fmt.Errorf("stream cache task: fail %s: %w", id, err)
	}
	return nil
}

func (r *streamCacheTaskRepository) Cancel(
	ctx context.Context, id string, totalBytes, doneBytes int64, at time.Time,
) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE stream_cache_tasks
		 SET state = ?, error = '', bytes_total = ?, bytes_done = ?, finished_at = ?
		 WHERE id = ?`,
		domain.StreamCacheTaskStateCanceled, totalBytes, doneBytes, at, id,
	); err != nil {
		return fmt.Errorf("stream cache task: cancel %s: %w", id, err)
	}
	return nil
}

// MarkActiveInterrupted closes rows a shutdown or a crash left active. The
// message is intentionally ignored: an intentional stop is not an error.
func (r *streamCacheTaskRepository) MarkActiveInterrupted(
	ctx context.Context, _ string, at time.Time,
) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE stream_cache_tasks SET state = ?, error = '', finished_at = ? WHERE state = ?`,
		domain.StreamCacheTaskStateCanceled, at, domain.StreamCacheTaskStateActive,
	)
	if err != nil {
		return 0, fmt.Errorf("stream cache task: mark interrupted: %w", err)
	}
	return res.RowsAffected()
}

func (r *streamCacheTaskRepository) List(ctx context.Context) ([]*domain.StreamCacheTask, error) {
	var tasks []*domain.StreamCacheTask
	if err := r.db.SelectContext(ctx, &tasks,
		`SELECT * FROM stream_cache_tasks ORDER BY COALESCE(finished_at, started_at) DESC`,
	); err != nil {
		return nil, fmt.Errorf("stream cache task: list: %w", err)
	}
	return tasks, nil
}

func (r *streamCacheTaskRepository) TrimFinished(ctx context.Context, keep int) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM stream_cache_tasks
		WHERE state IN (?, ?, ?)
		  AND id NOT IN (
		    SELECT id FROM stream_cache_tasks
		    WHERE state IN (?, ?, ?)
		    ORDER BY COALESCE(finished_at, started_at) DESC
		    LIMIT ?
		  )`,
		domain.StreamCacheTaskStateDone, domain.StreamCacheTaskStateFailed,
		domain.StreamCacheTaskStateCanceled,
		domain.StreamCacheTaskStateDone, domain.StreamCacheTaskStateFailed,
		domain.StreamCacheTaskStateCanceled,
		keep,
	)
	if err != nil {
		return 0, fmt.Errorf("stream cache task: trim history: %w", err)
	}
	return res.RowsAffected()
}
