package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"airmedy/internal/domain"

	"github.com/google/uuid"
)

type promotionTaskRepository struct {
	db *DB
}

// NewPromotionTaskRepository returns the SQLite-backed promotion history.
func NewPromotionTaskRepository(db *DB) domain.PromotionTaskRepository {
	return &promotionTaskRepository{db: db}
}

// EnsureQueued inserts a queued row unless an active one already exists for the
// identity. A repeated save while the worker has not finished must not create a
// second row the tasks page would show twice.
func (r *promotionTaskRepository) EnsureQueued(ctx context.Context, task *domain.PromotionTask) (string, error) {
	if task == nil {
		return "", fmt.Errorf("promotion task: nil task")
	}
	var existing string
	err := r.db.GetContext(ctx, &existing,
		`SELECT id FROM promotion_tasks
		 WHERE provider = ? AND external_id = ? AND state IN (?, ?)
		 ORDER BY created_at DESC LIMIT 1`,
		task.Provider, task.ExternalID, domain.PromotionStateQueued, domain.PromotionStateRunning,
	)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("promotion task: look up active row: %w", err)
	}

	if task.ID == "" {
		task.ID = uuid.New().String()
	}
	if task.State == "" {
		task.State = domain.PromotionStateQueued
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now()
	}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO promotion_tasks
		 (id, track_id, provider, external_id, title, artist, state, quality, error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?)`,
		task.ID, task.TrackID, task.Provider, task.ExternalID, task.Title, task.Artist,
		task.State, task.Quality, task.CreatedAt,
	); err != nil {
		return "", fmt.Errorf("promotion task: insert: %w", err)
	}
	return task.ID, nil
}

func (r *promotionTaskRepository) MarkRunning(ctx context.Context, id string, at time.Time) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE promotion_tasks SET state = ?, started_at = ?, error = '' WHERE id = ?`,
		domain.PromotionStateRunning, at, id,
	); err != nil {
		return fmt.Errorf("promotion task: mark running %s: %w", id, err)
	}
	return nil
}

func (r *promotionTaskRepository) MarkFinished(ctx context.Context, id, state, message string, at time.Time) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE promotion_tasks SET state = ?, error = ?, finished_at = ? WHERE id = ?`,
		state, message, at, id,
	); err != nil {
		return fmt.Errorf("promotion task: mark %s %s: %w", state, id, err)
	}
	return nil
}
