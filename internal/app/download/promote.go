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
// The request executor calls it only when no playback session owns the file and
// the player has not loaded the row; it is idempotent, so a retry after a crash
// returns the existing local row instead of importing a second file.
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
	coverURL, coverData, coverMIME := s.promoteCover(ctx, track, content.Cover)

	finalized, err := s.finalizer.Finalize(ctx, FinalizeInput{
		Path:        localPath,
		Root:        root,
		NamingOrder: settings.DownloadNamingOrder,
		Track:       track,
		Quality:     quality,
		PlaylistID:  nil,
		CoverURL:    coverURL,
		CoverData:   coverData,
		CoverMIME:   coverMIME,
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
	} else {
		if merged.SurvivorTrackID != "" {
			survivorID = merged.SurvivorTrackID
		}
		// The player must stop streaming the deleted row and continue from the
		// local file the promotion just created.
		s.emitTrackMerged(merged.RemovedTrackID, merged.SurvivorTrackID)
	}

	// The merge carried the streamed row's artwork key when it had one. If the
	// downloaded file had no embedded art and the old row had no key either,
	// save the cover the UI was showing from the cover cache.
	if err := s.ensurePromotedArtwork(ctx, survivorID, track); err != nil {
		s.logger.Warn("promote: could not attach the cached cover",
			"track_id", survivorID, "error", err)
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

// emitTaskUpdated tells the tasks page a request row changed.
func (s *Service) emitTaskUpdated() {
	s.sinkMu.RLock()
	sink := s.sink
	s.sinkMu.RUnlock()
	if sink != nil {
		sink.TaskUpdated()
	}
}

// NotifyTaskUpdated re-emits task:updated for another component (the media
// request worker) that changed a request row. The task page's row key is the
// stable request id, so a merge that replaces track_id must reach it.
func (s *Service) NotifyTaskUpdated() {
	s.emitTaskUpdated()
}

// promoteCover finds artwork for a streamed row without a search: the URL the
// search remembered, then the URL stored with the cached cover, and finally the
// cached bytes themselves. Bytes win because a promotion must embed the image
// the app has been showing even when the image host no longer answers.
func (s *Service) promoteCover(ctx context.Context, track *domain.TrackDTO, want bool) (string, []byte, string) {
	if !want || track == nil {
		return "", nil, ""
	}
	provider := domain.NormalizeProvider(track.Provider)
	externalID := strings.TrimSpace(track.ExternalID)

	remote := s.coverURL(track.ID)
	if remote == "" && s.coverAssets != nil && provider != "" && externalID != "" {
		resolved, err := s.coverAssets.RemoteForIdentity(ctx, provider, externalID)
		if err != nil {
			s.logger.Debug("promote: cover url lookup failed", "track_id", track.ID, "error", err)
		} else {
			remote = resolved
		}
	}
	if s.coverAssets != nil && provider != "" && externalID != "" {
		data, mime, err := s.coverAssets.BytesForIdentity(ctx, provider, externalID)
		if err == nil && len(data) > 0 {
			return remote, data, mime
		}
	}
	return remote, nil, ""
}

// ensurePromotedArtwork gives the promoted row (and its album) a cover when the
// imported file had no embedded art and the old remote row had no key. It reads
// the bytes the stream cover cache has been serving and stores them in the
// content-hash artwork cache, so the row keeps the image the UI was showing.
func (s *Service) ensurePromotedArtwork(ctx context.Context, survivorID string, remote *domain.TrackDTO) error {
	if survivorID == "" || s.tracks == nil || s.artworkCache == nil ||
		s.coverAssets == nil || s.merger == nil || remote == nil {
		return nil
	}
	survivor, err := s.tracks.GetByID(ctx, survivorID)
	if err != nil {
		return fmt.Errorf("load promoted row: %w", err)
	}
	if survivor == nil || strings.TrimSpace(survivor.ArtworkKey) != "" {
		return nil
	}
	provider := domain.NormalizeProvider(remote.Provider)
	externalID := strings.TrimSpace(remote.ExternalID)
	if provider == "" || externalID == "" {
		return nil
	}
	data, mime, err := s.coverAssets.BytesForIdentity(ctx, provider, externalID)
	if err != nil || len(data) == 0 {
		return err
	}
	key, err := s.artworkCache.Save(ctx, data, mime)
	if err != nil {
		return fmt.Errorf("save cached cover: %w", err)
	}
	if err := s.merger.CarryArtworkKey(ctx, survivorID, key); err != nil {
		return fmt.Errorf("attach cached cover: %w", err)
	}
	return nil
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
