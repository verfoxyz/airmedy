package domain

import (
	"context"
	"time"
)

// Last.fm "dislike" kinds. The value is persisted, so renaming one is a
// migration, not a refactor.
const (
	// LastFmDislikeTrack excludes one song from recommendations and radio.
	LastFmDislikeTrack = "track"
	// LastFmDislikeArtist excludes every song of one artist.
	LastFmDislikeArtist = "artist"
)

// LastFmDislike is one locally stored exclusion.
//
// MatchKey is the identity every recommendation list and the match cache share:
// the normalised artist+title pair for a track, the ArtistSetKey for an artist.
// Storing the rendered title and artist keeps the settings list readable after
// the source row is gone.
type LastFmDislike struct {
	ID        string    `json:"id" db:"id"`
	Kind      string    `json:"kind" db:"kind"`
	MatchKey  string    `json:"match_key" db:"match_key"`
	Title     string    `json:"title" db:"title"`
	Artist    string    `json:"artist" db:"artist"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}

// LastFmDislikeRepository persists the local exclusion list.
//
// A dislike is local-only: it never writes to Last.fm and never touches the
// library. Repeating the same kind+identity is idempotent.
type LastFmDislikeRepository interface {
	// ListDislikes returns every stored exclusion, newest first.
	ListDislikes(ctx context.Context) ([]LastFmDislike, error)
	// AddDislike inserts the entry unless the kind+identity pair already
	// exists, and returns the stored row either way.
	AddDislike(ctx context.Context, entry LastFmDislike) (LastFmDislike, error)
	// DeleteDislike removes one row by id. Unknown ids are ignored.
	DeleteDislike(ctx context.Context, id string) error
}
