package download

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"airmedy/internal/domain"
)

// duplicateCachePlan is the single decision shared by the scan and the clean:
// the scan reports it, the clean executes it.
type duplicateCachePlan struct {
	report  domain.DuplicateCacheReport
	entries []*domain.StreamCacheEntry
	orphans []domain.StreamCacheOrphan
}

// ScanDuplicateCache reports what a cleanup would remove. It never deletes.
func (s *Service) ScanDuplicateCache(ctx context.Context) (domain.DuplicateCacheReport, error) {
	plan, err := s.buildDuplicateCachePlan(ctx)
	return plan.report, err
}

// CleanDuplicateCache deletes the entries the scan would mark removable. When
// the loaded track's cache is among them, playback pauses, the stream session is
// released so the file can go, and the same track/position resumes afterwards.
// The queue is never touched.
func (s *Service) CleanDuplicateCache(ctx context.Context) (domain.DuplicateCacheReport, error) {
	plan, err := s.buildDuplicateCachePlan(ctx)
	report := plan.report
	if err != nil {
		return report, err
	}
	if len(plan.entries) == 0 && len(plan.orphans) == 0 {
		return report, nil
	}

	if player := s.cacheCleanupPlayer(); player != nil && s.planTouchesPlaying(ctx, player, plan.entries) {
		if pauseErr := player.PauseForCacheCleanup(ctx); pauseErr != nil {
			s.logger.Warn("cache cleanup: cannot pause playback", "error", pauseErr)
		} else {
			// Resume even when a delete returns early; the queue and current track
			// are untouched, so this only re-opens the same source.
			defer func() {
				if resumeErr := player.ResumeAfterCacheCleanup(context.Background()); resumeErr != nil {
					s.logger.Warn("cache cleanup: cannot resume playback", "error", resumeErr)
				}
			}()
		}
	}

	report.Skipped = append([]string(nil), plan.report.Skipped...)
	deletedEntries := 0
	deletedOrphans := 0
	var freed int64

	for _, entry := range plan.entries {
		if ctx.Err() != nil {
			break
		}
		provider := domain.NormalizeProvider(entry.Provider)
		externalID := strings.TrimSpace(entry.ExternalID)
		if provider == "" || externalID == "" {
			report.Skipped = append(report.Skipped, "no_identity: "+entry.Title)
			continue
		}
		if s.cached.HasActiveSession(provider, externalID) {
			report.Skipped = append(report.Skipped, "active_session: "+entry.Title)
			continue
		}
		if err := s.cached.DiscardEntry(ctx, trackFromEntry(entry)); err != nil {
			if errors.Is(err, domain.ErrStreamDiscardDeferred) {
				report.Skipped = append(report.Skipped, "active_session: "+entry.Title)
				continue
			}
			s.logger.Warn("cache cleanup: cannot delete entry",
				"cache_key", entry.CacheKey, "error", err)
			report.Skipped = append(report.Skipped, "delete_failed: "+entry.Title)
			continue
		}
		deletedEntries++
		freed += entryBytes(entry)
	}

	for _, orphan := range plan.orphans {
		if ctx.Err() != nil {
			break
		}
		if err := s.cached.DeleteFile(orphan.Name); err != nil {
			s.logger.Warn("cache cleanup: cannot delete orphan file", "file", orphan.Name, "error", err)
			report.Skipped = append(report.Skipped, "delete_failed: "+orphan.Name)
			continue
		}
		deletedOrphans++
		freed += orphan.Bytes
	}

	// The clean report describes what actually happened, not what was planned.
	report.Removable = deletedEntries
	report.OrphanFiles = deletedOrphans
	report.RemovableBytes = freed
	s.emitTaskUpdated()
	return report, nil
}

// buildDuplicateCachePlan classifies every entry once: promoted leftovers,
// identities already in the library, cross-source duplicate groups, and orphan
// files. The same plan drives the report and the deletion.
func (s *Service) buildDuplicateCachePlan(ctx context.Context) (duplicateCachePlan, error) {
	plan := duplicateCachePlan{
		report: domain.DuplicateCacheReport{Skipped: make([]string, 0)},
	}
	if s.cached == nil {
		return plan, nil
	}
	entries, err := s.cached.AllEntries(ctx)
	if err != nil {
		return plan, fmt.Errorf("duplicate cache: list entries: %w", err)
	}

	seen := make(map[string]struct{}, len(entries))
	addEntry := func(entry *domain.StreamCacheEntry, reason string) {
		if entry == nil {
			return
		}
		if _, duplicate := seen[entry.CacheKey]; duplicate {
			return
		}
		seen[entry.CacheKey] = struct{}{}
		plan.entries = append(plan.entries, entry)
		if reason == "promoted" {
			plan.report.PromotedLeftovers++
		}
	}

	groupable := make([]*domain.StreamCacheEntry, 0, len(entries))
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		provider := domain.NormalizeProvider(entry.Provider)
		externalID := strings.TrimSpace(entry.ExternalID)
		if provider == "" || externalID == "" {
			plan.report.Skipped = append(plan.report.Skipped, "no_identity: "+entry.Title)
			continue
		}
		if s.hasActiveRequest(ctx, provider, externalID) {
			// A request still needs this audio (promotion or a queued download).
			plan.report.Skipped = append(plan.report.Skipped, "active_request: "+entry.Title)
			continue
		}
		if local, localErr := s.HasLocalIdentity(ctx, provider, externalID); localErr != nil {
			s.logger.Warn("cache cleanup: cannot check the local identity",
				"provider", provider, "external_id", externalID, "error", localErr)
			plan.report.Skipped = append(plan.report.Skipped, "lookup_failed: "+entry.Title)
			continue
		} else if local {
			addEntry(entry, "already_local")
			continue
		}
		groupable = append(groupable, entry)
	}

	for _, group := range groupDuplicateEntries(groupable, s.canonicalArtistKey(ctx)) {
		if len(group) < 2 {
			continue
		}
		plan.report.DuplicateGroups++
		keeper, ok := pickDuplicateKeeper(group)
		if !ok {
			// Every candidate looks identical; deleting either would be a guess.
			plan.report.Skipped = append(plan.report.Skipped, "ambiguous: "+group[0].Title)
			continue
		}
		for index, entry := range group {
			if index == keeper {
				continue
			}
			if s.cached.HasActiveSession(
				domain.NormalizeProvider(entry.Provider), strings.TrimSpace(entry.ExternalID),
			) {
				plan.report.Skipped = append(plan.report.Skipped, "active_session: "+entry.Title)
				continue
			}
			addEntry(entry, "duplicate")
		}
	}

	orphans, orphanErr := s.cached.OrphanFiles(ctx)
	if orphanErr != nil {
		s.logger.Warn("cache cleanup: cannot list orphan files", "error", orphanErr)
	} else {
		plan.orphans = orphans
		plan.report.OrphanFiles = len(orphans)
	}

	plan.report.Removable = len(plan.entries)
	var bytes int64
	for _, entry := range plan.entries {
		bytes += entryBytes(entry)
	}
	for _, orphan := range plan.orphans {
		bytes += orphan.Bytes
	}
	plan.report.RemovableBytes = bytes
	return plan, nil
}

// canonicalArtistKey maps an entry's artist key to the smallest key in its
// one-hop alias set. Two entries cached under "Camellia" and "かめりあ" therefore
// land in the same duplicate group. Without an alias store it is the identity.
func (s *Service) canonicalArtistKey(ctx context.Context) func(string) string {
	if s.aliases == nil {
		return func(key string) string { return key }
	}
	return func(key string) string {
		if key == "" {
			return key
		}
		expanded, err := s.aliases.ExpandArtists(ctx, []string{key})
		if err != nil || len(expanded) == 0 {
			return key
		}
		return expanded[0]
	}
}

// groupDuplicateEntries clusters entries by normalized title + artist, using the
// same duration tolerance as cross-source matching. artistKey folds alias
// variants onto one group key. A group only holds entries that can safely be
// compared; an empty key is never grouped.
func groupDuplicateEntries(
	entries []*domain.StreamCacheEntry, artistKey func(string) string,
) [][]*domain.StreamCacheEntry {
	ordered := make([]*domain.StreamCacheEntry, 0, len(entries))
	canonical := make(map[*domain.StreamCacheEntry]string, len(entries))
	for _, entry := range entries {
		if entry == nil || entry.TitleKey == "" || entry.ArtistKey == "" {
			continue
		}
		ordered = append(ordered, entry)
		canonical[entry] = artistKey(entry.ArtistKey)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].TitleKey != ordered[j].TitleKey {
			return ordered[i].TitleKey < ordered[j].TitleKey
		}
		if canonical[ordered[i]] != canonical[ordered[j]] {
			return canonical[ordered[i]] < canonical[ordered[j]]
		}
		return ordered[i].LastUsed > ordered[j].LastUsed
	})

	groups := make([][]*domain.StreamCacheEntry, 0)
	for _, entry := range ordered {
		placed := false
		for index := range groups {
			representative := groups[index][0]
			if representative.TitleKey != entry.TitleKey ||
				canonical[representative] != canonical[entry] {
				continue
			}
			if !duplicateDurationsCompatible(representative.DurationMs, entry.DurationMs) {
				continue
			}
			groups[index] = append(groups[index], entry)
			placed = true
			break
		}
		if !placed {
			groups = append(groups, []*domain.StreamCacheEntry{entry})
		}
	}
	return groups
}

// duplicateDurationsCompatible mirrors the cache's cross-source matching: the
// tolerance only applies when both sides report a duration.
func duplicateDurationsCompatible(a, b int64) bool {
	if a <= 0 || b <= 0 {
		return true
	}
	difference := a - b
	if difference < 0 {
		difference = -difference
	}
	return difference <= 5000
}

// pickDuplicateKeeper returns the index to keep. It reports false when the best
// two entries cannot be told apart (quality, completeness and recency all
// equal): deleting one of them would be a guess.
func pickDuplicateKeeper(group []*domain.StreamCacheEntry) (int, bool) {
	best := 0
	for index := 1; index < len(group); index++ {
		if compareCacheEntries(group[index], group[best]) > 0 {
			best = index
		}
	}
	for index := range group {
		if index == best {
			continue
		}
		if compareCacheEntries(group[index], group[best]) == 0 {
			return 0, false
		}
	}
	return best, true
}

// compareCacheEntries orders two candidates by keep priority: quality, then a
// complete download, then the most recently used. Positive means a is better.
func compareCacheEntries(a, b *domain.StreamCacheEntry) int {
	if aRank, bRank := cacheQualityRank(a.Quality), cacheQualityRank(b.Quality); aRank != bRank {
		if aRank > bRank {
			return 1
		}
		return -1
	}
	if a.Complete != b.Complete {
		if a.Complete {
			return 1
		}
		return -1
	}
	switch {
	case a.LastUsed > b.LastUsed:
		return 1
	case a.LastUsed < b.LastUsed:
		return -1
	default:
		return 0
	}
}

// cacheQualityRank treats an unknown tier as the lowest, so an unlabelled entry
// never wins over a known one.
func cacheQualityRank(quality string) int {
	switch quality {
	case domain.QualityFlac:
		return 3
	case domain.Quality320k:
		return 2
	case domain.Quality128k:
		return 1
	default:
		return 0
	}
}

func entryBytes(entry *domain.StreamCacheEntry) int64 {
	if entry == nil {
		return 0
	}
	if entry.ReceivedBytes > 0 {
		return entry.ReceivedBytes
	}
	if entry.TotalBytes > 0 {
		return entry.TotalBytes
	}
	return 0
}

// trackFromEntry builds the minimal row DiscardEntry reads: it addresses the
// entry by remote identity.
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

// HasActiveRequest reports whether the identity still has work in flight. The
// cache startup sweep uses it to keep audio a request is about to promote or
// replace, and the cleanup uses it to skip the same entries.
func (s *Service) HasActiveRequest(ctx context.Context, provider, externalID string) bool {
	if s.requests == nil {
		return false
	}
	request, err := s.requests.GetByIdentity(ctx, provider, externalID)
	if err != nil || request == nil {
		return false
	}
	switch request.State {
	case domain.MediaRequestStateWanted, domain.MediaRequestStateQueued,
		domain.MediaRequestStateRunning:
		return true
	case domain.MediaRequestStateFailed:
		return request.NextAttemptAt != nil
	}
	return false
}

// hasActiveRequest is the internal spelling used by the cleanup plan.
func (s *Service) hasActiveRequest(ctx context.Context, provider, externalID string) bool {
	return s.HasActiveRequest(ctx, provider, externalID)
}

// planTouchesPlaying reports whether the loaded remote identity owns one of the
// entries being deleted. Only then does playback have to pause.
func (s *Service) planTouchesPlaying(
	ctx context.Context, player CacheCleanupPlayer, entries []*domain.StreamCacheEntry,
) bool {
	provider, externalID, ok := player.PlayingIdentity(ctx)
	if !ok {
		return false
	}
	provider = domain.NormalizeProvider(provider)
	externalID = strings.TrimSpace(externalID)
	if provider == "" || externalID == "" {
		return false
	}
	for _, entry := range entries {
		if domain.NormalizeProvider(entry.Provider) == provider &&
			strings.TrimSpace(entry.ExternalID) == externalID {
			return true
		}
	}
	return false
}
