// Package downloader moves bytes from a resolved stream URL onto local disk.
//
// It owns three responsibilities the app layer must not re-implement:
//
//   - the HTTP conversation (http:// as well as https://, since the CDN hands
//     out plain-http URLs, plus the Referer/UA the CDN requires),
//   - resumability: bytes accumulate in .tmp/<name>.part and a retry continues
//     from the byte count already on disk when the server answers 206,
//   - validation: byte count, container magic and duration, so a truncated or
//     error-page download never reaches the library as a "ready" track.
//
// Every attempt re-resolves the URL (see domain.StreamResolver); this package
// never caches a StreamRef.
package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"airmedy/internal/domain"
)

// DefaultUserAgent is sent when the resolver does not supply one. The public
// NetEase resolver already does; this is the fallback for other providers.
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// DefaultReferer is required by the CDN; requests without it answer 403.
const DefaultReferer = "https://music.163.com/"

// durationTolerance is how far the decoded duration may differ from the
// duration the remote track advertised. Container headers and the remote
// catalogue round differently, so a small gap is expected.
const durationTolerance = 3 * time.Second

// Errors returned by Download. They are matched with errors.Is so callers can
// pick a retry policy without parsing messages.
var (
	// ErrPermanent marks a failure a retry cannot fix: 403/404/410, an
	// unsupported scheme, or a track that has no remote source at all. It is
	// domain.ErrDownloadPermanent so the app layer can classify it without
	// importing this package.
	ErrPermanent = domain.ErrDownloadPermanent
	// ErrRateLimited means the server asked us to slow down (429). Retrying
	// immediately burns more quota, so the caller backs off first.
	ErrRateLimited = errors.New("download rate limited")
	// ErrUnsupportedSignature means the body is not audio at all (usually an
	// HTML error page served with 200). It wraps domain.ErrCorruptDownload so
	// the shared classifier treats it as a permanent failure.
	ErrUnsupportedSignature = fmt.Errorf("%w: unrecognised audio container", domain.ErrCorruptDownload)
	// ErrDurationMismatch means the file decoded but is not the track that was
	// asked for. Deliberately not wrapped by ErrCorruptDownload: it cannot be
	// fixed by re-downloading the same wrong URL.
	ErrDurationMismatch = errors.New("decoded duration does not match the track")
)

// ProgressFunc is called as bytes land on disk. done counts bytes of the final
// file (including bytes kept from a previous partial attempt) and total is the
// expected size, or 0 when the server sent no Content-Length.
type ProgressFunc func(done, total int64)

// PropertiesReader reports the decoded duration of a finished file. The
// taglib-backed metadata extractor satisfies it; tests pass a stub so the
// downloader stays independent of cgo for its own unit tests.
type PropertiesReader interface {
	Duration(path string) (time.Duration, error)
}

// Target describes where one download should land.
type Target struct {
	// Dir is the directory the finished file is written to. The .tmp staging
	// directory lives next to it, so the final rename never crosses a volume.
	Dir string
	// Name is the finished file name including the extension.
	Name string
	// Quality selects the bitrate requested from the resolver.
	Quality string
	// ExpectedDuration is the duration the catalogue reported. Zero disables
	// the duration check.
	ExpectedDuration time.Duration
}

// Result reports what a successful Download produced.
type Result struct {
	Path      string
	Container Container
	Bytes     int64
}

// Downloader performs ranged HTTP downloads with validation.
type Downloader struct {
	client     *http.Client
	properties PropertiesReader
	userAgent  string
	referer    string
	// tmpDirName is the staging directory under Target.Dir.
	tmpDirName string
}

// Option customises a Downloader. Options exist for tests and for injecting the
// production HTTP client; the zero-value defaults are the production ones.
type Option func(*Downloader)

// WithHTTPClient replaces the HTTP client (timeouts, test transport).
func WithHTTPClient(c *http.Client) Option {
	return func(d *Downloader) { d.client = c }
}

// WithPropertiesReader installs the duration checker.
func WithPropertiesReader(p PropertiesReader) Option {
	return func(d *Downloader) { d.properties = p }
}

// WithUserAgent overrides the fallback User-Agent.
func WithUserAgent(ua string) Option {
	return func(d *Downloader) { d.userAgent = ua }
}

// WithReferer overrides the Referer sent with every request.
func WithReferer(referer string) Option {
	return func(d *Downloader) { d.referer = referer }
}

// New returns a Downloader. properties may be nil, which disables the duration
// check (byte count and magic are still enforced).
func New(properties PropertiesReader, opts ...Option) *Downloader {
	d := &Downloader{
		client:     &http.Client{Timeout: 0}, // bounded per attempt by ctx, not per request
		properties: properties,
		userAgent:  DefaultUserAgent,
		referer:    DefaultReferer,
		tmpDirName: DefaultTmpDirName,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// TmpDir returns the staging directory for a download root.
func (d *Downloader) TmpDir(dir string) string { return filepath.Join(dir, d.tmpDirName) }

// PartPath returns the partial file a download accumulates in.
func (d *Downloader) PartPath(dir, name string) string {
	return filepath.Join(d.TmpDir(dir), name+".part")
}

// FinalPath returns where a finished download lands.
func (d *Downloader) FinalPath(dir, name string) string { return filepath.Join(dir, name) }

// Download resolves nothing itself: ref is the freshly resolved stream and
// track supplies the provider/external id used for re-resolution on retry. A
// partial file left by a previous attempt is resumed when the server supports
// Range; otherwise the bytes are dropped and the body is fetched from zero.
//
// On failure the .part file is kept (unless the body was proven unusable) so the
// next attempt can resume. On success the bytes are validated, renamed onto the
// final path and reported.
func (d *Downloader) DownloadInto(ctx context.Context, track *domain.TrackDTO, ref domain.StreamRef, target Target, progress ProgressFunc) (Result, error) {
	if track == nil {
		return Result{}, fmt.Errorf("download: nil track")
	}
	if err := validateTarget(target); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(d.TmpDir(target.Dir), 0o755); err != nil {
		return Result{}, fmt.Errorf("download: create staging dir: %w", err)
	}

	partPath := d.PartPath(target.Dir, target.Name)
	finalPath := d.FinalPath(target.Dir, target.Name)

	offset, err := fileSize(partPath)
	if err != nil {
		return Result{}, fmt.Errorf("download: inspect partial file: %w", err)
	}

	if err := d.fetch(ctx, track, ref, target, partPath, offset, progress); err != nil {
		if !errors.Is(err, errRangeNotSatisfiable) && !errors.Is(err, errResumeMismatch) {
			return Result{}, err
		}
		// The server refused our offset, or the bytes on disk belong to a
		// different file: start clean rather than keep a corrupt prefix.
		if rmErr := os.Remove(partPath); rmErr != nil && !os.IsNotExist(rmErr) {
			return Result{}, fmt.Errorf("download: discard stale partial file: %w", rmErr)
		}
		if err := d.fetch(ctx, track, ref, target, partPath, 0, progress); err != nil {
			return Result{}, err
		}
	}

	return d.finalize(ctx, target, partPath, finalPath, progress)
}

// fetch performs one HTTP GET, appending to partPath starting at offset.
func (d *Downloader) fetch(ctx context.Context, track *domain.TrackDTO, ref domain.StreamRef, target Target, partPath string, offset int64, progress ProgressFunc) error {
	if err := checkScheme(ref.URL); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.URL, nil)
	if err != nil {
		return fmt.Errorf("download: build request for %s: %w", track.ID, err)
	}
	applyHeaders(req, ref.Headers, d.referer, d.userAgent)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		// Transport failures (timeout, reset, DNS) are retryable: wrap so a
		// caller matching ErrPermanent does not mistake them for one.
		return fmt.Errorf("download: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusRequestedRangeNotSatisfiable:
		return errRangeNotSatisfiable
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w: http 429", ErrRateLimited)
	case http.StatusForbidden,
		http.StatusUnauthorized,
		http.StatusNotFound,
		http.StatusGone:
		return fmt.Errorf("%w: http %d", ErrPermanent, resp.StatusCode)
	case http.StatusOK:
		if offset > 0 {
			// Server ignored Range: the response is the whole file.
			offset = 0
		}
	case http.StatusPartialContent:
		if offset > 0 {
			start, err := parseContentRangeStart(resp.Header.Get("Content-Range"))
			if err != nil {
				return errResumeMismatch
			}
			if start != offset {
				return errResumeMismatch
			}
		}
	default:
		return fmt.Errorf("download: unexpected status %d (%s)", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	flags := os.O_CREATE | os.O_WRONLY
	if offset == 0 {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_APPEND
	}
	f, err := os.OpenFile(partPath, flags, 0o644)
	if err != nil {
		return fmt.Errorf("download: open partial file: %w", err)
	}
	defer func() { _ = f.Close() }()

	total := contentLength(resp)
	if total > 0 && offset > 0 {
		total += offset
	}
	if progress != nil {
		progress(offset, total)
	}

	written, err := copyWithProgress(f, resp.Body, offset, total, progress)
	if err != nil {
		// Keep the .part file so the next attempt resumes from `written`.
		return fmt.Errorf("download: body interrupted after %d bytes: %w", written, err)
	}

	if expected := contentLength(resp); expected >= 0 && written-offset != expected {
		return fmt.Errorf("download: short body (%d of %d bytes): %w", written-offset, expected, domain.ErrCorruptDownload)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("download: flush partial file: %w", err)
	}
	return nil
}

// finalize validates the completed .part file and renames it into place.
func (d *Downloader) finalize(ctx context.Context, target Target, partPath, finalPath string, progress ProgressFunc) (Result, error) {
	container, err := ValidateFile(partPath)
	if err != nil {
		// Not audio: resuming would only append to a broken prefix.
		if rmErr := os.Remove(partPath); rmErr != nil && !os.IsNotExist(rmErr) {
			return Result{}, fmt.Errorf("download: discard invalid file: %w", rmErr)
		}
		return Result{}, err
	}

	if target.ExpectedDuration > 0 && d.properties != nil {
		if err := d.checkDuration(ctx, partPath, target.ExpectedDuration); err != nil {
			return Result{}, err
		}
	}

	info, err := os.Stat(partPath)
	if err != nil {
		return Result{}, fmt.Errorf("download: stat completed file: %w", err)
	}
	if err := os.MkdirAll(target.Dir, 0o755); err != nil {
		return Result{}, fmt.Errorf("download: create target dir: %w", err)
	}
	if err := os.Rename(partPath, finalPath); err != nil {
		return Result{}, fmt.Errorf("download: move into place: %w", err)
	}

	if progress != nil {
		progress(info.Size(), info.Size())
	}
	return Result{Path: finalPath, Container: container, Bytes: info.Size()}, nil
}

func (d *Downloader) checkDuration(ctx context.Context, path string, expected time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	got, err := d.properties.Duration(path)
	if err != nil {
		// A file whose container is intact but whose properties cannot be read
		// is worth keeping; the import step reports decode problems anyway.
		return nil
	}
	if got <= 0 {
		return nil
	}
	diff := got - expected
	if diff < 0 {
		diff = -diff
	}
	if diff > durationTolerance {
		return fmt.Errorf("%w: got %s, expected %s", ErrDurationMismatch, got.Round(time.Second), expected.Round(time.Second))
	}
	return nil
}

// ValidateFile checks the container magic of a finished file on disk.
func ValidateFile(path string) (Container, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("download: open for validation: %w", err)
	}
	defer func() { _ = f.Close() }()

	head := make([]byte, snapshotLen)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("download: read header: %w", err)
	}
	return DetectSignature(head[:n])
}

// ServedContainer is the container announced by Content-Type, or "" when the
// server was vague. Used to notice quality-tier substitutions.
func ServedContainer(contentType string) Container {
	switch {
	case strings.Contains(contentType, "audio/mpeg"):
		return ContainerMP3
	case strings.Contains(contentType, "audio/flac"), strings.Contains(contentType, "audio/x-flac"):
		return ContainerFLAC
	case strings.Contains(contentType, "audio/mp4"), strings.Contains(contentType, "audio/x-m4a"):
		return ContainerM4A
	case strings.Contains(contentType, "audio/ogg"):
		return ContainerOGG
	case strings.Contains(contentType, "audio/wav"):
		return ContainerWAV
	}
	return ""
}

// CleanOrphanedParts removes .part files under dir (recursively) that have not
// been touched for olderThan. Called once at startup: a crash leaves the staging
// directory behind, and a half file from last week is never worth resuming.
// It returns how many files were deleted.
func CleanOrphanedParts(dir string, olderThan time.Duration) (int, error) {
	removed := 0
	cutoff := time.Now().Add(-olderThan)

	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			// A vanished directory is not an error worth aborting the sweep for.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".part" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		removed++
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return removed, nil
		}
		return removed, fmt.Errorf("download: clean orphaned partial files under %s: %w", dir, err)
	}
	return removed, nil
}

// TmpDirName is the staging directory name under the download root.
const DefaultTmpDirName = ".tmp"

// errRangeNotSatisfiable / errResumeMismatch are internal: Download retries once
// from zero when it sees them.
var (
	errRangeNotSatisfiable = errors.New("server rejected the resume offset")
	errResumeMismatch      = errors.New("server answered a different range")
)

// validateTarget rejects a Target that would escape the download directory or
// fetch over a scheme we do not speak.
func validateTarget(target Target) error {
	if strings.TrimSpace(target.Dir) == "" {
		return fmt.Errorf("download: empty target directory: %w", ErrPermanent)
	}
	name := strings.TrimSpace(target.Name)
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("download: invalid file name %q: %w", target.Name, ErrPermanent)
	}
	// Name is a file name, never a path: a separator would let a hostile
	// title escape the download directory.
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("download: file name must not contain a separator: %q: %w", name, ErrPermanent)
	}
	return nil
}

// checkScheme accepts http:// as well as https://. The PoC found the CDN hands
// out plain-http URLs; an https-only client fails on the very first track.
func checkScheme(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("download: parse stream url: %w: %w", err, ErrPermanent)
	}
	switch parsed.Scheme {
	case "http", "https":
		return nil
	case "":
		return fmt.Errorf("download: stream url has no scheme: %w", ErrPermanent)
	default:
		return fmt.Errorf("download: unsupported scheme %q: %w", parsed.Scheme, ErrPermanent)
	}
}

func applyHeaders(req *http.Request, fromResolver map[string]string, referer, userAgent string) {
	// Resolver headers win, then the CDN defaults fill the gaps. Both are
	// required by music.126.net; a request missing either answers 403.
	for k, v := range fromResolver {
		if strings.TrimSpace(v) != "" {
			req.Header.Set(k, v)
		}
	}
	if req.Header.Get("Referer") == "" {
		req.Header.Set("Referer", referer)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", userAgent)
	}
	req.Header.Set("Accept", "*/*")
}

// contentLength returns the body length the server announced, or -1 when it
// sent none (the PoC found the CDN frequently omits it).
func contentLength(resp *http.Response) int64 {
	if resp.ContentLength >= 0 {
		return resp.ContentLength
	}
	return -1
}

func parseContentRangeStart(value string) (int64, error) {
	// "bytes 1024-2047/2048"
	value = strings.TrimSpace(value)
	rest, ok := strings.CutPrefix(value, "bytes ")
	if !ok {
		return 0, fmt.Errorf("malformed Content-Range %q", value)
	}
	startStr, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, fmt.Errorf("malformed Content-Range %q", value)
	}
	var start int64
	if _, err := fmt.Sscan(startStr, &start); err != nil {
		return 0, fmt.Errorf("malformed Content-Range %q: %w", value, err)
	}
	return start, nil
}

func copyWithProgress(dst io.Writer, src io.Reader, offset, total int64, progress ProgressFunc) (int64, error) {
	buf := make([]byte, 32*1024)
	written := offset
	lastReport := time.Time{}

	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return written, err
			}
			written += int64(n)
			// Throttle: the queue row is updated per callback, so reporting
			// every 32 KiB chunk would hammer SQLite on a fast link.
			if progress != nil && (lastReport.IsZero() || time.Since(lastReport) >= progressInterval) {
				progress(written, total)
				lastReport = time.Now()
			}
		}
		if readErr == io.EOF {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}

const progressInterval = 250 * time.Millisecond

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return info.Size(), nil
}
