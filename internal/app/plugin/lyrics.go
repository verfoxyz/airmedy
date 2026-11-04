package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"airmedy/internal/domain"
)

// lyricTimeline matches one LRC timestamp line ("[mm:ss]" or "[mm:ss.xx]").
// The host refuses a lyric without one because a plain text body cannot scroll,
// which is the whole point of fetching from the platform that produced the row.
var lyricTimeline = regexp.MustCompile(`^\[\d{1,3}:\d{2}`)

// LyricsAdapter routes lyric.fetch to the running plugin that owns the track's
// source.
//
// It is a domain.PluginLyricFetcher so app/lyrics can consume it without
// importing app/plugin. A handled=false answer means the built-in providers
// should answer; a handled=true answer with an error still falls back, so a
// platform outage never removes the built-in route.
type LyricsAdapter struct {
	registry *Service
	logger   *slog.Logger
}

// NewLyricsAdapter builds the plugin-backed lyric route.
func NewLyricsAdapter(registry *Service, logger *slog.Logger) *LyricsAdapter {
	if logger == nil {
		logger = slog.Default()
	}
	return &LyricsAdapter{registry: registry, logger: logger}
}

// FetchForTrack implements domain.PluginLyricFetcher.
func (a *LyricsAdapter) FetchForTrack(ctx context.Context, track *domain.TrackDTO) (*domain.Lyric, bool, error) {
	if a.registry == nil || track == nil {
		return nil, false, nil
	}
	provider := domain.NormalizeProvider(track.Provider)
	if provider == "" {
		// A local track has no online identity: only the built-in providers can
		// answer by title/artist.
		return nil, false, nil
	}

	host, ok := a.registry.HostForLyricSource(ctx, provider)
	if !ok {
		return nil, false, nil
	}

	request := domain.PluginLyricRequest{
		Provider:   provider,
		ExternalID: strings.TrimSpace(track.ExternalID),
		Title:      strings.TrimSpace(track.Title),
		Artists:    artistNames(track),
		Album:      strings.TrimSpace(track.RawAlbumName),
		Duration:   track.Duration,
	}

	var result domain.PluginLyricResult
	if err := host.Call(ctx, domain.MethodLyricFetch, request, &result); err != nil {
		a.logger.Warn("lyric.fetch failed; falling back to the built-in providers",
			"provider", provider, "track_id", track.ID, "error", lyricErrorKind(err))
		return nil, true, fmt.Errorf("plugin %q lyric.fetch for %s: %w",
			host.Manifest().ID, provider, err)
	}

	content := strings.TrimSpace(result.Lyric)
	if content == "" {
		return nil, true, fmt.Errorf("plugin %q returned no lyric for %s", host.Manifest().ID, provider)
	}
	if !hasLyricTimeline(content) {
		return nil, true, fmt.Errorf("plugin %q returned a lyric without a timeline for %s", host.Manifest().ID, provider)
	}

	source := strings.TrimSpace(result.Source)
	if source == "" {
		source = provider + "-plugin"
	}
	return &domain.Lyric{
		TrackID: track.ID,
		Content: content,
		Source:  source,
	}, true, nil
}

// hasLyricTimeline reports whether at least one line carries an LRC timestamp.
func hasLyricTimeline(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		if lyricTimeline.MatchString(strings.TrimSpace(line)) {
			return true
		}
	}
	return false
}

// artistNames prefers the resolved entity list and falls back to the raw tag
// value, the same order the built-in providers use.
func artistNames(track *domain.TrackDTO) []string {
	names := make([]string, 0, len(track.Artists))
	for _, artist := range track.Artists {
		if artist != nil && strings.TrimSpace(artist.Name) != "" {
			names = append(names, strings.TrimSpace(artist.Name))
		}
	}
	if len(names) == 0 && strings.TrimSpace(track.RawArtistNames) != "" {
		names = append(names, strings.TrimSpace(track.RawArtistNames))
	}
	return names
}

// lyricErrorKind extracts data.kind for the log line without exposing the whole
// RPC error as a sentence.
func lyricErrorKind(err error) string {
	var rpcErr *domain.RPCError
	if !errors.As(err, &rpcErr) || len(rpcErr.Data) == 0 {
		return ""
	}
	var data domain.PluginErrorData
	if json.Unmarshal(rpcErr.Data, &data) != nil {
		return ""
	}
	return domain.NormalizeProvider(data.Kind)
}

var _ domain.PluginLyricFetcher = (*LyricsAdapter)(nil)
