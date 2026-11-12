package domain

import (
	"context"
	"time"
)

// Last.fm recommendation list kinds. The value is persisted, so renaming one is
// a migration, not a refactor.
const (
	// LastFmRecommendationTopTracks is the user's all-time most played tracks.
	LastFmRecommendationTopTracks = "top_tracks"
	// LastFmRecommendationSimilar is the "for you" list derived from those tracks.
	LastFmRecommendationSimilar = "similar"
)

// RecommendationItem is one persisted row of a recommendation list.
//
// It mirrors the row the page renders: Last.fm identity plus whatever the
// catalogue match already knows. Matching writes the same row back, so the
// status survives a restart until the user refreshes the list.
type RecommendationItem struct {
	Kind     string `json:"kind" db:"kind"`
	Position int    `json:"position" db:"position"`
	MatchKey string `json:"match_key" db:"match_key"`
	Title    string `json:"title" db:"title"`
	Artist   string `json:"artist" db:"artist"`
	// Artists is the credit list playback needs. Stored as JSON.
	Artists       []string `json:"artists" db:"-"`
	Album         string   `json:"album" db:"album"`
	Duration      int      `json:"duration" db:"duration"`
	CoverURL      string   `json:"cover_url" db:"cover_url"`
	CoverURLLarge string   `json:"cover_url_large" db:"cover_url_large"`
	Downloadable  bool     `json:"downloadable" db:"downloadable"`
	BlockedReason string   `json:"blocked_reason" db:"blocked_reason"`
	// Sources is the list of providers that returned this row. It is stored as
	// JSON, so the repository owns the column mapping (db:"-").
	Sources      []string `json:"sources" db:"-"`
	Matching     bool     `json:"matching" db:"matching"`
	Matched      bool     `json:"matched" db:"matched"`
	NoSource     bool     `json:"no_source" db:"no_source"`
	Provider     string   `json:"provider" db:"provider"`
	ExternalID   string   `json:"external_id" db:"external_id"`
	State        string   `json:"state" db:"state"`
	LocalTrackID string   `json:"local_track_id" db:"local_track_id"`
	// Suspicious keeps a flagged local match across restarts.
	Suspicious bool `json:"suspicious" db:"suspicious"`
}

// RecommendationList is one persisted list: its rows in Last.fm order plus the
// time the list was last rebuilt.
type RecommendationList struct {
	Kind      string               `json:"kind"`
	Title     string               `json:"title"`
	Items     []RecommendationItem `json:"items"`
	UpdatedAt time.Time            `json:"updated_at"`
}

// RecommendationListSummary is the card data of one list. TrackCount and
// MatchingCount are derived from the rows, so the summary never disagrees with
// the detail page.
type RecommendationListSummary struct {
	Kind          string `json:"kind"`
	Title         string `json:"title"`
	TrackCount    int    `json:"track_count"`
	MatchingCount int    `json:"matching_count"`
	// TotalDuration is the sum of the rows' duration in seconds.
	TotalDuration int `json:"total_duration"`
	// CoverURLs holds up to four covers, in list order, for the card mosaic.
	CoverURLs []string  `json:"cover_urls"`
	UpdatedAt time.Time `json:"updated_at"`
}

// LastFmRecommendationRepository persists the two recommendation lists.
//
// Saving a list replaces every row of that kind in one transaction; matching
// updates rows in place, so a long-running match never rewrites the whole list.
type LastFmRecommendationRepository interface {
	// ListRecommendationLists returns the stored list summaries, newest first.
	ListRecommendationLists(ctx context.Context) ([]RecommendationListSummary, error)
	// GetRecommendationList returns one list with its rows in position order.
	// A missing kind returns (nil, nil).
	GetRecommendationList(ctx context.Context, kind string) (*RecommendationList, error)
	// SaveRecommendationList replaces the rows of one kind and records its
	// title and update time.
	SaveRecommendationList(ctx context.Context, list *RecommendationList) error
	// UpdateRecommendationItems writes match progress back to existing rows.
	UpdateRecommendationItems(ctx context.Context, items []RecommendationItem) error
	// PendingRecommendationItems returns every row still waiting for a match.
	PendingRecommendationItems(ctx context.Context) ([]RecommendationItem, error)
}
