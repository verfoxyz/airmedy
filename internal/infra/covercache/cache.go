// Package covercache downloads remote catalogue covers once and serves them
// from a host-local file the WebView can render.
//
// It is a storage adapter, not a search feature: the plugin only reports remote
// URLs, and a WebView cannot fetch a remote image through a plugin process. The
// cache therefore owns the network fetch, the file, the SQLite index and the
// LRU eviction. `app/catalog` reaches it through `domain.CatalogCoverCache`.
package covercache

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// maxCoverBytes caps one downloaded image. A list thumbnail is tens of
// kilobytes; 2 MiB is already far above a 500×500 JPEG and stops a misbehaving
// URL from filling the cache with one enormous body.
const maxCoverBytes = 2 << 20

// userAgent identifies the host's artwork fetcher. Image CDNs reject the Go
// default agent.
const userAgent = "airmedy/cover-fetch"

// defaultTimeout bounds one image download. The UI treats a miss as a
// placeholder, so a slow image host must not hold a request open.
const defaultTimeout = 20 * time.Second

// acceptedCoverTypes are the image types the cache will store. An error page
// served with 200 is not artwork.
var acceptedCoverTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
}

// Config assembles a cache. Dir and DB are required; everything else has a
// usable default so a test only sets what it exercises.
type Config struct {
	// Dir is the cover directory. Files are named by the URL hash plus extension.
	Dir string
	// DB is the application database that holds the cover_cache index.
	DB *sql.DB
	// Client overrides the HTTP client (tests use a server's client).
	Client *http.Client
	// Logger receives download and eviction warnings.
	Logger *slog.Logger
	// Now overrides the clock for tests that assert `last_used`.
	Now func() time.Time
}

// Cache is the persistent cover store.
type Cache struct {
	dir    string
	db     *sql.DB
	client *http.Client
	logger *slog.Logger
	now    func() time.Time

	// flightMu guards inflight. Two rows that resolved to the same image URL
	// share one download instead of racing on the same file and index row.
	flightMu sync.Mutex
	inflight map[string]*flight
}

// flight is one in-progress download that concurrent callers wait on.
type flight struct {
	done chan struct{}
	url  string
	err  error
}

// New creates the cache directory and returns the store. It does not download
// anything; the first Get does.
func New(cfg Config) (*Cache, error) {
	if strings.TrimSpace(cfg.Dir) == "" {
		return nil, fmt.Errorf("cover cache: directory is empty")
	}
	if cfg.DB == nil {
		return nil, fmt.Errorf("cover cache: database is required")
	}
	if err := os.MkdirAll(cfg.Dir, 0755); err != nil {
		return nil, fmt.Errorf("cover cache: create directory: %w", err)
	}

	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	return &Cache{
		dir:      cfg.Dir,
		db:       cfg.DB,
		client:   client,
		logger:   logger,
		now:      now,
		inflight: make(map[string]*flight),
	}, nil
}

// Get returns the local asset URL for a remote cover, downloading and indexing
// it on the first request.
//
// A cache hit costs one UPDATE of `last_used` and no network request. A file
// that disappeared (a user cleaned the directory) removes its index row and is
// downloaded again, so the UI never keeps a broken URL.
func (c *Cache) Get(ctx context.Context, remoteURL string, limitBytes int64) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(remoteURL))
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" {
		return "", fmt.Errorf("cover cache: unsupported URL %q", remoteURL)
	}

	hash := hashURL(parsed.String())
	if assetURL, ok, err := c.hit(ctx, hash); err != nil {
		return "", err
	} else if ok {
		return assetURL, nil
	}

	// One download per URL: several rows on a page can share an image.
	c.flightMu.Lock()
	if existing, ok := c.inflight[hash]; ok {
		c.flightMu.Unlock()
		<-existing.done
		return existing.url, existing.err
	}
	current := &flight{done: make(chan struct{})}
	c.inflight[hash] = current
	c.flightMu.Unlock()

	assetURL, err := c.fetchAndStore(ctx, parsed.String(), hash, limitBytes)

	c.flightMu.Lock()
	current.url = assetURL
	current.err = err
	delete(c.inflight, hash)
	c.flightMu.Unlock()
	close(current.done)

	return assetURL, err
}

// hit answers a cache lookup. A row whose file is missing is deleted so the
// next request re-downloads it.
func (c *Cache) hit(ctx context.Context, hash string) (string, bool, error) {
	var name string
	err := c.db.QueryRowContext(ctx,
		`SELECT path FROM cover_cache WHERE url_hash = ?`, hash,
	).Scan(&name)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("cover cache: read index: %w", err)
	}

	filePath := filepath.Join(c.dir, name)
	if _, statErr := os.Stat(filePath); statErr != nil {
		if os.IsNotExist(statErr) {
			if _, delErr := c.db.ExecContext(ctx,
				`DELETE FROM cover_cache WHERE url_hash = ?`, hash); delErr != nil {
				return "", false, fmt.Errorf("cover cache: drop missing row: %w", delErr)
			}
			return "", false, nil
		}
		return "", false, fmt.Errorf("cover cache: stat %q: %w", filePath, statErr)
	}

	if _, err := c.db.ExecContext(ctx,
		`UPDATE cover_cache SET last_used = ? WHERE url_hash = ?`,
		c.now().Unix(), hash,
	); err != nil {
		// A hit whose timestamp cannot be written still serves the file; the
		// eviction order is slightly stale but the image is correct.
		c.logger.Warn("cover cache: cannot update last_used", "hash", hash, "error", err)
	}
	return assetPath(name), true, nil
}

// fetchAndStore downloads one cover, writes the file first and only then upserts
// the index, then evicts if the new row pushed the cache over its limit.
func (c *Cache) fetchAndStore(ctx context.Context, remoteURL, hash string, limitBytes int64) (string, error) {
	data, mime, err := c.download(ctx, remoteURL)
	if err != nil {
		return "", err
	}

	ext := extensionFor(mime)
	name := hash + ext
	filePath := filepath.Join(c.dir, name)

	// A concurrent process may have written the same image; the content is
	// addressed by URL hash, so its file is equivalent.
	if _, statErr := os.Stat(filePath); statErr == nil {
		if err := c.upsert(ctx, hash, name, int64(len(data)), mime); err != nil {
			return "", err
		}
		c.evict(ctx, limitBytes)
		return assetPath(name), nil
	}

	if err := writeFileAtomically(c.dir, filePath, data); err != nil {
		return "", err
	}
	if err := c.upsert(ctx, hash, name, int64(len(data)), mime); err != nil {
		// The file is intentionally left in place: a row is the only thing that
		// makes it visible, and an orphan is harmless until the next cleanup.
		return "", err
	}
	c.evict(ctx, limitBytes)
	return assetPath(name), nil
}

// download performs the bounded, typed image request.
func (c *Cache) download(ctx context.Context, remoteURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, remoteURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("cover cache: build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", refererFor(remoteURL))

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("cover cache: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("cover cache: unexpected status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCoverBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("cover cache: read body: %w", err)
	}
	if len(data) > maxCoverBytes {
		return nil, "", fmt.Errorf("cover cache: image exceeds %d bytes", maxCoverBytes)
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("cover cache: empty image")
	}

	mime := http.DetectContentType(data)
	if !acceptedCoverTypes[mime] {
		return nil, "", fmt.Errorf("cover cache: unsupported content type %q", mime)
	}
	return data, mime, nil
}

// upsert writes the index row after the file exists.
func (c *Cache) upsert(ctx context.Context, hash, name string, bytes int64, mime string) error {
	if _, err := c.db.ExecContext(ctx,
		`INSERT INTO cover_cache (url_hash, path, bytes, mime, last_used)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(url_hash) DO UPDATE SET
		   path = excluded.path,
		   bytes = excluded.bytes,
		   mime = excluded.mime,
		   last_used = excluded.last_used`,
		hash, name, bytes, mime, c.now().Unix(),
	); err != nil {
		return fmt.Errorf("cover cache: write index: %w", err)
	}
	return nil
}

// evict removes the least recently used rows until the total is under 95% of
// the limit. The 5% headroom keeps every write from triggering an eviction.
func (c *Cache) evict(ctx context.Context, limitBytes int64) {
	if limitBytes <= 0 {
		return
	}

	var total int64
	if err := c.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(bytes), 0) FROM cover_cache`).Scan(&total); err != nil {
		c.logger.Warn("cover cache: cannot measure cache size", "error", err)
		return
	}
	if total <= limitBytes {
		return
	}

	target := limitBytes * 95 / 100
	rows, err := c.db.QueryContext(ctx,
		`SELECT url_hash, path, bytes FROM cover_cache ORDER BY last_used ASC`)
	if err != nil {
		c.logger.Warn("cover cache: cannot list rows for eviction", "error", err)
		return
	}

	type victim struct {
		hash  string
		name  string
		bytes int64
	}
	victims := make([]victim, 0)
	for rows.Next() {
		if total <= target {
			break
		}
		var item victim
		if err := rows.Scan(&item.hash, &item.name, &item.bytes); err != nil {
			c.logger.Warn("cover cache: cannot read eviction row", "error", err)
			break
		}
		victims = append(victims, item)
		total -= item.bytes
	}
	if err := rows.Err(); err != nil {
		c.logger.Warn("cover cache: eviction scan failed", "error", err)
	}
	_ = rows.Close()

	for _, item := range victims {
		if err := os.Remove(filepath.Join(c.dir, item.name)); err != nil && !os.IsNotExist(err) {
			c.logger.Warn("cover cache: cannot remove file", "file", item.name, "error", err)
			// The row stays so the file is retried later; deleting the row first
			// would make the image unreachable while still occupying disk.
			continue
		}
		if _, err := c.db.ExecContext(ctx,
			`DELETE FROM cover_cache WHERE url_hash = ?`, item.hash); err != nil {
			c.logger.Warn("cover cache: cannot remove row", "hash", item.hash, "error", err)
		}
	}
}

// hashURL is the cache key. It is the URL's sha256, not the file's, so a
// request for the same image never downloads it twice.
func hashURL(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(sum[:])
}

// assetPath is the WebView-facing path served by the asset handler.
func assetPath(name string) string {
	return "/cover-cache/" + name
}

// extensionFor maps an accepted MIME type onto the file extension.
func extensionFor(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	default:
		return ".jpg"
	}
}

// refererFor keeps the image CDN's anti-hotlink check satisfied. KuGou's image
// host expects its own site; NetEase's CDN accepts the music site.
func refererFor(remoteURL string) string {
	parsed, err := url.Parse(remoteURL)
	if err == nil && strings.Contains(parsed.Hostname(), "kugou.com") {
		return "https://www.kugou.com/"
	}
	return "https://music.163.com/"
}

// writeFileAtomically writes a temp file in the cache directory and renames it,
// so the asset handler never serves a half-written JPEG.
func writeFileAtomically(dir, filePath string, data []byte) error {
	tmp, err := os.CreateTemp(dir, "cover-*.tmp")
	if err != nil {
		return fmt.Errorf("cover cache: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cover cache: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cover cache: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, filePath); err != nil {
		return fmt.Errorf("cover cache: move into place: %w", err)
	}
	return nil
}

// compile-time proof the cache satisfies the app-facing port.
var _ interface {
	Get(ctx context.Context, remoteURL string, limitBytes int64) (string, error)
} = (*Cache)(nil)
