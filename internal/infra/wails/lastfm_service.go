package wails

import (
	"airmedy/internal/app/lastfm"
	"airmedy/internal/domain"
	"context"
	"strconv"
	"time"
)

type LastFmService struct {
	svc         *lastfm.LastFmService
	recommender *lastfm.Recommender
	radio       *lastfm.Radio
}

func NewLastFmService(
	svc *lastfm.LastFmService, recommender *lastfm.Recommender, radio *lastfm.Radio,
) *LastFmService {
	return &LastFmService{svc: svc, recommender: recommender, radio: radio}
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
	// Source is "similar" for a direct track.getSimilar hit or "fallback" for a
	// row produced by the cold-account fallback chain.
	Source string `json:"source"`
	// MatchKey is the stable artist+title identity shared by the initial row and
	// every lastfm:recommendations-matched update.
	MatchKey string `json:"match_key"`
	// Matching is true until the background matcher finishes this row.
	Matching bool `json:"matching"`
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
	// Matching reports whether any row still awaits source matching.
	Matching bool `json:"matching"`
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
		Matching:       page.Matching,
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
			Source:        track.Source,
			MatchKey:      track.MatchKey,
			Matching:      track.Matching,
		})
	}
	return result
}

// TopTrackItemDTO is one row of the MVP top-tracks page.
type TopTrackItemDTO struct {
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Album    string `json:"album"`
	Duration int    `json:"duration"`
	// Artists is the credit list the playback action uses.
	Artists       []string `json:"artists"`
	CoverURL      string   `json:"cover_url"`
	CoverURLLarge string   `json:"cover_url_large"`

	Matching      bool     `json:"matching"`
	Matched       bool     `json:"matched"`
	NoSource      bool     `json:"no_source"`
	Provider      string   `json:"provider"`
	ExternalID    string   `json:"external_id"`
	State         string   `json:"state"`
	LocalTrackID  string   `json:"local_track_id"`
	BlockedReason string   `json:"blocked_reason"`
	Sources       []string `json:"sources"`
	Downloadable  bool     `json:"downloadable"`
	// Suspicious reports a local match whose duration disagrees; the row stays
	// playable and the UI asks before saving or downloading.
	Suspicious bool `json:"suspicious"`
	// Kind is the list this row belongs to: top_tracks, similar, discover or
	// weekly_echo:<from>.
	Kind string `json:"kind"`
	// MatchKey is artist + "\x00" + title; the frontend replaces rows by it.
	MatchKey string `json:"match_key"`
	// PlayCount is Last.fm's playcount inside a weekly chart.
	PlayCount int `json:"play_count"`
	// WeekFrom / WeekTo are the chart window of a weekly row, in Unix seconds.
	WeekFrom int64 `json:"week_from"`
	WeekTo   int64 `json:"week_to"`
}

// RecommendationListSummaryDTO is one card on the Last.fm page.
type RecommendationListSummaryDTO struct {
	Kind          string   `json:"kind"`
	Title         string   `json:"title"`
	TrackCount    int      `json:"track_count"`
	MatchingCount int      `json:"matching_count"`
	TotalDuration int      `json:"total_duration"`
	CoverURLs     []string `json:"cover_urls"`
	// WeekFrom / WeekTo are the chart window of a weekly_echo card.
	WeekFrom  int64  `json:"week_from"`
	WeekTo    int64  `json:"week_to"`
	UpdatedAt string `json:"updated_at"`
}

// RecommendationListCollectionDTO is the cards page payload.
type RecommendationListCollectionDTO struct {
	// Status is "ok" or "not_connected"; the frontend words it.
	Status string                         `json:"status"`
	Lists  []RecommendationListSummaryDTO `json:"lists"`
}

// RecommendationListDTO is one list detail page.
type RecommendationListDTO struct {
	Kind      string            `json:"kind"`
	Title     string            `json:"title"`
	Items     []TopTrackItemDTO `json:"items"`
	UpdatedAt string            `json:"updated_at"`
	// Weekly echo navigation: the selected window, its neighbours (0 when the
	// week is the oldest/newest one) and the discover "load more" flag.
	WeekFrom int64 `json:"week_from"`
	WeekTo   int64 `json:"week_to"`
	PrevFrom int64 `json:"prev_from"`
	NextFrom int64 `json:"next_from"`
	HasMore  bool  `json:"has_more"`
}

// ListRecommendationLists returns the persisted cards. autoRefresh lets the
// page's own toggle decide whether the discover TTL and a new week are checked;
// missing lists are always built once.
func (s *LastFmService) ListRecommendationLists(autoRefresh bool) (RecommendationListCollectionDTO, error) {
	collection, err := s.recommender.ListRecommendationListsWithRefresh(
		context.Background(), autoRefresh)
	if err != nil {
		return RecommendationListCollectionDTO{}, err
	}
	result := RecommendationListCollectionDTO{
		Status: collection.Status,
		Lists:  make([]RecommendationListSummaryDTO, 0, len(collection.Lists)),
	}
	for _, summary := range collection.Lists {
		result.Lists = append(result.Lists, RecommendationListSummaryDTO{
			Kind:          summary.Kind,
			Title:         summary.Title,
			TrackCount:    summary.TrackCount,
			MatchingCount: summary.MatchingCount,
			TotalDuration: summary.TotalDuration,
			CoverURLs:     append([]string{}, summary.CoverURLs...),
			WeekFrom:      summary.WeekFrom,
			WeekTo:        summary.WeekTo,
			UpdatedAt:     summary.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	return result, nil
}

// GetRecommendationList returns one list's persisted rows.
func (s *LastFmService) GetRecommendationList(kind string) (RecommendationListDTO, error) {
	list, err := s.recommender.GetRecommendationList(context.Background(), kind)
	if err != nil {
		return RecommendationListDTO{}, err
	}
	return toRecommendationListDTO(list), nil
}

// GetWeeklyEchoWeek returns one stored week, building it on first visit.
func (s *LastFmService) GetWeeklyEchoWeek(from int64) (RecommendationListDTO, error) {
	list, err := s.recommender.GetRecommendationList(
		context.Background(), weeklyEchoKindForWails(from))
	if err != nil {
		return RecommendationListDTO{}, err
	}
	return toRecommendationListDTO(list), nil
}

// RefreshWeeklyEchoWeek re-fetches the selected week.
func (s *LastFmService) RefreshWeeklyEchoWeek(from int64) error {
	return s.recommender.RefreshWeeklyEcho(context.Background(), from)
}

// LoadMoreRecommendationItems appends the next discover page.
func (s *LastFmService) LoadMoreRecommendationItems(kind string) (RecommendationListDTO, error) {
	list, err := s.recommender.LoadMoreRecommendationItems(context.Background(), kind)
	if err != nil {
		return RecommendationListDTO{}, err
	}
	return toRecommendationListDTO(list), nil
}

func weeklyEchoKindForWails(from int64) string {
	if from <= 0 {
		return "weekly_echo"
	}
	return "weekly_echo:" + strconv.FormatInt(from, 10)
}

func toRecommendationListDTO(list lastfm.RecommendationList) RecommendationListDTO {
	return RecommendationListDTO{
		Kind:      list.Kind,
		Title:     list.Title,
		Items:     toTopTrackItemDTOs(list.Items),
		UpdatedAt: list.UpdatedAt.UTC().Format(time.RFC3339),
		WeekFrom:  list.WeekFrom,
		WeekTo:    list.WeekTo,
		PrevFrom:  list.PrevFrom,
		NextFrom:  list.NextFrom,
		HasMore:   list.HasMore,
	}
}

// RefreshRecommendationList rebuilds exactly one list.
func (s *LastFmService) RefreshRecommendationList(kind string) error {
	return s.recommender.RefreshRecommendationList(context.Background(), kind)
}

// RefreshRecommendationItem re-runs source matching for one row.
func (s *LastFmService) RefreshRecommendationItem(kind, matchKey string) error {
	return s.recommender.RefreshRecommendationItem(context.Background(), kind, matchKey)
}

// RefreshAllRecommendationLists rebuilds both lists.
func (s *LastFmService) RefreshAllRecommendationLists() error {
	return s.recommender.RefreshAllRecommendationLists(context.Background())
}

// StartRadio builds the initial Last.fm radio queue from a track or identity.
func (s *LastFmService) StartRadio(request lastfm.RadioSeedRequest) (lastfm.RadioStartResult, error) {
	return s.radio.StartRadio(context.Background(), request)
}

// StopRadio ends the radio session; automatic refills stop immediately.
func (s *LastFmService) StopRadio() {
	s.radio.StopRadio()
}

// GetRadioActive reports whether the radio session is on.
func (s *LastFmService) GetRadioActive() bool {
	return s.radio.GetRadioActive()
}

// DislikeDTO is one locally stored recommendation exclusion.
//
// The list lives only in the local database: it never touches Last.fm and
// never hides a song from the library.
type DislikeDTO struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	MatchKey  string `json:"match_key"`
	Title     string `json:"title"`
	Artist    string `json:"artist"`
	CreatedAt string `json:"created_at"`
}

// Dislike excludes a song or an artist from discovery and radio. matchKey may
// be empty for a track, in which case the identity is rebuilt from the credits;
// an artist always uses its name.
func (s *LastFmService) Dislike(kind, matchKey, title, artist string) (DislikeDTO, error) {
	entry, err := s.recommender.Dislike(context.Background(), kind, matchKey, title, artist)
	if err != nil {
		return DislikeDTO{}, err
	}
	return toDislikeDTO(entry), nil
}

// Undislike removes one exclusion; the next refresh or refill may return it.
func (s *LastFmService) Undislike(id string) error {
	return s.recommender.Undislike(context.Background(), id)
}

// ListDislikes returns the settings page's exclusion list.
func (s *LastFmService) ListDislikes() ([]DislikeDTO, error) {
	rows, err := s.recommender.ListDislikes(context.Background())
	if err != nil {
		return nil, err
	}
	return toDislikeDTOs(rows), nil
}

func toDislikeDTO(entry domain.LastFmDislike) DislikeDTO {
	return DislikeDTO{
		ID:        entry.ID,
		Kind:      entry.Kind,
		MatchKey:  entry.MatchKey,
		Title:     entry.Title,
		Artist:    entry.Artist,
		CreatedAt: entry.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func toDislikeDTOs(rows []domain.LastFmDislike) []DislikeDTO {
	result := make([]DislikeDTO, 0, len(rows))
	for _, row := range rows {
		result = append(result, toDislikeDTO(row))
	}
	return result
}

func toTopTrackItemDTOs(items []lastfm.TopTrackItem) []TopTrackItemDTO {
	result := make([]TopTrackItemDTO, 0, len(items))
	for _, item := range items {
		result = append(result, TopTrackItemDTO{
			Title:         item.Title,
			Artist:        item.Artist,
			Album:         item.Album,
			Duration:      item.Duration,
			Artists:       append([]string{}, item.Artists...),
			CoverURL:      item.CoverURL,
			CoverURLLarge: item.CoverURLLarge,
			Matching:      item.Matching,
			Matched:       item.Matched,
			NoSource:      item.NoSource,
			Provider:      item.Provider,
			ExternalID:    item.ExternalID,
			State:         item.State,
			LocalTrackID:  item.LocalTrackID,
			BlockedReason: item.BlockedReason,
			Sources:       append([]string{}, item.Sources...),
			Downloadable:  item.Downloadable,
			Suspicious:    item.Suspicious,
			Kind:          item.Kind,
			MatchKey:      item.MatchKey,
			PlayCount:     item.PlayCount,
			WeekFrom:      item.WeekFrom,
			WeekTo:        item.WeekTo,
		})
	}
	return result
}
