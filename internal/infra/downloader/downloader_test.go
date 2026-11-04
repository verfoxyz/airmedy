package downloader

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airmedy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fixtures ---------------------------------------------------------------

// MPEG-1 Layer III, 32 kbps, 44.1 kHz, no padding — 104 bytes per frame,
// 1152 samples = 26.122 ms of audio. frameLen = 144 * 32000 / 44100.
const (
	testFrameLen     = 104
	testFrameSeconds = 1152.0 / 44100.0
	// 320 kbps is the quality tier used in the tests below; only the duration
	// matters for validation, so the fixture uses one fixed frame header.
	id3Header = "ID3\x04\x00\x00\x00\x00\x00\x00"
)

// buildMP3 returns a syntactically valid MP3 (ID3v2.4 header + LAME frames +
// Xing header) that decodes to roughly seconds of silence. taglib reads the
// Xing frame count, which is what makes the duration check testable without
// shipping a binary fixture.
func buildMP3(seconds float64) []byte {
	frames := int(seconds / testFrameSeconds)
	var buf bytes.Buffer
	buf.WriteString(id3Header)
	for i := 0; i < frames; i++ {
		frame := make([]byte, testFrameLen)
		// Sync word: 0xFFFB = MPEG-1, Layer III, no CRC.
		frame[0], frame[1] = 0xFF, 0xFB
		// Bitrate index 1 (32 kbps), sample-rate index 0 (44.1 kHz), no padding.
		frame[2] = 0x10
		frame[3] = 0x00
		if i == 0 {
			// Xing/Info header at the classic offset (after the 4-byte side info).
			copy(frame[36:], []byte("Xing"))
			// flags: frames field present
			binaryBE(frame[40:44], 0x1)
			binaryBE(frame[44:48], uint32(frames))
		}
		buf.Write(frame)
	}
	return buf.Bytes()
}

func binaryBE(dst []byte, v uint32) {
	dst[0] = byte(v >> 24)
	dst[1] = byte(v >> 16)
	dst[2] = byte(v >> 8)
	dst[3] = byte(v)
}

func testTrack() *domain.TrackDTO {
	return &domain.TrackDTO{Track: domain.Track{
		ID:         "track-1",
		Title:      "晴天",
		Provider:   "wy",
		ExternalID: "2668397359",
		Duration:   30,
	}}
}

type stubProperties struct {
	duration time.Duration
	err      error
}

func (s stubProperties) Duration(string) (time.Duration, error) { return s.duration, s.err }

func newTarget(dir string) Target {
	return Target{Dir: dir, Name: "晴天.mp3", Quality: domain.Quality320k, ExpectedDuration: 30 * time.Second}
}

// newTestHTTPServer wires a handler into an httptest server whose URL the test
// rewrites to plain http:// — the CDN really does serve http, and the downloader
// must accept it.
func newTestHTTPServer(t *testing.T, handler http.HandlerFunc) (url string, hits *atomic.Int32) {
	t.Helper()
	hits = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.Replace(srv.URL, "https://", "http://", 1), hits
}

func serveFull(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}
}

func newDownloader(opts ...Option) *Downloader {
	return New(stubProperties{duration: 30 * time.Second}, opts...)
}

// --- happy path -------------------------------------------------------------

func TestDownload_Success200(t *testing.T) {
	body := buildMP3(30)
	url, _ := newTestHTTPServer(t, serveFull(body))
	dir := t.TempDir()

	d := newDownloader()
	var lastDone, lastTotal int64
	res, err := d.DownloadInto(context.Background(), testTrack(), domain.StreamRef{URL: url}, newTarget(dir),
		func(done, total int64) { lastDone, lastTotal = done, total })

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "晴天.mp3"), res.Path)
	assert.Equal(t, ContainerMP3, res.Container)
	assert.EqualValues(t, len(body), res.Bytes)
	assert.EqualValues(t, len(body), lastDone)
	assert.EqualValues(t, len(body), lastTotal)

	onDisk, err := os.ReadFile(res.Path)
	require.NoError(t, err)
	assert.Equal(t, body, onDisk, "file must be byte-identical to the body")

	// The staging file must be gone: the rename is what makes it visible.
	_, statErr := os.Stat(d.PartPath(dir, "晴天.mp3"))
	assert.True(t, os.IsNotExist(statErr), "no .part file may survive a success")
}

func TestDownload_SendsRefererAndUserAgent(t *testing.T) {
	var gotReferer, gotUA, gotAccept string
	url, _ := newTestHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotReferer = r.Header.Get("Referer")
		gotUA = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		_, _ = w.Write(buildMP3(30))
	})

	// No headers from the resolver: the defaults must still be applied, since
	// a request without them answers 403 on the real CDN.
	_, err := newDownloader().DownloadInto(context.Background(), testTrack(), domain.StreamRef{URL: url}, newTarget(t.TempDir()), nil)
	require.NoError(t, err)
	assert.Equal(t, DefaultReferer, gotReferer)
	assert.Equal(t, DefaultUserAgent, gotUA)
	assert.Equal(t, "*/*", gotAccept)
}

func TestDownload_ResolverHeadersWin(t *testing.T) {
	var gotReferer, gotUA string
	url, _ := newTestHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotReferer = r.Header.Get("Referer")
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write(buildMP3(30))
	})

	ref := domain.StreamRef{URL: url, Headers: map[string]string{
		"Referer":    "https://example.test/",
		"User-Agent": "resolver-agent/1.0",
	}}
	_, err := newDownloader().DownloadInto(context.Background(), testTrack(), ref, newTarget(t.TempDir()), nil)
	require.NoError(t, err)
	assert.Equal(t, "https://example.test/", gotReferer)
	assert.Equal(t, "resolver-agent/1.0", gotUA)
}

// --- resume -----------------------------------------------------------------

func TestDownload_ResumesFromPartialFile(t *testing.T) {
	body := buildMP3(30)
	half := len(body) / 2

	var gotRange atomic.Value
	url, _ := newTestHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotRange.Store(r.Header.Get("Range"))
		http.ServeContent(w, r, "track.mp3", time.Time{}, bytes.NewReader(body))
	})

	dir := t.TempDir()
	d := newDownloader()
	part := d.PartPath(dir, "晴天.mp3")
	require.NoError(t, os.MkdirAll(filepath.Dir(part), 0o755))
	require.NoError(t, os.WriteFile(part, body[:half], 0o644))

	res, err := d.DownloadInto(context.Background(), testTrack(), domain.StreamRef{URL: url}, newTarget(dir), nil)
	require.NoError(t, err)

	assert.Equal(t, fmt.Sprintf("bytes=%d-", half), gotRange.Load())
	assert.EqualValues(t, len(body), res.Bytes)
	onDisk, err := os.ReadFile(res.Path)
	require.NoError(t, err)
	assert.Equal(t, body, onDisk, "resume must reassemble the same file")
}

func TestDownload_ServerIgnoresRangeStartsOver(t *testing.T) {
	body := buildMP3(30)
	// A plain 200 handler ignores Range entirely, which some CDN edges do.
	url, _ := newTestHTTPServer(t, serveFull(body))

	dir := t.TempDir()
	d := newDownloader()
	part := d.PartPath(dir, "晴天.mp3")
	require.NoError(t, os.MkdirAll(filepath.Dir(part), 0o755))
	require.NoError(t, os.WriteFile(part, body[:40], 0o644))

	_, err := d.DownloadInto(context.Background(), testTrack(), domain.StreamRef{URL: url}, newTarget(dir), nil)
	require.NoError(t, err)
	onDisk, err := os.ReadFile(filepath.Join(dir, "晴天.mp3"))
	require.NoError(t, err)
	assert.Equal(t, body, onDisk, "prefix must be truncated, not appended to")
}

func TestDownload_ResumeMismatchRestartsFromZero(t *testing.T) {
	body := buildMP3(30)
	// 206 with a Content-Range that does not start at our offset: the bytes on
	// disk are not a prefix of this file, so they must be discarded.
	url, hits := newTestHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(body)
			return
		}
		serveFull(body)(w, r)
	})

	dir := t.TempDir()
	d := newDownloader()
	part := d.PartPath(dir, "晴天.mp3")
	require.NoError(t, os.MkdirAll(filepath.Dir(part), 0o755))
	require.NoError(t, os.WriteFile(part, bytes.Repeat([]byte{0x00}, 50), 0o644))

	_, err := d.DownloadInto(context.Background(), testTrack(), domain.StreamRef{URL: url}, newTarget(dir), nil)
	require.NoError(t, err)
	assert.EqualValues(t, 2, hits.Load(), "must retry once without Range")

	onDisk, err := os.ReadFile(filepath.Join(dir, "晴天.mp3"))
	require.NoError(t, err)
	assert.Equal(t, body, onDisk)
}

// --- failure modes ----------------------------------------------------------

func TestDownload_InterruptedBodyKeepsPartFile(t *testing.T) {
	body := buildMP3(30)
	cut := len(body) / 3

	url, _ := newTestHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body[:cut])
		// Flush, then hard-close so the client sees a truncated body.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	})

	dir := t.TempDir()
	d := newDownloader()
	_, err := d.DownloadInto(context.Background(), testTrack(), domain.StreamRef{URL: url}, newTarget(dir), nil)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrPermanent)

	part := d.PartPath(dir, "晴天.mp3")
	info, statErr := os.Stat(part)
	require.NoError(t, statErr, "the partial file must survive so the retry can resume")
	assert.EqualValues(t, cut, info.Size())

	_, finalErr := os.Stat(d.FinalPath(dir, "晴天.mp3"))
	assert.True(t, os.IsNotExist(finalErr), "a failed download must never appear at the final path")
}

func TestDownload_403FailsImmediately(t *testing.T) {
	url, hits := newTestHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})

	_, err := newDownloader().DownloadInto(context.Background(), testTrack(), domain.StreamRef{URL: url}, newTarget(t.TempDir()), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermanent)
	assert.EqualValues(t, 1, hits.Load(), "a 403 must not be retried")
}

func TestDownload_404IsPermanent(t *testing.T) {
	url, _ := newTestHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	_, err := newDownloader().DownloadInto(context.Background(), testTrack(), domain.StreamRef{URL: url}, newTarget(t.TempDir()), nil)
	assert.ErrorIs(t, err, ErrPermanent)
}

func TestDownload_429IsRateLimitedNotPermanent(t *testing.T) {
	url, _ := newTestHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
	})
	_, err := newDownloader().DownloadInto(context.Background(), testTrack(), domain.StreamRef{URL: url}, newTarget(t.TempDir()), nil)
	assert.ErrorIs(t, err, ErrRateLimited)
	assert.NotErrorIs(t, err, ErrPermanent)
}

func TestDownload_TimeoutFails(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	url, _ := newTestHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := newDownloader().DownloadInto(ctx, testTrack(), domain.StreamRef{URL: url}, newTarget(t.TempDir()), nil)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrPermanent, "a timeout is worth retrying")
}

func TestDownload_RejectsNonHTTPScheme(t *testing.T) {
	_, err := newDownloader().DownloadInto(context.Background(), testTrack(),
		domain.StreamRef{URL: "ftp://example.test/track.mp3"}, newTarget(t.TempDir()), nil)
	assert.ErrorIs(t, err, ErrPermanent)
}

func TestDownload_HTMLBodyIsRejectedAndPartRemoved(t *testing.T) {
	url, _ := newTestHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body>版权受限</body></html>")
	})

	dir := t.TempDir()
	d := newDownloader()
	_, err := d.DownloadInto(context.Background(), testTrack(), domain.StreamRef{URL: url}, newTarget(dir), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnsupportedSignature)

	_, statErr := os.Stat(d.PartPath(dir, "晴天.mp3"))
	assert.True(t, os.IsNotExist(statErr), "an error page must not be kept for resuming")
}

func TestDownload_DurationMismatchFails(t *testing.T) {
	url, _ := newTestHTTPServer(t, serveFull(buildMP3(60)))

	d := New(stubProperties{duration: 60 * time.Second})
	target := newTarget(t.TempDir()) // expects 30s
	_, err := d.DownloadInto(context.Background(), testTrack(), domain.StreamRef{URL: url}, target, nil)
	assert.ErrorIs(t, err, ErrDurationMismatch)
	assert.NotErrorIs(t, err, domain.ErrCorruptDownload)
}

func TestDownload_DurationWithinTolerancePasses(t *testing.T) {
	url, _ := newTestHTTPServer(t, serveFull(buildMP3(30)))
	// 2.5s off is inside the ±3s tolerance.
	d := New(stubProperties{duration: 32500 * time.Millisecond})
	_, err := d.DownloadInto(context.Background(), testTrack(), domain.StreamRef{URL: url}, newTarget(t.TempDir()), nil)
	assert.NoError(t, err)
}

func TestDownload_NilPropertiesSkipsDurationCheck(t *testing.T) {
	url, _ := newTestHTTPServer(t, serveFull(buildMP3(30)))
	d := New(nil)
	_, err := d.DownloadInto(context.Background(), testTrack(), domain.StreamRef{URL: url}, newTarget(t.TempDir()), nil)
	assert.NoError(t, err)
}

func TestDownload_RejectsPathInName(t *testing.T) {
	target := newTarget(t.TempDir())
	target.Name = "../escape.mp3"
	_, err := newDownloader().DownloadInto(context.Background(), testTrack(), domain.StreamRef{URL: "http://example.test/a"}, target, nil)
	assert.ErrorIs(t, err, ErrPermanent)
}

// --- signature detection ----------------------------------------------------

func TestDetectSignature(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		want Container
		bad  bool
	}{
		{"id3", []byte("ID3\x04\x00\x00\x00\x00\x00\x00"), ContainerMP3, false},
		{"flac", []byte("fLaC\x00\x00\x00\x22"), ContainerFLAC, false},
		{"m4a", []byte{0x00, 0x00, 0x00, 0x20, 'f', 't', 'y', 'p', 'M', '4', 'A', ' '}, ContainerM4A, false},
		{"mpeg frame sync", []byte{0xFF, 0xFB, 0x90, 0x00}, ContainerMP3, false},
		{"ogg", []byte("OggS\x00\x02"), ContainerOGG, false},
		{"wav", []byte("RIFF\x24\x00\x00\x00WAVE"), ContainerWAV, false},
		{"html", []byte("<html><bo"), "", true},
		{"empty", nil, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DetectSignature(tc.head)
			if tc.bad {
				assert.ErrorIs(t, err, ErrUnsupportedSignature)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// --- orphan cleanup ---------------------------------------------------------

func TestCleanOrphanedParts(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, DefaultTmpDirName)
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "nested"), 0o755))

	oldPart := filepath.Join(tmp, "old.mp3.part")
	freshPart := filepath.Join(tmp, "nested", "fresh.mp3.part")
	keeper := filepath.Join(tmp, "keep.mp3")

	require.NoError(t, os.WriteFile(oldPart, []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(freshPart, []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(keeper, []byte("x"), 0o644))

	stale := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(oldPart, stale, stale))

	removed, err := CleanOrphanedParts(dir, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)

	_, err = os.Stat(oldPart)
	assert.True(t, os.IsNotExist(err), "a .part older than 24h must be removed")
	for _, keep := range []string{freshPart, keeper} {
		_, err := os.Stat(keep)
		assert.NoError(t, err, "%s must survive", keep)
	}
}

func TestCleanOrphanedParts_MissingDirIsNotAnError(t *testing.T) {
	removed, err := CleanOrphanedParts(filepath.Join(t.TempDir(), "does-not-exist"), time.Hour)
	require.NoError(t, err)
	assert.Zero(t, removed)
}
