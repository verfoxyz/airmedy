package wails

import (
	"airmedy/internal/app/lastfm"
	"context"
)

type LastFmService struct {
	svc         *lastfm.LastFmService
	recommender *lastfm.Recommender
}

func NewLastFmService(svc *lastfm.LastFmService, recommender *lastfm.Recommender) *LastFmService {
	return &LastFmService{svc: svc, recommender: recommender}
}

func (s *LastFmService) GetService() *lastfm.LastFmService {
	return s.svc
}

func (s *LastFmService) Connect(ctx context.Context) error {
	return s.svc.Connect(ctx)
}

func (s *LastFmService) Disconnect(ctx context.Context) error {
	return s.svc.Disconnect(ctx)
}

type LastFmStatus struct {
	Connected bool   `json:"connected"`
	Username  string `json:"username"`
	AvatarURL string `json:"avatar_url"`
}

func (s *LastFmService) GetStatus(ctx context.Context) LastFmStatus {
	connected, username, avatarURL := s.svc.GetStatus()
	return LastFmStatus{
		Connected: connected,
		Username:  username,
		AvatarURL: avatarURL,
	}
}

// RecommendationTrackDTO is one playable recommendation.
//
// The field names deliberately match CatalogRowDTO: the page feeds these rows to
// the same play/save actions as the cloud search, and matching the shape keeps
// the frontend from translating between two nearly identical objects.
type RecommendationTrackDTO struct {
	Provider   string   `json:"provider"`
	ExternalID string   `json:"external_id"`
	Title      string   `json:"title"`
	Artists    []string `json:"artists"`
	AlbumName  string   `json:"album_name"`
	Duration   int      `json:"duration"`
	CoverURL   string   `json:"cover_url"`
	// CoverURLLarge is the rendition the saved file embeds.
	CoverURLLarge string `json:"cover_url_large"`
	Downloadable  bool   `json:"downloadable"`
	BlockedReason string `json:"blocked_reason"`
	// State is "" (not registered), "in_library", "downloading", "failed" or
	// "saved"; it drives the same chips the search page renders.
	State string `json:"state"`
	// LocalTrackID is an on-disk row that already represents this song, so the
	// row can play immediately without a download.
	LocalTrackID string `json:"local_track_id"`
	// Sources lists every provider that returned this row.
	Sources []string `json:"sources"`
	// Score is the recommendation score within its list, highest first.
	Score float64 `json:"score"`
}

// SimilarArtistDTO is one artist in the similar-artists grid.
type SimilarArtistDTO struct {
	Name      string `json:"name"`
	ImageURL  string `json:"image_url"`
	Listeners int    `json:"listeners"`
	URL       string `json:"url"`
}

// RecommendationPageDTO is the whole Last.fm page.
type RecommendationPageDTO struct {
	// Status is "ok", "not_connected" or "partial"; the frontend words it.
	Status    string `json:"status"`
	Username  string `json:"username"`
	AvatarURL string `json:"avatar_url"`
	// Tracks is the combined list. The three source lists are independent and
	// may contain the same track with a different score.
	Tracks         []RecommendationTrackDTO `json:"tracks"`
	RecentTracks   []RecommendationTrackDTO `json:"recent_tracks"`
	LovedTracks    []RecommendationTrackDTO `json:"loved_tracks"`
	TopTracks      []RecommendationTrackDTO `json:"top_tracks"`
	SimilarArtists []SimilarArtistDTO       `json:"similar_artists"`
}

// GetRecommendations builds the Last.fm page. force skips the 30-minute cache.
//
// An unconnected account is a normal state the page explains, so it travels as
// status "not_connected" rather than as an error. A returned error is a real
// failure the page has no wording for.
func (s *LastFmService) GetRecommendations(force bool) (RecommendationPageDTO, error) {
	page, err := s.recommender.GetRecommendations(context.Background(), force)
	if err != nil {
		return RecommendationPageDTO{}, err
	}
	return toRecommendationPageDTO(page), nil
}

func toRecommendationPageDTO(page lastfm.RecommendationPage) RecommendationPageDTO {
	result := RecommendationPageDTO{
		Status:         page.Status,
		Username:       page.Username,
		AvatarURL:      page.AvatarURL,
		Tracks:         toRecommendationTrackDTOs(page.Tracks),
		RecentTracks:   toRecommendationTrackDTOs(page.RecentTracks),
		LovedTracks:    toRecommendationTrackDTOs(page.LovedTracks),
		TopTracks:      toRecommendationTrackDTOs(page.TopTracks),
		SimilarArtists: make([]SimilarArtistDTO, 0, len(page.SimilarArtists)),
	}
	for _, artist := range page.SimilarArtists {
		result.SimilarArtists = append(result.SimilarArtists, SimilarArtistDTO{
			Name:      artist.Name,
			ImageURL:  artist.ImageURL,
			Listeners: artist.Listeners,
			URL:       artist.URL,
		})
	}
	return result
}

func toRecommendationTrackDTOs(tracks []lastfm.RecommendedTrack) []RecommendationTrackDTO {
	result := make([]RecommendationTrackDTO, 0, len(tracks))
	for _, track := range tracks {
		result = append(result, RecommendationTrackDTO{
			Provider:      track.Provider,
			ExternalID:    track.ExternalID,
			Title:         track.Title,
			Artists:       append([]string{}, track.Artists...),
			AlbumName:     track.AlbumName,
			Duration:      track.Duration,
			CoverURL:      track.CoverURL,
			CoverURLLarge: track.CoverURLLarge,
			Downloadable:  track.Downloadable,
			BlockedReason: track.DownloadBlockedReason,
			State:         track.State,
			LocalTrackID:  track.LocalTrackID,
			Sources:       append([]string{}, track.Sources...),
			Score:         track.Score,
		})
	}
	return result
}
