package sqlite

import (
	"context"
	"fmt"
	"time"

	"airmedy/internal/domain"

	"github.com/jmoiron/sqlx"
)

type lastfmMatchCacheRepository struct {
	db *DB
}

// NewLastFmMatchCacheRepository returns the shared Last.fm match cache.
func NewLastFmMatchCacheRepository(db *DB) domain.LastFmMatchCacheRepository {
	return &lastfmMatchCacheRepository{db: db}
}

const lastfmMatchCacheColumns = `match_key, provider, external_id, local_track_id,
	no_source, suspicious, cover_url, duration, updated_at, expires_at`

// GetMatchCache returns the live entries for the given keys.
func (r *lastfmMatchCacheRepository) GetMatchCache(
	ctx context.Context, keys []string, now time.Time,
) ([]domain.LastFmMatchCacheEntry, error) {
	keys = uniqueMatchKeys(keys)
	if len(keys) == 0 {
		return []domain.LastFmMatchCacheEntry{}, nil
	}
	query, args, err := sqlx.In(
		`SELECT `+lastfmMatchCacheColumns+` FROM lastfm_match_cache
		 WHERE match_key IN (?) AND expires_at > ?`, keys, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("lastfm match cache: get: %w", err)
	}
	entries := make([]domain.LastFmMatchCacheEntry, 0, len(keys))
	if err := r.db.SelectContext(ctx, &entries, r.db.Rebind(query), args...); err != nil {
		return nil, fmt.Errorf("lastfm match cache: get: %w", err)
	}
	return entries, nil
}

// SaveMatchCache upserts the entries and keeps the table bounded.
func (r *lastfmMatchCacheRepository) SaveMatchCache(
	ctx context.Context,
	entries []domain.LastFmMatchCacheEntry,
	now time.Time,
	limit int,
) error {
	if len(entries) == 0 {
		return nil
	}
	return r.db.RunTx(ctx, func(ctx context.Context) error {
		exec := r.db.Ext(ctx)
		for _, entry := range entries {
			if entry.MatchKey == "" {
				continue
			}
			if _, err := exec.ExecContext(ctx,
				`INSERT INTO lastfm_match_cache (`+lastfmMatchCacheColumns+`)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				 ON CONFLICT(match_key) DO UPDATE SET
				   provider = excluded.provider,
				   external_id = excluded.external_id,
				   local_track_id = excluded.local_track_id,
				   no_source = excluded.no_source,
				   suspicious = excluded.suspicious,
				   cover_url = excluded.cover_url,
				   duration = excluded.duration,
				   updated_at = excluded.updated_at,
				   expires_at = excluded.expires_at`,
				entry.MatchKey, entry.Provider, entry.ExternalID, entry.LocalTrackID,
				entry.NoSource, entry.Suspicious, entry.CoverURL, entry.Duration,
				entry.UpdatedAt.UTC(), entry.ExpiresAt.UTC(),
			); err != nil {
				return fmt.Errorf("lastfm match cache: save %s: %w", entry.MatchKey, err)
			}
		}
		return r.prune(ctx, now, limit)
	})
}

// DeleteMatchCache removes the given keys.
func (r *lastfmMatchCacheRepository) DeleteMatchCache(
	ctx context.Context, keys []string,
) error {
	keys = uniqueMatchKeys(keys)
	if len(keys) == 0 {
		return nil
	}
	query, args, err := sqlx.In(`DELETE FROM lastfm_match_cache WHERE match_key IN (?)`, keys)
	if err != nil {
		return fmt.Errorf("lastfm match cache: delete: %w", err)
	}
	if _, err := r.db.Ext(ctx).ExecContext(ctx, r.db.Rebind(query), args...); err != nil {
		return fmt.Errorf("lastfm match cache: delete: %w", err)
	}
	return nil
}

// PruneMatchCache drops expired rows and trims the table to the newest limit.
func (r *lastfmMatchCacheRepository) PruneMatchCache(
	ctx context.Context, now time.Time, limit int,
) error {
	return r.db.RunTx(ctx, func(ctx context.Context) error {
		return r.prune(ctx, now, limit)
	})
}

func (r *lastfmMatchCacheRepository) prune(ctx context.Context, now time.Time, limit int) error {
	exec := r.db.Ext(ctx)
	if _, err := exec.ExecContext(ctx,
		`DELETE FROM lastfm_match_cache WHERE expires_at <= ?`, now.UTC()); err != nil {
		return fmt.Errorf("lastfm match cache: prune expired: %w", err)
	}
	if limit <= 0 {
		return nil
	}
	if _, err := exec.ExecContext(ctx,
		`DELETE FROM lastfm_match_cache WHERE match_key NOT IN (
		   SELECT match_key FROM lastfm_match_cache
		   ORDER BY updated_at DESC, match_key ASC LIMIT ?
		 )`, limit); err != nil {
		return fmt.Errorf("lastfm match cache: trim: %w", err)
	}
	return nil
}

func uniqueMatchKeys(keys []string) []string {
	result := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, key)
	}
	return result
}
