package domain

import (
	"context"
	"time"
)

// RecommendationSeedTrack is one local track that can seed Last.fm
// recommendations. Artists carries the credited names because Last.fm's
// similarity lookups take a single artist string.
type RecommendationSeedTrack struct {
	TrackID         string
	Title           string
	Artists         []string
	AlbumName       string
	ListenedSeconds int
	PlayCount       int
}

// RecommendationSeedRepository reads the local listening signals that rank
// recommendation seeds. It is deliberately separate from ListeningRepository:
// the recommender only needs a small ordered projection, not the full analytics
// payload.
type RecommendationSeedRepository interface {
	// MostListenedTracks returns tracks ordered by listening time since the
	// given day, so a user who does not scrobble still has taste data.
	MostListenedTracks(ctx context.Context, since time.Time, limit int) ([]RecommendationSeedTrack, error)
	// MostPlayedTracks returns the library's most played tracks.
	MostPlayedTracks(ctx context.Context, limit int) ([]RecommendationSeedTrack, error)
	// PlaylistTracks returns tracks that appear in a playlist, most played
	// first; a deliberate collection is a weaker but durable taste signal.
	PlaylistTracks(ctx context.Context, limit int) ([]RecommendationSeedTrack, error)
	// RecentlyListenedTracks returns tracks played since the given day, used to
	// keep radio batches from repeating what the user just heard.
	RecentlyListenedTracks(ctx context.Context, since time.Time, limit int) ([]RecommendationSeedTrack, error)
	// TopGenres returns the library's most frequent genre names, the local half
	// of the cold-start fallback.
	TopGenres(ctx context.Context, limit int) ([]string, error)
}
