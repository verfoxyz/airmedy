package stream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airmedy/internal/app/config"
	"airmedy/internal/domain"
)

type fakeResolver struct {
	ref domain.StreamRef
	err error
}

func (f *fakeResolver) Resolve(context.Context, *domain.TrackDTO, string) (domain.StreamRef, error) {
	return f.ref, f.err
}

type fakeSettings struct {
	settings *domain.AppSettings
}

func (f *fakeSettings) GetSettings(context.Context) (*domain.AppSettings, error) {
	return f.settings, nil
}

// fakeStreamCacheRepo is an in-memory StreamCacheRepository. The real SQL
// behaviour is covered in internal/infra/sqlite; here it lets the cache's
// matching and eviction ordering be tested without cgo.
type fakeStreamCacheRepo struct {
	mu      sync.Mutex
	entries map[string]*domain.StreamCacheEntry
}

func newFakeStreamCacheRepo() *fakeStreamCacheRepo {
	return &fakeStreamCacheRepo{entries: make(map[string]*domain.StreamCacheEntry)}
}

func cloneEntry(entry *domain.StreamCacheEntry) *domain.StreamCacheEntry {
	copied := *entry
	return &copied
}

func (f *fakeStreamCacheRepo) Upsert(_ context.Context, entry *domain.StreamCacheEntry) error {
	if entry == nil {
		return fmt.Errorf("nil entry")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[entry.CacheKey] = cloneEntry(entry)
	return nil
}

func (f *fakeStreamCacheRepo) GetByIdentity(_ context.Context, provider, externalID string) (*domain.StreamCacheEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, entry := range f.entries {
		if entry.Provider == provider && entry.ExternalID == externalID {
			return cloneEntry(entry), nil
		}
	}
	return nil, nil
}

func (f *fakeStreamCacheRepo) MatchByTitleArtist(_ context.Context, titleKey, artistKey string, durationMs, toleranceMs int64) (*domain.StreamCacheEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var best *domain.StreamCacheEntry
	for _, entry := range f.entries {
		if entry.TitleKey != titleKey || entry.ArtistKey != artistKey {
			continue
		}
		if durationMs != 0 && entry.DurationMs != 0 {
			diff := entry.DurationMs - durationMs
			if diff < 0 {
				diff = -diff
			}
			if diff > toleranceMs {
				continue
			}
		}
		if best == nil || entry.LastUsed > best.LastUsed {
			best = entry
		}
	}
	if best == nil {
		return nil, nil
	}
	return cloneEntry(best), nil
}

func (f *fakeStreamCacheRepo) ListForEviction(_ context.Context) ([]*domain.StreamCacheEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries := make([]*domain.StreamCacheEntry, 0, len(f.entries))
	for _, entry := range f.entries {
		entries = append(entries, cloneEntry(entry))
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].LastUsed < entries[j].LastUsed })
	return entries, nil
}

func (f *fakeStreamCacheRepo) update(key string, mutate func(*domain.StreamCacheEntry)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if entry, ok := f.entries[key]; ok {
		mutate(entry)
	}
	return nil
}

func (f *fakeStreamCacheRepo) UpdateProgress(_ context.Context, key string, seconds float64) error {
	return f.update(key, func(entry *domain.StreamCacheEntry) { entry.PlaybackProgress = seconds })
}

func (f *fakeStreamCacheRepo) UpdateComplete(_ context.Context, key string, totalBytes, receivedBytes int64, complete bool) error {
	return f.update(key, func(entry *domain.StreamCacheEntry) {
		entry.TotalBytes = totalBytes
		entry.ReceivedBytes = receivedBytes
		entry.Complete = complete
	})
}

func (f *fakeStreamCacheRepo) SetPendingPromote(_ context.Context, key string, pending bool) error {
	return f.update(key, func(entry *domain.StreamCacheEntry) { entry.PendingPromote = pending })
}

func (f *fakeStreamCacheRepo) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, key)
	return nil
}

func (f *fakeStreamCacheRepo) get(t *testing.T, key string) *domain.StreamCacheEntry {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.entries[key]
	if !ok {
		t.Fatalf("stream cache entry %s missing", key)
	}
	return cloneEntry(entry)
}

// fakeCoverPinner records pin/unpin calls by remote identity.
type fakeCoverPinner struct {
	mu      sync.Mutex
	pins    map[string]int
	unpins  map[string]int
}

func newFakeCoverPinner() *fakeCoverPinner {
	return &fakeCoverPinner{pins: make(map[string]int), unpins: make(map[string]int)}
}

func (f *fakeCoverPinner) Pin(_ context.Context, provider, externalID string) error {
	f.mu.Lock()
	f.pins[provider+"/"+externalID]++
	f.mu.Unlock()
	return nil
}

func (f *fakeCoverPinner) Unpin(_ context.Context, provider, externalID string) error {
	f.mu.Lock()
	f.unpins[provider+"/"+externalID]++
	f.mu.Unlock()
	return nil
}

func (f *fakeCoverPinner) pinCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pins[key]
}

func (f *fakeCoverPinner) unpinCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unpins[key]
}

func newTestCache(t *testing.T, resolver domain.StreamResolver) *Cache {
	t.Helper()
	return newTestCacheWithRepo(t, resolver, newFakeStreamCacheRepo())
}

func newTestCacheWithRepo(t *testing.T, resolver domain.StreamResolver, repo domain.StreamCacheRepository) *Cache {
	t.Helper()
	return newTestCacheWithParts(t, resolver, repo, nil)
}

func newTestCacheWithParts(t *testing.T, resolver domain.StreamResolver, repo domain.StreamCacheRepository, covers domain.CoverPinner) *Cache {
	t.Helper()
	cache := NewCache(
		&config.Config{DataDir: t.TempDir()},
		repo,
		resolver,
		&fakeSettings{settings: &domain.AppSettings{StreamCacheMB: domain.DefaultStreamCacheMB}},
		covers,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = cache.Stop(context.Background()) })
	return cache
}

func remoteTrack() *domain.TrackDTO {
	return &domain.TrackDTO{Track: domain.Track{ID: "t1", Title: "Song", Provider: "wy", ExternalID: "42"}}
}

func TestCacheProxiesFullBodyAndRange(t *testing.T) {
	data := bytes.Repeat([]byte("airmedy-stream-"), 4096)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "song.mp3", time.Time{}, bytes.NewReader(data))
	}))
	defer upstream.Close()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: upstream.URL + "/song.mp3"}})
	session, err := cache.Open(context.Background(), remoteTrack())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)

	resp, err := http.Get(session.URL)
	if err != nil {
		t.Fatalf("GET session: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !bytes.Equal(body, data) {
		t.Fatalf("body = %d bytes, want %d", len(body), len(data))
	}

	req, _ := http.NewRequest(http.MethodGet, session.URL, nil)
	req.Header.Set("Range", "bytes=1000-")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET ranged session: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if want := data[1000:]; !bytes.Equal(body, want) {
		t.Fatalf("ranged body = %d bytes, want %d", len(body), len(want))
	}
}

// TestCacheBlocksUntilBytesArrive proves the proxy waits for a growing file
// instead of returning EOF: the upstream pauses mid-body after the prebuffer
// water mark, and the client must receive every byte only after the rest is
// released.
func TestCacheBlocksUntilBytesArrive(t *testing.T) {
	data := bytes.Repeat([]byte{0x5a}, 2<<20)
	// More than the 1 MiB prebuffer, so Open returns before the pause.
	const pauseAfter = 1280 * 1024
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(data[:pauseAfter]); err != nil {
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(data[pauseAfter:])
	}))
	defer upstream.Close()
	defer func() {
		// Unblock the handler if the test fails before releasing it, otherwise
		// upstream.Close would wait forever. Registered after Close so it runs
		// first.
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: upstream.URL + "/song.mp3"}})
	session, err := cache.Open(context.Background(), remoteTrack())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)

	type result struct {
		body []byte
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		resp, getErr := http.Get(session.URL)
		if getErr != nil {
			resultCh <- result{err: getErr}
			return
		}
		defer resp.Body.Close()
		body, readErr := io.ReadAll(resp.Body)
		resultCh <- result{body: body, err: readErr}
	}()

	select {
	case got := <-resultCh:
		t.Fatalf("client finished early with %d bytes while upstream was paused: %v", len(got.body), got.err)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("read: %v", got.err)
		}
		if !bytes.Equal(got.body, data) {
			t.Fatalf("body = %d bytes, want %d", len(got.body), len(data))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client never received the released bytes")
	}
}

func TestEnforceLimitEvictsLeastCompleteFirst(t *testing.T) {
	dir := t.TempDir()
	repo := newFakeStreamCacheRepo()
	cache := &Cache{dir: dir, repo: repo, sessions: map[string]*cacheSession{}, logger: slog.Default()}

	writeEntry := func(id string, size int64, received, total int64, progress float64) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, id+".audio"), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := repo.Upsert(context.Background(), &domain.StreamCacheEntry{
			CacheKey: id, Provider: "wy", ExternalID: id, AudioPath: id + ".audio",
			ReceivedBytes: received, TotalBytes: total, PlaybackProgress: progress,
		}); err != nil {
			t.Fatal(err)
		}
	}
	writeEntry("nearly-done", 1024, 900, 1000, 1)
	writeEntry("barely-started", 1024, 100, 1000, 50)

	// 2 KiB on disk, limit 1.5 KiB: exactly one entry must go, the less
	// complete one, regardless of its playback progress.
	cache.enforceLimitBytes(1536)

	if _, err := os.Stat(filepath.Join(dir, "barely-started.audio")); !os.IsNotExist(err) {
		t.Fatalf("least complete entry was not evicted (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "nearly-done.audio")); err != nil {
		t.Fatalf("more complete entry was evicted: %v", err)
	}
	repo.mu.Lock()
	_, stillIndexed := repo.entries["barely-started"]
	repo.mu.Unlock()
	if stillIndexed {
		t.Fatal("metadata of the evicted entry was left behind")
	}
}

func TestEnforceLimitSkipsActiveSession(t *testing.T) {
	dir := t.TempDir()
	repo := newFakeStreamCacheRepo()
	cache := &Cache{dir: dir, repo: repo, sessions: map[string]*cacheSession{}, logger: slog.Default()}

	if err := os.WriteFile(filepath.Join(dir, "playing.audio"), make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.Upsert(context.Background(), &domain.StreamCacheEntry{
		CacheKey: "playing", AudioPath: "playing.audio", TotalBytes: 2048, ReceivedBytes: 1024,
	}); err != nil {
		t.Fatal(err)
	}
	cache.sessions["playing"] = &cacheSession{id: "playing", playing: true}

	cache.enforceLimitBytes(1)

	if _, err := os.Stat(filepath.Join(dir, "playing.audio")); err != nil {
		t.Fatalf("active session was evicted: %v", err)
	}
	if entry := repo.get(t, "playing"); entry == nil {
		t.Fatal("active session's metadata was evicted")
	}
}

func TestParseRangeHeader(t *testing.T) {
	tests := []struct {
		in       string
		start    int64
		hasRange bool
		wantErr  bool
	}{
		{"", 0, false, false},
		{"bytes=0-", 0, true, false},
		{"bytes=1234-", 1234, true, false},
		{"bytes=100-200", 100, true, false},
		{"bytes=-500", 0, false, true},
		{"bytes=0-1,4-5", 0, false, true},
		{"items=0-", 0, false, true},
		{"bytes=abc-", 0, false, true},
	}
	for _, tt := range tests {
		start, hasRange, err := parseRangeHeader(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseRangeHeader(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if start != tt.start || hasRange != tt.hasRange {
			t.Errorf("parseRangeHeader(%q) = (%d,%v), want (%d,%v)", tt.in, start, hasRange, tt.start, tt.hasRange)
		}
	}
}

// chunkedUpstream serves a body slowly in fixed-size chunks and records the
// Range headers it receives, so tests can observe resume behaviour.
type chunkedUpstream struct {
	body  []byte
	chunk int
	delay time.Duration

	hits atomic.Int32
	mu   sync.Mutex
	rngs []string
}

func (u *chunkedUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.hits.Add(1)
	start := int64(0)
	if value := r.Header.Get("Range"); value != "" {
		u.mu.Lock()
		u.rngs = append(u.rngs, value)
		u.mu.Unlock()
		if parsed, hasRange, err := parseRangeHeader(value); err == nil && hasRange {
			start = parsed
		}
	}
	if start > int64(len(u.body)) {
		start = int64(len(u.body))
	}
	remaining := u.body[start:]
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(remaining)))
	if start > 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(u.body)-1, len(u.body)))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	flusher, _ := w.(http.Flusher)
	for off := 0; off < len(remaining); off += u.chunk {
		end := off + u.chunk
		if end > len(remaining) {
			end = len(remaining)
		}
		if _, err := w.Write(remaining[off:end]); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		if u.delay > 0 {
			select {
			case <-time.After(u.delay):
			case <-r.Context().Done():
				return
			}
		}
	}
}

func (u *chunkedUpstream) lastRange() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.rngs) == 0 {
		return ""
	}
	return u.rngs[len(u.rngs)-1]
}

// waitForRange polls until the upstream has recorded a Range request, because
// Open may return from the prebuffer before the download goroutine has issued
// its resumed request.
func (u *chunkedUpstream) waitForRange(timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for {
		if value := u.lastRange(); value != "" {
			return value
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// sessionBytes returns the bytes the cache has received for the session.
func sessionBytes(t *testing.T, cache *Cache, session *Session) int64 {
	t.Helper()
	cache.mu.Lock()
	s := cache.sessions[session.ID]
	cache.mu.Unlock()
	if s == nil {
		t.Fatalf("session %s missing", session.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.received
}

func TestOpenWaitsForPrebuffer(t *testing.T) {
	data := bytes.Repeat([]byte{0x42}, 4<<20) // 4 MiB
	upstream := &chunkedUpstream{body: data, chunk: 32 << 10, delay: 20 * time.Millisecond}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}})
	session, err := cache.Open(context.Background(), remoteTrack())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)

	if got := sessionBytes(t, cache, session); got < prebufferBytes {
		t.Fatalf("Open returned with %d cached bytes, want at least the %d-byte water mark", got, prebufferBytes)
	}
}

func TestOpenReturnsImmediatelyForCompleteCache(t *testing.T) {
	upstream := &chunkedUpstream{body: bytes.Repeat([]byte{7}, 2<<20), chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}})
	data := bytes.Repeat([]byte{7}, 2<<20)
	id := cacheKeyFor("wy", "42")
	if err := os.WriteFile(filepath.Join(cache.dir, id+".audio"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cache.repo.Upsert(context.Background(), &domain.StreamCacheEntry{
		CacheKey: id, Provider: "wy", ExternalID: "42", AudioPath: id + ".audio",
		Title: "Song", TitleKey: domain.NormalizationKey("Song"), Artist: "Singer",
		ArtistKey: domain.NormalizationKey("Singer"), DurationMs: 200000,
		TotalBytes: int64(len(data)), ReceivedBytes: int64(len(data)), Complete: true,
	}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	session, err := cache.Open(context.Background(), remoteTrack())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Open took %v for a complete entry", elapsed)
	}
	if hits := upstream.hits.Load(); hits != 0 {
		t.Fatalf("complete entry hit upstream %d times", hits)
	}
}

func TestOpenTimesOutOnStalledUpstream(t *testing.T) {
	stall := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Length", "1048576")
		w.WriteHeader(http.StatusOK)
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(stall)

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}})
	start := time.Now()
	_, err := cache.Open(context.Background(), remoteTrack())
	if err == nil {
		t.Fatal("Open succeeded with a stalled upstream")
	}
	if !errors.Is(err, domain.ErrStreamUnavailable) {
		t.Fatalf("error = %v, want ErrStreamUnavailable", err)
	}
	if elapsed := time.Since(start); elapsed < prebufferTimeout-500*time.Millisecond {
		t.Fatalf("Open failed after %v, before the %v prebuffer timeout", elapsed, prebufferTimeout)
	}
}

func TestOpenPrebufferHonoursContextCancel(t *testing.T) {
	stall := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(stall)

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := cache.Open(ctx, remoteTrack())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("cancelled Open took %v", elapsed)
	}
}

func TestOpenFailsWhenUpstreamErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}})
	_, err := cache.Open(context.Background(), remoteTrack())
	if err == nil {
		t.Fatal("Open succeeded against a 500 upstream")
	}
	if !errors.Is(err, domain.ErrStreamUnavailable) {
		t.Fatalf("error = %v, want ErrStreamUnavailable", err)
	}
}

func TestReopenResumesFromCachedBytes(t *testing.T) {
	data := bytes.Repeat([]byte{0x19}, 4<<20)
	upstream := &chunkedUpstream{body: data, chunk: 64 << 10, delay: 10 * time.Millisecond}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}})
	first, err := cache.Open(context.Background(), remoteTrack())
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	receivedBeforeClose := sessionBytes(t, cache, first)
	cache.Close(first)
	if receivedBeforeClose < prebufferBytes {
		t.Fatalf("first session held %d bytes, want at least %d", receivedBeforeClose, prebufferBytes)
	}

	second, err := cache.Open(context.Background(), remoteTrack())
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer cache.Close(second)

	if value := upstream.waitForRange(2 * time.Second); value == "" || value == "bytes=0-" {
		t.Fatalf("second request did not resume with a Range header (got %q)", value)
	}

	resp, err := http.Get(second.URL)
	if err != nil {
		t.Fatalf("GET resumed session: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(body, data) {
		t.Fatalf("resumed body = %d bytes, want %d", len(body), len(data))
	}
}

// seedCompleteEntry writes a fully cached entry and its audio file.
func seedCompleteEntry(t *testing.T, cache *Cache, repo *fakeStreamCacheRepo, provider, externalID, title, artist string, durationMs int64, data []byte) string {
	t.Helper()
	key := cacheKeyFor(provider, externalID)
	if err := os.WriteFile(filepath.Join(cache.dir, key+".audio"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.Upsert(context.Background(), &domain.StreamCacheEntry{
		CacheKey:      key,
		Provider:      provider,
		ExternalID:    externalID,
		Title:         title,
		TitleKey:      domain.NormalizationKey(title),
		Artist:        artist,
		ArtistKey:     domain.NormalizationKey(artist),
		DurationMs:    durationMs,
		AudioPath:     key + ".audio",
		TotalBytes:    int64(len(data)),
		ReceivedBytes: int64(len(data)),
		Complete:      true,
		CreatedAt:     1,
		LastUsed:      1,
	}); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestOpenReusesAnotherProvidersCacheByTitleArtist(t *testing.T) {
	upstream := &chunkedUpstream{body: bytes.Repeat([]byte{9}, 2<<20), chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	repo := newFakeStreamCacheRepo()
	cache := newTestCacheWithRepo(t, &fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}}, repo)
	wyKey := seedCompleteEntry(t, cache, repo, "wy", "42", "屋顶", "周杰伦", 200000, bytes.Repeat([]byte{9}, 2<<20))

	kgTrack := &domain.TrackDTO{Track: domain.Track{
		ID: "t2", Title: "屋顶", Provider: "kg", ExternalID: "999", Duration: 200,
	}, Artists: []*domain.Artist{{Name: "周杰伦"}}}

	session, err := cache.Open(context.Background(), kgTrack)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)

	if session.ID != wyKey {
		t.Fatalf("session id = %s, want the wy entry %s", session.ID, wyKey)
	}
	if hits := upstream.hits.Load(); hits != 0 {
		t.Fatalf("cross-source hit fetched upstream %d times, want 0", hits)
	}
	if entry, err := repo.GetByIdentity(context.Background(), "kg", "999"); err != nil {
		t.Fatalf("GetByIdentity: %v", err)
	} else if entry != nil {
		t.Fatalf("cross-source playback must not create a kg row: %+v", entry)
	}
}

func TestOpenMatchesWhenEitherDurationIsUnknown(t *testing.T) {
	repo := newFakeStreamCacheRepo()
	cache := newTestCacheWithRepo(t, &fakeResolver{err: fmt.Errorf("must not resolve")}, repo)
	wyKey := seedCompleteEntry(t, cache, repo, "wy", "42", "屋顶", "周杰伦", 0, bytes.Repeat([]byte{4}, 2<<20))

	kgTrack := &domain.TrackDTO{Track: domain.Track{
		ID: "t2", Title: "屋顶", Provider: "kg", ExternalID: "999", Duration: 200,
	}, Artists: []*domain.Artist{{Name: "周杰伦"}}}

	session, err := cache.Open(context.Background(), kgTrack)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)
	if session.ID != wyKey {
		t.Fatalf("session id = %s, want %s when one side has no duration", session.ID, wyKey)
	}
}

func TestOpenDoesNotMatchOutsideDurationTolerance(t *testing.T) {
	upstream := &chunkedUpstream{body: bytes.Repeat([]byte{3}, 2<<20), chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	repo := newFakeStreamCacheRepo()
	cache := newTestCacheWithRepo(t, &fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}}, repo)
	seedCompleteEntry(t, cache, repo, "wy", "42", "屋顶", "周杰伦", 200000, bytes.Repeat([]byte{3}, 2<<20))

	// Same title and artist, but 60 s apart: a different recording.
	kgTrack := &domain.TrackDTO{Track: domain.Track{
		ID: "t2", Title: "屋顶", Provider: "kg", ExternalID: "999", Duration: 260,
	}, Artists: []*domain.Artist{{Name: "周杰伦"}}}

	session, err := cache.Open(context.Background(), kgTrack)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)

	if want := cacheKeyFor("kg", "999"); session.ID != want {
		t.Fatalf("session id = %s, want a new kg entry %s", session.ID, want)
	}
	if hits := upstream.hits.Load(); hits == 0 {
		t.Fatal("a non-match must fetch the new provider instead of reusing the other source")
	}
}

func TestStartRemovesLegacyJSONAndOrphanAudio(t *testing.T) {
	dir := t.TempDir()
	streamDir := filepath.Join(dir, "stream-cache")
	if err := os.MkdirAll(streamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repo := newFakeStreamCacheRepo()
	key := cacheKeyFor("wy", "42")
	if err := repo.Upsert(context.Background(), &domain.StreamCacheEntry{
		CacheKey: key, Provider: "wy", ExternalID: "42", AudioPath: key + ".audio",
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{key + ".audio", key + ".json", "orphan.audio", "orphan.json"} {
		if err := os.WriteFile(filepath.Join(streamDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cache := NewCache(
		&config.Config{DataDir: dir},
		repo,
		&fakeResolver{},
		&fakeSettings{settings: &domain.AppSettings{StreamCacheMB: domain.DefaultStreamCacheMB}},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = cache.Stop(context.Background()) }()

	for _, gone := range []string{key + ".json", "orphan.audio", "orphan.json"} {
		if _, err := os.Stat(filepath.Join(streamDir, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s should have been removed (stat err = %v)", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(streamDir, key+".audio")); err != nil {
		t.Fatalf("referenced audio was removed: %v", err)
	}
}

func TestReportProgressOnlyWritesAtCheckpointsOrBackstop(t *testing.T) {
	upstream := &chunkedUpstream{body: bytes.Repeat([]byte{8}, 2<<20), chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	repo := newFakeStreamCacheRepo()
	cache := newTestCacheWithRepo(t, &fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}}, repo)
	session, err := cache.Open(context.Background(), remoteTrack())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// A ticker report updates memory only: the row keeps its old position.
	cache.ReportProgress(session, 12.4)
	if entry := repo.get(t, session.ID); entry.PlaybackProgress != 0 {
		t.Fatalf("progress was persisted on a ticker report: %v", entry.PlaybackProgress)
	}

	// Pause (or another explicit checkpoint) flushes it.
	cache.FlushProgress(session)
	if entry := repo.get(t, session.ID); entry.PlaybackProgress != 12.4 {
		t.Fatalf("FlushProgress persisted %v, want 12.4", entry.PlaybackProgress)
	}

	// Thirty seconds later the periodic backstop writes without an explicit call.
	cache.mu.Lock()
	s := cache.sessions[session.ID]
	cache.mu.Unlock()
	s.mu.Lock()
	s.lastFlush = time.Now().Add(-progressFlushInterval - time.Second)
	s.mu.Unlock()
	cache.ReportProgress(session, 42)
	if entry := repo.get(t, session.ID); entry.PlaybackProgress != 42 {
		t.Fatalf("backstop persisted %v, want 42", entry.PlaybackProgress)
	}

	// Close writes the final position.
	cache.ReportProgress(session, 55)
	cache.Close(session)
	if entry := repo.get(t, session.ID); entry.PlaybackProgress != 55 {
		t.Fatalf("Close persisted %v, want 55", entry.PlaybackProgress)
	}
}

func TestOpenPinsTheStreamCover(t *testing.T) {
	upstream := &chunkedUpstream{body: bytes.Repeat([]byte{6}, 2<<20), chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	pinner := newFakeCoverPinner()
	cache := newTestCacheWithParts(t,
		&fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}},
		newFakeStreamCacheRepo(), pinner)

	session, err := cache.Open(context.Background(), remoteTrack())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)

	if pinner.pinCount("wy/42") == 0 {
		t.Fatal("Open did not pin the cover for the streamed identity")
	}
}

func TestEvictionUnpinsTheCover(t *testing.T) {
	dir := t.TempDir()
	repo := newFakeStreamCacheRepo()
	pinner := newFakeCoverPinner()
	cache := &Cache{dir: dir, repo: repo, covers: pinner, sessions: map[string]*cacheSession{}, logger: slog.Default()}

	if err := os.WriteFile(filepath.Join(dir, "old.audio"), make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.Upsert(context.Background(), &domain.StreamCacheEntry{
		CacheKey: "old", Provider: "wy", ExternalID: "42", AudioPath: "old.audio",
		TotalBytes: 2048, ReceivedBytes: 512,
	}); err != nil {
		t.Fatal(err)
	}

	cache.enforceLimitBytes(1)

	if pinner.unpinCount("wy/42") == 0 {
		t.Fatal("eviction did not unpin the evicted entry's cover")
	}
}

func TestPrebufferTarget(t *testing.T) {
	tests := []struct {
		total    int64
		duration int
		want     int64
	}{
		{0, 0, prebufferBytes},
		{10 << 20, 0, prebufferBytes},
		// 10 MiB over 300 s ≈ 35 KiB/s → a 3 s lead is ~105 KiB, well under 1 MiB.
		{10 << 20, 300, int64((10 << 20) * 3 / 300)},
		// 40 MiB over 60 s ≈ 683 KiB/s → the 3 s lead exceeds 1 MiB, so the floor wins.
		{40 << 20, 60, prebufferBytes},
	}
	for _, tt := range tests {
		if got := prebufferTarget(tt.total, tt.duration); got != tt.want {
			t.Errorf("prebufferTarget(%d,%d) = %d, want %d", tt.total, tt.duration, got, tt.want)
		}
	}
}
