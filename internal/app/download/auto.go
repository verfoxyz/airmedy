package download

import (
	"context"
	"errors"

	"airmedy/internal/domain"
)

// ShouldAutoDownload decides whether adding a track to a playlist should queue
// a download. It is a pure decision so the coordinator and the tests agree.
//
// The track must be remote (a local file needs nothing) and the setting must be
// on. A track that already has an active job needs nothing either: adding it to
// a second playlist before the first download finished must not start a race.
func ShouldAutoDownload(settings *domain.AppSettings, track *domain.TrackDTO, activeJob *domain.DownloadJob) bool {
	if settings == nil || !settings.AutoDownloadOnPlaylistAdd {
		return false
	}
	if track == nil || !track.IsRemote() {
		return false
	}
	return activeJob == nil
}

// OnRemoteTrackSaved is the single decision point for a saved remote row.
//
// A cached copy wins: the audio is already on disk, so the row only needs a
// durable promotion task (the worker moves it once playback releases the file).
// Without a cache the old behaviour applies — an explicit user save always
// downloads, a playlist add follows the auto-download setting.
//
// It returns the row state to show immediately: "downloading" when a job was
// queued or a promotion was requested, otherwise "saved" (no work in flight).
// Failures are returned so a caller can log them; they never undo the save that
// already happened.
func (s *Service) OnRemoteTrackSaved(ctx context.Context, ev domain.RemoteTrackSaved) (string, error) {
	track := ev.Track
	if track == nil || !track.IsRemote() {
		return "", nil
	}
	if active, err := s.jobs.GetActiveByTrackID(ctx, track.ID); err == nil && active != nil {
		return domain.CatalogRowStateDownloading, nil
	} else if err != nil {
		return "", err
	}

	if s.hasCachedAudio(ctx, track) {
		s.PromoteOnSave(ctx, track)
		// "downloading" is the UI's one task state: the search row does not
		// distinguish a promotion from a download.
		return domain.CatalogRowStateDownloading, nil
	}

	allowed := ev.Explicit
	if !allowed {
		settings, err := s.settings.GetSettings(ctx)
		if err != nil {
			return "", err
		}
		allowed = ShouldAutoDownload(settings, track, nil)
	}
	if !allowed {
		return domain.CatalogRowStateSaved, nil
	}

	coverURL := ev.CoverURL
	if coverURL == "" && s.coverAssets != nil {
		provider := domain.NormalizeProvider(track.Provider)
		if provider != "" && track.ExternalID != "" {
			if resolved, err := s.coverAssets.RemoteForIdentity(ctx, provider, track.ExternalID); err == nil {
				coverURL = resolved
			}
		}
	}
	if err := s.EnqueueWithCover(ctx, track.ID, ev.PlaylistID, s.DefaultQuality(ctx), coverURL); err != nil {
		if errors.Is(err, domain.ErrDownloadAlreadyQueued) {
			return domain.CatalogRowStateDownloading, nil
		}
		return "", err
	}
	return domain.CatalogRowStateDownloading, nil
}

// hasCachedAudio reports whether the stream cache already holds this identity's
// audio. CachedFileInfo never creates an entry and never goes online.
func (s *Service) hasCachedAudio(ctx context.Context, track *domain.TrackDTO) bool {
	if s.cached == nil {
		return false
	}
	_, _, _, found, err := s.cached.CachedFileInfo(ctx, track)
	if err != nil {
		s.logger.Warn("download: cannot inspect the stream cache", "track_id", track.ID, "error", err)
		return false
	}
	return found
}
