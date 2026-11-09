package stream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"airmedy/internal/app/config"
	"airmedy/internal/domain"
)

// Prebuffer watermarks. Open returns only after enough of the body is on disk
// for FFmpeg's startup reads to hit the cache instead of the proxy's blocking
// loop, which would otherwise stall miniaudio's data-source callback.
const (
	// prebufferBytes is the floor that must be on disk before playback starts.
	prebufferBytes int64 = 1 << 20 // 1 MiB
	// prebufferSeconds approximates a safe lead when the total size is known.
	prebufferSeconds = 3.0
	// prebufferTimeout bounds the wait; the caller gets an error past it.
	prebufferTimeout = 10 * time.Second
)

// matchDurationToleranceMs is how far two sources may disagree about a track's
// duration and still be treated as the same recording. Kept tight on purpose:
// a wrong cross-source hit plays the wrong audio. Not a setting until false
// matches are reported.
const matchDurationToleranceMs int64 = 5000

// progressFlushInterval bounds how much playback progress a crash can lose. It
// only affects eviction ordering, so a periodic backstop is enough; the value
// is written on pause, close, completion, failure and shutdown.
const progressFlushInterval = 30 * time.Second

// Cache is the local HTTP proxy that makes a remote track playable.
//
// FFmpeg only ever opens http://127.0.0.1:<port>/stream/<key>; the remote URL,
// its TLS, redirects and retries stay in Go. Each session downloads the
// upstream body sequentially into <DataDir>/stream-cache/<key>.audio, and the
// proxy blocks a reader until the requested bytes have arrived so a growing
// file never looks like EOF to the decoder.
//
// Metadata lives in SQLite (stream_cache), not in a sidecar file: playback
// progress updates are a row UPDATE instead of an atomic file replacement.
// Entries are addressed by remote identity first, then matched across providers
// by normalized title/artist plus a small duration tolerance. The cache is
// disposable: the size limit evicts the least-complete entry first, then the
// one with the least playback progress.
type Cache struct {
	dir      string
	repo     domain.StreamCacheRepository
	resolver domain.StreamResolver
	settings SettingsProvider
	covers   domain.CoverPinner
	logger   *slog.Logger

	client *http.Client

	mu       sync.Mutex
	sessions map[string]*cacheSession
	server   *http.Server
	listener net.Listener
	port     int
	started  bool
}

// SettingsProvider is the settings slice the cache needs. *appsettings.Service
// satisfies it; taking the narrow port keeps this package out of appsettings.
type SettingsProvider interface {
	GetSettings(ctx context.Context) (*domain.AppSettings, error)
}

// Session is one playback window over a cached stream. ID is derived from the
// track's remote identity, so the same track always maps to the same entry.
type Session struct {
	ID  string
	URL string
}

// cacheSession is the in-memory state of one playback window.
type cacheSession struct {
	id         string
	provider   string
	externalID string
	track      *domain.TrackDTO
	audioPath  string

	mu          sync.Mutex
	received    int64
	total       int64 // -1 until the upstream announces a length
	contentType string
	complete    bool
	failed      error
	playing     bool
	background  bool
	progress    float64
	lastFlush   time.Time
	closed      bool

	fileMu sync.RWMutex
	file   *os.File

	cancel context.CancelFunc
	done   chan struct{}
}

// NewCache constructs the proxy. Start binds its loopback listener.
func NewCache(
	cfg *config.Config,
	repo domain.StreamCacheRepository,
	resolver domain.StreamResolver,
	settings SettingsProvider,
	covers domain.CoverPinner,
	logger *slog.Logger,
) *Cache {
	if logger == nil {
		logger = slog.Default()
	}
	return &Cache{
		dir:      cfg.StreamCachePath(),
		repo:     repo,
		resolver: resolver,
		settings: settings,
		covers:   covers,
		logger:   logger,
		sessions: make(map[string]*cacheSession),
		client: &http.Client{
			// No overall timeout: the body is written as it arrives. Only the
			// handshake is bounded, so a dead host cannot stall a session
			// forever.
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				ResponseHeaderTimeout: 20 * time.Second,
				ForceAttemptHTTP2:     true,
			},
		},
	}
}

// Start creates the cache directory, drops metadata left by the JSON-era cache,
// and binds the proxy to a loopback port.
func (c *Cache) Start(ctx context.Context) error {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return fmt.Errorf("create stream cache directory: %w", err)
	}
	c.cleanupLegacyFiles(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for stream cache proxy: %w", err)
	}
	c.listener = listener
	c.port = listener.Addr().(*net.TCPAddr).Port
	c.server = &http.Server{
		Handler:           http.HandlerFunc(c.handleStream),
		ReadHeaderTimeout: 10 * time.Second,
	}
	c.started = true
	go func() {
		if serveErr := c.server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			c.logger.Error("Stream cache proxy stopped", "error", serveErr)
		}
	}()
	c.logger.Info("Stream cache proxy listening", "port", c.port, "dir", c.dir)
	return nil
}

// Stop shuts the proxy down and releases every session's file.
func (c *Cache) Stop(ctx context.Context) error {
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return nil
	}
	server := c.server
	c.started = false
	c.mu.Unlock()

	// Close the sessions first: a streaming handler only returns once its
	// session is closed or the client disconnects, and Shutdown waits for
	// active handlers.
	c.closeAllSessions()
	err := server.Shutdown(ctx)
	c.mu.Lock()
	c.sessions = make(map[string]*cacheSession)
	c.mu.Unlock()
	return err
}

// LocalURL reports the proxy base URL, or "" before Start.
func (c *Cache) LocalURL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", c.port)
}

// Open starts (or reuses) the playback session for track and returns its local
// URL. Identity (provider + external id) is looked up first; when no entry
// exists, the same normalized title/artist cached under another provider is
// reused so switching sources never re-downloads the song.
func (c *Cache) Open(ctx context.Context, track *domain.TrackDTO) (*Session, error) {
	if track == nil {
		return nil, fmt.Errorf("open stream: nil track")
	}
	provider := domain.NormalizeProvider(track.Provider)
	if provider == "" || strings.TrimSpace(track.ExternalID) == "" {
		return nil, fmt.Errorf("%w: track %s has no provider/external id", domain.ErrStreamUnavailable, track.ID)
	}

	identityKey := cacheKeyFor(provider, track.ExternalID)
	if session, err := c.takeSession(ctx, identityKey); err != nil {
		return nil, err
	} else if session != nil {
		return session, nil
	}

	entry, err := c.resolveEntry(ctx, provider, track)
	if err != nil {
		return nil, err
	}
	id := entry.CacheKey
	c.pinCover(entry)

	// A cross-source match may already be downloading under another identity.
	if id != identityKey {
		if session, err := c.takeSession(ctx, id); err != nil {
			return nil, err
		} else if session != nil {
			return session, nil
		}
	}

	s, err := c.startSession(entry, track, false)
	if err != nil {
		return nil, err
	}
	// Only fetch what is missing. A complete entry is served from disk.
	if !s.isComplete() {
		// Do not hand FFmpeg a URL that would block on its first read: wait for
		// a safe lead, a failure, or the timeout before returning.
		if waitErr := c.waitForPrebuffer(ctx, s); waitErr != nil {
			c.abandonSession(s)
			return nil, waitErr
		}
	}

	c.enforceLimit(ctx)
	return &Session{ID: id, URL: c.localURL() + "/stream/" + id}, nil
}

// startSession registers a cache session for entry and starts its download when
// bytes are missing. background marks a session opened only to complete the
// cache, which is detached when the download ends instead of waiting for Close.
func (c *Cache) startSession(entry *domain.StreamCacheEntry, track *domain.TrackDTO, background bool) (*cacheSession, error) {
	base := c.localURL()
	if base == "" {
		return nil, fmt.Errorf("open stream: cache proxy is not running")
	}

	s := &cacheSession{
		id:         entry.CacheKey,
		provider:   entry.Provider,
		externalID: entry.ExternalID,
		track:      track,
		audioPath:  filepath.Join(c.dir, entry.AudioPath),
		total:      -1,
		playing:    !background,
		background: background,
		lastFlush:  time.Now(),
		done:       make(chan struct{}),
	}
	if entry.TotalBytes > 0 {
		s.total = entry.TotalBytes
	}
	s.received = entry.ReceivedBytes
	s.complete = entry.Complete
	s.progress = entry.PlaybackProgress
	if err := c.openAudio(s); err != nil {
		return nil, err
	}
	// A metadata row can claim completion after the file was truncated or lost;
	// trust the bytes actually on disk so playback does not serve an empty body.
	if s.complete && (s.received <= 0 || (s.total > 0 && s.received < s.total)) {
		s.complete = false
	}

	c.mu.Lock()
	c.sessions[s.id] = s
	c.mu.Unlock()

	if s.complete {
		close(s.done)
		return s, nil
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go c.runSession(runCtx, s)
	return s, nil
}

// localURL is the proxy base URL, or "" before Start.
func (c *Cache) localURL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.localURLUnlocked()
}

// takeSession reuses an open session for key. (nil, nil) means none exists and
// the caller should start one; an error means a reused session could not be
// primed and was abandoned.
func (c *Cache) takeSession(ctx context.Context, key string) (*Session, error) {
	c.mu.Lock()
	existing, ok := c.sessions[key]
	if !ok {
		c.mu.Unlock()
		return nil, nil
	}
	existing.mu.Lock()
	failed := existing.failed != nil
	if !failed {
		existing.playing = true
		// Playback now owns the session: completion waiters must not detach it.
		existing.background = false
	}
	existing.mu.Unlock()
	if !failed {
		base := c.localURLUnlocked()
		c.mu.Unlock()
		if waitErr := c.waitForPrebuffer(ctx, existing); waitErr != nil {
			c.abandonSession(existing)
			return nil, waitErr
		}
		return &Session{ID: key, URL: base + "/stream/" + key}, nil
	}
	// A failed download must be retried from scratch rather than reusing the
	// dead session (its upstream URL is likely expired).
	delete(c.sessions, key)
	c.mu.Unlock()
	c.stopSession(existing)
	return nil, nil
}

// resolveEntry finds the cache row for a track. An identity hit wins; otherwise
// the same normalized title/artist cached under another provider is reused. A
// matched row keeps its original identity: playback context comes from track,
// and a later promotion downloads under track's identity.
func (c *Cache) resolveEntry(ctx context.Context, provider string, track *domain.TrackDTO) (*domain.StreamCacheEntry, error) {
	now := time.Now().Unix()
	entry, err := c.lookupEntry(ctx, provider, track)
	if err != nil {
		return nil, err
	}
	if entry != nil {
		entry.LastUsed = now
		if upsertErr := c.repo.Upsert(ctx, entry); upsertErr != nil {
			c.logger.Warn("Failed to touch stream cache entry", "cache_key", entry.CacheKey, "error", upsertErr)
		}
		return entry, nil
	}

	title := strings.TrimSpace(track.Title)
	artist := primaryArtist(track)
	titleKey := domain.NormalizationKey(title)
	artistKey := domain.NormalizationKey(artist)

	key := cacheKeyFor(provider, track.ExternalID)
	entry = &domain.StreamCacheEntry{
		CacheKey:   key,
		Provider:   provider,
		ExternalID: track.ExternalID,
		Title:      title,
		TitleKey:   titleKey,
		Artist:     artist,
		ArtistKey:  artistKey,
		Artists:    artistList(track),
		Album:      albumTitle(track),
		DurationMs: int64(track.Duration) * 1000,
		Quality:    c.streamQuality(ctx),
		AudioPath:  key + ".audio",
		CreatedAt:  now,
		LastUsed:   now,
	}
	if err := c.repo.Upsert(ctx, entry); err != nil {
		return nil, fmt.Errorf("register stream cache entry: %w", err)
	}
	return entry, nil
}

// lookupEntry finds an existing entry by identity first, then by the same
// normalized title/artist under another provider. It never creates a row.
func (c *Cache) lookupEntry(ctx context.Context, provider string, track *domain.TrackDTO) (*domain.StreamCacheEntry, error) {
	entry, err := c.repo.GetByIdentity(ctx, provider, track.ExternalID)
	if err != nil {
		return nil, fmt.Errorf("look up stream cache: %w", err)
	}
	if entry != nil {
		return entry, nil
	}
	titleKey := domain.NormalizationKey(strings.TrimSpace(track.Title))
	artistKey := domain.NormalizationKey(primaryArtist(track))
	if titleKey == "" || artistKey == "" {
		return nil, nil
	}
	match, err := c.repo.MatchByTitleArtist(ctx, titleKey, artistKey,
		int64(track.Duration)*1000, matchDurationToleranceMs)
	if err != nil {
		return nil, fmt.Errorf("match stream cache: %w", err)
	}
	return match, nil
}

// primaryArtist is the matching key's artist: the first credited artist, or the
// first raw name when relations were not loaded.
func primaryArtist(track *domain.TrackDTO) string {
	for _, artist := range track.Artists {
		if artist != nil && strings.TrimSpace(artist.Name) != "" {
			return strings.TrimSpace(artist.Name)
		}
	}
	raw := strings.TrimSpace(track.RawArtistNames)
	if raw == "" {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(raw, ";", 2)[0])
}

// artistList is the display-only, all-artists string stored on the entry.
func artistList(track *domain.TrackDTO) string {
	names := make([]string, 0, len(track.Artists))
	for _, artist := range track.Artists {
		if artist != nil && strings.TrimSpace(artist.Name) != "" {
			names = append(names, strings.TrimSpace(artist.Name))
		}
	}
	if len(names) == 0 {
		return strings.TrimSpace(track.RawArtistNames)
	}
	return strings.Join(names, "; ")
}

func albumTitle(track *domain.TrackDTO) string {
	if track.Album != nil && strings.TrimSpace(track.Album.Title) != "" {
		return strings.TrimSpace(track.Album.Title)
	}
	return strings.TrimSpace(track.RawAlbumName)
}

// Close ends a playback window and cancels its download. The cached bytes stay
// on disk for the next play or for eviction.
func (c *Cache) Close(session *Session) {
	if session == nil {
		return
	}
	c.mu.Lock()
	s, ok := c.sessions[session.ID]
	if ok {
		delete(c.sessions, session.ID)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	c.stopSession(s)
}

// ReportProgress records how far the player has advanced, in seconds. It only
// touches memory unless thirty seconds have passed since the last flush: the
// player reports every tick, while readers of this value (eviction, a replay)
// run at pause/close time. FlushProgress supplies those explicit checkpoints.
func (c *Cache) ReportProgress(session *Session, seconds float64) {
	if session == nil || seconds < 0 {
		return
	}
	c.mu.Lock()
	s := c.sessions[session.ID]
	c.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	s.progress = seconds
	flush := time.Since(s.lastFlush) >= progressFlushInterval
	if flush {
		s.lastFlush = time.Now()
	}
	s.mu.Unlock()
	if flush {
		c.saveProgress(s)
		c.saveStats(s)
	}
}

// FlushProgress writes the current position and byte counters. The player calls
// it on pause; Close and Stop cover the remaining lifecycle boundaries.
func (c *Cache) FlushProgress(session *Session) {
	if session == nil {
		return
	}
	c.mu.Lock()
	s := c.sessions[session.ID]
	c.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	s.lastFlush = time.Now()
	s.mu.Unlock()
	c.saveProgress(s)
	c.saveStats(s)
}

// CachedFileInfo reports the entry for track without creating a row or touching
// the network. found is false when no entry (identity or cross-source) exists.
func (c *Cache) CachedFileInfo(ctx context.Context, track *domain.TrackDTO) (path, quality string, complete, found bool, err error) {
	if track == nil {
		return "", "", false, false, nil
	}
	provider := domain.NormalizeProvider(track.Provider)
	if provider == "" || strings.TrimSpace(track.ExternalID) == "" {
		return "", "", false, false, nil
	}
	entry, err := c.lookupEntry(ctx, provider, track)
	if err != nil {
		return "", "", false, false, err
	}
	if entry == nil {
		return "", "", false, false, nil
	}
	return filepath.Join(c.dir, entry.AudioPath), entry.Quality, c.entryCompleteOnDisk(entry), true, nil
}

// EnsureComplete completes the cache entry in the background and closes the
// returned channel once the bytes are on disk and no playback session owns the
// file. The error reports a failed download; a nil error means complete.
func (c *Cache) EnsureComplete(ctx context.Context, track *domain.TrackDTO) (<-chan error, error) {
	if track == nil {
		return nil, fmt.Errorf("ensure stream cache: nil track")
	}
	provider := domain.NormalizeProvider(track.Provider)
	if provider == "" || strings.TrimSpace(track.ExternalID) == "" {
		return nil, fmt.Errorf("%w: track %s has no provider/external id", domain.ErrStreamUnavailable, track.ID)
	}
	entry, err := c.resolveEntry(ctx, provider, track)
	if err != nil {
		return nil, err
	}
	c.pinCover(entry)

	c.mu.Lock()
	existing := c.sessions[entry.CacheKey]
	c.mu.Unlock()

	if existing != nil {
		existing.mu.Lock()
		failed := existing.failed != nil
		existing.mu.Unlock()
		if failed {
			// Retry from scratch: the upstream URL of a dead attempt is expired.
			c.mu.Lock()
			if c.sessions[entry.CacheKey] == existing {
				delete(c.sessions, entry.CacheKey)
			}
			c.mu.Unlock()
			c.stopSession(existing)
			existing = nil
		}
	}

	if existing == nil {
		if c.entryCompleteOnDisk(entry) {
			done := make(chan error, 1)
			close(done)
			return done, nil
		}
		existing, err = c.startSession(entry, track, true)
		if err != nil {
			return nil, err
		}
	}
	return c.watchCompletion(existing), nil
}

// MarkPendingPromote flags the track's cache entry so eviction keeps it. It
// creates the entry when the track was never streamed; EnsureComplete then
// fills it.
func (c *Cache) MarkPendingPromote(ctx context.Context, track *domain.TrackDTO) error {
	if track == nil {
		return nil
	}
	provider := domain.NormalizeProvider(track.Provider)
	if provider == "" || strings.TrimSpace(track.ExternalID) == "" {
		return fmt.Errorf("%w: track %s has no provider/external id", domain.ErrStreamUnavailable, track.ID)
	}
	entry, err := c.resolveEntry(ctx, provider, track)
	if err != nil {
		return err
	}
	c.pinCover(entry)
	return c.repo.SetPendingPromote(ctx, entry.CacheKey, true)
}

// DiscardEntry deletes a cache entry and its audio. A session that is still
// playing is left alone: deleting the file under the decoder breaks playback,
// and the entry will be evicted when it closes.
func (c *Cache) DiscardEntry(ctx context.Context, track *domain.TrackDTO) error {
	if track == nil {
		return nil
	}
	provider := domain.NormalizeProvider(track.Provider)
	if provider == "" || strings.TrimSpace(track.ExternalID) == "" {
		return nil
	}
	entry, err := c.lookupEntry(ctx, provider, track)
	if err != nil || entry == nil {
		return err
	}

	c.mu.Lock()
	_, active := c.sessions[entry.CacheKey]
	c.mu.Unlock()
	if active {
		return nil
	}

	if err := os.Remove(filepath.Join(c.dir, entry.AudioPath)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("discard stream cache audio %s: %w", entry.CacheKey, err)
	}
	if err := c.repo.Delete(ctx, entry.CacheKey); err != nil {
		return err
	}
	c.unpinCover(entry.Provider, entry.ExternalID)
	return nil
}

// entryCompleteOnDisk trusts the bytes actually present, not just the row: a
// truncated file after an unclean shutdown must look incomplete.
func (c *Cache) entryCompleteOnDisk(entry *domain.StreamCacheEntry) bool {
	if !entry.Complete {
		return false
	}
	info, err := os.Stat(filepath.Join(c.dir, entry.AudioPath))
	if err != nil || info.Size() <= 0 {
		return false
	}
	if entry.ReceivedBytes > info.Size() {
		return false
	}
	return entry.TotalBytes <= 0 || info.Size() >= entry.TotalBytes
}

// watchCompletion closes its channel when the session's download ended and a
// playback owner (if any) released the file. A background session is detached
// immediately so the promoted file is not held open.
func (c *Cache) watchCompletion(s *cacheSession) <-chan error {
	done := make(chan error, 1)
	go func() {
		<-s.done
		s.mu.Lock()
		err := s.failed
		background := s.background
		s.mu.Unlock()

		if background {
			c.mu.Lock()
			if c.sessions[s.id] == s {
				delete(c.sessions, s.id)
			}
			c.mu.Unlock()
			c.stopSession(s)
		} else {
			for {
				c.mu.Lock()
				_, active := c.sessions[s.id]
				c.mu.Unlock()
				if !active {
					break
				}
				time.Sleep(250 * time.Millisecond)
			}
		}

		done <- err
		close(done)
	}()
	return done
}

func (c *Cache) localURLUnlocked() string {
	if !c.started {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", c.port)
}

// waitForPrebuffer blocks until the cache holds a safe lead for the decoder's
// first reads. It returns nil as soon as the entry is complete or the water
// mark is reached, and an error on timeout, cancellation or download failure.
func (c *Cache) waitForPrebuffer(ctx context.Context, s *cacheSession) error {
	deadline := time.Now().Add(prebufferTimeout)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	for {
		s.mu.Lock()
		received := s.received
		total := s.total
		complete := s.complete
		failed := s.failed
		closed := s.closed
		s.mu.Unlock()

		switch {
		case complete:
			return nil
		case failed != nil:
			return failed
		case closed:
			return fmt.Errorf("%w: stream session for %s closed before prebuffer (%d bytes)",
				domain.ErrStreamUnavailable, s.id, received)
		case received >= prebufferTarget(total, s.track.Duration):
			return nil
		case time.Now().After(deadline):
			return fmt.Errorf("%w: stream prebuffer timeout: %s (%d bytes)",
				domain.ErrStreamUnavailable, s.id, received)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			// The download goroutine ended before the water mark. Classify the
			// final state instead of spinning on the closed channel.
			s.mu.Lock()
			complete = s.complete
			failed = s.failed
			closed = s.closed
			received = s.received
			s.mu.Unlock()
			switch {
			case complete:
				return nil
			case failed != nil:
				return failed
			case closed:
				return fmt.Errorf("%w: stream session for %s closed before prebuffer (%d bytes)",
					domain.ErrStreamUnavailable, s.id, received)
			default:
				return fmt.Errorf("%w: stream prebuffer interrupted: %s (%d bytes)",
					domain.ErrStreamUnavailable, s.id, received)
			}
		case <-ticker.C:
		}
	}
}

// prebufferTarget is 1 MiB, reduced to a three-second lead when the total size
// and duration are both known, so a small or low-bitrate file is not held back
// waiting for a megabyte that may be most of the track.
func prebufferTarget(total int64, durationSeconds int) int64 {
	target := prebufferBytes
	if total > 0 && durationSeconds > 0 {
		lead := int64(float64(total) / float64(durationSeconds) * prebufferSeconds)
		if lead > 0 && lead < target {
			target = lead
		}
	}
	return target
}

// abandonSession removes a session whose Open failed and releases its file.
// The cache file and metadata stay on disk so a retry resumes from them.
func (c *Cache) abandonSession(s *cacheSession) {
	c.mu.Lock()
	if c.sessions[s.id] == s {
		delete(c.sessions, s.id)
	}
	c.mu.Unlock()
	c.stopSession(s)
}

func (c *Cache) openAudio(s *cacheSession) error {
	file, err := os.OpenFile(s.audioPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("open stream cache file: %w", err)
	}
	if info, statErr := file.Stat(); statErr == nil {
		// A metadata row may claim more than the file holds after an unclean
		// shutdown; never resume past the bytes actually on disk.
		if info.Size() < s.received {
			s.received = info.Size()
		}
	}
	s.file = file
	return nil
}

// runSession resolves the remote URL and downloads it sequentially, retrying a
// transport failure a couple of times from the byte already on disk.
func (c *Cache) runSession(ctx context.Context, s *cacheSession) {
	defer close(s.done)

	quality := c.streamQuality(ctx)
	var ref domain.StreamRef
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return
		}
		ref, err = c.resolver.Resolve(ctx, s.track, quality)
		if err == nil {
			break
		}
		if !errors.Is(err, domain.ErrStreamRateLimited) && attempt == 0 {
			// Permanent failures (no plugin, VIP) cannot improve by retrying.
			break
		}
		if !sleepContext(ctx, time.Duration(attempt+1)*time.Second) {
			return
		}
	}
	if err != nil {
		c.fail(s, err)
		return
	}

	for attempt := 0; attempt < 3; attempt++ {
		err = c.fetch(ctx, s, ref)
		if err == nil {
			s.mu.Lock()
			s.complete = true
			s.failed = nil
			s.mu.Unlock()
			c.saveStats(s)
			// The cover may have been cached while the audio was downloading;
			// pin it now that the entry is complete.
			c.pinCoverIdentity(s.provider, s.externalID)
			c.enforceLimit(context.Background())
			return
		}
		if ctx.Err() != nil {
			return
		}
		if !sleepContext(ctx, time.Duration(attempt+1)*time.Second) {
			return
		}
	}
	c.fail(s, err)
}

// fetch performs one Range continuation and writes the body to the cache file.
func (c *Cache) fetch(ctx context.Context, s *cacheSession, ref domain.StreamRef) error {
	s.mu.Lock()
	offset := s.received
	s.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.URL, nil)
	if err != nil {
		return fmt.Errorf("build upstream request: %w", err)
	}
	for key, value := range ref.Headers {
		req.Header.Set(key, value)
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch stream: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// The server ignored Range: restart from zero.
		if offset > 0 {
			if truncateErr := s.file.Truncate(0); truncateErr != nil {
				return fmt.Errorf("reset stream cache file: %w", truncateErr)
			}
			s.mu.Lock()
			s.received = 0
			s.mu.Unlock()
			offset = 0
		}
	case http.StatusPartialContent:
	default:
		return fmt.Errorf("%w: upstream returned HTTP %d", domain.ErrStreamUnavailable, resp.StatusCode)
	}

	total := resp.ContentLength
	if resp.StatusCode == http.StatusPartialContent {
		if _, _, contentTotal := parseContentRange(resp.Header.Get("Content-Range")); contentTotal > 0 {
			total = contentTotal - offset
		}
	}
	s.mu.Lock()
	if total >= 0 {
		s.total = offset + total
	} else if s.total < 0 {
		s.total = -1
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		s.contentType = ct
	}
	s.mu.Unlock()

	if _, err := s.file.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek stream cache file: %w", err)
	}
	buf := make([]byte, 64*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := s.file.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("write stream cache file: %w", writeErr)
			}
			s.mu.Lock()
			s.received += int64(n)
			s.mu.Unlock()
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read upstream body: %w", readErr)
		}
	}
	c.saveStats(s)
	return nil
}

func (c *Cache) fail(s *cacheSession, err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.failed = err
	s.mu.Unlock()
	c.logger.Warn("Stream cache download failed",
		"provider", s.track.Provider, "external_id", s.track.ExternalID, "error", err)
	c.saveStats(s)
}

// handleStream serves one cached entry, honouring a single "bytes=N-" range.
func (c *Cache) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/stream/")
	c.mu.Lock()
	s := c.sessions[id]
	c.mu.Unlock()
	if s == nil {
		http.Error(w, "stream session not found", http.StatusNotFound)
		return
	}

	start, hasRange, rangeErr := parseRangeHeader(r.Header.Get("Range"))
	if rangeErr != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", s.totalOrZero()))
		http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	// Wait for the upstream handshake so Content-Type/Length are real. Without
	// this the response would have to start before the first byte is known.
	if !s.waitReady(r.Context(), 30*time.Second) {
		if r.Context().Err() != nil {
			return
		}
		http.Error(w, "stream unavailable", http.StatusBadGateway)
		return
	}

	s.mu.Lock()
	total := s.total
	contentType := s.contentType
	failed := s.failed
	s.mu.Unlock()
	if failed != nil {
		http.Error(w, "stream unavailable", http.StatusBadGateway)
		return
	}

	w.Header().Set("Accept-Ranges", "bytes")
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	status := http.StatusOK
	if hasRange && total >= 0 {
		if start >= total {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
			http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, total-1, total))
	}
	if total >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(total-start, 10))
	}
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 64*1024)
	offset := start
	for {
		if r.Context().Err() != nil || s.isClosed() {
			return
		}
		n, readErr := s.readAt(buf, offset)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return
			}
			offset += int64(n)
			if flusher != nil {
				flusher.Flush()
			}
			continue
		}
		if readErr != nil {
			return
		}
		// Nothing available yet: block briefly instead of returning EOF, which
		// the decoder would read as end-of-track.
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			// Download finished or failed; loop once more to drain what landed.
			if s.isComplete() && offset >= s.receivedSnapshot() {
				return
			}
			select {
			case <-time.After(20 * time.Millisecond):
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// parseRangeHeader accepts the single-segment form FFmpeg sends. It returns
// hasRange=false for an absent header so a plain GET is a 200.
func parseRangeHeader(value string) (start int64, hasRange bool, err error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false, nil
	}
	if !strings.HasPrefix(value, "bytes=") {
		return 0, false, errors.New("only bytes ranges are supported")
	}
	spec := strings.TrimSpace(strings.TrimPrefix(value, "bytes="))
	if strings.Contains(spec, ",") {
		return 0, false, errors.New("multiple ranges are not supported")
	}
	first := strings.SplitN(spec, "-", 2)[0]
	if first == "" {
		return 0, false, errors.New("suffix ranges are not supported")
	}
	start, err = strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 {
		return 0, false, errors.New("invalid range start")
	}
	return start, true, nil
}

// parseContentRange reads "bytes 0-99/1000" and returns the start, end and
// total. Missing or "*" totals come back as 0.
func parseContentRange(value string) (start, end, total int64) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, 0
	}
	spec := strings.TrimPrefix(value, "bytes ")
	slash := strings.LastIndex(spec, "/")
	if slash < 0 {
		return 0, 0, 0
	}
	totalPart := spec[slash+1:]
	if totalPart != "*" {
		if parsed, err := strconv.ParseInt(totalPart, 10, 64); err == nil {
			total = parsed
		}
	}
	rangePart := spec[:slash]
	bounds := strings.SplitN(rangePart, "-", 2)
	if len(bounds) == 2 {
		start, _ = strconv.ParseInt(bounds[0], 10, 64)
		end, _ = strconv.ParseInt(bounds[1], 10, 64)
	}
	return start, end, total
}

func (c *Cache) streamQuality(ctx context.Context) string {
	if c.settings == nil {
		return domain.DefaultDownloadQuality
	}
	settings, err := c.settings.GetSettings(ctx)
	if err != nil || settings == nil {
		return domain.DefaultDownloadQuality
	}
	return domain.ResolveDownloadQuality(settings.StreamQuality)
}

func (c *Cache) cacheLimitBytes(ctx context.Context) int64 {
	mb := domain.DefaultStreamCacheMB
	if c.settings != nil {
		if settings, err := c.settings.GetSettings(ctx); err == nil && settings != nil {
			mb = domain.ClampStreamCacheMB(settings.StreamCacheMB)
		}
	}
	return int64(mb) * 1024 * 1024
}

func (c *Cache) stopSession(s *cacheSession) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.playing = false
	s.mu.Unlock()

	if s.cancel != nil {
		s.cancel()
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
	}
	c.saveStats(s)
	c.saveProgress(s)

	s.fileMu.Lock()
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
	s.fileMu.Unlock()
}

func (c *Cache) closeAllSessions() {
	c.mu.Lock()
	sessions := make([]*cacheSession, 0, len(c.sessions))
	for _, s := range c.sessions {
		sessions = append(sessions, s)
	}
	c.mu.Unlock()
	for _, s := range sessions {
		c.stopSession(s)
	}
}

// saveStats persists the byte counters and completeness. A missing row is not
// fatal: the cache is disposable and Open re-registers it next time.
func (c *Cache) saveStats(s *cacheSession) {
	s.mu.Lock()
	total := s.total
	received := s.received
	complete := s.complete
	s.mu.Unlock()
	if total < 0 {
		total = 0
	}
	if err := c.repo.UpdateComplete(context.Background(), s.id, total, received, complete); err != nil {
		c.logger.Warn("Failed to persist stream cache bytes", "cache_key", s.id, "error", err)
	}
}

// saveProgress persists the playback position used by eviction ordering.
func (c *Cache) saveProgress(s *cacheSession) {
	s.mu.Lock()
	progress := s.progress
	s.mu.Unlock()
	if err := c.repo.UpdateProgress(context.Background(), s.id, progress); err != nil {
		c.logger.Warn("Failed to persist stream cache progress", "cache_key", s.id, "error", err)
	}
}

// pinCover marks a stream entry's identity so its cover is evicted last.
func (c *Cache) pinCover(entry *domain.StreamCacheEntry) {
	if entry == nil {
		return
	}
	c.pinCoverIdentity(entry.Provider, entry.ExternalID)
}

func (c *Cache) pinCoverIdentity(provider, externalID string) {
	if c.covers == nil || provider == "" || externalID == "" {
		return
	}
	if err := c.covers.Pin(context.Background(), provider, externalID); err != nil {
		c.logger.Warn("Failed to pin stream cover", "provider", provider, "external_id", externalID, "error", err)
	}
}

func (c *Cache) unpinCover(provider, externalID string) {
	if c.covers == nil || provider == "" || externalID == "" {
		return
	}
	if err := c.covers.Unpin(context.Background(), provider, externalID); err != nil {
		c.logger.Warn("Failed to unpin stream cover", "provider", provider, "external_id", externalID, "error", err)
	}
}

// cleanupLegacyFiles removes the JSON sidecars of the pre-database cache and
// any audio file no row references. Metadata is rebuilt on demand, so dropping
// it is safe; the audio files that remain are still addressable.
func (c *Cache) cleanupLegacyFiles(ctx context.Context) {
	entries, err := c.repo.ListForEviction(ctx)
	if err != nil {
		c.logger.Warn("Failed to list stream cache entries for startup cleanup", "error", err)
		return
	}
	referenced := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		referenced[entry.CacheKey] = struct{}{}
	}
	dirEntries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	for _, dirEntry := range dirEntries {
		if dirEntry.IsDir() {
			continue
		}
		name := dirEntry.Name()
		switch {
		case strings.HasSuffix(name, ".json"), strings.HasSuffix(name, ".json.tmp"):
			_ = os.Remove(filepath.Join(c.dir, name))
		case strings.HasSuffix(name, ".audio"):
			key := strings.TrimSuffix(name, ".audio")
			if _, ok := referenced[key]; !ok {
				_ = os.Remove(filepath.Join(c.dir, name))
			}
		}
	}
}

// enforceLimit deletes whole entries until the directory fits the configured
// limit. It deletes the least-complete first, then the least played. Sessions
// that are still open (playing or paused) are never touched.
func (c *Cache) enforceLimit(ctx context.Context) {
	c.enforceLimitBytes(c.cacheLimitBytes(ctx))
}

func (c *Cache) enforceLimitBytes(limit int64) {
	if limit <= 0 {
		return
	}
	ctx := context.Background()
	entries, err := c.repo.ListForEviction(ctx)
	if err != nil {
		c.logger.Warn("Failed to list stream cache entries for eviction", "error", err)
		return
	}

	type candidate struct {
		entry        *domain.StreamCacheEntry
		size         int64
		completeness float64
		progress     float64
	}

	c.mu.Lock()
	active := make(map[string]struct{}, len(c.sessions))
	for id := range c.sessions {
		active[id] = struct{}{}
	}
	c.mu.Unlock()

	var total int64
	candidates := make([]candidate, 0, len(entries))
	for _, entry := range entries {
		info, statErr := os.Stat(filepath.Join(c.dir, entry.AudioPath))
		if statErr != nil {
			// The row outlived its file (manual delete, failed write): drop it so
			// it cannot reserve a slot or confuse matching.
			if err := c.repo.Delete(ctx, entry.CacheKey); err != nil {
				c.logger.Warn("Failed to drop stream cache row without audio", "cache_key", entry.CacheKey, "error", err)
			}
			c.unpinCover(entry.Provider, entry.ExternalID)
			continue
		}
		total += info.Size()
		candidates = append(candidates, candidate{
			entry:        entry,
			size:         info.Size(),
			completeness: completenessOf(entry),
			progress:     entry.PlaybackProgress,
		})
	}
	if total <= limit {
		return
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].completeness != candidates[j].completeness {
			return candidates[i].completeness < candidates[j].completeness
		}
		return candidates[i].progress < candidates[j].progress
	})

	for _, cand := range candidates {
		if total <= limit {
			return
		}
		if _, inUse := active[cand.entry.CacheKey]; inUse {
			continue
		}
		if cand.entry.PendingPromote {
			continue
		}
		if err := os.Remove(filepath.Join(c.dir, cand.entry.AudioPath)); err == nil {
			total -= cand.size
		}
		if err := c.repo.Delete(ctx, cand.entry.CacheKey); err != nil {
			c.logger.Warn("Failed to delete stream cache row", "cache_key", cand.entry.CacheKey, "error", err)
		}
		c.unpinCover(cand.entry.Provider, cand.entry.ExternalID)
	}
}

func completenessOf(entry *domain.StreamCacheEntry) float64 {
	if entry.Complete {
		return 1
	}
	if entry.TotalBytes <= 0 {
		return 0
	}
	ratio := float64(entry.ReceivedBytes) / float64(entry.TotalBytes)
	if ratio < 0 {
		return 0
	}
	if ratio > 1 {
		return 1
	}
	return ratio
}

// cacheKeyFor is sha256(provider + "/" + external_id) in hex. The slash keeps
// the two inputs unambiguous; the whole digest is used because it is also the
// audio file name.
func cacheKeyFor(provider, externalID string) string {
	sum := sha256.Sum256([]byte(provider + "/" + externalID))
	return hex.EncodeToString(sum[:])
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// --- cacheSession helpers ----------------------------------------------------

func (s *cacheSession) waitReady(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		ready := s.contentType != "" || s.failed != nil || s.complete
		s.mu.Unlock()
		if ready {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-s.done:
			s.mu.Lock()
			ready := s.contentType != "" || s.failed != nil || s.complete
			s.mu.Unlock()
			return ready
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// readAt copies up to len(buf) downloaded bytes starting at offset. It returns
// (0, nil) when the bytes have not arrived yet; the caller decides how to block.
func (s *cacheSession) readAt(buf []byte, offset int64) (int, error) {
	s.mu.Lock()
	available := s.received - offset
	failed := s.failed
	s.mu.Unlock()
	if available <= 0 {
		return 0, nil
	}
	if int64(len(buf)) > available {
		buf = buf[:available]
	}
	s.fileMu.RLock()
	defer s.fileMu.RUnlock()
	if s.file == nil {
		return 0, os.ErrClosed
	}
	n, err := s.file.ReadAt(buf, offset)
	if errors.Is(err, io.EOF) && n > 0 {
		err = nil
	}
	// A concurrent failure is reported only after the landed bytes are drained.
	if err == nil && failed != nil {
		err = failed
	}
	return n, err
}

func (s *cacheSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *cacheSession) isComplete() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.complete
}

func (s *cacheSession) receivedSnapshot() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.received
}

func (s *cacheSession) totalOrZero() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total < 0 {
		return 0
	}
	return s.total
}
