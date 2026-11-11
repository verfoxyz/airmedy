package lastfm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"airmedy/internal/domain"
)

const (
	// requestTimeout bounds one Last.fm request. A request that misses it counts
	// as a failure and the page renders what already arrived.
	requestTimeout = 15 * time.Second
	// maxConcurrentRequests is Last.fm's practical ceiling: the public limit is
	// about five requests per second and three in flight stays under it even when
	// one call is retried elsewhere.
	maxConcurrentRequests = 3
	// minRequestGap keeps the request rate at or below four per second.
	minRequestGap = 250 * time.Millisecond
	// maxResponseBytes bounds one API body; Last.fm answers are small and a
	// truncated or hostile body must not be read into memory wholesale.
	maxResponseBytes = 4 << 20
)

// LastFmTrack is one track the read-only endpoints return.
//
// It is the host's own shape rather than the wire object: the recommendation
// service must not know which endpoint produced a track, and every endpoint
// spells the artist image and duration fields differently.
type LastFmTrack struct {
	Artist    string
	Title     string
	Album     string
	ImageURL  string
	Duration  int
	PlayCount int
	MBID      string
	URL       string
}

// LastFmArtist is one similar artist.
type LastFmArtist struct {
	Name      string
	ImageURL  string
	Listeners int
	URL       string
}

// LastFmTag is one tag and how strongly a user or artist is associated with it.
type LastFmTag struct {
	Name  string
	Count int
}

// rateLimiter caps in-flight requests and spaces them by a minimum gap.
//
// Both bounds are needed: the concurrency slot alone still allows a burst of
// three simultaneous calls, which Last.fm counts against the same per-second
// window. now/wait are seams so a test can assert the schedule without sleeping.
type rateLimiter struct {
	slots chan struct{}
	gap   time.Duration

	mu   sync.Mutex
	next time.Time

	now  func() time.Time
	wait func(context.Context, time.Duration) error
}

func newRateLimiter(concurrency int, gap time.Duration) *rateLimiter {
	if concurrency <= 0 {
		concurrency = 1
	}
	return &rateLimiter{
		slots: make(chan struct{}, concurrency),
		gap:   gap,
		now:   time.Now,
		wait:  waitFor,
	}
}

func waitFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// acquire reserves one concurrency slot and returns the release function.
//
// The returned function is idempotent: a caller that defers it and also calls it
// on an error path must not free the slot twice.
func (l *rateLimiter) acquire(ctx context.Context) (func(), error) {
	select {
	case l.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	l.mu.Lock()
	now := l.now()
	delay := time.Duration(0)
	if l.next.After(now) {
		delay = l.next.Sub(now)
	}
	l.next = now.Add(delay).Add(l.gap)
	l.mu.Unlock()

	if delay > 0 {
		if err := l.wait(ctx, delay); err != nil {
			<-l.slots
			return nil, err
		}
	}

	var once sync.Once
	release := func() { once.Do(func() { <-l.slots }) }
	return release, nil
}

// GetRecentTracks returns the user's most recent scrobbles, newest first.
func (s *LastFmService) GetRecentTracks(ctx context.Context, limit int) ([]LastFmTrack, error) {
	user, err := s.requireUser()
	if err != nil {
		return nil, err
	}

	var response struct {
		RecentTracks struct {
			Track flexList[lastFmTrackWire] `json:"track"`
		} `json:"recenttracks"`
	}
	err = s.callRead(ctx, map[string]string{
		"method": "user.getRecentTracks",
		"user":   user,
		"limit":  strconv.Itoa(clampLimit(limit, 50)),
	}, &response)
	if err != nil {
		return nil, err
	}

	tracks := make([]LastFmTrack, 0, len(response.RecentTracks.Track))
	for _, wire := range response.RecentTracks.Track {
		track := wire.toTrack()
		if track.Title == "" || track.Artist == "" {
			continue
		}
		tracks = append(tracks, track)
	}
	return tracks, nil
}

// GetTopTracks returns the user's most played tracks in one period
// ("7day", "1month", ...).
func (s *LastFmService) GetTopTracks(ctx context.Context, period string, limit int) ([]LastFmTrack, error) {
	user, err := s.requireUser()
	if err != nil {
		return nil, err
	}
	period = strings.TrimSpace(period)
	if period == "" {
		period = "1month"
	}

	var response struct {
		TopTracks struct {
			Track flexList[lastFmTrackWire] `json:"track"`
		} `json:"toptracks"`
	}
	err = s.callRead(ctx, map[string]string{
		"method": "user.getTopTracks",
		"user":   user,
		"period": period,
		"limit":  strconv.Itoa(clampLimit(limit, 20)),
	}, &response)
	if err != nil {
		return nil, err
	}

	tracks := make([]LastFmTrack, 0, len(response.TopTracks.Track))
	for _, wire := range response.TopTracks.Track {
		track := wire.toTrack()
		if track.Title == "" || track.Artist == "" {
			continue
		}
		tracks = append(tracks, track)
	}
	return tracks, nil
}

// GetLovedTracks returns the user's loved tracks, most recent first.
func (s *LastFmService) GetLovedTracks(ctx context.Context, limit int) ([]LastFmTrack, error) {
	user, err := s.requireUser()
	if err != nil {
		return nil, err
	}

	var response struct {
		LovedTracks struct {
			Track flexList[lastFmTrackWire] `json:"track"`
		} `json:"lovedtracks"`
	}
	err = s.callRead(ctx, map[string]string{
		"method": "user.getLovedTracks",
		"user":   user,
		"limit":  strconv.Itoa(clampLimit(limit, 50)),
	}, &response)
	if err != nil {
		return nil, err
	}

	tracks := make([]LastFmTrack, 0, len(response.LovedTracks.Track))
	for _, wire := range response.LovedTracks.Track {
		track := wire.toTrack()
		if track.Title == "" || track.Artist == "" {
			continue
		}
		tracks = append(tracks, track)
	}
	return tracks, nil
}

// GetSimilarTracks returns Last.fm's similar tracks for one seed.
//
// autocorrect=1 lets Last.fm resolve a misspelled artist or title instead of
// returning nothing; the host then filters the answer against its own seeds.
func (s *LastFmService) GetSimilarTracks(ctx context.Context, artist, title string, limit int) ([]LastFmTrack, error) {
	if _, err := s.requireUser(); err != nil {
		return nil, err
	}
	artist = strings.TrimSpace(artist)
	title = strings.TrimSpace(title)
	if artist == "" || title == "" {
		return nil, fmt.Errorf("last.fm similar tracks: artist and title are required")
	}

	var response struct {
		SimilarTracks struct {
			Track flexList[lastFmTrackWire] `json:"track"`
		} `json:"similartracks"`
	}
	err := s.callRead(ctx, map[string]string{
		"method":      "track.getSimilar",
		"artist":      artist,
		"track":       title,
		"limit":       strconv.Itoa(clampLimit(limit, 10)),
		"autocorrect": "1",
	}, &response)
	if err != nil {
		return nil, err
	}

	tracks := make([]LastFmTrack, 0, len(response.SimilarTracks.Track))
	for _, wire := range response.SimilarTracks.Track {
		track := wire.toTrack()
		if track.Title == "" || track.Artist == "" {
			continue
		}
		tracks = append(tracks, track)
	}
	return tracks, nil
}

// GetSimilarArtists returns Last.fm's similar artists for one seed artist.
func (s *LastFmService) GetSimilarArtists(ctx context.Context, artist string, limit int) ([]LastFmArtist, error) {
	if _, err := s.requireUser(); err != nil {
		return nil, err
	}
	artist = strings.TrimSpace(artist)
	if artist == "" {
		return nil, fmt.Errorf("last.fm similar artists: artist is required")
	}

	var response struct {
		SimilarArtists struct {
			Artist flexList[lastFmSimilarArtistWire] `json:"artist"`
		} `json:"similarartists"`
	}
	err := s.callRead(ctx, map[string]string{
		"method": "artist.getSimilar",
		"artist": artist,
		"limit":  strconv.Itoa(clampLimit(limit, 10)),
	}, &response)
	if err != nil {
		return nil, err
	}

	artists := make([]LastFmArtist, 0, len(response.SimilarArtists.Artist))
	for _, wire := range response.SimilarArtists.Artist {
		name := strings.TrimSpace(wire.Name)
		if name == "" {
			continue
		}
		artists = append(artists, LastFmArtist{
			Name:      name,
			ImageURL:  usableArtistImage(largestImage(wire.Image)),
			Listeners: int(wire.Listeners),
			URL:       strings.TrimSpace(wire.URL),
		})
	}
	return artists, nil
}

// GetArtistTopTracks returns one artist's most popular tracks.
//
// The recommendation fallback uses it after track.getSimilar answers with an
// empty list: the seed has no neighbour data, but its similar artists usually do.
func (s *LastFmService) GetArtistTopTracks(ctx context.Context, artist string, limit int) ([]LastFmTrack, error) {
	if _, err := s.requireUser(); err != nil {
		return nil, err
	}
	artist = strings.TrimSpace(artist)
	if artist == "" {
		return nil, fmt.Errorf("last.fm artist top tracks: artist is required")
	}

	var response struct {
		TopTracks struct {
			Track flexList[lastFmTrackWire] `json:"track"`
		} `json:"toptracks"`
	}
	err := s.callRead(ctx, map[string]string{
		"method": "artist.getTopTracks",
		"artist": artist,
		"limit":  strconv.Itoa(clampLimit(limit, 5)),
	}, &response)
	if err != nil {
		return nil, err
	}
	return parseTrackList(response.TopTracks.Track), nil
}

// GetUserTopTags returns the tags the connected user listens to most.
//
// It is only one half of the global fallback: many accounts have no top tags at
// all, so the recommender also aggregates every seed artist's tags.
func (s *LastFmService) GetUserTopTags(ctx context.Context, limit int) ([]LastFmTag, error) {
	user, err := s.requireUser()
	if err != nil {
		return nil, err
	}

	var response struct {
		TopTags struct {
			Tag flexList[lastFmTagWire] `json:"tag"`
		} `json:"toptags"`
	}
	err = s.callRead(ctx, map[string]string{
		"method": "user.getTopTags",
		"user":   user,
		"limit":  strconv.Itoa(clampLimit(limit, 10)),
	}, &response)
	if err != nil {
		return nil, err
	}
	return parseTagList(response.TopTags.Tag), nil
}

// GetArtistTopTags returns the tags Last.fm associates with one artist.
func (s *LastFmService) GetArtistTopTags(ctx context.Context, artist string, limit int) ([]LastFmTag, error) {
	if _, err := s.requireUser(); err != nil {
		return nil, err
	}
	artist = strings.TrimSpace(artist)
	if artist == "" {
		return nil, fmt.Errorf("last.fm artist top tags: artist is required")
	}

	var response struct {
		TopTags struct {
			Tag flexList[lastFmTagWire] `json:"tag"`
		} `json:"toptags"`
	}
	err := s.callRead(ctx, map[string]string{
		"method": "artist.getTopTags",
		"artist": artist,
		"limit":  strconv.Itoa(clampLimit(limit, 5)),
	}, &response)
	if err != nil {
		return nil, err
	}
	return parseTagList(response.TopTags.Tag), nil
}

// GetTagTopTracks returns the globally popular tracks under one tag.
func (s *LastFmService) GetTagTopTracks(ctx context.Context, tag string, limit int) ([]LastFmTrack, error) {
	if _, err := s.requireUser(); err != nil {
		return nil, err
	}
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return nil, fmt.Errorf("last.fm tag top tracks: tag is required")
	}

	var response struct {
		Tracks struct {
			Track flexList[lastFmTrackWire] `json:"track"`
		} `json:"tracks"`
	}
	err := s.callRead(ctx, map[string]string{
		"method": "tag.getTopTracks",
		"tag":    tag,
		"limit":  strconv.Itoa(clampLimit(limit, 10)),
	}, &response)
	if err != nil {
		return nil, err
	}
	return parseTrackList(response.Tracks.Track), nil
}

// GetChartTopTracks is the last-resort global chart used when a cold account
// produces no seeds at all.
func (s *LastFmService) GetChartTopTracks(ctx context.Context, limit int) ([]LastFmTrack, error) {
	if _, err := s.requireUser(); err != nil {
		return nil, err
	}

	var response struct {
		Tracks struct {
			Track flexList[lastFmTrackWire] `json:"track"`
		} `json:"tracks"`
	}
	err := s.callRead(ctx, map[string]string{
		"method": "chart.getTopTracks",
		"limit":  strconv.Itoa(clampLimit(limit, 20)),
	}, &response)
	if err != nil {
		return nil, err
	}
	return parseTrackList(response.Tracks.Track), nil
}

// parseTrackList drops unusable rows from a wire list.
func parseTrackList(wireTracks flexList[lastFmTrackWire]) []LastFmTrack {
	tracks := make([]LastFmTrack, 0, len(wireTracks))
	for _, wire := range wireTracks {
		track := wire.toTrack()
		if track.Title == "" || track.Artist == "" {
			continue
		}
		tracks = append(tracks, track)
	}
	return tracks
}

func parseTagList(wireTags flexList[lastFmTagWire]) []LastFmTag {
	tags := make([]LastFmTag, 0, len(wireTags))
	for _, wire := range wireTags {
		name := strings.TrimSpace(wire.Name)
		if name == "" {
			continue
		}
		tags = append(tags, LastFmTag{Name: name, Count: int(wire.Count)})
	}
	return tags
}

// lastFmPlaceholderImageMarker identifies Last.fm's default artist star image.
// The API returns it for artists without artwork, and the browser renders it as
// a broken or blank avatar if the URL is used directly.
const lastFmPlaceholderImageMarker = "2a96cbd8b46e442fc41c2b86b821562f"

func usableArtistImage(url string) string {
	trimmed := strings.TrimSpace(url)
	if trimmed == "" || strings.Contains(trimmed, lastFmPlaceholderImageMarker) {
		return ""
	}
	return trimmed
}

// requireUser is the single connection check for the read-only endpoints.
func (s *LastFmService) requireUser() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	username := strings.TrimSpace(s.username)
	if s.sessionKey == "" || username == "" {
		return "", domain.ErrLastFmNotConnected
	}
	return username, nil
}

// callRead performs one unauthenticated read request through the shared limiter.
func (s *LastFmService) callRead(ctx context.Context, params map[string]string, target any) error {
	if strings.TrimSpace(s.apiKey) == "" {
		return fmt.Errorf("last.fm API key is not configured")
	}
	params["api_key"] = s.apiKey
	params["format"] = "json"

	query := url.Values{}
	for key, value := range params {
		query.Set(key, value)
	}
	endpoint := apiBase + "?" + query.Encode()

	release, err := s.limiter.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()

	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("last.fm %s: build request: %w", params["method"], err)
	}
	response, err := s.http.Do(request)
	if err != nil {
		return fmt.Errorf("last.fm %s: %w", params["method"], err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("last.fm %s: read response: %w", params["method"], err)
	}

	// Last.fm reports application errors with HTTP 200 in some cases and a
	// non-2xx status in others, so the body's error object is checked first.
	var apiError lastFmError
	if json.Unmarshal(body, &apiError) == nil && apiError.Error != 0 {
		return fmt.Errorf("last.fm %s: %s", params["method"], apiError.Message)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("last.fm %s: http %s", params["method"], response.Status)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("last.fm %s: decode response: %w", params["method"], err)
	}
	return nil
}

func clampLimit(limit, fallback int) int {
	if limit <= 0 {
		limit = fallback
	}
	if limit > 200 {
		limit = 200
	}
	return limit
}

// --- Wire shapes -------------------------------------------------------------

type lastFmError struct {
	Error   int    `json:"error"`
	Message string `json:"message"`
}

type lastFmImage struct {
	Text string `json:"#text"`
	Size string `json:"size"`
}

type lastFmArtistRef struct {
	Text string `json:"#text"`
	MBID string `json:"mbid"`
}

type lastFmTrackWire struct {
	Name   string          `json:"name"`
	Artist lastFmArtistRef `json:"artist"`
	Album  struct {
		Text string `json:"#text"`
	} `json:"album"`
	Image     []lastFmImage `json:"image"`
	Duration  flexInt       `json:"duration"`
	PlayCount flexInt       `json:"playcount"`
	MBID      string        `json:"mbid"`
	URL       string        `json:"url"`
}

func (w lastFmTrackWire) toTrack() LastFmTrack {
	return LastFmTrack{
		Artist:    strings.TrimSpace(w.Artist.Text),
		Title:     strings.TrimSpace(w.Name),
		Album:     strings.TrimSpace(w.Album.Text),
		ImageURL:  largestImage(w.Image),
		Duration:  int(w.Duration),
		PlayCount: int(w.PlayCount),
		MBID:      strings.TrimSpace(w.MBID),
		URL:       strings.TrimSpace(w.URL),
	}
}

type lastFmSimilarArtistWire struct {
	Name      string        `json:"name"`
	Image     []lastFmImage `json:"image"`
	Listeners flexInt       `json:"listeners"`
	URL       string        `json:"url"`
}

type lastFmTagWire struct {
	Name  string  `json:"name"`
	Count flexInt `json:"count"`
}

// largestImage picks the biggest rendition Last.fm reported.
//
// The array is ordered small→extralarge, but the largest entry is sometimes
// empty while a smaller one is not, so the first non-empty from the end wins.
func largestImage(images []lastFmImage) string {
	for index := len(images) - 1; index >= 0; index-- {
		if text := strings.TrimSpace(images[index].Text); text != "" {
			return text
		}
	}
	return ""
}

// flexList decodes an array that Last.fm collapses to a single object when the
// answer has exactly one element.
type flexList[T any] []T

func (l *flexList[T]) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*l = nil
		return nil
	}
	if trimmed[0] == '[' {
		return json.Unmarshal(data, (*[]T)(l))
	}
	var single T
	if err := json.Unmarshal(data, &single); err != nil {
		return err
	}
	*l = []T{single}
	return nil
}

// flexInt accepts the string-or-number spelling Last.fm uses inconsistently.
type flexInt int

func (n *flexInt) UnmarshalJSON(data []byte) error {
	text := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if text == "" || text == "null" {
		*n = 0
		return nil
	}
	value, err := strconv.Atoi(text)
	if err != nil {
		return fmt.Errorf("flex int %q: %w", text, err)
	}
	*n = flexInt(value)
	return nil
}
