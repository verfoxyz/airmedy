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
	"strings"
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

func (f *fakeStreamCacheRepo) UpdateQuality(_ context.Context, key, quality string) error {
	return f.update(key, func(entry *domain.StreamCacheEntry) { entry.Quality = quality })
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
	mu     sync.Mutex
	pins   map[string]int
	unpins map[string]int
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

// fakeStreamCacheTaskRepo records the sessions the cache reports to the tasks
// page. It is deliberately a value copy: the cache keeps mutating its own row
// pointer after Start.
type fakeStreamCacheTaskRepo struct {
	mu   sync.Mutex
	rows map[string]*domain.StreamCacheTask
}

func newFakeStreamCacheTaskRepo() *fakeStreamCacheTaskRepo {
	return &fakeStreamCacheTaskRepo{rows: make(map[string]*domain.StreamCacheTask)}
}

func (f *fakeStreamCacheTaskRepo) Start(_ context.Context, task *domain.StreamCacheTask) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := *task
	f.rows[task.ID] = &copied
	return nil
}

func (f *fakeStreamCacheTaskRepo) Finish(
	_ context.Context, id string, totalBytes, doneBytes int64, at time.Time,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.rows[id]; ok {
		row.State = domain.StreamCacheTaskStateDone
		row.BytesTotal = totalBytes
		row.BytesDone = doneBytes
		row.Error = ""
		finished := at
		row.FinishedAt = &finished
	}
	return nil
}

func (f *fakeStreamCacheTaskRepo) Fail(
	_ context.Context, id, message string, totalBytes, doneBytes int64, at time.Time,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.rows[id]; ok {
		row.State = domain.StreamCacheTaskStateFailed
		row.Error = message
		row.BytesTotal = totalBytes
		row.BytesDone = doneBytes
		finished := at
		row.FinishedAt = &finished
	}
	return nil
}

func (f *fakeStreamCacheTaskRepo) Cancel(
	_ context.Context, id string, totalBytes, doneBytes int64, at time.Time,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.rows[id]; ok {
		row.State = domain.StreamCacheTaskStateCanceled
		row.Error = ""
		row.BytesTotal = totalBytes
		row.BytesDone = doneBytes
		finished := at
		row.FinishedAt = &finished
	}
	return nil
}

func (f *fakeStreamCacheTaskRepo) MarkActiveInterrupted(
	_ context.Context, _ string, at time.Time,
) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var changed int64
	for _, row := range f.rows {
		if row.State != domain.StreamCacheTaskStateActive {
			continue
		}
		row.State = domain.StreamCacheTaskStateCanceled
		row.Error = ""
		finished := at
		row.FinishedAt = &finished
		changed++
	}
	return changed, nil
}

func (f *fakeStreamCacheTaskRepo) List(context.Context) ([]*domain.StreamCacheTask, error) {
	return f.all(), nil
}

func (f *fakeStreamCacheTaskRepo) TrimFinished(context.Context, int) (int64, error) {
	return 0, nil
}

func (f *fakeStreamCacheTaskRepo) all() []*domain.StreamCacheTask {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := make([]*domain.StreamCacheTask, 0, len(f.rows))
	for _, row := range f.rows {
		copied := *row
		rows = append(rows, &copied)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].StartedAt.Before(rows[j].StartedAt)
	})
	return rows
}

func (f *fakeStreamCacheTaskRepo) waitForState(t *testing.T, state string) domain.StreamCacheTask {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows := f.all()
		for i := len(rows) - 1; i >= 0; i-- {
			if rows[i].State == state {
				return *rows[i]
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no stream cache task reached state %q (rows: %+v)", state, f.all())
		}
		time.Sleep(5 * time.Millisecond)
	}
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

// stubLocalIdentity is the guard the composition root installs.
type stubLocalIdentity struct {
	local bool
	err   error
}

func (s stubLocalIdentity) HasLocalIdentity(context.Context, string, string) (bool, error) {
	return s.local, s.err
}

// TestOpen_RefusesAnIdentityThatIsAlreadyLocal: a promoted row keeps its
// identity, so a player still holding the old remote DTO must not recreate a
// cache entry for it.
func TestOpen_RefusesAnIdentityThatIsAlreadyLocal(t *testing.T) {
	repo := newFakeStreamCacheRepo()
	cache := newTestCacheWithRepo(t, &fakeResolver{}, repo)
	cache.SetLocalIdentityChecker(stubLocalIdentity{local: true})

	track := &domain.TrackDTO{Track: domain.Track{
		ID: "remote-1", Path: domain.RemotePath("wy", "42"), Title: "Song",
		Provider: "wy", ExternalID: "42", Availability: domain.AvailabilityRemote,
	}}
	_, err := cache.Open(context.Background(), track)
	if !errors.Is(err, domain.ErrStreamAlreadyLocal) {
		t.Fatalf("Open error = %v, want ErrStreamAlreadyLocal", err)
	}

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.entries) != 0 {
		t.Fatalf("an entry was created for a local identity: %+v", repo.entries)
	}
}

// TestStartupCleanupKeepsAudioAPendingRequestStillNeeds: a cache whose identity
// is already local is obsolete, unless a media request is still waiting to
// promote or replace it.
func TestStartupCleanupKeepsAudioAPendingRequestStillNeeds(t *testing.T) {
	repo := newFakeStreamCacheRepo()
	cache := newTestCacheWithRepo(t, &fakeResolver{}, repo)
	cache.SetLocalIdentityChecker(stubLocalIdentity{local: true})
	active := false
	cache.SetActiveRequestChecker(func(context.Context, string, string) bool { return active })
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(cache.dir, "wy-42.audio"), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.Upsert(ctx, &domain.StreamCacheEntry{
		CacheKey: "wy/42", Provider: "wy", ExternalID: "42", AudioPath: "wy-42.audio",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	active = true
	cache.cleanupLegacyFiles(ctx)
	if entry, _ := repo.GetByIdentity(ctx, "wy", "42"); entry == nil {
		t.Fatal("the startup sweep deleted a cache a request still needs")
	}

	active = false
	cache.cleanupLegacyFiles(ctx)
	if entry, _ := repo.GetByIdentity(ctx, "wy", "42"); entry != nil {
		t.Fatalf("obsolete cache survived the startup sweep: %+v", entry)
	}
	if _, err := os.Stat(filepath.Join(cache.dir, "wy-42.audio")); !os.IsNotExist(err) {
		t.Fatalf("obsolete audio survived the startup sweep: %v", err)
	}
}

// prefetchTrack is the same remote row with a known duration, so the water mark
// can be derived from the real size instead of a bitrate estimate.
func prefetchTrack(duration int) *domain.TrackDTO {
	return &domain.TrackDTO{Track: domain.Track{
		ID: "t1", Title: "Song", Provider: "wy", ExternalID: "42", Duration: duration,
	}}
}

// waitForStableBytes polls until the session's byte count stops changing, which
// is how a paused-at-the-water-mark download looks from outside.
func waitForStableBytes(t *testing.T, cache *Cache, session *Session) int64 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	last := int64(-1)
	stable := 0
	for {
		got := sessionBytes(t, cache, session)
		if got == last && got > 0 {
			stable++
		} else {
			stable = 0
			last = got
		}
		if stable >= 5 {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream never settled (last %d bytes)", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestBoundedPrefetchPausesAtTheWaterMark: with playback still at zero, the
// download stops roughly 45 s ahead instead of fetching the whole song.
func TestBoundedPrefetchPausesAtTheWaterMark(t *testing.T) {
	const durationSeconds = 300
	data := bytes.Repeat([]byte{0x33}, 4<<20)
	upstream := &chunkedUpstream{body: data, chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}})
	session, err := cache.Open(context.Background(), prefetchTrack(durationSeconds))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)

	got := waitForStableBytes(t, cache, session)
	waterMark := int64(prefetchAheadSeconds) * int64(len(data)) / durationSeconds
	if got < waterMark {
		t.Fatalf("paused at %d bytes, want at least the %d-byte water mark", got, waterMark)
	}
	if got > waterMark+128<<10 {
		t.Fatalf("downloaded %d bytes, far past the %d-byte water mark", got, waterMark)
	}
	if got >= int64(len(data)) {
		t.Fatalf("downloaded the whole %d-byte body while playback was at zero", len(data))
	}
}

// TestBoundedPrefetchResumesWhenPlaybackAdvances: the water mark moves with the
// playback position, and bytes already fetched are kept.
func TestBoundedPrefetchResumesWhenPlaybackAdvances(t *testing.T) {
	const durationSeconds = 300
	data := bytes.Repeat([]byte{0x34}, 4<<20)
	upstream := &chunkedUpstream{body: data, chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}})
	session, err := cache.Open(context.Background(), prefetchTrack(durationSeconds))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)

	before := waitForStableBytes(t, cache, session)
	cache.ReportProgress(session, 30)

	deadline := time.Now().Add(5 * time.Second)
	for sessionBytes(t, cache, session) <= before {
		if time.Now().After(deadline) {
			t.Fatalf("download stayed at %d bytes after playback advanced", before)
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := waitForStableBytes(t, cache, session)
	if got < before {
		t.Fatalf("received bytes shrank from %d to %d", before, got)
	}
	waterMark := int64(prefetchAheadSeconds+30) * int64(len(data)) / durationSeconds
	if got < waterMark {
		t.Fatalf("resumed to %d bytes, want at least %d", got, waterMark)
	}
}

// TestBoundedPrefetchBackwardSeekDoesNotDownload: seeking behind the downloaded
// position makes the lead larger, so the download stays paused.
func TestBoundedPrefetchBackwardSeekDoesNotDownload(t *testing.T) {
	const durationSeconds = 300
	data := bytes.Repeat([]byte{0x35}, 4<<20)
	upstream := &chunkedUpstream{body: data, chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}})
	session, err := cache.Open(context.Background(), prefetchTrack(durationSeconds))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)

	initial := waitForStableBytes(t, cache, session)
	cache.ReportProgress(session, 30)
	deadline := time.Now().Add(5 * time.Second)
	for sessionBytes(t, cache, session) <= initial {
		if time.Now().After(deadline) {
			t.Fatalf("download did not resume past %d bytes for the forward position", initial)
		}
		time.Sleep(20 * time.Millisecond)
	}
	advanced := waitForStableBytes(t, cache, session)

	cache.ReportProgress(session, 10)
	time.Sleep(300 * time.Millisecond)
	if got := sessionBytes(t, cache, session); got != advanced {
		t.Fatalf("a backward seek resumed the download: %d -> %d bytes", advanced, got)
	}
}

// TestBoundedPrefetchForwardSeekResumes: jumping past the water mark wakes the
// paused download immediately.
func TestBoundedPrefetchForwardSeekResumes(t *testing.T) {
	const durationSeconds = 300
	data := bytes.Repeat([]byte{0x36}, 4<<20)
	upstream := &chunkedUpstream{body: data, chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}})
	session, err := cache.Open(context.Background(), prefetchTrack(durationSeconds))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)

	before := waitForStableBytes(t, cache, session)
	cache.ReportProgress(session, 200)

	deadline := time.Now().Add(5 * time.Second)
	for sessionBytes(t, cache, session) <= before {
		if time.Now().After(deadline) {
			t.Fatalf("download stayed at %d bytes after a forward seek", before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDecodeDemandLiftsThePrefetchWaterMark: a decoder parked at the write edge
// (a seek's binary search, or the first probe) must keep the download running
// even though playback has not advanced past the water mark.
func TestDecodeDemandLiftsThePrefetchWaterMark(t *testing.T) {
	const durationSeconds = 300
	data := bytes.Repeat([]byte{0x38}, 8<<20)
	upstream := &chunkedUpstream{body: data, chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}})
	session, err := cache.Open(context.Background(), prefetchTrack(durationSeconds))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)

	paused := waitForStableBytes(t, cache, session)
	cache.SetDecodeOffset(session, paused)

	deadline := time.Now().Add(5 * time.Second)
	for sessionBytes(t, cache, session) < paused+demandWindowBytes {
		if time.Now().After(deadline) {
			t.Fatalf("download stayed at %d bytes, want at least %d (decoder demand)",
				sessionBytes(t, cache, session), paused+demandWindowBytes)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDecodeDemandReArmsTheWaterMark: once the download is far ahead of the
// decoder again, the normal prefetch limit applies and no more bytes arrive.
func TestDecodeDemandReArmsTheWaterMark(t *testing.T) {
	const durationSeconds = 300
	data := bytes.Repeat([]byte{0x39}, 8<<20)
	upstream := &chunkedUpstream{body: data, chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}})
	session, err := cache.Open(context.Background(), prefetchTrack(durationSeconds))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)

	paused := waitForStableBytes(t, cache, session)
	cache.SetDecodeOffset(session, paused)
	deadline := time.Now().Add(5 * time.Second)
	for sessionBytes(t, cache, session) < paused+demandWindowBytes {
		if time.Now().After(deadline) {
			t.Fatalf("download did not resume on decoder demand")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ahead := waitForStableBytes(t, cache, session)

	// The decoder jumped back to the head: the download is far ahead now, so the
	// playback water mark takes over again.
	cache.SetDecodeOffset(session, 0)
	time.Sleep(400 * time.Millisecond)
	if got := sessionBytes(t, cache, session); got != ahead {
		t.Fatalf("download kept growing after demand ended: %d -> %d bytes", ahead, got)
	}
}

func TestSetDecodeOffsetIgnoresMissingSessions(t *testing.T) {
	cache := newTestCache(t, &fakeResolver{})
	cache.SetDecodeOffset(nil, 1<<20)
	cache.SetDecodeOffset(&Session{ID: "missing"}, 1<<20)
}

// TestEnsureSeekableDownloadsUpToTheTarget: a stream seek must not start until
// the requested position plus the margin is on disk.
func TestEnsureSeekableDownloadsUpToTheTarget(t *testing.T) {
	const durationSeconds = 300
	data := bytes.Repeat([]byte{0x3a}, 8<<20)
	upstream := &chunkedUpstream{body: data, chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}})
	session, err := cache.Open(context.Background(), prefetchTrack(durationSeconds))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)

	paused := waitForStableBytes(t, cache, session)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cache.EnsureSeekable(ctx, session, 200); err != nil {
		t.Fatalf("EnsureSeekable: %v", err)
	}
	want := int64(200)*int64(len(data))/durationSeconds + seekBufferBytes
	if got := sessionBytes(t, cache, session); got < want {
		t.Fatalf("EnsureSeekable returned at %d bytes, want at least %d (from %d)", got, want, paused)
	}
}

// TestEnsureSeekableReturnsImmediatelyWhenTheTargetIsCached: already-downloaded
// bytes need no wait and no extra download.
func TestEnsureSeekableReturnsImmediatelyWhenTheTargetIsCached(t *testing.T) {
	cache := newTestCache(t, &fakeResolver{})
	s := &cacheSession{id: "cached", received: 4 << 20, total: 8 << 20, progressChanged: make(chan struct{}, 1)}
	cache.sessions[s.id] = s

	if err := cache.EnsureSeekable(context.Background(), &Session{ID: "cached"}, 10); err != nil {
		t.Fatalf("EnsureSeekable: %v", err)
	}
	if s.seekTarget != 0 {
		t.Fatalf("seekTarget = %d, want no lift when the bytes are already there", s.seekTarget)
	}
}

func TestEnsureSeekableReportsADownloadFailure(t *testing.T) {
	cache := newTestCache(t, &fakeResolver{})
	s := &cacheSession{
		id: "failed", total: 1 << 20, received: 1,
		failed: errors.New("boom"), progressChanged: make(chan struct{}, 1),
	}
	cache.sessions[s.id] = s

	if err := cache.EnsureSeekable(context.Background(), &Session{ID: "failed"}, 10); err == nil {
		t.Fatal("EnsureSeekable returned nil for a failed download")
	}
}

func TestEnsureSeekableHonorsContextCancellation(t *testing.T) {
	cache := newTestCache(t, &fakeResolver{})
	s := &cacheSession{id: "slow", total: -1, progressChanged: make(chan struct{}, 1)}
	cache.sessions[s.id] = s

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := cache.EnsureSeekable(ctx, &Session{ID: "slow"}, 10)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("EnsureSeekable = %v, want the deadline error", err)
	}
}

// TestBoundedPrefetchUsesTheBitrateWhenDurationIsUnknown: without a duration the
// water mark falls back to the resolved tier's byte rate.
func TestBoundedPrefetchUsesTheBitrateWhenDurationIsUnknown(t *testing.T) {
	data := bytes.Repeat([]byte{0x37}, 4<<20)
	upstream := &chunkedUpstream{body: data, chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}})
	session, err := cache.Open(context.Background(), remoteTrack())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)

	got := waitForStableBytes(t, cache, session)
	// The default stream quality is 320k, so the fallback is ~40 KiB/s.
	waterMark := int64(prefetchAheadSeconds) * 40 * 1024
	if got < waterMark {
		t.Fatalf("paused at %d bytes, want at least the %d-byte bitrate water mark", got, waterMark)
	}
	if got >= int64(len(data)) {
		t.Fatalf("downloaded the whole body without a duration (%d bytes)", got)
	}
}

// TestEnsureCompleteLiftsTheWaterMark: a promotion or download needs the whole
// file, so a complete request runs to EOF and marks the entry complete.
func TestEnsureCompleteLiftsTheWaterMark(t *testing.T) {
	data := bytes.Repeat([]byte{0x38}, 4<<20)
	upstream := &chunkedUpstream{body: data, chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	repo := newFakeStreamCacheRepo()
	cache := newTestCacheWithRepo(t,
		&fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}}, repo)
	track := prefetchTrack(300)
	session, err := cache.Open(context.Background(), track)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)
	waitForStableBytes(t, cache, session)

	done, err := cache.EnsureComplete(context.Background(), track)
	if err != nil {
		t.Fatalf("EnsureComplete: %v", err)
	}
	if done == nil {
		t.Fatal("EnsureComplete returned no completion channel")
	}
	// The session is still playing, so the channel closes only after Close; the
	// bytes and the row are the observable completion here.
	deadline := time.Now().Add(5 * time.Second)
	for {
		entry := repo.get(t, session.ID)
		if entry.Complete && entry.ReceivedBytes == int64(len(data)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("entry = complete:%v bytes:%d, want the whole %d-byte file",
				entry.Complete, entry.ReceivedBytes, len(data))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestHalfPlayedCompletesTheCache: crossing half the track lifts the water mark
// and downloads the rest; before that the bounded prefetch still holds.
func TestHalfPlayedCompletesTheCache(t *testing.T) {
	const durationSeconds = 300
	data := bytes.Repeat([]byte{0x39}, 4<<20)
	upstream := &chunkedUpstream{body: data, chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	repo := newFakeStreamCacheRepo()
	cache := newTestCacheWithRepo(t,
		&fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}}, repo)
	session, err := cache.Open(context.Background(), prefetchTrack(durationSeconds))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)
	waitForStableBytes(t, cache, session)

	cache.ReportProgress(session, float64(durationSeconds)*0.49)
	time.Sleep(150 * time.Millisecond)
	if entry := repo.get(t, session.ID); entry.Complete || entry.ReceivedBytes >= int64(len(data)) {
		t.Fatalf("49%% completed the cache (complete=%v bytes=%d)", entry.Complete, entry.ReceivedBytes)
	}

	cache.ReportProgress(session, float64(durationSeconds)*0.5)
	deadline := time.Now().Add(5 * time.Second)
	for {
		entry := repo.get(t, session.ID)
		if entry.Complete && entry.ReceivedBytes == int64(len(data)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("entry = complete:%v bytes:%d, want the whole %d-byte file",
				entry.Complete, entry.ReceivedBytes, len(data))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Further reports must not change a finished entry.
	cache.ReportProgress(session, float64(durationSeconds)*0.9)
	if entry := repo.get(t, session.ID); !entry.Complete || entry.ReceivedBytes != int64(len(data)) {
		t.Fatalf("entry changed after completion: %+v", entry)
	}
}

// TestHalfPlayedNeedsADuration: without a known duration there is no half-way
// point, so the bounded prefetch stays in charge.
func TestHalfPlayedNeedsADuration(t *testing.T) {
	data := bytes.Repeat([]byte{0x3a}, 4<<20)
	upstream := &chunkedUpstream{body: data, chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	repo := newFakeStreamCacheRepo()
	cache := newTestCacheWithRepo(t,
		&fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}}, repo)
	session, err := cache.Open(context.Background(), prefetchTrack(0))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)
	waitForStableBytes(t, cache, session)

	cache.ReportProgress(session, 10)
	time.Sleep(150 * time.Millisecond)

	cache.mu.Lock()
	state := cache.sessions[session.ID]
	cache.mu.Unlock()
	if state == nil {
		t.Fatalf("session %s missing", session.ID)
	}
	state.mu.Lock()
	halfPlayed := state.halfPlayed
	completeRequested := state.completeRequested
	state.mu.Unlock()
	if halfPlayed || completeRequested {
		t.Fatalf("half played without a duration (half=%v requested=%v)",
			halfPlayed, completeRequested)
	}
	if entry := repo.get(t, session.ID); entry.Complete {
		t.Fatalf("unknown duration completed the cache")
	}
}

// TestHalfPlayedOnAReopenedSession: seeking to 60% reloads the track; the new
// session completes as soon as the position is reported.
func TestHalfPlayedOnAReopenedSession(t *testing.T) {
	const durationSeconds = 300
	data := bytes.Repeat([]byte{0x3b}, 4<<20)
	upstream := &chunkedUpstream{body: data, chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	repo := newFakeStreamCacheRepo()
	cache := newTestCacheWithRepo(t,
		&fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}}, repo)
	first, err := cache.Open(context.Background(), prefetchTrack(durationSeconds))
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	waitForStableBytes(t, cache, first)
	cache.Close(first)

	session, err := cache.Open(context.Background(), prefetchTrack(durationSeconds))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer cache.Close(session)
	cache.ReportProgress(session, float64(durationSeconds)*0.6)

	deadline := time.Now().Add(5 * time.Second)
	for {
		entry := repo.get(t, session.ID)
		if entry.Complete && entry.ReceivedBytes == int64(len(data)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("entry = complete:%v bytes:%d, want the whole file after reopening at 60%%",
				entry.Complete, entry.ReceivedBytes)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestBoundedPrefetchCloseKeepsTheDownloadedBytes: stopping playback leaves the
// partial cache and its metadata for the next resume.
func TestBoundedPrefetchCloseKeepsTheDownloadedBytes(t *testing.T) {
	data := bytes.Repeat([]byte{0x39}, 4<<20)
	upstream := &chunkedUpstream{body: data, chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	repo := newFakeStreamCacheRepo()
	cache := newTestCacheWithRepo(t,
		&fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}}, repo)
	session, err := cache.Open(context.Background(), prefetchTrack(300))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got := waitForStableBytes(t, cache, session)
	cache.Close(session)

	entry := repo.get(t, session.ID)
	if entry.Complete {
		t.Fatal("a closed bounded download must not be marked complete")
	}
	if entry.ReceivedBytes == 0 || entry.ReceivedBytes > int64(len(data)) {
		t.Fatalf("stored bytes = %d, want the partial %d-byte download", entry.ReceivedBytes, got)
	}
	info, err := os.Stat(filepath.Join(cache.dir, entry.AudioPath))
	if err != nil {
		t.Fatalf("partial audio missing after close: %v", err)
	}
	if info.Size() < got {
		t.Fatalf("audio file = %d bytes, want at least the %d received", info.Size(), got)
	}
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

// qualityScriptResolver answers per requested tier and records the order, so a
// test can assert the fallback chain.
type qualityScriptResolver struct {
	mu    sync.Mutex
	refs  map[string]domain.StreamRef
	errs  map[string]error
	calls []string
}

func (r *qualityScriptResolver) Resolve(_ context.Context, _ *domain.TrackDTO, quality string) (domain.StreamRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, quality)
	if err, failed := r.errs[quality]; failed {
		return domain.StreamRef{}, err
	}
	return r.refs[quality], nil
}

func (r *qualityScriptResolver) tried() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func newFallbackCache(t *testing.T, resolver domain.StreamResolver, repo domain.StreamCacheRepository) *Cache {
	t.Helper()
	cache := NewCache(
		&config.Config{DataDir: t.TempDir()},
		repo,
		resolver,
		&fakeSettings{settings: &domain.AppSettings{
			StreamQuality: domain.QualityFlac,
			StreamCacheMB: domain.DefaultStreamCacheMB,
		}},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = cache.Stop(context.Background()) })
	return cache
}

// TestCacheFallsBackToTheNextQuality: a source without a flac link must stream
// 320k instead of failing, and the cache row records the tier that worked.
func TestCacheFallsBackToTheNextQuality(t *testing.T) {
	data := bytes.Repeat([]byte{0x11}, 4096)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/flac")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	}))
	defer upstream.Close()

	repo := newFakeStreamCacheRepo()
	resolver := &qualityScriptResolver{
		refs: map[string]domain.StreamRef{domain.Quality320k: {URL: upstream.URL + "/song.flac"}},
		errs: map[string]error{domain.QualityFlac: fmt.Errorf(
			"%w: no flac link", domain.ErrStreamQualityUnavailable)},
	}
	cache := newFallbackCache(t, resolver, repo)

	session, err := cache.Open(context.Background(), remoteTrack())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)

	if tried := resolver.tried(); len(tried) != 2 || tried[0] != domain.QualityFlac || tried[1] != domain.Quality320k {
		t.Fatalf("resolver tried %v, want [flac 320k]", tried)
	}
	entry := repo.get(t, session.ID)
	if entry.Quality != domain.Quality320k {
		t.Fatalf("cache quality = %q, want the actual tier 320k", entry.Quality)
	}
	if !entry.Complete {
		t.Fatal("the fallback stream never completed")
	}
}

// TestCacheReportsEveryTriedQualityWhenAllFail: the error names the attempted
// tiers, and no lower tier is retried past the configured floor.
func TestCacheReportsEveryTriedQualityWhenAllFail(t *testing.T) {
	noLink := func(quality string) error {
		return fmt.Errorf("%w: no %s link", domain.ErrStreamQualityUnavailable, quality)
	}
	resolver := &qualityScriptResolver{errs: map[string]error{
		domain.QualityFlac: noLink(domain.QualityFlac),
		domain.Quality320k: noLink(domain.Quality320k),
		domain.Quality128k: noLink(domain.Quality128k),
	}}
	cache := newFallbackCache(t, resolver, newFakeStreamCacheRepo())

	_, err := cache.Open(context.Background(), remoteTrack())
	if err == nil {
		t.Fatal("expected the stream to fail when every tier has no link")
	}
	if !strings.Contains(err.Error(), "no playable quality among") {
		t.Fatalf("error = %v, want the tried tiers", err)
	}
	if tried := resolver.tried(); len(tried) != 3 {
		t.Fatalf("resolver tried %v, want all three tiers", tried)
	}
}

// TestCacheDoesNotDegradeOnTransportFailures: a network failure is not a
// missing quality, so the chain must stop after the configured tier.
func TestCacheDoesNotDegradeOnTransportFailures(t *testing.T) {
	resolver := &qualityScriptResolver{errs: map[string]error{
		domain.QualityFlac: errors.New("connection reset"),
	}}
	cache := newFallbackCache(t, resolver, newFakeStreamCacheRepo())

	_, err := cache.Open(context.Background(), remoteTrack())
	if err == nil {
		t.Fatal("expected the stream to fail on a transport error")
	}
	if tried := resolver.tried(); len(tried) != 1 || tried[0] != domain.QualityFlac {
		t.Fatalf("resolver tried %v, want only the configured tier", tried)
	}
}

// TestCacheServesAllRangeForms covers the probe shapes FFmpeg sends against a
// seekable stream: bounded, open-ended and suffix ranges must return exactly
// the requested bytes with matching Content-Range, while out-of-range and
// multi-range requests must be 416.
func TestCacheServesAllRangeForms(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 512) // 8192 bytes
	total := int64(len(data))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "song.flac", time.Time{}, bytes.NewReader(data))
	}))
	defer upstream.Close()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: upstream.URL + "/song.flac"}})
	session, err := cache.Open(context.Background(), remoteTrack())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)

	tests := []struct {
		name       string
		header     string
		wantStatus int
		wantStart  int64
		wantEnd    int64
		wantBody   []byte
	}{
		{"bounded", "bytes=0-99", http.StatusPartialContent, 0, 99, data[:100]},
		{"open", "bytes=100-", http.StatusPartialContent, 100, total - 1, data[100:]},
		{"suffix", "bytes=-1024", http.StatusPartialContent, total - 1024, total - 1, data[total-1024:]},
		{"suffix wider than body", "bytes=-999999", http.StatusPartialContent, 0, total - 1, data},
		{"bounded clamped to body", "bytes=8000-999999", http.StatusPartialContent, 8000, total - 1, data[8000:]},
		{"start past end", "bytes=999999-", http.StatusRequestedRangeNotSatisfiable, 0, 0, nil},
		{"multi range", "bytes=0-1,4-5", http.StatusRequestedRangeNotSatisfiable, 0, 0, nil},
		{"zero suffix", "bytes=-0", http.StatusRequestedRangeNotSatisfiable, 0, 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, session.URL, nil)
			req.Header.Set("Range", tt.header)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET %q: %v", tt.header, err)
			}
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				t.Fatalf("read body: %v", readErr)
			}
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantStatus != http.StatusPartialContent {
				if got, want := resp.Header.Get("Content-Range"), fmt.Sprintf("bytes */%d", total); got != want {
					t.Fatalf("Content-Range = %q, want %q", got, want)
				}
				return
			}
			if got, want := resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-%d/%d", tt.wantStart, tt.wantEnd, total); got != want {
				t.Fatalf("Content-Range = %q, want %q", got, want)
			}
			if got, want := resp.Header.Get("Content-Length"), strconv.FormatInt(tt.wantEnd-tt.wantStart+1, 10); got != want {
				t.Fatalf("Content-Length = %q, want %q", got, want)
			}
			if !bytes.Equal(body, tt.wantBody) {
				t.Fatalf("body = %d bytes, want %d", len(body), len(tt.wantBody))
			}
		})
	}

	resp, err := http.Get(session.URL)
	if err != nil {
		t.Fatalf("GET without range: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want bytes", got)
	}
	if !bytes.Equal(body, data) {
		t.Fatalf("full body = %d bytes, want %d", len(body), len(data))
	}
}

// TestCacheRangeBlocksUntilBytesArrive: a bounded range extending past the
// downloaded watermark must not end there; the handler resumes once the
// download catches up, so a growing file never looks truncated.
func TestCacheRangeBlocksUntilBytesArrive(t *testing.T) {
	data := bytes.Repeat([]byte{0x5a}, 2<<20)
	const pauseAfter = 1280 * 1024
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/flac")
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
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: upstream.URL + "/song.flac"}})
	session, err := cache.Open(context.Background(), remoteTrack())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)

	const rangeStart, rangeEnd = 1000000, 1500000
	type result struct {
		body []byte
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, session.URL, nil)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", rangeStart, rangeEnd))
		resp, getErr := http.DefaultClient.Do(req)
		if getErr != nil {
			resultCh <- result{err: getErr}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, readErr := io.ReadAll(resp.Body)
		resultCh <- result{body: body, err: readErr}
	}()

	select {
	case got := <-resultCh:
		t.Fatalf("client finished early with %d bytes while the tail was paused: %v", len(got.body), got.err)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("read: %v", got.err)
		}
		want := data[rangeStart : rangeEnd+1]
		if !bytes.Equal(got.body, want) {
			t.Fatalf("body = %d bytes, want %d", len(got.body), len(want))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client never received the released range")
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
	// This test reads the whole body; ask for completion so the prefetch water
	// mark does not pause the download mid-read.
	if _, ensureErr := cache.EnsureComplete(context.Background(), remoteTrack()); ensureErr != nil {
		t.Fatalf("EnsureComplete: %v", ensureErr)
	}

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
		want     byteRange
		hasRange bool
		wantErr  bool
	}{
		{"", byteRange{}, false, false},
		{"bytes=0-", byteRange{start: 0, end: -1}, true, false},
		{"bytes=1234-", byteRange{start: 1234, end: -1}, true, false},
		{"bytes=100-200", byteRange{start: 100, end: 200}, true, false},
		{"bytes=-500", byteRange{suffixLen: 500}, true, false},
		{"bytes=-0", byteRange{}, false, true},
		{"bytes=", byteRange{}, false, true},
		{"bytes=200-100", byteRange{}, false, true},
		{"bytes=0-1,4-5", byteRange{}, false, true},
		{"items=0-", byteRange{}, false, true},
		{"bytes=abc-", byteRange{}, false, true},
	}
	for _, tt := range tests {
		got, hasRange, err := parseRangeHeader(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseRangeHeader(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if got != tt.want || hasRange != tt.hasRange {
			t.Errorf("parseRangeHeader(%q) = (%+v,%v), want (%+v,%v)", tt.in, got, hasRange, tt.want, tt.hasRange)
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
		if parsed, hasRange, err := parseRangeHeader(value); err == nil && hasRange && parsed.suffixLen == 0 {
			start = parsed.start
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

// delayedHeaderUpstream keeps the response headers back so a partially cached
// session can be opened before the source has answered.
type delayedHeaderUpstream struct {
	body  []byte
	delay time.Duration
}

func (u *delayedHeaderUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case <-time.After(u.delay):
	case <-r.Context().Done():
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(u.body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(u.body)
}

// TestCachedRequestRangeBoundary pins the "already on disk" decision, including
// the `end == received` edge: end is inclusive, so its exclusive bound is end+1.
func TestCachedRequestRangeBoundary(t *testing.T) {
	const cached = int64(1000)
	cases := []struct {
		name     string
		spec     byteRange
		hasRange bool
		total    int64
		start    int64
		end      int64
		ok       bool
	}{
		{"range ending at the cached edge", byteRange{start: 0, end: 999}, true, 4000, 0, 999, true},
		{"range one byte past the cached edge", byteRange{start: 0, end: 1000}, true, 4000, 0, 1000, false},
		{"explicit range without a known total", byteRange{start: 100, end: 199}, true, 0, 100, 199, true},
		{"explicit range past the cached bytes", byteRange{start: 900, end: 1099}, true, 0, 900, 1099, false},
		{"open range needs the total", byteRange{start: 0, end: -1}, true, 0, 0, 0, false},
		{"open range covered by a complete file", byteRange{start: 0, end: -1}, true, 1000, 0, 999, true},
		{"suffix range needs the total", byteRange{suffixLen: 100}, true, 0, 0, 0, false},
		{"suffix range inside the cached bytes", byteRange{suffixLen: 100}, true, 1000, 900, 999, true},
		{"plain request for a complete file", byteRange{}, false, 1000, 0, 999, true},
		{"plain request for a partial file", byteRange{}, false, 2000, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end, ok := cachedRequestRange(tc.spec, tc.hasRange, tc.total, cached)
			if ok != tc.ok || (ok && (start != tc.start || end != tc.end)) {
				t.Fatalf("cachedRequestRange = %d-%d/%v, want %d-%d/%v",
					start, end, ok, tc.start, tc.end, tc.ok)
			}
		})
	}
}

// TestCachedRangeIsServedWithoutTheUpstream: a request inside the buffered part
// must answer from disk even while the source has not sent its headers yet.
func TestCachedRangeIsServedWithoutTheUpstream(t *testing.T) {
	data := bytes.Repeat([]byte{0x3d}, 4<<20)
	srv := httptest.NewServer(&delayedHeaderUpstream{body: data, delay: 800 * time.Millisecond})
	defer srv.Close()

	repo := newFakeStreamCacheRepo()
	cache := newTestCacheWithRepo(t,
		&fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}}, repo)
	id := cacheKeyFor("wy", "42")
	prefilled := data[:2<<20]
	if err := os.WriteFile(filepath.Join(cache.dir, id+".audio"), prefilled, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.Upsert(context.Background(), &domain.StreamCacheEntry{
		CacheKey: id, Provider: "wy", ExternalID: "42", AudioPath: id + ".audio",
		Title: "Song", TitleKey: domain.NormalizationKey("Song"), Artist: "Singer",
		ArtistKey: domain.NormalizationKey("Singer"), DurationMs: 300000,
		TotalBytes: int64(len(data)), ReceivedBytes: int64(len(prefilled)),
	}); err != nil {
		t.Fatal(err)
	}
	session, err := cache.Open(context.Background(), prefetchTrack(300))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)

	req, err := http.NewRequest(http.MethodGet, cache.LocalURL()+"/stream/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-1023")
	started := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("range request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 400*time.Millisecond {
		t.Fatalf("cached range took %s, want it to bypass the delayed upstream", elapsed)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if !bytes.Equal(body, data[:1024]) {
		t.Fatalf("body = %d bytes, want the first 1024 cached bytes", len(body))
	}
}

// TestWaitReadyWaitsForAnUnansweredSession: a session opened from bytes already
// on disk returns from Open before the upstream response exists. The decoder
// must wait for it instead of loading a URL the proxy cannot answer yet.
func TestWaitReadyWaitsForAnUnansweredSession(t *testing.T) {
	data := bytes.Repeat([]byte{0x3c}, 4<<20)
	srv := httptest.NewServer(&delayedHeaderUpstream{body: data, delay: 200 * time.Millisecond})
	defer srv.Close()

	repo := newFakeStreamCacheRepo()
	cache := newTestCacheWithRepo(t,
		&fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}}, repo)
	id := cacheKeyFor("wy", "42")
	prefilled := data[:2<<20]
	if err := os.WriteFile(filepath.Join(cache.dir, id+".audio"), prefilled, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.Upsert(context.Background(), &domain.StreamCacheEntry{
		CacheKey: id, Provider: "wy", ExternalID: "42", AudioPath: id + ".audio",
		Title: "Song", TitleKey: domain.NormalizationKey("Song"), Artist: "Singer",
		ArtistKey: domain.NormalizationKey("Singer"), DurationMs: 300000,
		TotalBytes: int64(len(data)), ReceivedBytes: int64(len(prefilled)),
	}); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	session, err := cache.Open(context.Background(), prefetchTrack(300))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)
	if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
		t.Fatalf("Open took %s; the prefilled bytes should satisfy the prebuffer", elapsed)
	}

	if err := cache.WaitReady(context.Background(), session); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
		t.Fatalf("WaitReady returned after %s, want it to wait for the upstream headers", elapsed)
	}
}

func TestWaitReadyRejectsAMissingSession(t *testing.T) {
	cache := newTestCache(t, &fakeResolver{})
	err := cache.WaitReady(context.Background(), &Session{ID: "missing"})
	if !errors.Is(err, domain.ErrStreamUnavailable) {
		t.Fatalf("WaitReady error = %v, want ErrStreamUnavailable", err)
	}
}

// TestWaitHeadReturnsWhenTheHeadIsBuffered: the decoder gets a session whose
// local path and final size are known once enough bytes are on disk.
func TestWaitHeadReturnsWhenTheHeadIsBuffered(t *testing.T) {
	data := bytes.Repeat([]byte{0x3e}, 4<<20)
	upstream := &chunkedUpstream{body: data, chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}})
	session, err := cache.Open(context.Background(), prefetchTrack(300))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cache.WaitHead(ctx, session); err != nil {
		t.Fatalf("WaitHead: %v", err)
	}
	if session.Path == "" || !strings.HasSuffix(session.Path, ".audio") {
		t.Fatalf("session path = %q, want the cache file", session.Path)
	}
	if session.TotalBytes != int64(len(data)) {
		t.Fatalf("session total = %d, want %d", session.TotalBytes, len(data))
	}
}

// TestWaitHeadReportsAFailedSession: a failed download must not look like a
// slow head; the caller surfaces the error instead of hanging.
func TestWaitHeadReportsAFailedSession(t *testing.T) {
	cache := newTestCache(t, &fakeResolver{})
	s := &cacheSession{
		id:              "failed-session",
		progressChanged: make(chan struct{}, 1),
		done:            make(chan struct{}),
		total:           4 << 20,
		failed:          errors.New("upstream failed"),
	}
	cache.mu.Lock()
	cache.sessions[s.id] = s
	cache.mu.Unlock()
	defer func() {
		cache.mu.Lock()
		delete(cache.sessions, s.id)
		cache.mu.Unlock()
	}()

	err := cache.WaitHead(context.Background(), &Session{ID: s.id})
	if err == nil || !strings.Contains(err.Error(), "upstream failed") {
		t.Fatalf("WaitHead error = %v, want the download failure", err)
	}
}

// TestWaitHeadReturnsWhenTheContextIsCanceled: exit cancels the wait instead of
// blocking for the full head timeout.
func TestWaitHeadReturnsWhenTheContextIsCanceled(t *testing.T) {
	cache := newTestCache(t, &fakeResolver{})
	s := &cacheSession{
		id:              "slow-head",
		progressChanged: make(chan struct{}, 1),
		done:            make(chan struct{}),
		total:           4 << 20,
	}
	cache.mu.Lock()
	cache.sessions[s.id] = s
	cache.mu.Unlock()
	defer func() {
		cache.mu.Lock()
		delete(cache.sessions, s.id)
		cache.mu.Unlock()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	started := time.Now()
	err := cache.WaitHead(ctx, &Session{ID: s.id})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitHead error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("WaitHead returned after %s, want a prompt cancel", elapsed)
	}
}

// TestStopSessionIsBounded: a download goroutine that ignores cancellation
// cannot hold shutdown for its 30s timeout.
func TestStopSessionIsBounded(t *testing.T) {
	cache := newTestCache(t, &fakeResolver{})
	s := &cacheSession{
		id:              "stuck",
		progressChanged: make(chan struct{}, 1),
		done:            make(chan struct{}), // never closes
		cancel:          func() {},
		total:           1024,
	}
	started := time.Now()
	cache.stopSession(s)
	if elapsed := time.Since(started); elapsed > sessionStopWait+500*time.Millisecond {
		t.Fatalf("stopSession waited %s, want about %s", elapsed, sessionStopWait)
	}
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
	// The test reads the whole file; completion lifts the prefetch water mark.
	if _, ensureErr := cache.EnsureComplete(context.Background(), remoteTrack()); ensureErr != nil {
		t.Fatalf("EnsureComplete: %v", ensureErr)
	}

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

func TestOpenMatchesReorderedArtistCredits(t *testing.T) {
	repo := newFakeStreamCacheRepo()
	cache := newTestCacheWithRepo(t, &fakeResolver{err: fmt.Errorf("must not resolve")}, repo)

	data := bytes.Repeat([]byte{5}, 2<<20)
	key := cacheKeyFor("wy", "42")
	if err := os.WriteFile(filepath.Join(cache.dir, key+".audio"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	title := "Synthetic Fracture"
	if err := repo.Upsert(context.Background(), &domain.StreamCacheEntry{
		CacheKey:      key,
		Provider:      "wy",
		ExternalID:    "42",
		Title:         title,
		TitleKey:      domain.NormalizationKey(title),
		Artist:        "Sad Keyboard Guy; Gardens",
		ArtistKey:     domain.ArtistSetKey([]string{"Sad Keyboard Guy", "Gardens"}),
		DurationMs:    200000,
		AudioPath:     key + ".audio",
		TotalBytes:    int64(len(data)),
		ReceivedBytes: int64(len(data)),
		Complete:      true,
		CreatedAt:     1,
		LastUsed:      1,
	}); err != nil {
		t.Fatal(err)
	}

	kgTrack := &domain.TrackDTO{Track: domain.Track{
		ID: "t2", Title: title, Provider: "kg", ExternalID: "999", Duration: 200,
	}, Artists: []*domain.Artist{{Name: "Gardens"}, {Name: "Sad Keyboard Guy"}}}

	session, err := cache.Open(context.Background(), kgTrack)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close(session)
	if session.ID != key {
		t.Fatalf("session id = %s, want the wy entry %s despite the reordered credits", session.ID, key)
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

func TestDiscardEntryDefersUntilPlaybackCloses(t *testing.T) {
	upstream := &chunkedUpstream{body: bytes.Repeat([]byte{7}, 2<<20), chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	repo := newFakeStreamCacheRepo()
	cache := newTestCacheWithRepo(t,
		&fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}}, repo)
	ctx := context.Background()
	track := remoteTrack()

	session, err := cache.Open(ctx, track)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	closed := make(chan string, 1)
	cache.OnSessionClosed(func(provider, externalID string) {
		closed <- provider + "/" + externalID
	})

	if err := cache.DiscardEntry(ctx, track); !errors.Is(err, domain.ErrStreamDiscardDeferred) {
		t.Fatalf("DiscardEntry = %v, want ErrStreamDiscardDeferred", err)
	}
	// The cache owns the retry: the audio and the row stay until the session ends.
	entry := repo.get(t, session.ID)
	if _, statErr := os.Stat(filepath.Join(cache.dir, entry.AudioPath)); statErr != nil {
		t.Fatalf("the audio must stay while a session owns it: %v", statErr)
	}

	cache.Close(session)
	select {
	case got := <-closed:
		if got != "wy/42" {
			t.Fatalf("session-closed callback got %q, want wy/42", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Closing the session did not notify the promotion observer")
	}

	// The deferred delete ran as part of the close, without another caller.
	deadline := time.Now().Add(2 * time.Second)
	for {
		remaining, err := repo.GetByIdentity(ctx, "wy", "42")
		if err == nil && remaining == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("entry = %+v, err = %v, want the deferred delete", remaining, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, statErr := os.Stat(filepath.Join(cache.dir, entry.AudioPath)); !os.IsNotExist(statErr) {
		t.Fatalf("audio still on disk after the deferred delete: %v", statErr)
	}
}

func TestDiscardEntryAcceptsAnEntryWhoseFileIsGone(t *testing.T) {
	repo := newFakeStreamCacheRepo()
	cache := &Cache{
		dir: t.TempDir(), repo: repo, sessions: map[string]*cacheSession{},
		deferredDiscards: map[string]*domain.StreamCacheEntry{}, logger: slog.Default(),
	}
	ctx := context.Background()

	if err := repo.Upsert(ctx, &domain.StreamCacheEntry{
		CacheKey: "wy/42", Provider: "wy", ExternalID: "42", AudioPath: "wy-42.audio",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := cache.DiscardEntry(ctx, remoteTrack()); err != nil {
		t.Fatalf("DiscardEntry = %v, want success for a moved file", err)
	}
	if remaining, err := repo.GetByIdentity(ctx, "wy", "42"); err != nil || remaining != nil {
		t.Fatalf("entry = %+v, err = %v, want the row deleted", remaining, err)
	}
}

func TestHasActiveSessionReportsOnlyTheOpenIdentity(t *testing.T) {
	upstream := &chunkedUpstream{body: bytes.Repeat([]byte{8}, 2<<20), chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	repo := newFakeStreamCacheRepo()
	cache := newTestCacheWithRepo(t,
		&fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}}, repo)
	ctx := context.Background()

	session, err := cache.Open(ctx, remoteTrack())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !cache.HasActiveSession("wy", "42") {
		t.Fatal("HasActiveSession = false while a session is open")
	}
	if cache.HasActiveSession("kg", "7") {
		t.Fatal("HasActiveSession matched an unrelated identity")
	}

	cache.Close(session)
	if cache.HasActiveSession("wy", "42") {
		t.Fatal("HasActiveSession = true after Close")
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

// TestCacheRecordsOneTaskPerActualDownload: the first play downloads and leaves
// one finished task; a replay served from disk must not add another.
func TestCacheRecordsOneTaskPerActualDownload(t *testing.T) {
	data := bytes.Repeat([]byte("airmedy-stream-"), 512)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "song.mp3", time.Time{}, bytes.NewReader(data))
	}))
	defer upstream.Close()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: upstream.URL + "/song.mp3"}})
	tasks := newFakeStreamCacheTaskRepo()
	cache.SetTaskHistory(tasks)

	track := remoteTrack()
	session, err := cache.Open(context.Background(), track)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cache.Close(session)

	done := tasks.waitForState(t, domain.StreamCacheTaskStateDone)
	if done.Title != "Song" {
		t.Fatalf("task title = %q, want the cache entry title", done.Title)
	}
	if done.BytesTotal != int64(len(data)) || done.BytesDone != int64(len(data)) {
		t.Fatalf("task bytes = %d/%d, want %d/%d",
			done.BytesDone, done.BytesTotal, len(data), len(data))
	}

	session, err = cache.Open(context.Background(), track)
	if err != nil {
		t.Fatalf("second Open (cache hit): %v", err)
	}
	defer cache.Close(session)
	if rows := tasks.all(); len(rows) != 1 {
		t.Fatalf("a cache hit added a task row: %+v", rows)
	}
}

// TestCacheRecordsAFailedTask: a resolver failure must close the started row
// with its reason instead of leaving it active forever.
func TestCacheRecordsAFailedTask(t *testing.T) {
	cache := newTestCache(t, &fakeResolver{err: domain.ErrStreamUnavailable})
	tasks := newFakeStreamCacheTaskRepo()
	cache.SetTaskHistory(tasks)

	if _, err := cache.Open(context.Background(), remoteTrack()); err == nil {
		t.Fatal("Open should fail when the resolver has no source")
	}

	failed := tasks.waitForState(t, domain.StreamCacheTaskStateFailed)
	if failed.Error == "" {
		t.Fatal("failed task has no error message")
	}
	if failed.Title != "Song" {
		t.Fatalf("task title = %q, want the cache entry title", failed.Title)
	}
}

// TestCacheRecordsACanceledTaskOnClose: a track switch (or Stop) stops the
// session on purpose; the row is history, not an error.
func TestCacheRecordsACanceledTaskOnClose(t *testing.T) {
	data := bytes.Repeat([]byte{0x23}, 4<<20)
	upstream := &chunkedUpstream{body: data, chunk: 64 << 10}
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: srv.URL + "/song.mp3"}})
	tasks := newFakeStreamCacheTaskRepo()
	cache.SetTaskHistory(tasks)

	session, err := cache.Open(context.Background(), prefetchTrack(300))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cache.Close(session)

	canceled := tasks.waitForState(t, domain.StreamCacheTaskStateCanceled)
	if canceled.Error != "" {
		t.Fatalf("canceled task carries an error: %q", canceled.Error)
	}
	if canceled.FinishedAt == nil {
		t.Fatal("canceled task has no finish time")
	}
}

// TestCacheRecordsEverySession: each download session is its own history row,
// so evicting the cache and playing again leaves two.
func TestCacheRecordsEverySession(t *testing.T) {
	data := bytes.Repeat([]byte{0x22}, 4096)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "song.mp3", time.Time{}, bytes.NewReader(data))
	}))
	defer upstream.Close()

	cache := newTestCache(t, &fakeResolver{ref: domain.StreamRef{URL: upstream.URL + "/song.mp3"}})
	tasks := newFakeStreamCacheTaskRepo()
	cache.SetTaskHistory(tasks)

	track := remoteTrack()
	session, err := cache.Open(context.Background(), track)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	cache.Close(session)
	tasks.waitForState(t, domain.StreamCacheTaskStateDone)

	if err := cache.DiscardEntry(context.Background(), track); err != nil {
		t.Fatalf("DiscardEntry: %v", err)
	}
	session, err = cache.Open(context.Background(), track)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer cache.Close(session)

	deadline := time.Now().Add(2 * time.Second)
	for {
		rows := tasks.all()
		if len(rows) == 2 && rows[1].State == domain.StreamCacheTaskStateDone {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("second session did not create a finished task (rows: %+v)", rows)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
