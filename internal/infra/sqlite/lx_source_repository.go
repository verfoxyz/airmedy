package sqlite

import (
	"context"
	"fmt"

	"airmedy/internal/domain"
)

type lxSourceRepository struct {
	db *DB
}

// NewLxSourceRepository returns the SQLite-backed lx source list.
func NewLxSourceRepository(db *DB) domain.LxSourceRepository {
	return &lxSourceRepository{db: db}
}

// List returns every source ordered by position, then id.
//
// The id tie-break makes the order total: two rows can share a position if an
// older database was edited by hand, and a total order keeps the plugin's try
// sequence and the UI's drag order identical.
func (r *lxSourceRepository) List(ctx context.Context) ([]domain.LxSource, error) {
	sources := []domain.LxSource{}
	if err := r.db.SelectContext(ctx, &sources,
		`SELECT id, name, file_path, enabled, position, created_at, updated_at
		   FROM lx_sources
		  ORDER BY position, id`,
	); err != nil {
		return nil, fmt.Errorf("failed to list lx sources: %w", err)
	}
	return sources, nil
}

// Create inserts one source.
func (r *lxSourceRepository) Create(ctx context.Context, source domain.LxSource) error {
	if source.ID == "" {
		return fmt.Errorf("failed to create lx source: empty id")
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO lx_sources (id, name, file_path, enabled, position)
		 VALUES (?, ?, ?, ?, ?)`,
		source.ID, source.Name, source.FilePath, source.Enabled, source.Position,
	)
	if err != nil {
		return fmt.Errorf("failed to create lx source %q: %w", source.ID, err)
	}
	return nil
}

// Update writes the editable fields of an existing row.
func (r *lxSourceRepository) Update(ctx context.Context, source domain.LxSource) error {
	result, err := r.db.ExecContext(ctx,
		`UPDATE lx_sources
		    SET name = ?, enabled = ?, updated_at = CURRENT_TIMESTAMP
		  WHERE id = ?`,
		source.Name, source.Enabled, source.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to update lx source %q: %w", source.ID, err)
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil {
		return fmt.Errorf("failed to update lx source %q: %w", source.ID, rowsErr)
	} else if changed == 0 {
		return fmt.Errorf("%w: %q", domain.ErrLxSourceNotFound, source.ID)
	}
	return nil
}

// Delete removes one row. A missing row is not an error: the UI must be able to
// clear an entry whose file an outside actor already removed.
func (r *lxSourceRepository) Delete(ctx context.Context, id string) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM lx_sources WHERE id = ?`, id); err != nil {
		return fmt.Errorf("failed to delete lx source %q: %w", id, err)
	}
	return nil
}

// Reorder assigns positions 0..n-1 in the given order.
//
// The rewrite is one transaction: a reader (the plugin start path builds the
// environment from this list) must never observe a half-rewritten priority
// order, where two sources both look like "first". Unknown ids are skipped so a
// stale UI list cannot fail the whole operation.
func (r *lxSourceRepository) Reorder(ctx context.Context, ids []string) error {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin lx source reorder: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	position := 0
	for _, id := range ids {
		if id == "" {
			continue
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE lx_sources
			    SET position = ?, updated_at = CURRENT_TIMESTAMP
			  WHERE id = ?`,
			position, id,
		)
		if err != nil {
			return fmt.Errorf("failed to move lx source %q: %w", id, err)
		}
		// An unknown id moved nothing, so it must not consume a position:
		// otherwise a stale UI list would leave holes in the priority range
		// while still looking contiguous to the user.
		changed, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return fmt.Errorf("failed to move lx source %q: %w", id, rowsErr)
		}
		if changed > 0 {
			position++
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit lx source reorder: %w", err)
	}
	return nil
}
