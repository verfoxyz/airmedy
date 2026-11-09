package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"airmedy/internal/domain"
)

type streamCacheRepository struct {
	db *DB
}

// NewStreamCacheRepository returns the SQLite-backed stream cache index.
func NewStreamCacheRepository(db *DB) domain.StreamCacheRepository {
	return &streamCacheRepository{db: db}
}

const streamCacheColumns = `cache_key, provider, external_id, title, title_key,
	artist, artist_key, artists, album, duration_ms, quality, audio_path,
	total_bytes, received_bytes, complete, playback_progress, pending_promote,
	created_at, last_used`

// Upsert writes the whole row. created_at is preserved on conflict so a replay
// cannot rewrite the entry's age.
func (r *streamCacheRepository) Upsert(ctx context.Context, entry *domain.StreamCacheEntry) error {
	if entry == nil {
		return fmt.Errorf("stream cache: nil entry")
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO stream_cache (`+streamCacheColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(cache_key) DO UPDATE SET
		   provider = excluded.provider,
		   external_id = excluded.external_id,
		   title = excluded.title,
		   title_key = excluded.title_key,
		   artist = excluded.artist,
		   artist_key = excluded.artist_key,
		   artists = excluded.artists,
		   album = excluded.album,
		   duration_ms = excluded.duration_ms,
		   quality = excluded.quality,
		   audio_path = excluded.audio_path,
		   total_bytes = excluded.total_bytes,
		   received_bytes = excluded.received_bytes,
		   complete = excluded.complete,
		   playback_progress = excluded.playback_progress,
		   pending_promote = excluded.pending_promote,
		   last_used = excluded.last_used`,
		entry.CacheKey, entry.Provider, entry.ExternalID, entry.Title, entry.TitleKey,
		entry.Artist, entry.ArtistKey, entry.Artists, entry.Album, entry.DurationMs,
		entry.Quality, entry.AudioPath, entry.TotalBytes, entry.ReceivedBytes,
		entry.Complete, entry.PlaybackProgress, entry.PendingPromote,
		entry.CreatedAt, entry.LastUsed,
	)
	if err != nil {
		return fmt.Errorf("failed to upsert stream cache entry %s: %w", entry.CacheKey, err)
	}
	return nil
}

func (r *streamCacheRepository) GetByIdentity(ctx context.Context, provider, externalID string) (*domain.StreamCacheEntry, error) {
	var entry domain.StreamCacheEntry
	err := r.db.GetContext(ctx, &entry,
		`SELECT `+streamCacheColumns+` FROM stream_cache
		 WHERE provider = ? AND external_id = ?
		 LIMIT 1`,
		provider, externalID,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read stream cache %q/%q: %w", provider, externalID, err)
	}
	return &entry, nil
}

// MatchByTitleArtist finds the same song cached from another provider. Both
// normalized fields must be equal; duration only participates when both sides
// know it, so a row without a duration can still hit.
func (r *streamCacheRepository) MatchByTitleArtist(ctx context.Context, titleKey, artistKey string, durationMs, toleranceMs int64) (*domain.StreamCacheEntry, error) {
	var entry domain.StreamCacheEntry
	err := r.db.GetContext(ctx, &entry,
		`SELECT `+streamCacheColumns+` FROM stream_cache
		 WHERE title_key = ? AND artist_key = ?
		   AND (? = 0 OR duration_ms = 0 OR ABS(duration_ms - ?) <= ?)
		 ORDER BY last_used DESC
		 LIMIT 1`,
		titleKey, artistKey, durationMs, durationMs, toleranceMs,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to match stream cache %q/%q: %w", titleKey, artistKey, err)
	}
	return &entry, nil
}

func (r *streamCacheRepository) ListForEviction(ctx context.Context) ([]*domain.StreamCacheEntry, error) {
	var entries []*domain.StreamCacheEntry
	if err := r.db.SelectContext(ctx, &entries,
		`SELECT `+streamCacheColumns+` FROM stream_cache ORDER BY last_used ASC`,
	); err != nil {
		return nil, fmt.Errorf("failed to list stream cache entries: %w", err)
	}
	return entries, nil
}

func (r *streamCacheRepository) UpdateProgress(ctx context.Context, cacheKey string, seconds float64) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE stream_cache SET playback_progress = ? WHERE cache_key = ?`,
		seconds, cacheKey,
	); err != nil {
		return fmt.Errorf("failed to update stream cache progress %s: %w", cacheKey, err)
	}
	return nil
}

func (r *streamCacheRepository) UpdateComplete(ctx context.Context, cacheKey string, totalBytes, receivedBytes int64, complete bool) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE stream_cache
		 SET total_bytes = ?, received_bytes = ?, complete = ?
		 WHERE cache_key = ?`,
		totalBytes, receivedBytes, complete, cacheKey,
	); err != nil {
		return fmt.Errorf("failed to update stream cache bytes %s: %w", cacheKey, err)
	}
	return nil
}

func (r *streamCacheRepository) SetPendingPromote(ctx context.Context, cacheKey string, pending bool) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE stream_cache SET pending_promote = ? WHERE cache_key = ?`,
		pending, cacheKey,
	); err != nil {
		return fmt.Errorf("failed to set stream cache promotion flag %s: %w", cacheKey, err)
	}
	return nil
}

func (r *streamCacheRepository) Delete(ctx context.Context, cacheKey string) error {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM stream_cache WHERE cache_key = ?`, cacheKey,
	); err != nil {
		return fmt.Errorf("failed to delete stream cache entry %s: %w", cacheKey, err)
	}
	return nil
}
