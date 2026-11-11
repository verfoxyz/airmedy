package covercache

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airmedy/internal/infra/sqlite"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jpegBody builds bytes that `http.DetectContentType` recognises as a JPEG.
// The cache does not decode images, so the payload only needs a valid signature
// and a deterministic size.
func jpegBody(label string, size int) []byte {
	body := make([]byte, size)
	copy(body, []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00})
	if len(label) > 0 {
		copy(body[len(body)-len(label):], label)
	}
	return body
}

func discardingLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestCache opens a real migrated database so the 000089 migration is part
// of what the test proves.
func newTestCache(t *testing.T, dir string, client *http.Client, now func() time.Time) (*Cache, *sql.DB) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "airmedy.db")
	db, err := sqlite.NewDB(dbPath, discardingLogger())
	require.NoError(t, err, "migrations must create cover_cache")
	t.Cleanup(func() { _ = db.Close() })

	raw := db.DB.DB
	cache, err := New(Config{
		Dir:    dir,
		DB:     raw,
		Client: client,
		Logger: discardingLogger(),
		Now:    now,
	})
	require.NoError(t, err)
	return cache, raw
}

func TestGetDownloadsOnceAndUpdatesLastUsed(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(jpegBody("cover", 64))
	}))
	defer server.Close()

	clock := int64(1000)
	now := func() time.Time { clock++; return time.Unix(clock, 0) }
	cache, db := newTestCache(t, t.TempDir(), server.Client(), now)

	first, err := cache.Get(context.Background(), server.URL+"/a.jpg", "wy", "42", 10<<20)
	require.NoError(t, err)
	assert.Equal(t, int32(1), requests.Load())
	assert.Equal(t, "/cover-cache/"+hashURL(server.URL+"/a.jpg")+".jpg", first)

	var lastUsed int64
	require.NoError(t, db.QueryRow(`SELECT last_used FROM cover_cache`).Scan(&lastUsed))
	assert.Greater(t, lastUsed, int64(1000))

	second, err := cache.Get(context.Background(), server.URL+"/a.jpg", "wy", "42", 10<<20)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, int32(1), requests.Load(), "a hit must not touch the network")

	require.NoError(t, db.QueryRow(`SELECT last_used FROM cover_cache`).Scan(&lastUsed))
	assert.Greater(t, lastUsed, int64(1000))
}

func TestEvictionRemovesLeastRecentlyUsedDownTo95Percent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(jpegBody(r.URL.Path, 100))
	}))
	defer server.Close()

	clock := int64(0)
	now := func() time.Time { clock++; return time.Unix(clock, 0) }
	cache, db := newTestCache(t, t.TempDir(), server.Client(), now)

	// 300 bytes limit, four 100-byte images: the fourth write must evict two
	// because the target is 95% (285 bytes), not the hard limit.
	for index := 0; index < 4; index++ {
		_, err := cache.Get(context.Background(), fmt.Sprintf("%s/%d.jpg", server.URL, index), "wy", "42", 300)
		require.NoError(t, err)
	}

	var count int
	var total int64
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM cover_cache`).Scan(&count))
	require.NoError(t, db.QueryRow(`SELECT COALESCE(SUM(bytes), 0) FROM cover_cache`).Scan(&total))
	assert.Equal(t, 2, count)
	assert.LessOrEqual(t, total, int64(300*95/100))

	var oldest string
	require.NoError(t, db.QueryRow(
		`SELECT path FROM cover_cache ORDER BY last_used ASC LIMIT 1`).Scan(&oldest))
	assert.True(t, filepath.Base(oldest) != "", "the newest rows keep their files")
}

func TestPinnedCoverOutlivesUnpinnedOnes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(jpegBody(r.URL.Path, 100))
	}))
	defer server.Close()

	clock := int64(0)
	now := func() time.Time { clock++; return time.Unix(clock, 0) }
	cache, db := newTestCache(t, t.TempDir(), server.Client(), now)
	ctx := context.Background()

	_, err := cache.Get(ctx, server.URL+"/a.jpg", "wy", "1", 10<<20)
	require.NoError(t, err)
	_, err = cache.Get(ctx, server.URL+"/b.jpg", "wy", "2", 10<<20)
	require.NoError(t, err)

	require.NoError(t, cache.Pin(ctx, "wy", "2"))

	var provider, externalID string
	require.NoError(t, db.QueryRow(
		`SELECT provider, external_id FROM cover_cache WHERE url_hash = ?`,
		hashURL(server.URL+"/b.jpg"),
	).Scan(&provider, &externalID))
	assert.Equal(t, "wy", provider)
	assert.Equal(t, "2", externalID)

	// 200 bytes on disk, target 142: the unpinned row must go first and alone.
	cache.evict(ctx, 150)

	var remaining int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM cover_cache WHERE provider = 'wy' AND external_id = '2'`,
	).Scan(&remaining))
	assert.Equal(t, 1, remaining, "the pinned cover must survive")

	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM cover_cache WHERE provider = 'wy' AND external_id = '1'`,
	).Scan(&remaining))
	assert.Equal(t, 0, remaining, "the unpinned cover must be evicted first")

	// Unpinning releases it for the next eviction pass.
	require.NoError(t, cache.Unpin(ctx, "wy", "2"))
	cache.evict(ctx, 1)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM cover_cache`).Scan(&remaining))
	assert.Equal(t, 0, remaining)
}

func TestAssetForIdentityFindsTheCachedCover(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(jpegBody("cover", 64))
	}))
	defer server.Close()

	dir := t.TempDir()
	cache, _ := newTestCache(t, dir, server.Client(), func() time.Time { return time.Unix(1, 0) })
	ctx := context.Background()

	assetURL, err := cache.Get(ctx, server.URL+"/a.jpg", "wy", "42", 10<<20)
	require.NoError(t, err)

	found, err := cache.AssetForIdentity(ctx, "wy", "42")
	require.NoError(t, err)
	assert.Equal(t, assetURL, found)

	missing, err := cache.AssetForIdentity(ctx, "kg", "1")
	require.NoError(t, err)
	assert.Empty(t, missing)

	// A row whose file vanished must not point the WebView at a 404.
	require.NoError(t, os.Remove(filepath.Join(dir, filepath.Base(assetURL))))
	missing, err = cache.AssetForIdentity(ctx, "wy", "42")
	require.NoError(t, err)
	assert.Empty(t, missing)
}

func TestRemoteAndBytesForIdentityReuseTheCachedImage(t *testing.T) {
	body := jpegBody("cover", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	dir := t.TempDir()
	cache, db := newTestCache(t, dir, server.Client(), func() time.Time { return time.Unix(7, 0) })
	ctx := context.Background()

	assetURL, err := cache.Get(ctx, server.URL+"/a.jpg", "wy", "42", 10<<20)
	require.NoError(t, err)

	var storedURL string
	require.NoError(t, db.QueryRow(`SELECT url FROM cover_cache`).Scan(&storedURL))
	assert.Equal(t, server.URL+"/a.jpg", storedURL, "the row keeps the URL it was downloaded from")

	remote, err := cache.RemoteForIdentity(ctx, "wy", "42")
	require.NoError(t, err)
	assert.Equal(t, server.URL+"/a.jpg", remote)

	data, mime, err := cache.BytesForIdentity(ctx, "wy", "42")
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", mime)
	assert.Equal(t, body, data, "the cached bytes are returned without another download")

	missing, err := cache.RemoteForIdentity(ctx, "kg", "1")
	require.NoError(t, err)
	assert.Empty(t, missing)

	missingData, _, err := cache.BytesForIdentity(ctx, "kg", "1")
	require.NoError(t, err)
	assert.Empty(t, missingData)

	// A file that vanished means "not cached", not an error.
	require.NoError(t, os.Remove(filepath.Join(dir, filepath.Base(assetURL))))
	goneData, _, err := cache.BytesForIdentity(ctx, "wy", "42")
	require.NoError(t, err)
	assert.Empty(t, goneData)
}

func TestSharedCoverIsFoundByEveryIdentity(t *testing.T) {
	body := jpegBody("shared", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	cache, db := newTestCache(t, t.TempDir(), server.Client(), func() time.Time { return time.Unix(11, 0) })
	ctx := context.Background()
	remote := server.URL + "/shared.jpg"

	first, err := cache.Get(ctx, remote, "wy", "1336689106", 10<<20)
	require.NoError(t, err)
	// The same album image, requested by another track of that album.
	second, err := cache.Get(ctx, remote, "wy", "1336656391", 10<<20)
	require.NoError(t, err)
	assert.Equal(t, first, second, "the image is downloaded and stored once")

	data, mime, err := cache.BytesForIdentity(ctx, "wy", "1336656391")
	require.NoError(t, err)
	assert.Equal(t, body, data)
	assert.Equal(t, "image/jpeg", mime)

	var links int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM cover_cache_links WHERE url_hash = ?`, hashURL(remote),
	).Scan(&links))
	assert.Equal(t, 2, links, "both identities must be linked to the shared image")

	// Pinning either identity pins the shared URL.
	require.NoError(t, cache.Pin(ctx, "wy", "1336656391"))
	var pinned int
	require.NoError(t, db.QueryRow(
		`SELECT pinned FROM cover_cache WHERE url_hash = ?`, hashURL(remote),
	).Scan(&pinned))
	assert.Equal(t, 1, pinned)
	require.NoError(t, cache.Unpin(ctx, "wy", "1336656391"))
	require.NoError(t, db.QueryRow(
		`SELECT pinned FROM cover_cache WHERE url_hash = ?`, hashURL(remote),
	).Scan(&pinned))
	assert.Equal(t, 0, pinned)
}

func TestMissingFileDropsTheIndexRowAndRedownloads(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(jpegBody("cover", 32))
	}))
	defer server.Close()

	dir := t.TempDir()
	cache, db := newTestCache(t, dir, server.Client(), func() time.Time { return time.Unix(5, 0) })

	assetURL, err := cache.Get(context.Background(), server.URL+"/a.jpg", "wy", "42", 10<<20)
	require.NoError(t, err)
	name := filepath.Base(assetURL)
	require.NoError(t, os.Remove(filepath.Join(dir, name)))

	again, err := cache.Get(context.Background(), server.URL+"/a.jpg", "wy", "42", 10<<20)
	require.NoError(t, err)
	assert.Equal(t, assetURL, again)
	assert.Equal(t, int32(2), requests.Load(), "a missing file must be fetched again")

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM cover_cache`).Scan(&count))
	assert.Equal(t, 1, count, "the stale row is replaced, not duplicated")
}

func TestOversizedImageIsRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		body := jpegBody("big", maxCoverBytes+1)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	dir := t.TempDir()
	cache, db := newTestCache(t, dir, server.Client(), func() time.Time { return time.Unix(1, 0) })

	_, err := cache.Get(context.Background(), server.URL+"/big.jpg", "wy", "42", 10<<20)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM cover_cache`).Scan(&count))
	assert.Equal(t, 0, count)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "a failed download must not leave a file")
}

func TestNonImageResponseIsRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>busy</html>"))
	}))
	defer server.Close()

	cache, db := newTestCache(t, t.TempDir(), server.Client(), func() time.Time { return time.Unix(1, 0) })
	_, err := cache.Get(context.Background(), server.URL+"/busy", "wy", "42", 10<<20)
	require.Error(t, err)

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM cover_cache`).Scan(&count))
	assert.Equal(t, 0, count)
}

func TestConcurrentGetsShareOneDownload(t *testing.T) {
	var requests atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		<-release
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(jpegBody("cover", 32))
	}))
	defer server.Close()

	cache, _ := newTestCache(t, t.TempDir(), server.Client(), func() time.Time { return time.Unix(1, 0) })

	var wg sync.WaitGroup
	errs := make([]error, 3)
	for index := 0; index < 3; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, errs[index] = cache.Get(context.Background(), server.URL+"/a.jpg", "wy", "42", 10<<20)
		}(index)
	}

	// Give the three callers time to reach the single-flight guard, then let the
	// one in-flight download finish.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), requests.Load(), "concurrent lookups share one download")
}

func TestUnsupportedURLIsRejected(t *testing.T) {
	cache, _ := newTestCache(t, t.TempDir(), http.DefaultClient, func() time.Time { return time.Unix(1, 0) })
	_, err := cache.Get(context.Background(), "file:///etc/passwd", "wy", "42", 10<<20)
	require.Error(t, err)
}
