package domain

// DuplicateCacheReport describes what a stream-cache cleanup would remove (scan)
// or did remove (clean). Counts are cache entries; orphan files are counted
// separately because they have no row.
type DuplicateCacheReport struct {
	// DuplicateGroups is the number of normalized title/artist groups that held
	// more than one entry.
	DuplicateGroups int `json:"duplicate_groups"`
	// Removable is the number of cache entries to delete (or deleted).
	Removable int `json:"removable"`
	// RemovableBytes is the disk space those entries and orphan files occupy.
	RemovableBytes int64 `json:"removable_bytes"`
	// OrphanFiles is the number of .audio files with no stream_cache row.
	OrphanFiles int `json:"orphan_files"`
	// PromotedLeftovers is the number of entries whose audio was already moved
	// into the library (promoted) or flagged for deletion.
	PromotedLeftovers int `json:"promoted_leftovers"`
	// Skipped lists why an otherwise removable entry was left alone.
	Skipped []string `json:"skipped"`
}

// StreamCacheOrphan is one .audio file in the cache directory that no
// stream_cache row references. Bytes is its size on disk at scan time.
type StreamCacheOrphan struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}
