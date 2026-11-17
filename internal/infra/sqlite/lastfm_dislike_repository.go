package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"airmedy/internal/domain"
)

type lastfmDislikeRepository struct {
	db *DB
}

// NewLastFmDislikeRepository returns the local recommendation exclusion list.
func NewLastFmDislikeRepository(db *DB) domain.LastFmDislikeRepository {
	return &lastfmDislikeRepository{db: db}
}

const lastfmDislikeColumns = `id, kind, match_key, title, artist, created_at`

// ListDislikes returns every exclusion, newest first.
func (r *lastfmDislikeRepository) ListDislikes(
	ctx context.Context,
) ([]domain.LastFmDislike, error) {
	rows := make([]domain.LastFmDislike, 0)
	if err := r.db.SelectContext(ctx, &rows,
		`SELECT `+lastfmDislikeColumns+` FROM dislikes
		 ORDER BY created_at DESC, id ASC`); err != nil {
		return nil, fmt.Errorf("lastfm dislikes: list: %w", err)
	}
	return rows, nil
}

// AddDislike inserts the entry unless its identity is already excluded. The
// stored row wins on a repeat, so created_at and id stay stable.
func (r *lastfmDislikeRepository) AddDislike(
	ctx context.Context, entry domain.LastFmDislike,
) (domain.LastFmDislike, error) {
	if entry.ID == "" || entry.Kind == "" || entry.MatchKey == "" {
		return domain.LastFmDislike{}, fmt.Errorf("lastfm dislikes: incomplete entry")
	}
	if _, err := r.db.Ext(ctx).ExecContext(ctx,
		`INSERT INTO dislikes (`+lastfmDislikeColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(kind, match_key) DO NOTHING`,
		entry.ID, entry.Kind, entry.MatchKey, entry.Title, entry.Artist,
		entry.CreatedAt.UTC(),
	); err != nil {
		return domain.LastFmDislike{}, fmt.Errorf("lastfm dislikes: add: %w", err)
	}
	var stored domain.LastFmDislike
	if err := r.db.GetContext(ctx, &stored,
		`SELECT `+lastfmDislikeColumns+` FROM dislikes
		 WHERE kind = ? AND match_key = ?`, entry.Kind, entry.MatchKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.LastFmDislike{}, fmt.Errorf("lastfm dislikes: add %s: row vanished", entry.Kind)
		}
		return domain.LastFmDislike{}, fmt.Errorf("lastfm dislikes: add: %w", err)
	}
	return stored, nil
}

// DeleteDislike removes one row by id.
func (r *lastfmDislikeRepository) DeleteDislike(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	if _, err := r.db.Ext(ctx).ExecContext(ctx,
		`DELETE FROM dislikes WHERE id = ?`, id); err != nil {
		return fmt.Errorf("lastfm dislikes: delete: %w", err)
	}
	return nil
}
