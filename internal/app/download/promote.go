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

// PromoteOnSave records a durable promotion request when a remote track joins a
// playlist or becomes a favorite.
//
// Promotion moves the cache file, so the work cannot run while playback owns it.
// Nothing is moved here: the flag and the history row are written immediately
// (a request survives a restart) and the worker retries once the session closes.
//
// An active download job already produces the file at the configured quality;
// promotion would duplicate that work.
func (s *Service) PromoteOnSave(ctx context.Context, track *domain.TrackDTO) {
	if s.cached == nil || track == nil || !track.IsRemote() {
		return
	}
	if active, err := s.jobs.GetActiveByTrackID(ctx, track.ID); err == nil && active != nil {
		return
	}
	if err := s.cached.MarkPendingPromote(ctx, track); err != nil {
		s.logger.Warn("promote: cannot flag the stream cache entry", "track_id", track.ID, "error", err)
		return
	}
	if s.promotionTask != nil {
		task := &domain.PromotionTask{
			TrackID:    track.ID,
			Provider:   domain.NormalizeProvider(track.Provider),
			ExternalID: strings.TrimSpace(track.ExternalID),
			Title:      track.Title,
			Artist:     tagArtist(track),
		}
		if _, err := s.promotionTask.EnsureQueued(ctx, task); err != nil {
			s.logger.Warn("promote: cannot record the request", "track_id", track.ID, "error", err)
		} else {
			s.emitTaskUpdated()
		}
	}
	s.WakeCacheMaintenance()
}

// WakeCacheMaintenance asks the maintenance worker to look for pending work
// now. It never blocks: a wake already queued covers the request.
func (s *Service) WakeCacheMaintenance() {
	if s.maintenanceWake == nil {
		return
	}
	select {
	case s.maintenanceWake <- struct{}{}:
	default:
	}
}

// maintenanceLoop sweeps once on start (a request made before a restart, or one
// deferred by playback, must finish without user action), then on every wake
// and every promotionSweepInterval.
func (s *Service) maintenanceLoop(stop, done chan struct{}) {
	defer close(done)

	s.RunCacheMaintenanceSweep(context.Background())
	timer := time.NewTimer(maintenanceSweepInterval)
	defer timer.Stop()

	for {
		select {
		case <-stop:
			return
		case <-s.maintenanceWake:
		case <-timer.C:
		}
		s.RunCacheMaintenanceSweep(context.Background())
		timer.Reset(maintenanceSweepInterval)
	}
}

// RunCacheMaintenanceSweep finishes deferred cache work: pending deletions
// first (a finished download or a moved promotion), then pending promotions.
// It is single-flight, so a session closing during a sweep cannot start a
// second one.
func (s *Service) RunCacheMaintenanceSweep(ctx context.Context) {
	if s.cached == nil || s.promotionTask == nil {
		return
	}
	if !s.maintenanceSweeping.CompareAndSwap(false, true) {
		return
	}
	defer s.maintenanceSweeping.Store(false)

	discards, err := s.cached.PendingDiscards(ctx)
	if err != nil {
		s.logger.Warn("promote: cannot list pending discards", "error", err)
	}
	for _, entry := range discards {
		if ctx.Err() != nil {
			return
		}
		s.sweepDiscard(ctx, entry)
	}

	promotions, err := s.cached.PendingPromotions(ctx)
	if err != nil {
		s.logger.Warn("promote: cannot list pending entries", "error", err)
		return
	}
	for _, entry := range promotions {
		if ctx.Err() != nil {
			return
		}
		s.sweepPromotion(ctx, entry)
	}
}

// sweepDiscard deletes a cache nobody needs any more. A session still holding
// the file means "try again later"; the entry keeps discard_pending.
func (s *Service) sweepDiscard(ctx context.Context, entry *domain.StreamCacheEntry) {
	if entry == nil {
		return
	}
	provider := domain.NormalizeProvider(entry.Provider)
	externalID := strings.TrimSpace(entry.ExternalID)
	if provider == "" || externalID == "" {
		return
	}
	if s.cached.HasActiveSession(provider, externalID) {
		return
	}
	if err := s.cached.DiscardEntry(ctx, trackFromEntry(entry)); err != nil {
		if errors.Is(err, domain.ErrStreamDiscardDeferred) {
			return
		}
		s.logger.Warn("promote: cannot delete the cache entry", "cache_key", entry.CacheKey, "error", err)
	}
}

// sweepPromotion advances one entry. A session still holding the file means
// "try again later": the cache is the decoder's source and must not move.
func (s *Service) sweepPromotion(ctx context.Context, entry *domain.StreamCacheEntry) {
	if entry == nil {
		return
	}
	provider := domain.NormalizeProvider(entry.Provider)
	externalID := strings.TrimSpace(entry.ExternalID)
	if provider == "" || externalID == "" {
		return
	}
	if s.cached.HasActiveSession(provider, externalID) {
		return
	}

	if entry.Promoted || entry.DiscardPending {
		// The move (if any) already happened: only the cache is left, and
		// completing it again would re-download the bytes.
		if !entry.DiscardPending {
			if err := s.cached.SetDiscardPending(ctx, trackFromEntry(entry), true); err != nil {
				s.logger.Warn("promote: cannot flag a promoted entry for cleanup",
					"cache_key", entry.CacheKey, "error", err)
			}
		}
		s.sweepDiscard(ctx, entry)
		return
	}

	track, err := s.merger.FindByRemoteIdentity(ctx, provider, externalID)
	if err != nil {
		s.logger.Warn("promote: cannot look up the row",
			"provider", provider, "external_id", externalID, "error", err)
		return
	}
	if track == nil || !track.IsRemote() {
		// The row was deleted, or another path already downloaded it: the cached
		// audio is redundant, and promoting it would resurrect the row.
		if err := s.cached.SetDiscardPending(ctx, trackFromEntry(entry), true); err != nil {
			s.logger.Warn("promote: cannot flag an obsolete entry for cleanup",
				"cache_key", entry.CacheKey, "error", err)
			return
		}
		s.sweepDiscard(ctx, entry)
		return
	}
	s.executePromotion(ctx, entry, track)
}

// executePromotion completes the cache and either promotes it or hands the
// track to the download queue when the cache is below the configured quality.
func (s *Service) executePromotion(ctx context.Context, entry *domain.StreamCacheEntry, track *domain.TrackDTO) {
	taskID := s.startPromotionTask(ctx, entry, track)

	done, err := s.cached.EnsureComplete(ctx, track)
	if err != nil {
		s.failPromotion(ctx, taskID, err)
		return
	}
	if err := <-done; err != nil {
		s.failPromotion(ctx, taskID, err)
		return
	}

	path, cacheQuality, complete, found, err := s.cached.CachedFileInfo(ctx, track)
	if err != nil {
		s.failPromotion(ctx, taskID, err)
		return
	}
	if !found || !complete {
		s.failPromotion(ctx, taskID, errors.New("stream cache is incomplete"))
		return
	}

	settings, err := s.settings.GetSettings(ctx)
	if err != nil {
		s.failPromotion(ctx, taskID, err)
		return
	}
	downloadQuality := domain.ResolveDownloadQuality(settings.DownloadQuality)
	if cachedQualityRank(cacheQuality) < downloadQualityRank(downloadQuality) {
		// The cached audio is below the download setting: fetch the better copy
		// through the normal queue; a finished download discards the cache.
		if err := s.Enqueue(ctx, track.ID, "", downloadQuality); err != nil &&
			!errors.Is(err, domain.ErrDownloadAlreadyQueued) {
			s.failPromotion(ctx, taskID, err)
			return
		}
		if err := s.cached.ClearPendingPromote(ctx, track); err != nil {
			s.logger.Warn("promote: cannot clear the pending flag", "track_id", track.ID, "error", err)
		}
		s.finishPromotionTask(ctx, taskID, domain.PromotionStateDone, "")
		return
	}

	if _, err := s.PromoteCachedFile(ctx, track, path, cacheQuality); err != nil {
		s.failPromotion(ctx, taskID, err)
		return
	}
	if err := s.cached.SetPromoted(ctx, track, true); err != nil {
		// Without the flag the next sweep would complete the entry again and
		// re-download audio that is already in the library.
		s.failPromotion(ctx, taskID, err)
		return
	}
	if err := s.cached.SetDiscardPending(ctx, track, true); err != nil {
		s.logger.Warn("promote: cannot flag the moved cache for cleanup", "track_id", track.ID, "error", err)
	}
	if err := s.cached.DiscardEntry(ctx, track); err != nil &&
		!errors.Is(err, domain.ErrStreamDiscardDeferred) {
		s.logger.Warn("promote: cache cleanup deferred", "track_id", track.ID, "error", err)
	}
	s.finishPromotionTask(ctx, taskID, domain.PromotionStateDone, "")
}

// trackFromEntry builds the minimal row EnsureComplete and DiscardEntry read:
// both address the entry by remote identity.
func trackFromEntry(entry *domain.StreamCacheEntry) *domain.TrackDTO {
	if entry == nil {
		return nil
	}
	return &domain.TrackDTO{Track: domain.Track{
		ID:           entry.CacheKey,
		Path:         domain.RemotePath(entry.Provider, entry.ExternalID),
		Title:        entry.Title,
		Provider:     entry.Provider,
		ExternalID:   entry.ExternalID,
		Availability: domain.AvailabilityRemote,
	}}
}

// startPromotionTask records that the worker is executing the entry and returns
// the history row id (empty when there is no repository).
func (s *Service) startPromotionTask(ctx context.Context, entry *domain.StreamCacheEntry, track *domain.TrackDTO) string {
	if s.promotionTask == nil {
		return ""
	}
	task := &domain.PromotionTask{
		TrackID:    track.ID,
		Provider:   entry.Provider,
		ExternalID: entry.ExternalID,
		Title:      track.Title,
		Artist:     tagArtist(track),
		Quality:    entry.Quality,
	}
	id, err := s.promotionTask.EnsureQueued(ctx, task)
	if err != nil {
		s.logger.Warn("promote: cannot record the running task", "track_id", track.ID, "error", err)
		return ""
	}
	if err := s.promotionTask.MarkRunning(ctx, id, time.Now()); err != nil {
		s.logger.Warn("promote: cannot mark the task running", "task_id", id, "error", err)
	}
	s.emitTaskUpdated()
	return id
}

func (s *Service) finishPromotionTask(ctx context.Context, taskID, state, message string) {
	if taskID == "" || s.promotionTask == nil {
		return
	}
	if err := s.promotionTask.MarkFinished(ctx, taskID, state, message, time.Now()); err != nil {
		s.logger.Warn("promote: cannot record the task result", "task_id", taskID, "error", err)
		return
	}
	s.emitTaskUpdated()
}

// failPromotion records a failed attempt. The entry keeps pending_promote, so
// the next sweep retries it; other entries are unaffected.
func (s *Service) failPromotion(ctx context.Context, taskID string, cause error) {
	if cause == nil {
		cause = errors.New("promotion failed")
	}
	s.logger.Warn("promote: attempt failed", "task_id", taskID, "error", cause)
	s.finishPromotionTask(ctx, taskID, domain.PromotionStateFailed, cause.Error())
}

// emitTaskUpdated tells the tasks page a promotion row changed.
func (s *Service) emitTaskUpdated() {
	s.sinkMu.RLock()
	sink := s.sink
	s.sinkMu.RUnlock()
	if sink != nil {
		sink.TaskUpdated()
	}
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
