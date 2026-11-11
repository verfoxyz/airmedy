package lastfm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"airmedy/internal/domain"
)

// Scrobble queue tuning. Last.fm accepts at most fifty scrobbles per request;
// the retry schedule backs off far enough out that an offline session does not
// hammer the API, and the retention bounds keep a long outage from growing the
// queue without limit.
const (
	scrobbleBatchLimit    = 50
	scrobbleMaxAttempts   = 20
	scrobbleQueueLimit    = 5000
	scrobbleRetention     = 30 * 24 * time.Hour
	scrobbleRetryInterval = 5 * time.Minute
)

// scrobbleBackoff is the delay before the next attempt, indexed by attempt
// number. A row past the last step keeps the longest delay until it fails
// maxAttempts times and is dropped by cleanup.
var scrobbleBackoff = []time.Duration{
	time.Minute,
	5 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	6 * time.Hour,
}

func backoffFor(attempt int) time.Duration {
	if attempt <= 1 {
		return scrobbleBackoff[0]
	}
	if attempt > len(scrobbleBackoff) {
		return scrobbleBackoff[len(scrobbleBackoff)-1]
	}
	return scrobbleBackoff[attempt-1]
}

// StartScrobbleQueue starts the retry worker. It flushes once at startup (rows
// survive a crash), every five minutes, and whenever a scrobble is queued or a
// batch succeeds.
func (s *LastFmService) StartScrobbleQueue() {
	s.flushMu.Lock()
	if s.flushCancel != nil {
		s.flushMu.Unlock()
		return
	}
	// The lifecycle OnStart context is cancelled when startup finishes, so the
	// worker owns a context that lives until OnStop.
	ctx, cancel := context.WithCancel(context.Background())
	s.flushCancel = cancel
	s.flushMu.Unlock()

	go s.scrobbleQueueLoop(ctx)
}

// StopScrobbleQueue stops the retry worker. The queue itself stays on disk.
func (s *LastFmService) StopScrobbleQueue() {
	s.flushMu.Lock()
	cancel := s.flushCancel
	s.flushCancel = nil
	s.flushMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *LastFmService) scrobbleQueueLoop(ctx context.Context) {
	s.flushPendingScrobbles(ctx)

	ticker := time.NewTicker(scrobbleRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.flushPendingScrobbles(ctx)
		case <-s.scrobbleWake:
			s.flushPendingScrobbles(ctx)
		}
	}
}

// wakeScrobbleQueue asks the worker to flush without blocking the caller.
func (s *LastFmService) wakeScrobbleQueue() {
	if s.scrobbleWake == nil {
		return
	}
	select {
	case s.scrobbleWake <- struct{}{}:
	default:
	}
}

// pendingScrobbleFromTrack snapshots the fields Last.fm needs.
func pendingScrobbleFromTrack(track *domain.TrackDTO, startTime time.Time) (domain.PendingScrobble, bool) {
	if track == nil {
		return domain.PendingScrobble{}, false
	}
	artist := ""
	if len(track.Artists) > 0 && track.Artists[0] != nil {
		artist = strings.TrimSpace(track.Artists[0].Name)
	}
	if artist == "" || strings.TrimSpace(track.Title) == "" {
		return domain.PendingScrobble{}, false
	}

	entry := domain.PendingScrobble{
		Artist:      artist,
		Title:       strings.TrimSpace(track.Title),
		StartedAt:   startTime.Unix(),
		TrackNumber: track.TrackNumber,
		Duration:    track.Duration,
	}
	if track.Album != nil && track.Album.Title != "" && track.Album.Title != "Unknown Album" {
		entry.Album = track.Album.Title
	}
	if len(track.AlbumArtists) > 0 && track.AlbumArtists[0] != nil {
		entry.AlbumArtist = track.AlbumArtists[0].Name
	}
	return entry, true
}

func (s *LastFmService) currentTime() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// flushPendingScrobbles sends one batch of due scrobbles.
//
// The whole request either succeeds or fails: on failure every row in the batch
// gets one attempt and a backoff, and the next flush retries them in play order.
// On success accepted rows are deleted and ignored rows are marked so they are
// never sent again.
func (s *LastFmService) flushPendingScrobbles(ctx context.Context) {
	if s.scrobbles == nil {
		return
	}
	s.mu.RLock()
	sk := s.sessionKey
	s.mu.RUnlock()
	if sk == "" {
		return
	}

	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	now := s.currentTime()
	if removed, err := s.scrobbles.Cleanup(ctx, now.Add(-scrobbleRetention), scrobbleMaxAttempts, scrobbleQueueLimit); err != nil {
		s.logger.Warn("lastfm queue cleanup failed", "error", err)
	} else if removed > 0 {
		s.logger.Warn("lastfm queue dropped expired scrobbles", "count", removed)
	}

	due, err := s.scrobbles.Due(ctx, now, scrobbleMaxAttempts, scrobbleBatchLimit)
	if err != nil {
		s.logger.Warn("lastfm queue could not list pending scrobbles", "error", err)
		return
	}
	if len(due) == 0 {
		return
	}

	ignored, err := s.postScrobbleBatch(ctx, sk, due)
	if err != nil {
		retryAfter := retryAfterFrom(err)
		for _, entry := range due {
			next := now.Add(backoffFor(entry.Attempts + 1))
			if retryAfter > 0 {
				next = now.Add(retryAfter)
			}
			if markErr := s.scrobbles.MarkFailure(ctx, entry.ID, err.Error(), next); markErr != nil {
				s.logger.Warn("lastfm queue could not record a failure", "id", entry.ID, "error", markErr)
			}
		}
		s.logger.Warn("lastfm scrobble batch failed",
			"count", len(due), "retry_after", retryAfter, "error", err)
		return
	}

	accepted := make([]string, 0, len(due))
	for _, entry := range due {
		if ignored[scrobbleIdentity(entry.Artist, entry.Title, entry.StartedAt)] {
			if markErr := s.scrobbles.MarkIgnored(ctx, entry.ID, "ignored by last.fm"); markErr != nil {
				s.logger.Warn("lastfm queue could not mark an ignored scrobble", "id", entry.ID, "error", markErr)
			}
			continue
		}
		accepted = append(accepted, entry.ID)
	}
	if len(accepted) > 0 {
		if err := s.scrobbles.Delete(ctx, accepted); err != nil {
			s.logger.Warn("lastfm queue could not delete accepted scrobbles", "error", err)
			return
		}
	}
	s.logger.Debug("lastfm scrobbles flushed", "accepted", len(accepted), "ignored", len(due)-len(accepted))

	// A full batch means more rows are probably due. Wake the worker once; the
	// guard prevents a spin when nothing was accepted.
	if len(due) == scrobbleBatchLimit && len(accepted) > 0 {
		s.wakeScrobbleQueue()
	}
}

// postOneScrobble is the fallback for a deployment without a durable queue.
func (s *LastFmService) postOneScrobble(entry domain.PendingScrobble) {
	s.mu.RLock()
	sk := s.sessionKey
	s.mu.RUnlock()
	if sk == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if _, err := s.postScrobbleBatch(ctx, sk, []domain.PendingScrobble{entry}); err != nil {
		s.logger.Warn("lastfm scrobble failed", "title", entry.Title, "error", err)
	}
}

// scrobbleHTTPError carries the status and any Retry-After hint.
type scrobbleHTTPError struct {
	Status     int
	RetryAfter time.Duration
	Body       string
}

func (e *scrobbleHTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("last.fm track.scrobble: http %d", e.Status)
	}
	return fmt.Sprintf("last.fm track.scrobble: http %d: %s", e.Status, e.Body)
}

func retryAfterFrom(err error) time.Duration {
	var httpErr *scrobbleHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.RetryAfter
	}
	return 0
}

func parseRetryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func scrobbleIdentity(artist, title string, timestamp int64) string {
	return strings.ToLower(strings.TrimSpace(artist)) + "\x00" +
		strings.ToLower(strings.TrimSpace(title)) + "\x00" +
		strconv.FormatInt(timestamp, 10)
}

// scrobbleBatchEntry is one scrobble Last.fm echoes back.
type scrobbleBatchEntry struct {
	Artist lastFmArtistRef `json:"artist"`
	Track  struct {
		Text string `json:"#text"`
	} `json:"track"`
	Timestamp      flexInt `json:"timestamp"`
	IgnoredMessage struct {
		Code flexInt `json:"code"`
		Text string  `json:"#text"`
	} `json:"ignoredMessage"`
}

// scrobbleBatchResponse is the subset of track.scrobble's answer the queue uses.
type scrobbleBatchResponse struct {
	Scrobbles struct {
		// Last.fm collapses a one-element array to an object.
		Scrobble flexList[scrobbleBatchEntry] `json:"scrobble"`
	} `json:"scrobbles"`
	Error   int    `json:"error"`
	Message string `json:"message"`
}

// postScrobbleBatch submits one batch and returns the identities Last.fm
// reported as ignored.
func (s *LastFmService) postScrobbleBatch(
	ctx context.Context, sessionKey string, entries []domain.PendingScrobble,
) (map[string]bool, error) {
	if len(entries) == 0 {
		return map[string]bool{}, nil
	}

	params := map[string]string{
		"method":  "track.scrobble",
		"api_key": s.apiKey,
		"sk":      sessionKey,
		"format":  "json",
	}
	for index, entry := range entries {
		suffix := "[" + strconv.Itoa(index) + "]"
		params["artist"+suffix] = entry.Artist
		params["track"+suffix] = entry.Title
		params["timestamp"+suffix] = strconv.FormatInt(entry.StartedAt, 10)
		if entry.Album != "" {
			params["album"+suffix] = entry.Album
		}
		if entry.AlbumArtist != "" {
			params["albumArtist"+suffix] = entry.AlbumArtist
		}
		if entry.TrackNumber > 0 {
			params["trackNumber"+suffix] = strconv.Itoa(entry.TrackNumber)
		}
		if entry.Duration > 0 {
			params["duration"+suffix] = strconv.Itoa(entry.Duration)
		}
	}
	params["api_sig"] = s.generateSignature(params)

	release, err := s.limiter.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(
		requestCtx, http.MethodPost, apiBase, strings.NewReader(s.toUrlValues(params).Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("last.fm track.scrobble: build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := s.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("last.fm track.scrobble: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("last.fm track.scrobble: read response: %w", err)
	}

	if response.StatusCode == http.StatusTooManyRequests {
		return nil, &scrobbleHTTPError{
			Status:     response.StatusCode,
			RetryAfter: parseRetryAfter(response.Header.Get("Retry-After")),
			Body:       strings.TrimSpace(string(body)),
		}
	}
	if response.StatusCode != http.StatusOK {
		return nil, &scrobbleHTTPError{
			Status: response.StatusCode,
			Body:   strings.TrimSpace(string(body)),
		}
	}

	var payload scrobbleBatchResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("last.fm track.scrobble: decode response: %w", err)
	}
	if payload.Error != 0 {
		return nil, fmt.Errorf("last.fm track.scrobble: %s", payload.Message)
	}

	ignored := make(map[string]bool)
	for _, scrobble := range payload.Scrobbles.Scrobble {
		if scrobble.IgnoredMessage.Code == 0 {
			continue
		}
		artist := strings.TrimSpace(scrobble.Artist.Name)
		if artist == "" {
			artist = strings.TrimSpace(scrobble.Artist.Text)
		}
		ignored[scrobbleIdentity(artist, scrobble.Track.Text, int64(scrobble.Timestamp))] = true
	}
	return ignored, nil
}
