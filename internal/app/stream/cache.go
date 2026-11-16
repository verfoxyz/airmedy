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

	"github.com/google/uuid"
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

	// sessionStopWait bounds how long Close/Stop waits for one download
	// goroutine. A blocked HTTP read is canceled first; the wait is only a
	// guard, so shutdown cannot be held hostage by a stuck goroutine.
	sessionStopWait = 500 * time.Millisecond

	// prefetchAheadSeconds is how far ahead of the playback position the stream
	// download runs. The download pauses at this water mark and resumes when
	// playback advances, so a song is not fetched in full just to start playing.
	// A complete request (promotion or an explicit download) ignores it.
	prefetchAheadSeconds = 45

	// demandWindowBytes is how close the decoder may read to the write edge
	// before the water mark is lifted: a demuxer binary-searching for a seek, or
	// a paused-at-zero decoder, must not be starved by the prefetch limit.
	demandWindowBytes int64 = 2 << 20 // 2 MiB
	// demandResumeBytes re-arms the water mark once the download is this far
	// ahead of the decoder again (hysteresis: no start/stop churn).
	demandResumeBytes int64 = 4 << 20 // 4 MiB

	// seekBufferBytes is the extra lead kept on disk before a stream seek so the
	// reopened decoder's own search never reaches the growing edge.
	seekBufferBytes int64 = 1 << 20 // 1 MiB

	// decoderHeadBytes is the amount the cache-file decoder needs on disk before
	// FFmpeg is pointed at the file; the custom AVIO blocks for the rest.
	decoderHeadBytes int64 = 2 << 20
	// decoderHeadTimeout bounds that wait.
	decoderHeadTimeout = 15 * time.Second
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
	// deferredDiscards holds entries whose audio could not be deleted because a
	// session still owned the file. The cache deletes them as soon as that
	// session closes; the state lives in memory because a restart's startup
	// cleanup already drops caches whose identity is local.
	deferredDiscards map[string]*domain.StreamCacheEntry
	// sessionClosed notifies promotion, which must wait until the decoder no
	// longer owns the file before moving or deleting it.
	sessionClosed []func(provider, externalID string)
	server        *http.Server
	listener      net.Listener
	port          int
	started       bool

	// localIdentity lets Open refuse a row that already became a library file.
	// *download.Service satisfies it; the cache only needs the yes/no answer.
	localIdentity LocalIdentityChecker
	// activeRequest lets the startup sweep keep audio a media request still
	// needs. Optional; without it a local-identity cache is always obsolete.
	activeRequest func(ctx context.Context, provider, externalID string) bool
	// aliases expands the artist key for cross-source matching. Optional.
	aliases domain.ArtistAliasRepository
	// tasks records one row per actual download session for the tasks page.
	// Optional: a cache without it still plays and caches.
	tasks domain.StreamCacheTaskRepository
}

// SettingsProvider is the settings slice the cache needs. *appsettings.Service
// satisfies it; taking the narrow port keeps this package out of appsettings.
type SettingsProvider interface {
	GetSettings(ctx context.Context) (*domain.AppSettings, error)
}

// LocalIdentityChecker answers whether a remote identity now belongs to a
// library file. Wiring happens in the composition root, so this package does not
// import the download service.
type LocalIdentityChecker interface {
	HasLocalIdentity(ctx context.Context, provider, externalID string) (bool, error)
}

// SetArtistAliases installs the alias store used for cross-source matching.
// Optional; without it the literal artist key still matches.
func (c *Cache) SetArtistAliases(aliases domain.ArtistAliasRepository) {
	c.aliases = aliases
}

// SetLocalIdentityChecker installs the guard used by Open and the startup
// sweep. Safe to call before Start; the cache never writes through it.
func (c *Cache) SetLocalIdentityChecker(checker LocalIdentityChecker) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.localIdentity = checker
}

// SetActiveRequestChecker installs the guard that keeps a cache entry a media
// request still needs during the startup sweep.
func (c *Cache) SetActiveRequestChecker(check func(ctx context.Context, provider, externalID string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activeRequest = check
}

// SetTaskHistory installs the stream-cache task log the tasks page reads. Safe
// to call before Start; without it caching is unchanged.
func (c *Cache) SetTaskHistory(tasks domain.StreamCacheTaskRepository) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tasks = tasks
}

// Session is one playback window over a cached stream. ID is derived from the
// track's remote identity, so the same track always maps to the same entry.
// Path/TotalBytes are the local file the decoder reads directly (cache-file
// mode); URL remains for the debug HTTP proxy.
type Session struct {
	ID         string
	URL        string
	Path       string
	TotalBytes int64
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
	quality     string
	complete    bool
	failed      error
	playing     bool
	background  bool
	progress    float64
	lastFlush   time.Time
	closed      bool
	// completeRequested means a promotion or download needs the whole file:
	// the prefetch water mark is ignored and the download runs to EOF.
	completeRequested bool
	// halfPlayed marks the one-time "playback reached half the track" trigger.
	// It is separate from completeRequested because a promotion may have asked
	// for the whole file before the half-way point.
	halfPlayed bool
	// headTarget lifts the prefetch water mark until the decoder's probe head
	// is on disk. 0 means no cache-file decoder is waiting.
	headTarget int64
	// decodeOffset is the decoder's current read position in the cache file,
	// reported by the player. decodeDemand is the hysteresis state that keeps
	// the water mark lifted until the download is demandResumeBytes ahead.
	decodeOffset int64
	decodeDemand bool
	// seekTarget is the byte count a pending stream seek needs on disk; the
	// water mark is ignored until it is reached.
	seekTarget int64
	// progressChanged wakes the download when playback advances, seeks or a
	// complete request arrives. Capacity 1; sends never block.
	progressChanged chan struct{}
	// taskID is the stream_cache_tasks row for this download, empty when the
	// entry was already complete or no task log is installed.
	taskID string

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
		dir:              cfg.StreamCachePath(),
		repo:             repo,
		resolver:         resolver,
		settings:         settings,
		covers:           covers,
		logger:           logger,
		sessions:         make(map[string]*cacheSession),
		deferredDiscards: make(map[string]*domain.StreamCacheEntry),
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
	if c.started {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()
	// Sessions left active by a crash can never finish; close them before new
	// playback starts so the tasks page does not show phantom downloads.
	c.interruptTasks(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
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
	// session is closed or the client disconnects.
	c.closeAllSessions()
	c.interruptTasks(ctx)
	// Close (not Shutdown): active handlers may be parked in waitReady and do
	// not observe a Shutdown; Close drops the connections immediately.
	err := server.Close()
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

	// A promoted row keeps its identity, so a player still holding the old
	// remote DTO must not recreate a cache under it. Refuse before any session,
	// row or file exists; the caller reloads the local row instead.
	c.mu.Lock()
	checker := c.localIdentity
	c.mu.Unlock()
	if checker != nil {
		local, err := checker.HasLocalIdentity(ctx, provider, strings.TrimSpace(track.ExternalID))
		if err != nil {
			return nil, fmt.Errorf("open stream: check local identity: %w", err)
		}
		if local {
			return nil, fmt.Errorf("%w: %s/%s", domain.ErrStreamAlreadyLocal, provider, track.ExternalID)
		}
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

	s, err := c.startSession(ctx, entry, track, false)
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
	return &Session{
		ID:         id,
		URL:        c.localURL() + "/stream/" + id,
		Path:       s.audioPath,
		TotalBytes: s.totalOrZero(),
	}, nil
}

// startSession registers a cache session for entry and starts its download when
// bytes are missing. background marks a session opened only to complete the
// cache, which is detached when the download ends instead of waiting for Close.
func (c *Cache) startSession(
	ctx context.Context, entry *domain.StreamCacheEntry, track *domain.TrackDTO, background bool,
) (*cacheSession, error) {
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
		quality:    entry.Quality,
		playing:    !background,
		background: background,
		// A background session exists only to complete the entry, so it must
		// never stop at the prefetch water mark.
		completeRequested: background,
		lastFlush:         time.Now(),
		progressChanged:   make(chan struct{}, 1),
		done:              make(chan struct{}),
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
	c.startTask(ctx, s, entry)
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
		return &Session{
			ID:         key,
			URL:        base + "/stream/" + key,
			Path:       existing.audioPath,
			TotalBytes: existing.totalOrZero(),
		}, nil
	}
	// A failed download must be retried from scratch rather than reusing the
	// dead session (its upstream URL is likely expired).
	delete(c.sessions, key)
	c.mu.Unlock()
	c.stopSession(existing)
	c.notifySessionClosed(existing)
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
	artistKey := domain.ArtistSetKey(artistNames(track))

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
	artistKey := domain.ArtistSetKey(artistNames(track))
	if titleKey == "" || artistKey == "" {
		return nil, nil
	}
	match, err := c.matchCachedByTitleArtist(ctx, titleKey, artistKey, track.Duration)
	if err != nil {
		return nil, err
	}
	return match, nil
}

// matchCachedByTitleArtist tries the literal artist key first, then its enabled
// aliases (one hop), so a cache filled as "かめりあ" is reused for "Camellia".
func (c *Cache) matchCachedByTitleArtist(
	ctx context.Context, titleKey, artistKey string, duration int,
) (*domain.StreamCacheEntry, error) {
	keys := []string{artistKey}
	if c.aliases != nil {
		expanded, err := c.aliases.ExpandArtists(ctx, []string{artistKey})
		if err != nil {
			c.logger.Debug("stream cache: cannot expand artist aliases",
				"artist_key", artistKey, "error", err)
		} else {
			for _, key := range expanded {
				if key != artistKey {
					keys = append(keys, key)
				}
			}
		}
	}
	for _, key := range keys {
		match, err := c.repo.MatchByTitleArtist(ctx, titleKey, key,
			int64(duration)*1000, matchDurationToleranceMs)
		if err != nil {
			return nil, fmt.Errorf("match stream cache: %w", err)
		}
		if match != nil {
			return match, nil
		}
	}
	return nil, nil
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

// artistNames is the credit list used for matching: the resolved artists, or the
// raw tag value split on the multi-value separator when relations were not
// loaded.
func artistNames(track *domain.TrackDTO) []string {
	names := make([]string, 0, len(track.Artists))
	for _, artist := range track.Artists {
		if artist != nil && strings.TrimSpace(artist.Name) != "" {
			names = append(names, strings.TrimSpace(artist.Name))
		}
	}
	if len(names) > 0 {
		return names
	}
	raw := strings.TrimSpace(track.RawArtistNames)
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ";")
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
	c.notifySessionClosed(s)
}

// cachedBytesOnDisk is the bytes a range request can read right now: the
// session counter capped by the file's real length, so metadata that survived a
// truncated file cannot make the proxy promise bytes it does not hold.
func (s *cacheSession) cachedBytesOnDisk() int64 {
	s.mu.Lock()
	received := s.received
	s.mu.Unlock()
	s.fileMu.RLock()
	defer s.fileMu.RUnlock()
	if s.file == nil {
		return 0
	}
	if info, err := s.file.Stat(); err == nil && info.Size() < received {
		return info.Size()
	}
	return received
}

// BufferedBytes is the bytes a decoder can read right now; a growing value
// means the download is still buffering.
func (c *Cache) BufferedBytes(session *Session) int64 {
	if session == nil {
		return 0
	}
	c.mu.Lock()
	s := c.sessions[session.ID]
	c.mu.Unlock()
	if s == nil {
		return 0
	}
	return s.cachedBytesOnDisk()
}

// SessionError reports why the download failed, if it did.
func (c *Cache) SessionError(session *Session) error {
	if session == nil {
		return nil
	}
	c.mu.Lock()
	s := c.sessions[session.ID]
	c.mu.Unlock()
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

// WaitComplete blocks until the download finishes. A failed or closed session
// returns its reason instead, so an unknown-length decoder never sees EOF early.
func (c *Cache) WaitComplete(ctx context.Context, session *Session) error {
	if session == nil {
		return nil
	}
	c.mu.Lock()
	s := c.sessions[session.ID]
	c.mu.Unlock()
	if s == nil {
		return fmt.Errorf("%w: stream session %s is not active",
			domain.ErrStreamUnavailable, session.ID)
	}
	for {
		s.mu.Lock()
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
			return fmt.Errorf("%w: stream session %s closed before completion",
				domain.ErrStreamUnavailable, session.ID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// WaitHead blocks until the cache file has enough bytes for the decoder to
// probe (min(2 MiB, total)), or reports a failure. The FFmpeg AVIO then blocks
// per read, so the head wait only keeps the initial probe from stalling.
func (c *Cache) WaitHead(ctx context.Context, session *Session) error {
	if session == nil {
		return nil
	}
	c.mu.Lock()
	s := c.sessions[session.ID]
	c.mu.Unlock()
	if s == nil {
		return fmt.Errorf("%w: stream session %s is not active",
			domain.ErrStreamUnavailable, session.ID)
	}

	deadline := time.Now().Add(decoderHeadTimeout)
	for {
		s.mu.Lock()
		total := s.total
		failed := s.failed
		closed := s.closed
		complete := s.complete
		target := decoderHeadBytes
		if total > 0 && total < target {
			target = total
		}
		lifted := false
		if s.headTarget < target {
			// Let the download run past the playback water mark until the head
			// exists; a paused-at-zero session would otherwise never get there.
			s.headTarget = target
			lifted = true
		}
		s.mu.Unlock()
		if lifted {
			signalStreamProgress(s)
		}

		received := s.cachedBytesOnDisk()
		switch {
		case complete || (target > 0 && received >= target):
			return nil
		case failed != nil:
			return failed
		case closed:
			return fmt.Errorf("%w: stream session %s closed before the head was buffered",
				domain.ErrStreamUnavailable, session.ID)
		case time.Now().After(deadline):
			return fmt.Errorf("%w: stream session %s has only %d/%d head bytes",
				domain.ErrStreamUnavailable, session.ID, received, target)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// WaitReady blocks until the session's proxy knows the upstream response, or
// reports why it cannot. A session opened from partially cached bytes can
// satisfy the prebuffer without having contacted the source yet; pointing the
// decoder at that URL makes the load fail immediately.
func (c *Cache) WaitReady(ctx context.Context, session *Session) error {
	if session == nil {
		return nil
	}
	c.mu.Lock()
	s := c.sessions[session.ID]
	c.mu.Unlock()
	if s == nil {
		return fmt.Errorf("%w: stream session %s is not active", domain.ErrStreamUnavailable, session.ID)
	}
	if !s.waitReady(ctx, 30*time.Second) {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.mu.Lock()
		failed := s.failed
		s.mu.Unlock()
		if failed != nil {
			return failed
		}
		return fmt.Errorf("%w: stream session %s has no upstream response",
			domain.ErrStreamUnavailable, session.ID)
	}
	s.mu.Lock()
	failed := s.failed
	s.mu.Unlock()
	return failed
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
	changed := seconds != s.progress
	s.progress = seconds
	// Reaching the middle of the track is the signal to cache the rest: the
	// user is committed to the song, so the water mark must stop limiting the
	// download. A promotion may already have requested completion; the half-way
	// marker is still recorded so a later session does not re-trigger it.
	halfTriggered := false
	if !s.halfPlayed && !s.complete && s.track != nil && s.track.Duration > 0 &&
		seconds >= float64(s.track.Duration)/2 {
		s.halfPlayed = true
		if !s.completeRequested {
			s.completeRequested = true
			halfTriggered = true
		}
	}
	flush := time.Since(s.lastFlush) >= progressFlushInterval
	if flush {
		s.lastFlush = time.Now()
	}
	s.mu.Unlock()
	if changed || halfTriggered {
		// The download may be paused at the prefetch water mark; a seek or a
		// tick forward is what lets it continue.
		signalStreamProgress(s)
	}
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

// SetDecodeOffset records the decoder's current read position in the cache
// file. A decoder parked near the write edge (a seek's binary search, or the
// initial probe) pulls the download past the playback water mark; without this
// the demuxer can be starved at the very edge the prefetch limit created.
func (c *Cache) SetDecodeOffset(session *Session, offset int64) {
	if session == nil || offset < 0 {
		return
	}
	c.mu.Lock()
	s := c.sessions[session.ID]
	c.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	changed := s.decodeOffset != offset
	s.decodeOffset = offset
	s.mu.Unlock()
	if changed {
		// Wake a download paused at the water mark immediately instead of
		// waiting for waitForPrefetch's one-second fallback.
		signalStreamProgress(s)
	}
}

// EnsureSeekable blocks until about seconds of audio plus a 1 MiB margin are on
// disk. It lifts the prefetch water mark for that window and does not touch the
// decoder: the caller reopens the decoder afterwards, which then timestamp-seeks
// inside downloaded bytes instead of stalling at the growing edge.
func (c *Cache) EnsureSeekable(ctx context.Context, session *Session, seconds float64) error {
	if session == nil {
		return nil
	}
	if seconds < 0 {
		seconds = 0
	}
	c.mu.Lock()
	s := c.sessions[session.ID]
	c.mu.Unlock()
	if s == nil {
		return fmt.Errorf("%w: stream session %s is not active", domain.ErrStreamUnavailable, session.ID)
	}

	s.mu.Lock()
	need := int64(seconds*s.bytesPerSecondLocked()) + seekBufferBytes
	if s.total > 0 && need > s.total {
		need = s.total
	}
	if s.complete || s.received >= need {
		s.mu.Unlock()
		return nil
	}
	if need > s.seekTarget {
		s.seekTarget = need
	}
	s.mu.Unlock()
	// The download may be parked at the water mark: wake it for the seek window.
	signalStreamProgress(s)

	for {
		s.mu.Lock()
		complete := s.complete
		received := s.received
		failed := s.failed
		closed := s.closed
		total := s.total
		s.mu.Unlock()

		switch {
		case complete || received >= need:
			return nil
		case failed != nil:
			return failed
		case closed:
			return fmt.Errorf("%w: stream session %s closed while buffering for a seek",
				domain.ErrStreamUnavailable, session.ID)
		case total > 0 && received >= total:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
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
		requested := existing.completeRequested
		existing.completeRequested = true
		existing.mu.Unlock()
		if !requested {
			// Waiving the prefetch water mark must wake a download paused at it.
			signalStreamProgress(existing)
		}
		if failed {
			// Retry from scratch: the upstream URL of a dead attempt is expired.
			c.mu.Lock()
			if c.sessions[entry.CacheKey] == existing {
				delete(c.sessions, entry.CacheKey)
			}
			c.mu.Unlock()
			c.stopSession(existing)
			c.notifySessionClosed(existing)
			existing = nil
		}
	}

	if existing == nil {
		if c.entryCompleteOnDisk(entry) {
			done := make(chan error, 1)
			close(done)
			return done, nil
		}
		existing, err = c.startSession(ctx, entry, track, true)
		if err != nil {
			return nil, err
		}
	}
	return c.watchCompletion(existing), nil
}

// OnSessionClosed registers a callback fired after a session has been removed
// from the cache. Promotion uses it to retry as soon as playback releases the
// file instead of waiting for its next periodic sweep.
func (c *Cache) OnSessionClosed(fn func(provider, externalID string)) {
	if fn == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionClosed = append(c.sessionClosed, fn)
}

// AllEntries lists every cached entry, least recently used first. The cleanup
// uses it to find caches whose identity already became a library file.
func (c *Cache) AllEntries(ctx context.Context) ([]*domain.StreamCacheEntry, error) {
	return c.repo.ListForEviction(ctx)
}

// OrphanFiles lists .audio files in the cache directory that no entry
// references. Deleting them frees disk without touching any row.
func (c *Cache) OrphanFiles(ctx context.Context) ([]domain.StreamCacheOrphan, error) {
	entries, err := c.repo.ListForEviction(ctx)
	if err != nil {
		return nil, fmt.Errorf("list stream cache entries: %w", err)
	}
	known := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry != nil && entry.AudioPath != "" {
			known[entry.AudioPath] = struct{}{}
		}
	}

	dirEntries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil, fmt.Errorf("read stream cache directory: %w", err)
	}
	orphans := make([]domain.StreamCacheOrphan, 0)
	for _, dirEntry := range dirEntries {
		name := dirEntry.Name()
		if dirEntry.IsDir() || !strings.HasSuffix(name, ".audio") {
			continue
		}
		if _, ok := known[name]; ok {
			continue
		}
		size := int64(0)
		if info, infoErr := dirEntry.Info(); infoErr == nil {
			size = info.Size()
		}
		orphans = append(orphans, domain.StreamCacheOrphan{Name: name, Bytes: size})
	}
	return orphans, nil
}

// DeleteFile removes one cache file by its flat name. The name must be a plain
// file inside the cache directory, so a caller cannot delete outside it.
func (c *Cache) DeleteFile(name string) error {
	if name == "" || filepath.Base(name) != name || !strings.HasSuffix(name, ".audio") {
		return fmt.Errorf("stream cache: invalid file name %q", name)
	}
	if err := os.Remove(filepath.Join(c.dir, name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete stream cache file %s: %w", name, err)
	}
	return nil
}

// HasActiveSession reports whether a playback or background session still holds
// the entry. Promotion moves the file, so it must wait for this to be false.
func (c *Cache) HasActiveSession(provider, externalID string) bool {
	provider = domain.NormalizeProvider(provider)
	externalID = strings.TrimSpace(externalID)
	if provider == "" || externalID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sessions {
		if s.provider == provider && s.externalID == externalID {
			return true
		}
	}
	return false
}

// HasPlayingSession reports whether a healthy playback session (not a
// background completion) owns the entry. Only then can a request finish the
// partial cache: with no player attached, a normal download reports failures
// and retries with the queue's backoff.
func (c *Cache) HasPlayingSession(provider, externalID string) bool {
	provider = domain.NormalizeProvider(provider)
	externalID = strings.TrimSpace(externalID)
	if provider == "" || externalID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sessions {
		if s.provider != provider || s.externalID != externalID {
			continue
		}
		s.mu.Lock()
		playing := s.playing && !s.background && s.failed == nil && !s.closed
		s.mu.Unlock()
		if playing {
			return true
		}
	}
	return false
}

// notifySessionClosed wakes the observers after the session left the map. It
// runs without the cache lock: a listener must not be able to deadlock playback.
// A discard requested while this session owned the file runs here, now that the
// audio is released.
func (c *Cache) notifySessionClosed(s *cacheSession) {
	if s == nil {
		return
	}
	c.mu.Lock()
	entry, deferred := c.deferredDiscards[s.id]
	if deferred {
		delete(c.deferredDiscards, s.id)
	}
	listeners := append([]func(string, string){}, c.sessionClosed...)
	c.mu.Unlock()
	if deferred && entry != nil {
		c.removeEntry(entry)
	}
	for _, fn := range listeners {
		fn(s.provider, s.externalID)
	}
}

// removeEntry deletes one cache row and its audio file. It is the shared tail of
// DiscardEntry and the deferred discard that runs when a session closes.
func (c *Cache) removeEntry(entry *domain.StreamCacheEntry) {
	if entry == nil {
		return
	}
	if err := os.Remove(filepath.Join(c.dir, entry.AudioPath)); err != nil && !os.IsNotExist(err) {
		c.logger.Warn("Failed to delete stream cache audio", "cache_key", entry.CacheKey, "error", err)
	}
	if err := c.repo.Delete(context.Background(), entry.CacheKey); err != nil {
		c.logger.Warn("Failed to delete stream cache row", "cache_key", entry.CacheKey, "error", err)
		return
	}
	c.unpinCover(entry.Provider, entry.ExternalID)
}

// entryFor resolves a track's entry through the same identity-then-cross-source
// path playback uses.
func (c *Cache) entryFor(ctx context.Context, track *domain.TrackDTO) (*domain.StreamCacheEntry, error) {
	if track == nil {
		return nil, nil
	}
	provider := domain.NormalizeProvider(track.Provider)
	if provider == "" || strings.TrimSpace(track.ExternalID) == "" {
		return nil, nil
	}
	return c.lookupEntry(ctx, provider, track)
}

// DiscardEntry deletes a cache entry and its audio.
//
// A session that is still playing defers the deletion — removing the file under
// the decoder breaks playback — and returns domain.ErrStreamDiscardDeferred; the
// cache remembers the entry and deletes it as soon as that session closes. A
// promoted entry whose file is already gone is a success, not an error: the move
// is the only thing that mattered.
func (c *Cache) DiscardEntry(ctx context.Context, track *domain.TrackDTO) error {
	entry, err := c.entryFor(ctx, track)
	if err != nil || entry == nil {
		return err
	}

	c.mu.Lock()
	_, active := c.sessions[entry.CacheKey]
	if active {
		c.deferredDiscards[entry.CacheKey] = entry
		c.mu.Unlock()
		return domain.ErrStreamDiscardDeferred
	}
	delete(c.deferredDiscards, entry.CacheKey)
	c.mu.Unlock()

	c.removeEntry(entry)
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
			c.notifySessionClosed(s)
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

// prefetchTarget is 1 MiB, reduced to a three-second lead when the total size
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

// prefetchLimitReachedLocked reports whether the download has run far enough
// ahead of playback to pause. A complete request (promotion or download) and a
// finished entry never pause.
func (s *cacheSession) prefetchLimitReachedLocked() bool {
	if s.completeRequested || s.complete || s.closed {
		return false
	}
	// The cache-file decoder cannot probe until its head is on disk; ignore the
	// water mark for that window even when playback is still at zero.
	if s.headTarget > 0 && s.received < s.headTarget {
		return false
	}
	// A pending seek needs its target bytes regardless of the playback water
	// mark; the download may continue until they are on disk.
	if s.seekTarget > 0 && s.received < s.seekTarget {
		return false
	}
	// Decoder demand beats the water mark: if the reader is within
	// demandWindowBytes of the write edge, keep downloading. The lifted state is
	// sticky until the download is demandResumeBytes ahead again, so it cannot
	// flap on every poll.
	if s.decodeDemand {
		if s.received-s.decodeOffset > demandResumeBytes {
			s.decodeDemand = false
		}
	} else if s.decodeOffset > 0 && s.received-s.decodeOffset < demandWindowBytes {
		s.decodeDemand = true
	}
	if s.decodeDemand {
		return false
	}
	bytesPerSecond := s.bytesPerSecondLocked()
	ahead := float64(s.received) - s.progress*bytesPerSecond
	return ahead >= float64(prefetchAheadSeconds)*bytesPerSecond
}

// bytesPerSecondLocked derives the streaming bitrate. The real size and
// duration win; otherwise the resolved tier is used, with 320k as the fallback
// for an unlabelled entry.
func (s *cacheSession) bytesPerSecondLocked() float64 {
	duration := 0
	if s.track != nil {
		duration = s.track.Duration
	}
	if s.total > 0 && duration > 0 {
		return float64(s.total) / float64(duration)
	}
	return streamBytesPerSecond(s.quality)
}

// streamBytesPerSecond is the rough byte rate of a quality tier, used only when
// the upstream did not report both a size and a duration.
func streamBytesPerSecond(quality string) float64 {
	switch quality {
	case domain.QualityFlac:
		return 150 * 1024
	case domain.Quality128k:
		return 16 * 1024
	case domain.Quality320k:
		return 40 * 1024
	}
	// Unknown tiers are estimated as 320k, the default stream quality.
	return 40 * 1024
}

// signalStreamProgress wakes a download paused at the prefetch water mark. It
// never blocks: a pending wake already covers this change.
func signalStreamProgress(s *cacheSession) {
	if s == nil || s.progressChanged == nil {
		return
	}
	select {
	case s.progressChanged <- struct{}{}:
	default:
	}
}

// waitForPrefetch blocks until playback advances past the water mark, the entry
// is completed, or the session ends. It re-checks once a second so a stalled
// position cannot hide a seek that was not reported as a change.
func (c *Cache) waitForPrefetch(ctx context.Context, s *cacheSession) bool {
	for {
		s.mu.Lock()
		paused := s.prefetchLimitReachedLocked()
		s.mu.Unlock()
		if !paused {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-s.progressChanged:
		case <-time.After(time.Second):
		}
	}
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
	c.notifySessionClosed(s)
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

	requested := c.streamQuality(ctx)
	var (
		ref    domain.StreamRef
		actual string
		err    error
	)
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return
		}
		// The resolver walks down from the configured tier when a source has no
		// link at that quality; actual is the tier that worked.
		ref, actual, err = domain.ResolveWithQualityFallback(ctx, c.resolver, s.track, requested)
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
	c.recordActualQuality(s, requested, actual)

	for attempt := 0; attempt < 3; attempt++ {
		err = c.fetch(ctx, s, ref)
		if err == nil {
			s.mu.Lock()
			s.complete = true
			s.failed = nil
			s.mu.Unlock()
			c.finishTask(s)
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
			paused := s.prefetchLimitReachedLocked()
			s.mu.Unlock()
			if paused {
				// Stop reading here rather than buffering the whole song: the
				// unread tail stays upstream until playback advances.
				c.saveStats(s)
				if !c.waitForPrefetch(ctx, s) {
					return ctx.Err()
				}
			}
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
	c.failTask(s, err)
	c.saveStats(s)
}

// handleStream serves one cached entry. The single-segment Range forms FFmpeg
// sends (bytes=N-M, bytes=N-, bytes=-N) are honoured; bytes that have not
// arrived yet make the handler wait instead of returning EOF.
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

	rangeHeader := r.Header.Get("Range")
	spec, hasRange, rangeErr := parseRangeHeader(rangeHeader)
	status := http.StatusOK
	contentRange := ""
	var written int64
	startedAt := time.Now()
	firstByteMs := int64(-1)
	// miniaudio only surfaces "ma_player_load failed: -10", so the proxy side
	// of a failed load must be diagnosable from its own log: the requested
	// Range, how long the response took to start, the status returned and the
	// bytes actually written. Cached-range requests are normal traffic, so the
	// line is Info; a failed response is Warn.
	defer func() {
		s.mu.Lock()
		received, total := s.received, s.total
		s.mu.Unlock()
		level := slog.LevelInfo
		if status >= http.StatusBadRequest {
			level = slog.LevelWarn
		}
		c.logger.Log(context.Background(), level, "Stream proxy response",
			"id", id, "method", r.Method, "range", rangeHeader,
			"status", status, "content_range", contentRange, "written", written,
			"first_byte_ms", firstByteMs, "elapsed_ms", time.Since(startedAt).Milliseconds(),
			"received_bytes", received, "total_bytes", total)
	}()

	if rangeErr != nil {
		contentRange = fmt.Sprintf("bytes */%d", s.totalOrZero())
		w.Header().Set("Content-Range", contentRange)
		status = http.StatusRequestedRangeNotSatisfiable
		http.Error(w, "invalid range", status)
		return
	}

	s.mu.Lock()
	total := s.total
	contentType := s.contentType
	s.mu.Unlock()

	// A range already on disk is served without touching the upstream. A seek
	// inside the buffered part used to wait for a handshake that could fail or
	// expire, turning a perfectly playable range into a 502.
	if start, end, ok := cachedRequestRange(spec, hasRange, total, s.cachedBytesOnDisk()); ok {
		status = http.StatusOK
		if hasRange {
			status = http.StatusPartialContent
		}
		w.Header().Set("Accept-Ranges", "bytes")
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		if hasRange {
			totalLabel := "*"
			if total > 0 {
				totalLabel = strconv.FormatInt(total, 10)
			}
			contentRange = fmt.Sprintf("bytes %d-%d/%s", start, end, totalLabel)
			w.Header().Set("Content-Range", contentRange)
		}
		if end >= start {
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		}
		w.WriteHeader(status)
		firstByteMs = time.Since(startedAt).Milliseconds()
		if r.Method == http.MethodHead {
			return
		}
		written = serveFromSession(w, r, s, start, end+1)
		return
	}

	// Wait for the upstream handshake so Content-Type/Length are real. Without
	// this the response would have to start before the first byte is known.
	if !s.waitReady(r.Context(), 30*time.Second) {
		if r.Context().Err() != nil {
			status = 0
			return
		}
		s.mu.Lock()
		failed := s.failed
		received, knownTotal := s.received, s.total
		s.mu.Unlock()
		c.logger.Warn("Stream proxy: session not ready",
			"id", id, "range", rangeHeader, "received_bytes", received,
			"total_bytes", knownTotal, "failed", failed)
		status = http.StatusBadGateway
		http.Error(w, "stream unavailable", status)
		return
	}

	s.mu.Lock()
	total = s.total
	contentType = s.contentType
	failed := s.failed
	s.mu.Unlock()
	if failed != nil {
		status = http.StatusBadGateway
		http.Error(w, "stream unavailable", status)
		return
	}

	w.Header().Set("Accept-Ranges", "bytes")
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}

	start := int64(0)
	end := int64(-1) // inclusive; -1 means "to the end"
	if hasRange && total >= 0 {
		var ok bool
		start, end, ok = spec.resolve(total)
		if !ok {
			contentRange = fmt.Sprintf("bytes */%d", total)
			w.Header().Set("Content-Range", contentRange)
			status = http.StatusRequestedRangeNotSatisfiable
			http.Error(w, "range not satisfiable", status)
			return
		}
		status = http.StatusPartialContent
		contentRange = fmt.Sprintf("bytes %d-%d/%d", start, end, total)
		w.Header().Set("Content-Range", contentRange)
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	} else if total >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
	}
	w.WriteHeader(status)
	firstByteMs = time.Since(startedAt).Milliseconds()
	if r.Method == http.MethodHead {
		return
	}

	limit := int64(-1) // exclusive upper bound; -1 means "until the session ends"
	if status == http.StatusPartialContent {
		limit = end + 1
	}
	written = serveFromSession(w, r, s, start, limit)
}

// cachedRequestRange resolves the requested range against the bytes on disk.
// ok means the whole range can be served without an upstream response.
func cachedRequestRange(
	spec byteRange, hasRange bool, total, cached int64,
) (start, end int64, ok bool) {
	if cached <= 0 {
		return 0, 0, false
	}
	if !hasRange {
		if total > 0 && total <= cached {
			return 0, total - 1, true
		}
		return 0, 0, false
	}
	if start, end, resolved := spec.resolve(total); resolved {
		return start, end, end+1 <= cached
	}
	// An explicit end does not need the total: the client named it.
	if spec.suffixLen == 0 && spec.end >= 0 && spec.end >= spec.start {
		return spec.start, spec.end, spec.end+1 <= cached
	}
	return 0, 0, false
}

// serveFromSession writes bytes from offset. limit is exclusive; a value <= 0
// means "until the session ends". It blocks while the download catches up, so
// the decoder never reads a short body as end-of-track.
func serveFromSession(
	w http.ResponseWriter, r *http.Request, s *cacheSession, offset, limit int64,
) int64 {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 64*1024)
	bounded := limit > 0
	var written int64
	for {
		if r.Context().Err() != nil || s.isClosed() {
			return written
		}
		if bounded && offset >= limit {
			return written
		}
		readBuf := buf
		if bounded && limit-offset < int64(len(readBuf)) {
			readBuf = readBuf[:int(limit-offset)]
		}
		n, readErr := s.readAt(readBuf, offset)
		if n > 0 {
			if _, err := w.Write(readBuf[:n]); err != nil {
				return written
			}
			written += int64(n)
			offset += int64(n)
			if flusher != nil {
				flusher.Flush()
			}
			continue
		}
		if readErr != nil {
			return written
		}
		// Nothing available yet: block briefly instead of returning EOF, which
		// the decoder would read as end-of-track.
		select {
		case <-r.Context().Done():
			return written
		case <-s.done:
			// Download finished or failed; loop once more to drain what landed.
			if s.isComplete() && offset >= s.receivedSnapshot() {
				return written
			}
			select {
			case <-time.After(20 * time.Millisecond):
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// byteRange is a parsed Range header. Exactly one of suffixLen (bytes=-N) or
// start/end (bytes=N-M, bytes=N-) is meaningful.
type byteRange struct {
	start     int64
	end       int64 // -1 when the client left the upper bound open
	suffixLen int64
}

// resolve maps the request onto a body of total bytes. ok is false when the
// range cannot be served, which must produce a 416.
func (r byteRange) resolve(total int64) (start, end int64, ok bool) {
	if total <= 0 {
		return 0, 0, false
	}
	if r.suffixLen > 0 {
		n := r.suffixLen
		if n > total {
			n = total
		}
		return total - n, total - 1, true
	}
	if r.start >= total {
		return 0, 0, false
	}
	end = total - 1
	if r.end >= 0 && r.end < end {
		end = r.end
	}
	return r.start, end, true
}

// parseRangeHeader accepts the single-segment forms FFmpeg sends. It returns
// hasRange=false for an absent header so a plain GET is a 200.
func parseRangeHeader(value string) (byteRange, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return byteRange{}, false, nil
	}
	if !strings.HasPrefix(value, "bytes=") {
		return byteRange{}, false, errors.New("only bytes ranges are supported")
	}
	spec := strings.TrimSpace(strings.TrimPrefix(value, "bytes="))
	if spec == "" {
		return byteRange{}, false, errors.New("empty range")
	}
	if strings.Contains(spec, ",") {
		return byteRange{}, false, errors.New("multiple ranges are not supported")
	}
	bounds := strings.SplitN(spec, "-", 2)
	if len(bounds) != 2 {
		return byteRange{}, false, errors.New("invalid range")
	}
	first := strings.TrimSpace(bounds[0])
	last := strings.TrimSpace(bounds[1])
	if first == "" {
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 {
			return byteRange{}, false, errors.New("invalid suffix range")
		}
		return byteRange{suffixLen: n}, true, nil
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 {
		return byteRange{}, false, errors.New("invalid range start")
	}
	if last == "" {
		return byteRange{start: start, end: -1}, true, nil
	}
	end, err := strconv.ParseInt(last, 10, 64)
	if err != nil || end < start {
		return byteRange{}, false, errors.New("invalid range end")
	}
	return byteRange{start: start, end: end}, true, nil
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
	case <-time.After(sessionStopWait):
		// The goroutine is blocked in a call that ignored cancellation. Close
		// the file anyway: a later write fails instead of holding exit open.
		c.logger.Warn("Stream session did not stop in time",
			"id", s.id, "wait", sessionStopWait)
	}
	// A download cut short by Close/Stop never reaches runSession's failure
	// path; close its task row here. It was stopped on purpose (track switch,
	// shutdown), so it is recorded as canceled, never as an error.
	s.mu.Lock()
	complete := s.complete
	failed := s.failed != nil
	s.mu.Unlock()
	if !complete && !failed {
		c.cancelTask(s)
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
	// Close in parallel: each wait is bounded, but a sequential loop over many
	// sessions would multiply the bound into the exit time.
	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func(session *cacheSession) {
			defer wg.Done()
			c.stopSession(session)
		}(s)
	}
	wg.Wait()
}

// recordActualQuality persists the tier that actually resolved when it differs
// from the configured one. The cache row is what promotion and eviction compare
// against, so leaving the requested tier there would make a 320k download look
// like a flac one.
func (c *Cache) recordActualQuality(s *cacheSession, requested, actual string) {
	if actual == "" || actual == requested {
		return
	}
	s.mu.Lock()
	s.quality = actual
	s.mu.Unlock()
	c.logger.Info("Stream quality fallback",
		"cache_key", s.id, "provider", s.provider, "external_id", s.externalID,
		"requested", requested, "actual", actual)
	if err := c.repo.UpdateQuality(context.Background(), s.id, actual); err != nil {
		c.logger.Warn("Failed to persist stream cache quality", "cache_key", s.id, "error", err)
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

// taskRepo reads the optional task log; the setter may run at any time.
func (c *Cache) taskRepo() domain.StreamCacheTaskRepository {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tasks
}

// startTask records one actual download session. A cache hit (complete entry)
// returns before this runs, so replays of finished caches add no rows.
func (c *Cache) startTask(ctx context.Context, s *cacheSession, entry *domain.StreamCacheEntry) {
	repo := c.taskRepo()
	if repo == nil {
		return
	}
	total := s.total
	if total < 0 {
		total = 0
	}
	// Prefer the track being played: a cross-source hit reuses an entry whose
	// stored title came from the other provider's metadata.
	title := strings.TrimSpace(s.track.Title)
	if title == "" {
		title = entry.Title
	}
	artist := primaryArtist(s.track)
	if artist == "" {
		artist = entry.Artist
	}
	task := &domain.StreamCacheTask{
		ID:         uuid.NewString(),
		Provider:   entry.Provider,
		ExternalID: entry.ExternalID,
		Title:      title,
		Artist:     artist,
		Quality:    entry.Quality,
		State:      domain.StreamCacheTaskStateActive,
		BytesTotal: total,
		BytesDone:  s.received,
		StartedAt:  time.Now(),
	}
	if err := repo.Start(ctx, task); err != nil {
		c.logger.Warn("Failed to record a stream cache task",
			"provider", entry.Provider, "external_id", entry.ExternalID, "error", err)
		return
	}
	s.mu.Lock()
	s.taskID = task.ID
	s.mu.Unlock()
}

// finishTask closes the session's task row with its final byte counters.
func (c *Cache) finishTask(s *cacheSession) {
	id, total, done := s.taskSnapshot()
	if id == "" {
		return
	}
	repo := c.taskRepo()
	if repo == nil {
		return
	}
	if total < 0 {
		total = 0
	}
	if err := repo.Finish(context.Background(), id, total, done, time.Now()); err != nil {
		c.logger.Warn("Failed to finish a stream cache task", "task_id", id, "error", err)
	}
}

// cancelTask closes the session's row as canceled (an intentional stop).
func (c *Cache) cancelTask(s *cacheSession) {
	id, total, done := s.taskSnapshot()
	if id == "" {
		return
	}
	repo := c.taskRepo()
	if repo == nil {
		return
	}
	if total < 0 {
		total = 0
	}
	if err := repo.Cancel(context.Background(), id, total, done, time.Now()); err != nil {
		c.logger.Warn("Failed to cancel a stream cache task", "task_id", id, "error", err)
	}
}

// failTask closes the session's row as failed. A session without a row (cache
// hit or no task log) is a no-op.
func (c *Cache) failTask(s *cacheSession, cause error) {
	id, total, done := s.taskSnapshot()
	if id == "" || cause == nil {
		return
	}
	repo := c.taskRepo()
	if repo == nil {
		return
	}
	if total < 0 {
		total = 0
	}
	if err := repo.Fail(context.Background(), id, cause.Error(), total, done, time.Now()); err != nil {
		c.logger.Warn("Failed to fail a stream cache task", "task_id", id, "error", err)
	}
}

// interruptTasks closes active rows left behind by a shutdown or a crash.
func (c *Cache) interruptTasks(ctx context.Context) {
	repo := c.taskRepo()
	if repo == nil {
		return
	}
	changed, err := repo.MarkActiveInterrupted(ctx, "interrupted", time.Now())
	if err != nil {
		c.logger.Warn("Failed to close interrupted stream cache tasks", "error", err)
		return
	}
	if changed > 0 {
		c.logger.Info("Stream cache: closed interrupted tasks", "count", changed)
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

// cleanupLegacyFiles removes the JSON sidecars of the pre-database cache, any
// audio file no row references, and rows whose identity already became a
// library file. Metadata is rebuilt on demand, so dropping it is safe; the
// audio files that remain are still addressable. A cache a media request still
// needs is kept so a pending promotion does not turn into a download.
func (c *Cache) cleanupLegacyFiles(ctx context.Context) {
	entries, err := c.repo.ListForEviction(ctx)
	if err != nil {
		c.logger.Warn("Failed to list stream cache entries for startup cleanup", "error", err)
		return
	}
	c.mu.Lock()
	localIdentity := c.localIdentity
	activeRequest := c.activeRequest
	c.mu.Unlock()

	referenced := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		if c.obsoleteLocalEntry(ctx, entry, localIdentity, activeRequest) {
			if err := os.Remove(filepath.Join(c.dir, entry.AudioPath)); err != nil && !os.IsNotExist(err) {
				c.logger.Warn("Failed to delete an already-local stream cache",
					"cache_key", entry.CacheKey, "error", err)
			}
			if err := c.repo.Delete(ctx, entry.CacheKey); err != nil {
				c.logger.Warn("Failed to drop an already-local stream cache row",
					"cache_key", entry.CacheKey, "error", err)
			}
			c.unpinCover(entry.Provider, entry.ExternalID)
			continue
		}
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

// obsoleteLocalEntry reports whether a cache row describes audio that is
// already a library file and no request still needs.
func (c *Cache) obsoleteLocalEntry(
	ctx context.Context,
	entry *domain.StreamCacheEntry,
	localIdentity LocalIdentityChecker,
	activeRequest func(ctx context.Context, provider, externalID string) bool,
) bool {
	if localIdentity == nil {
		return false
	}
	provider := domain.NormalizeProvider(entry.Provider)
	externalID := strings.TrimSpace(entry.ExternalID)
	if provider == "" || externalID == "" {
		return false
	}
	if activeRequest != nil && activeRequest(ctx, provider, externalID) {
		return false
	}
	local, err := localIdentity.HasLocalIdentity(ctx, provider, externalID)
	if err != nil {
		c.logger.Debug("Stream cache: cannot check the local identity",
			"provider", provider, "external_id", externalID, "error", err)
		return false
	}
	return local
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

// taskSnapshot reads the task row id and byte counters under the session lock.
func (s *cacheSession) taskSnapshot() (id string, total, done int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.taskID, s.total, s.received
}
