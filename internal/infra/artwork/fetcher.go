package artwork

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"airmedy/internal/domain"
)

// maxCoverBytes caps a cover download. Cover art is embedded in the audio file,
// so an unbounded body would let a misbehaving URL bloat a track by hundreds of
// megabytes; 8 MiB is far above any real 1500x1500 JPEG.
const maxCoverBytes = 8 << 20

// acceptedCoverTypes are the image types taglib can embed.
var acceptedCoverTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
}

// CoverFetcher downloads cover art for tagging a downloaded track.
type CoverFetcher struct {
	client    *http.Client
	userAgent string
}

var _ domain.ArtworkFetcher = (*CoverFetcher)(nil)

// NewCoverFetcher returns a fetcher with a bounded body and a hard timeout. The
// caller is the download pipeline, which must not stall on a slow image host.
func NewCoverFetcher() *CoverFetcher {
	return &CoverFetcher{
		client:    &http.Client{Timeout: 20 * time.Second},
		userAgent: "airmedy/cover-fetch",
	}
}

// Fetch implements domain.ArtworkFetcher.
func (f *CoverFetcher) Fetch(ctx context.Context, rawURL string) ([]byte, string, error) {
	if rawURL == "" {
		return nil, "", nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("fetch cover: build request: %w", err)
	}
	req.Header.Set("User-Agent", f.userAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch cover: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch cover: unexpected status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCoverBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("fetch cover: read body: %w", err)
	}
	if len(data) > maxCoverBytes {
		return nil, "", fmt.Errorf("fetch cover: image exceeds %d bytes", maxCoverBytes)
	}
	if len(data) == 0 {
		return nil, "", nil
	}

	mimeType := http.DetectContentType(data)
	if !acceptedCoverTypes[mimeType] {
		// An error page served with 200 must not be embedded as a "cover".
		return nil, "", fmt.Errorf("fetch cover: unsupported content type %q", mimeType)
	}
	return data, mimeType, nil
}
