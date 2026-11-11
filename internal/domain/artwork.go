package domain

import "context"

type ArtworkCache interface {
	Save(ctx context.Context, data []byte, mimeType string) (string, error) // Returns the path/key to the cached artwork
	GetPath(key string) string
	GetVariantPath(key string, variant string) string
	Exists(key string) bool
	CleanupOrphaned(ctx context.Context, activeKeys map[string]bool) error
}

// TrackArtworkStore records a materialised artwork key on one track row.
//
// It is deliberately narrower than TrackRepository: a streamed row gains its
// cover after import, and writing that one column must not rewrite the row the
// player is holding.
type TrackArtworkStore interface {
	SetTrackArtworkKey(ctx context.Context, trackID, artworkKey string) error
}
