package download

import (
	"context"
	"fmt"
	"os"
	"strings"

	"airmedy/internal/domain"
)

// PromoteCachedFile turns a file the stream cache already downloaded into a
// library track without touching the network. It reuses the normal finalize
// chain (tags, cover, lyrics, merge), then reports the row that owns the file.
//
// It is idempotent: when the remote identity already belongs to a local row, the
// existing row is returned and nothing is imported again. The caller deletes the
// stream-cache entry afterwards; this method moves the file into the library.
func (s *Service) PromoteCachedFile(
	ctx context.Context,
	track *domain.TrackDTO,
	localPath, quality string,
) (string, error) {
	if track == nil {
		return "", fmt.Errorf("promote: nil track")
	}
	if strings.TrimSpace(localPath) == "" {
		return "", fmt.Errorf("promote: no cached file for %s", track.ID)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// A previous promotion may already have carried the identity to the imported
	// row and deleted the remote one. A repeat call must not import a second file.
	if track.Provider != "" && track.ExternalID != "" && s.merger != nil {
		existing, err := s.merger.FindByRemoteIdentity(ctx, track.Provider, track.ExternalID)
		if err != nil {
			return "", fmt.Errorf("promote: look up %s/%s: %w", track.Provider, track.ExternalID, err)
		}
		if existing != nil && !existing.IsRemote() {
			return existing.ID, nil
		}
	}
	if !track.IsRemote() {
		return track.ID, nil
	}

	if _, err := os.Stat(localPath); err != nil {
		return "", fmt.Errorf("promote: cached file %s: %w", localPath, err)
	}

	settings, err := s.settings.GetSettings(ctx)
	if err != nil {
		return "", fmt.Errorf("promote: load settings: %w", err)
	}
	root, err := ResolveDownloadDir(settings.DownloadDir)
	if err != nil {
		return "", fmt.Errorf("promote: resolve download directory: %w", err)
	}
	content := domain.ResolveDownloadContent(settings)
	coverURL := ""
	if content.Cover {
		coverURL = s.coverURL(track.ID)
	}

	finalized, err := s.finalizer.Finalize(ctx, FinalizeInput{
		Path:        localPath,
		Root:        root,
		NamingOrder: settings.DownloadNamingOrder,
		Track:       track,
		Quality:     quality,
		PlaylistID:  nil,
		CoverURL:    coverURL,
		Content:     content,
	})
	if err != nil {
		return "", fmt.Errorf("promote %s: %w", track.ID, err)
	}

	// Merge exactly as a finished download does: the imported row survives and
	// inherits the remote identity, playlists, favorites and play count.
	survivorID := finalized.TrackID
	merged, mergeErr := s.merger.Merge(ctx, MergeInput{
		RemotePath:    track.Path,
		Provider:      track.Provider,
		ExternalID:    track.ExternalID,
		TargetPath:    finalized.TargetPath,
		TargetTrackID: finalized.TrackID,
	})
	if mergeErr != nil {
		s.logger.Warn("promote: merge after a cached file failed",
			"track_id", track.ID, "error", mergeErr)
	} else if merged.SurvivorTrackID != "" {
		survivorID = merged.SurvivorTrackID
	}

	if content.Lyrics && s.lyrics != nil {
		survivor, err := s.tracks.GetByID(ctx, survivorID)
		if err != nil {
			s.logger.Warn("promote: cannot load the row for lyrics", "track_id", survivorID, "error", err)
		} else if survivor != nil {
			if err := s.lyrics.FetchForTrack(ctx, survivor); err != nil {
				s.logger.Warn("promote: lyrics unavailable", "track_id", survivorID, "error", err)
			}
		}
	}

	if err := s.applyDecodedDuration(ctx, finalized.TrackID, finalized); err != nil {
		s.logger.Warn("promote: failed to store the decoded duration", "track_id", finalized.TrackID, "error", err)
	}
	s.forgetCover(track.ID)
	return survivorID, nil
}

// PromoteOnSave is called when a remote track joins a playlist or becomes a
// favorite. It marks the stream entry and does the work in the background, so a
// promotion failure can never fail the save.
//
// An active download job already produces the file at the configured quality;
// promotion would duplicate that work.
func (s *Service) PromoteOnSave(ctx context.Context, track *domain.TrackDTO, playlistID string) {
	if s.cached == nil || track == nil || !track.IsRemote() {
		return
	}
	if active, err := s.jobs.GetActiveByTrackID(ctx, track.ID); err == nil && active != nil {
		return
	}
	trackCopy := *track
	go s.promoteSaved(context.Background(), &trackCopy, playlistID)
}

// promoteSaved completes the cache, then promotes it or re-downloads at the
// configured quality when the cache is below it.
func (s *Service) promoteSaved(ctx context.Context, track *domain.TrackDTO, playlistID string) {
	if err := s.cached.MarkPendingPromote(ctx, track); err != nil {
		s.logger.Warn("promote: cannot flag the stream cache entry", "track_id", track.ID, "error", err)
		return
	}
	done, err := s.cached.EnsureComplete(ctx, track)
	if err != nil {
		s.logger.Warn("promote: cannot complete the stream cache", "track_id", track.ID, "error", err)
		return
	}
	if err := <-done; err != nil {
		s.logger.Warn("promote: stream cache did not complete", "track_id", track.ID, "error", err)
		return
	}

	path, cacheQuality, complete, found, err := s.cached.CachedFileInfo(ctx, track)
	if err != nil {
		s.logger.Warn("promote: cannot inspect the stream cache", "track_id", track.ID, "error", err)
		return
	}
	if !found || !complete {
		return
	}

	settings, err := s.settings.GetSettings(ctx)
	if err != nil {
		s.logger.Warn("promote: cannot read settings", "track_id", track.ID, "error", err)
		return
	}
	downloadQuality := domain.ResolveDownloadQuality(settings.DownloadQuality)
	if cachedQualityRank(cacheQuality) < downloadQualityRank(downloadQuality) {
		// The cached audio is below the download setting: fetch the better copy
		// through the normal queue; a finished download discards the cache.
		if err := s.Enqueue(ctx, track.ID, playlistID, downloadQuality); err != nil {
			s.logger.Info("promote: higher-quality download not queued", "track_id", track.ID, "error", err)
		}
		return
	}

	if _, err := s.PromoteCachedFile(ctx, track, path, cacheQuality); err != nil {
		s.logger.Warn("promote: cached file could not be promoted", "track_id", track.ID, "error", err)
		return
	}
	if err := s.cached.DiscardEntry(ctx, track); err != nil {
		s.logger.Warn("promote: cannot drop the promoted cache entry", "track_id", track.ID, "error", err)
	}
}

// cachedQualityRank treats an unknown cached tier as the lowest, so it never
// wins over a known download setting.
func cachedQualityRank(quality string) int {
	switch quality {
	case domain.Quality128k:
		return 1
	case domain.Quality320k:
		return 2
	case domain.QualityFlac:
		return 3
	}
	return 0
}

func downloadQualityRank(quality string) int {
	switch domain.ResolveDownloadQuality(quality) {
	case domain.Quality128k:
		return 1
	case domain.Quality320k:
		return 2
	}
	return 3
}
