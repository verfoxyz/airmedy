package domain

import "context"

// StreamCacheEntry is the metadata row for one cached remote stream. The audio
// body lives beside the database as <CacheKey>.audio; everything needed to
// resume, evict or match it across providers lives here.
//
// CreatedAt and LastUsed are Unix seconds. DurationMs is 0 when the remote row
// did not report a duration; matching then ignores the duration tolerance.
type StreamCacheEntry struct {
	CacheKey         string  `json:"cache_key" db:"cache_key"`
	Provider         string  `json:"provider" db:"provider"`
	ExternalID       string  `json:"external_id" db:"external_id"`
	Title            string  `json:"title" db:"title"`
	TitleKey         string  `json:"title_key" db:"title_key"`
	Artist           string  `json:"artist" db:"artist"`
	ArtistKey        string  `json:"artist_key" db:"artist_key"`
	Artists          string  `json:"artists" db:"artists"`
	Album            string  `json:"album" db:"album"`
	DurationMs       int64   `json:"duration_ms" db:"duration_ms"`
	Quality          string  `json:"quality" db:"quality"`
	AudioPath        string  `json:"audio_path" db:"audio_path"`
	TotalBytes       int64   `json:"total_bytes" db:"total_bytes"`
	ReceivedBytes    int64   `json:"received_bytes" db:"received_bytes"`
	Complete         bool    `json:"complete" db:"complete"`
	PlaybackProgress float64 `json:"playback_progress" db:"playback_progress"`
	PendingPromote   bool    `json:"pending_promote" db:"pending_promote"`
	// Promoted means the audio was moved into the library. Only the cache row
	// and file are left to clean up; the entry must not be completed again.
	Promoted bool `json:"promoted" db:"promoted"`
	// DiscardPending means the cache is no longer needed and must be deleted
	// once no session owns it. It is independent of Promoted: a finished
	// download sets it without moving anything.
	DiscardPending bool  `json:"discard_pending" db:"discard_pending"`
	CreatedAt      int64 `json:"created_at" db:"created_at"`
	LastUsed       int64 `json:"last_used" db:"last_used"`
}

// StreamCacheRepository persists stream-cache metadata.
//
// Metadata lives in SQLite rather than a sidecar file per entry so eviction and
// cross-source matching survive restarts without rewriting files during
// playback. Audio bytes are not stored here.
type StreamCacheRepository interface {
	// Upsert writes the whole row, inserting or replacing by CacheKey.
	Upsert(ctx context.Context, entry *StreamCacheEntry) error
	// GetByIdentity returns the row for provider + external id, or nil.
	GetByIdentity(ctx context.Context, provider, externalID string) (*StreamCacheEntry, error)
	// MatchByTitleArtist returns the most recently used row whose normalized
	// title and artist both equal the arguments. When either side's duration is
	// 0 the duration is ignored; otherwise it must be within toleranceMs.
	MatchByTitleArtist(ctx context.Context, titleKey, artistKey string, durationMs, toleranceMs int64) (*StreamCacheEntry, error)
	// ListForEviction returns every row, least recently used first.
	ListForEviction(ctx context.Context) ([]*StreamCacheEntry, error)
	UpdateProgress(ctx context.Context, cacheKey string, seconds float64) error
	UpdateComplete(ctx context.Context, cacheKey string, totalBytes, receivedBytes int64, complete bool) error
	SetPendingPromote(ctx context.Context, cacheKey string, pending bool) error
	// SetPromoted records that the entry's audio was moved into the library.
	SetPromoted(ctx context.Context, cacheKey string, promoted bool) error
	// SetDiscardPending flags the cache for deletion once it goes idle.
	SetDiscardPending(ctx context.Context, cacheKey string, pending bool) error
	// ListPendingPromote returns every entry the promotion worker must handle.
	ListPendingPromote(ctx context.Context) ([]*StreamCacheEntry, error)
	// ListPendingDiscard returns every entry whose cache must be deleted.
	ListPendingDiscard(ctx context.Context) ([]*StreamCacheEntry, error)
	Delete(ctx context.Context, cacheKey string) error
}
