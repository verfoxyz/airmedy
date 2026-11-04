package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"airmedy/internal/domain"
)

// RemoteTracks is the slice of the download pipeline the actions need.
//
// It is a port so app/catalog never imports app/download: registering a remote
// identity, queueing it and reading the user's quality preference are three
// questions, and the download service already answers all three.
type RemoteTracks interface {
	// EnsureRemoteTrack registers the identity and returns its row, creating it
	// only when nothing represents it yet.
	EnsureRemoteTrack(ctx context.Context, spec domain.RemoteTrackSpec) (*domain.TrackDTO, error)
	// EnqueueWithCover queues one download and carries the cover URL the search
	// result supplied, so the finished file can embed the artwork. Empty is fine.
	// domain.ErrDownloadAlreadyQueued is success for an action: the user asked
	// for the download, and it is already happening.
	EnqueueWithCover(ctx context.Context, trackID, playlistID, quality, coverURL string) error
	// DefaultQuality reports the user's download quality.
	DefaultQuality(ctx context.Context) string
}

// SettingsReader is the slice of the settings service that decides whether a
// playlist add also downloads the audio.
type SettingsReader interface {
	GetSettings(ctx context.Context) (*domain.AppSettings, error)
}

// LocalTracks exposes one library row so an action can reuse a track the user
// already has on disk instead of registering a second remote identity.
type LocalTracks interface {
	GetTrack(ctx context.Context, trackID string) (*domain.TrackDTO, error)
}

// FavoritesWriter flags a track as a favorite.
//
// Favorites are a flag, not playlist membership: the virtual Favorites list is
// derived from it, so setting it is the whole action.
type FavoritesWriter interface {
	SetFavorite(ctx context.Context, trackID string, favorite bool) error
}

// PlaylistMembership adds an already registered track to one playlist.
type PlaylistMembership interface {
	AddTrack(ctx context.Context, playlistID, trackID string) error
}

// ActionRequest is the user's intent for one search row.
//
// The row's fields travel with it: a search result is never persisted, so the
// action carries everything the library needs to create the remote row. The
// identity (Provider, ExternalID) is the only required part.
type ActionRequest struct {
	Provider   string
	ExternalID string
	Title      string
	Artists    []string
	AlbumName  string
	// Duration is seconds; 0 when the search did not know it.
	Duration int
	// CoverURL is kept for the finished file's artwork. Empty is fine.
	CoverURL string
	// PlaylistID, when set, also adds the track to that playlist. "favorites" is
	// the favorite flag instead, handled by Favorite.
	PlaylistID string
	// Favorite sets the favorite flag. Both fields may be set.
	Favorite bool
	// LocalTrackID is the library row the search already matched this song to
	// (an imported file). When set, the action reuses that row: no remote row is
	// created and nothing is downloaded.
	LocalTrackID string
	// ForceDownload ignores LocalTrackID and downloads a fresh copy, which is the
	// user's explicit "I still want the provider's file".
	ForceDownload bool
}

// ActionOutcome is what the row should show after the action.
type ActionOutcome struct {
	TrackID string
	// State is a domain.CatalogRowState* value: "" or downloading after a save,
	// in_library when the audio was already on disk.
	State string
}

// Act performs the two things a search row can do: put the track in the library
// (always) and optionally flag it as a favorite or add it to a playlist.
//
// A favorite always downloads: flagging a track is the user saying "I want this
// song". A playlist add follows the "download when adding to a playlist"
// setting: with it off the track is registered and added to the playlist only,
// and the row reports CatalogRowStateSaved instead of pretending a download
// started. When both are requested, the favorite wins.
func (s *Service) Act(ctx context.Context, request ActionRequest) (ActionOutcome, error) {
	if s.library == nil {
		return ActionOutcome{}, fmt.Errorf("catalog action: no download pipeline is available")
	}
	if request.Favorite && s.favorites == nil {
		return ActionOutcome{}, fmt.Errorf("catalog action: no favorites writer is available")
	}

	provider := domain.NormalizeProvider(request.Provider)
	externalID := strings.TrimSpace(request.ExternalID)
	if provider == "" || externalID == "" {
		return ActionOutcome{}, fmt.Errorf("%w: provider and external id are required", domain.ErrCatalogActionInvalid)
	}
	if request.PlaylistID != "" && s.playlists == nil {
		return ActionOutcome{}, fmt.Errorf("catalog action: no playlist writer is available")
	}

	// An imported file has no provider identity, so the search row may already be
	// in the library through a different row. Reusing it is the whole point: a
	// second download of the same song is exactly the duplicate the user sees.
	localTrackID := ""
	if request.LocalTrackID != "" && !request.ForceDownload && s.localTracks != nil {
		local, err := s.localTracks.GetTrack(ctx, request.LocalTrackID)
		if err != nil {
			return ActionOutcome{}, fmt.Errorf("catalog action: load local track %s: %w", request.LocalTrackID, err)
		}
		// Gone or already remote: fall back to the normal registration path.
		if local != nil && !local.IsRemote() {
			localTrackID = local.ID
		}
	}
	if localTrackID != "" {
		if request.Favorite {
			if err := s.favorites.SetFavorite(ctx, localTrackID, true); err != nil {
				return ActionOutcome{}, fmt.Errorf("catalog action: favorite %s: %w", localTrackID, err)
			}
		}
		if request.PlaylistID != "" {
			if err := s.playlists.AddTrack(ctx, request.PlaylistID, localTrackID); err != nil {
				return ActionOutcome{}, fmt.Errorf("catalog action: add %s to playlist %s: %w",
					localTrackID, request.PlaylistID, err)
			}
		}
		return ActionOutcome{TrackID: localTrackID, State: domain.CatalogRowStateInLibrary}, nil
	}

	// A source that can search is not necessarily downloadable: nothing can fetch
	// a provider until an enabled plugin declares it and implements
	// stream.resolve. Refusing here keeps a doomed job out of the queue and gives
	// the user the actionable reason instead of a bare "unavailable".
	if s.resolver == nil || !s.resolver.CanResolve(provider) {
		return ActionOutcome{}, fmt.Errorf(
			"%w: no enabled source can download provider %q (install one that declares %q under \"providers\")",
			domain.ErrCatalogActionInvalid, provider, provider)
	}

	// Decide before touching anything: a settings read that fails must not leave
	// the track registered and in a playlist with no download.
	download := true
	if request.PlaylistID != "" && !request.Favorite {
		enabled, err := s.autoDownloadOnPlaylistAdd(ctx)
		if err != nil {
			return ActionOutcome{}, fmt.Errorf("catalog action: read the playlist download setting: %w", err)
		}
		download = enabled
	}

	// The search row may have been rendered with thumbnails off, so the plugin
	// omitted cover_url. The last URL seen for this identity still lets the
	// finished download embed artwork; if this process never saw one, ask the
	// plugin once more with covers forced on (the plan's "re-fetch on save").
	coverURL := strings.TrimSpace(request.CoverURL)
	if coverURL == "" {
		coverURL = s.cachedCover(domain.CatalogIdentity{Provider: provider, ExternalID: externalID})
	}
	if coverURL == "" && s.searcher != nil && strings.TrimSpace(request.Title) != "" {
		coverURL = s.fetchCoverForSave(ctx, provider, externalID, request.Title)
	}

	spec := domain.RemoteTrackSpec{
		Provider:   provider,
		ExternalID: externalID,
		Title:      strings.TrimSpace(request.Title),
		Artists:    normaliseArtists(request.Artists),
		AlbumName:  strings.TrimSpace(request.AlbumName),
		Duration:   request.Duration,
		CoverURL:   coverURL,
	}

	track, err := s.library.EnsureRemoteTrack(ctx, spec)
	if err != nil {
		return ActionOutcome{}, fmt.Errorf("catalog action: register %s/%s: %w", provider, externalID, err)
	}
	if track == nil {
		// Defensive: a nil row without an error would mean the download service
		// lost the track it just confirmed.
		return ActionOutcome{}, fmt.Errorf("catalog action: %s/%s registered without a row", provider, externalID)
	}

	if request.Favorite {
		if err := s.favorites.SetFavorite(ctx, track.ID, true); err != nil {
			return ActionOutcome{}, fmt.Errorf("catalog action: favorite %s: %w", track.ID, err)
		}
	}

	if request.PlaylistID != "" {
		// Membership first, so the playlist shows the track immediately instead
		// of after the download. The job's playlist id repeats it at finalize
		// time; that path is idempotent.
		if err := s.playlists.AddTrack(ctx, request.PlaylistID, track.ID); err != nil {
			return ActionOutcome{}, fmt.Errorf("catalog action: add %s to playlist %s: %w",
				track.ID, request.PlaylistID, err)
		}
	}

	// The audio is already on disk when a previous download finished, and queueing
	// it again would fail ("already available locally"). The row then reads as
	// in-library instead of downloading.
	if !track.IsRemote() {
		return ActionOutcome{TrackID: track.ID, State: domain.CatalogRowStateInLibrary}, nil
	}

	if !download {
		// Metadata-only save: the row is in the library and in the playlist, but
		// no job exists. It is not "downloading" and it must not look like a
		// failure; the user can still start the download explicitly.
		return ActionOutcome{TrackID: track.ID, State: domain.CatalogRowStateSaved}, nil
	}

	if err := s.library.EnqueueWithCover(ctx, track.ID, request.PlaylistID,
		s.library.DefaultQuality(ctx), spec.CoverURL); err != nil &&
		!errors.Is(err, domain.ErrDownloadAlreadyQueued) {
		return ActionOutcome{TrackID: track.ID}, fmt.Errorf("catalog action: enqueue %s: %w", track.ID, err)
	}

	return ActionOutcome{TrackID: track.ID, State: domain.CatalogRowStateDownloading}, nil
}

// fetchCoverForSave asks the source once more with covers forced on. It is the
// fallback for "the user hid thumbnails before this process ever saw a cover":
// the plugin otherwise omits cover_url, and the download would have no artwork.
// The lookup is best-effort; a failure leaves the file without a cover.
func (s *Service) fetchCoverForSave(ctx context.Context, provider, externalID, title string) string {
	result, err := s.searcher.SearchSource(ctx, provider, domain.PluginSearchRequest{
		Source:     provider,
		Query:      title,
		Limit:      domain.MaxCatalogSearchPageSize,
		Offset:     0,
		ForceCover: true,
	})
	if err != nil {
		s.logger.Debug("catalog: cover lookup for save failed",
			"provider", provider, "external_id", externalID, "error", err)
		return ""
	}
	for _, row := range result.Rows {
		if row.ExternalID != externalID {
			continue
		}
		if cover := strings.TrimSpace(row.CoverURLLarge); cover != "" {
			return cover
		}
		if cover := strings.TrimSpace(row.CoverURL); cover != "" {
			return cover
		}
	}
	return ""
}

// autoDownloadOnPlaylistAdd reports the user's "download when adding to a
// playlist" preference. A deployment without a settings reader keeps the
// historical behaviour (download), which is also the default every new install
// gets.
func (s *Service) autoDownloadOnPlaylistAdd(ctx context.Context) (bool, error) {
	if s.settings == nil {
		return true, nil
	}
	settings, err := s.settings.GetSettings(ctx)
	if err != nil {
		return false, err
	}
	if settings == nil {
		return true, nil
	}
	return settings.AutoDownloadOnPlaylistAdd, nil
}

// normaliseArtists trims each value and drops blanks, so a plugin that reported
// padded or empty artist names cannot create "  ; " in the row.
func normaliseArtists(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
