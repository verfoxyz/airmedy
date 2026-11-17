package download

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"airmedy/internal/domain"
)

// HasLocalTrack implements mediarequest.LocalTrackChecker: the identity resolves
// to a ready row whose file still exists. A row deleted with the file, or a
// stale row whose file was removed outside the app, reports false so a finished
// request can be re-opened.
func (s *Service) HasLocalTrack(ctx context.Context, provider, externalID string) (bool, error) {
	track, err := s.ResolveLocalTrack(ctx, provider, externalID)
	if err != nil {
		return false, err
	}
	return track != nil, nil
}

// ResolveLocalTrack implements mediarequest.LocalTrackChecker: the identity's
// row only counts when it is on disk and the file is still there.
func (s *Service) ResolveLocalTrack(
	ctx context.Context, provider, externalID string,
) (*domain.TrackDTO, error) {
	if s.merger == nil {
		return nil, nil
	}
	track, err := s.merger.FindByRemoteIdentity(ctx, domain.NormalizeProvider(provider), strings.TrimSpace(externalID))
	if err != nil || track == nil || track.IsRemote() {
		return nil, err
	}
	info, statErr := os.Stat(track.Path)
	if statErr != nil || info.IsDir() {
		return nil, nil
	}
	return track, nil
}

// FindLocalMatch implements mediarequest.LocalTrackChecker: the fallback for a
// request whose track row was merged away. Title + artist are the only identity
// it has left, and no duration is known, so the matcher scans the whole range.
func (s *Service) FindLocalMatch(
	ctx context.Context, title, artist string,
) (*domain.TrackDTO, error) {
	match, err := s.MatchLocalTrack(ctx, title, []string{artist}, 0)
	if err != nil || match == nil {
		return nil, err
	}
	return match.Track, nil
}

// LocateTrack implements domain.MediaRequestExecutor: the row currently
// registered for an online identity, remote or local.
func (s *Service) LocateTrack(
	ctx context.Context, provider, externalID string,
) (*domain.TrackDTO, error) {
	if s.merger == nil {
		return nil, nil
	}
	return s.merger.FindByRemoteIdentity(ctx, domain.NormalizeProvider(provider), externalID)
}

// Execute implements domain.MediaRequestExecutor: the request's execution
// detail lives here, next to the cache and queue rules it reuses.
//
// A complete cache whose tier meets (or was not asked against) the wanted
// quality is promoted in place — no job, no promotion table. Anything else goes
// through the normal download path. When playback still owns the cached file
// the request is deferred: it stays wanted and the worker retries after the
// session-close wake.
func (s *Service) Execute(
	ctx context.Context, track *domain.TrackDTO, desired, quality string,
) (string, error) {
	if track == nil {
		return domain.MediaRequestPhaseResolving, fmt.Errorf("media request: nil track")
	}
	// A job already in flight owns the work; starting a promotion beside it
	// would either be refused or duplicate the download.
	if active, err := s.jobs.GetActiveByTrackID(ctx, track.ID); err == nil && active != nil {
		return domain.MediaRequestPhaseDownloading, nil
	}
	if s.cached != nil {
		path, cachedQuality, complete, found, err := s.cached.CachedFileInfo(ctx, track)
		if err != nil {
			return domain.MediaRequestPhaseResolving,
				fmt.Errorf("media request: inspect the stream cache: %w", err)
		}
		if found && cachedQualitySufficient(cachedQuality, quality) {
			if !complete {
				// The cache already holds part of the file at an acceptable tier.
				// Finish it instead of downloading a second copy, but only while
				// playback owns the entry: with no player attached a normal
				// download is the path that reports failures and backs off.
				if s.cached.HasPlayingSession(
					domain.NormalizeProvider(track.Provider), strings.TrimSpace(track.ExternalID),
				) {
					if _, err := s.cached.EnsureComplete(ctx, track); err != nil {
						return domain.MediaRequestPhaseDownloading,
							fmt.Errorf("media request: complete the stream cache: %w", err)
					}
					return domain.MediaRequestPhaseDownloading, domain.ErrMediaRequestDeferred
				}
			} else {
				if s.promotionBlocked(ctx, track) {
					return domain.MediaRequestPhasePromoting, domain.ErrMediaRequestDeferred
				}
				if _, err := s.PromoteCachedFile(ctx, track, path, cachedQuality); err != nil {
					return domain.MediaRequestPhasePromoting, err
				}
				// The merge normally moves the identity onto the imported row. If it
				// failed, the request would stay running forever because LocateTrack
				// still sees the remote row; fail it so the retry downloads instead.
				located, err := s.LocateTrack(ctx, track.Provider, track.ExternalID)
				if err != nil {
					return domain.MediaRequestPhasePromoting, err
				}
				if located == nil || located.IsRemote() {
					return domain.MediaRequestPhasePromoting,
						fmt.Errorf("media request: promotion left %s/%s remote", track.Provider, track.ExternalID)
				}
				if err := s.cached.DiscardEntry(ctx, track); err != nil &&
					!errors.Is(err, domain.ErrStreamDiscardDeferred) {
					s.logger.Warn("media request: promoted cache cleanup failed",
						"track_id", track.ID, "error", err)
				}
				return domain.MediaRequestPhasePromoting, nil
			}
		}
	}
	wanted := domain.ResolveDownloadQuality(quality)
	if err := s.EnqueueWithCover(ctx, track.ID, "", wanted, ""); err != nil {
		if errors.Is(err, domain.ErrDownloadAlreadyQueued) {
			return domain.MediaRequestPhaseDownloading, nil
		}
		return domain.MediaRequestPhaseResolving, err
	}
	return domain.MediaRequestPhaseDownloading, nil
}

// promotionBlocked reports whether moving the cached file would break playback:
// a cache session still owns it, or the decoder has the remote row loaded (a
// load window or a retry). Both conditions clear on session close, which wakes
// the worker.
func (s *Service) promotionBlocked(ctx context.Context, track *domain.TrackDTO) bool {
	if s.cached != nil && s.cached.HasActiveSession(
		domain.NormalizeProvider(track.Provider), strings.TrimSpace(track.ExternalID),
	) {
		return true
	}
	return s.playingTrackID(ctx) == track.ID
}

// cachedQualitySufficient applies the spec's comparison: an empty wanted
// quality means "promote whatever tier the cache holds"; otherwise the cached
// tier must be at least the download setting's tier.
func cachedQualitySufficient(cached, wanted string) bool {
	if strings.TrimSpace(wanted) == "" {
		return true
	}
	return cachedQualityRank(cached) >= downloadQualityRank(wanted)
}

// Status implements domain.MediaRequestExecutor: it looks at the newest download
// job created after the request started running and reports a permanent
// failure. A newer successful run therefore clears an older failure.
//
// Jobs are capped history (500 finished rows), and only the worker's running
// requests reach this path.
func (s *Service) Status(
	ctx context.Context, track *domain.TrackDTO, since time.Time,
) (string, error) {
	if track == nil {
		return "", nil
	}
	jobs, err := s.jobs.List(ctx)
	if err != nil {
		return "", fmt.Errorf("media request status: list downloads: %w", err)
	}
	var newest *domain.DownloadJob
	for _, job := range jobs {
		if job == nil || job.TrackID != track.ID || !job.CreatedAt.After(since) {
			continue
		}
		if newest == nil || job.CreatedAt.After(newest.CreatedAt) {
			newest = job
		}
	}
	if newest == nil {
		return "", nil
	}
	switch newest.State {
	case domain.DownloadStateCanceled:
		return "canceled", nil
	case domain.DownloadStateFailed:
		return newest.LastError, nil
	}
	return "", nil
}
