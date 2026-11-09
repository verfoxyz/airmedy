package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func newTestCache(t *testing.T, resolver domain.StreamResolver) *Cache {
	t.Helper()
	cache := NewCache(
		&config.Config{DataDir: t.TempDir()},
		resolver,
		&fakeSettings{settings: &domain.AppSettings{StreamCacheMB: domain.DefaultStreamCacheMB}},
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
	cache := &Cache{dir: dir, sessions: map[string]*cacheSession{}, logger: slog.Default()}

	writeEntry := func(id string, size int64, received, total int64, progress float64) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, id+".audio"), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		meta, _ := json.Marshal(cacheMeta{
			Provider: "wy", ExternalID: id,
			ReceivedBytes: received, TotalBytes: total, PlaybackProgress: progress,
		})
		if err := os.WriteFile(filepath.Join(dir, id+".json"), meta, 0o644); err != nil {
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
	if _, err := os.Stat(filepath.Join(dir, "barely-started.json")); !os.IsNotExist(err) {
		t.Fatalf("metadata of the evicted entry was left behind")
	}
}

func TestEnforceLimitSkipsActiveSession(t *testing.T) {
	dir := t.TempDir()
	cache := &Cache{dir: dir, sessions: map[string]*cacheSession{}, logger: slog.Default()}

	if err := os.WriteFile(filepath.Join(dir, "playing.audio"), make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	cache.sessions["playing"] = &cacheSession{id: "playing", playing: true}

	cache.enforceLimitBytes(1)

	if _, err := os.Stat(filepath.Join(dir, "playing.audio")); err != nil {
		t.Fatalf("active session was evicted: %v", err)
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
	id := cacheID("wy", "42")
	if err := os.WriteFile(filepath.Join(cache.dir, id+".audio"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(cacheMeta{
		Provider: "wy", ExternalID: "42",
		TotalBytes: int64(len(data)), ReceivedBytes: int64(len(data)), Complete: true,
	})
	if err := os.WriteFile(filepath.Join(cache.dir, id+".json"), meta, 0o644); err != nil {
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
