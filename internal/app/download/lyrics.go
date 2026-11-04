package download

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"airmedy/internal/domain"
)

// LyricsWriter fetches and stores lyrics for a track that owns its audio file.
//
// It is used by Service after the merge, so the lyric row is attached to the
// surviving track (the remote row is deleted by then). A nil writer means the
// deployment has no lyrics capability and the step is skipped.
type LyricsWriter interface {
	// FetchForTrack asks the configured providers for lyrics and persists them.
	// Returning nil lyrics with a nil error means "no lyrics available", which
	// is not a download failure.
	FetchForTrack(ctx context.Context, track *domain.TrackDTO) error
}

// LyricsFetcher is the slice of the lyrics service the download pipeline uses.
//
// Declared here rather than imported from the lyrics package so the download
// service depends on a behaviour, not on another app package. *lyrics.Service
// satisfies it through the adapter below.
type LyricsFetcher interface {
	// FetchFromProviders asks the enabled providers for a lyric for track.
	FetchFromProviders(ctx context.Context, track *domain.TrackDTO, enableLrclib, enableKugou bool) (*domain.Lyric, error)
}

// LyricsSettingsReader reads the two provider toggles. It is separate from
// SettingsReader because the lyric decision is about providers, while the
// download decision is about content, and a test may want to stub one alone.
type LyricsSettingsReader interface {
	GetSettings(ctx context.Context) (*domain.AppSettings, error)
}

// DownloadLyrics adapts the lyrics service onto the finalizer's LyricsWriter.
//
// Its whole job is the provider-enabled policy: the download pipeline asks
// "fetch lyrics for this track", and this decides which providers that means
// from the user's settings. The fetch itself stays in the lyrics package.
type DownloadLyrics struct {
	fetcher  LyricsFetcher
	settings LyricsSettingsReader
	logger   *slog.Logger
}

// NewDownloadLyrics wires the adapter. fetcher may be nil, in which case the
// finalizer skips the lyric step entirely.
func NewDownloadLyrics(fetcher LyricsFetcher, settings LyricsSettingsReader, logger *slog.Logger) *DownloadLyrics {
	if logger == nil {
		logger = slog.Default()
	}
	return &DownloadLyrics{fetcher: fetcher, settings: settings, logger: logger}
}

// FetchForTrack implements LyricsWriter.
//
// "No lyrics found" is not an error: a track without lyrics is a perfectly
// normal outcome, and turning it into a download failure would fail an
// otherwise successful download.
func (l *DownloadLyrics) FetchForTrack(ctx context.Context, track *domain.TrackDTO) error {
	if l.fetcher == nil || track == nil {
		return nil
	}

	enableLrclib, enableKugou := true, true
	if l.settings != nil {
		if settings, err := l.settings.GetSettings(ctx); err == nil && settings != nil {
			enableLrclib = settings.EnableLrclib
			enableKugou = settings.EnableKugou
		}
	}
	if !enableLrclib && !enableKugou {
		// The user turned both providers off: the download setting asks for
		// lyrics, but there is nowhere to get them from.
		l.logger.Debug("download: lyrics requested but no provider is enabled", "track_id", track.ID)
		return nil
	}

	lyric, err := l.fetcher.FetchFromProviders(ctx, track, enableLrclib, enableKugou)
	if err != nil {
		return fmt.Errorf("fetch lyrics for %s: %w", track.ID, err)
	}
	if lyric == nil || strings.TrimSpace(lyric.Content) == "" {
		l.logger.Debug("download: no lyrics found", "track_id", track.ID)
		return nil
	}
	l.logger.Info("download: lyrics saved", "track_id", track.ID, "source", lyric.Source)
	return nil
}

// Compile-time proof that the adapter still satisfies the service's port.
var _ LyricsWriter = (*DownloadLyrics)(nil)
