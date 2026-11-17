package domain

import (
	"context"
	"time"
)

// LastFmMatchCacheEntry is one persisted catalogue match for a Last.fm track.
//
// The identity is the normalised artist+title pair the recommendation rows are
// keyed by, so every list and every restart shares one answer. An entry is
// either a playable match (Provider+ExternalID or LocalTrackID), a definitive
// "no playable source" row, or a suspicious local hit that may be retried
// earlier.
type LastFmMatchCacheEntry struct {
	MatchKey     string    `json:"match_key" db:"match_key"`
	Provider     string    `json:"provider" db:"provider"`
	ExternalID   string    `json:"external_id" db:"external_id"`
	LocalTrackID string    `json:"local_track_id" db:"local_track_id"`
	NoSource     bool      `json:"no_source" db:"no_source"`
	Suspicious   bool      `json:"suspicious" db:"suspicious"`
	CoverURL     string    `json:"cover_url" db:"cover_url"`
	Duration     int       `json:"duration" db:"duration"`
	UpdatedAt    time.Time `json:"updated_at" db:"updated_at"`
	ExpiresAt    time.Time `json:"expires_at" db:"expires_at"`
}

// LastFmMatchCacheRepository persists the shared Last.fm match cache.
//
// Reads never return expired rows; writes are upserts, so re-matching one row
// replaces its entry without touching the others. Pruning keeps the table
// bounded; it is not on the matching path.
type LastFmMatchCacheRepository interface {
	// GetMatchCache returns the live entries for the given keys. Missing and
	// expired keys are simply absent from the result.
	GetMatchCache(ctx context.Context, keys []string, now time.Time) ([]LastFmMatchCacheEntry, error)
	// SaveMatchCache upserts the entries, then prunes expired rows and trims
	// the table to the newest limit entries.
	SaveMatchCache(ctx context.Context, entries []LastFmMatchCacheEntry, now time.Time, limit int) error
	// DeleteMatchCache removes the given keys. Unknown keys are ignored.
	DeleteMatchCache(ctx context.Context, keys []string) error
	// PruneMatchCache deletes expired rows and trims the table to the newest
	// limit entries.
	PruneMatchCache(ctx context.Context, now time.Time, limit int) error
}
