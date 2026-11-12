package download_test

import (
	"context"
	"errors"
	"testing"

	"airmedy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubCleanupPlayer records the pause/resume handshake the cleanup performs.
type stubCleanupPlayer struct {
	provider   string
	externalID string
	playing    bool
	pauses     int
	resumes    int
	pauseErr   error
}

func (p *stubCleanupPlayer) PlayingIdentity(context.Context) (string, string, bool) {
	return p.provider, p.externalID, p.playing
}

func (p *stubCleanupPlayer) PauseForCacheCleanup(context.Context) error {
	p.pauses++
	return p.pauseErr
}

func (p *stubCleanupPlayer) ResumeAfterCacheCleanup(context.Context) error {
	p.resumes++
	return nil
}

func cacheEntry(cacheKey, provider, externalID, quality string, complete bool, lastUsed int64) *domain.StreamCacheEntry {
	return &domain.StreamCacheEntry{
		CacheKey:      cacheKey,
		Provider:      provider,
		ExternalID:    externalID,
		Title:         "Song",
		TitleKey:      "song",
		ArtistKey:     "artist",
		DurationMs:    200000,
		Quality:       quality,
		Complete:      complete,
		ReceivedBytes: 1000,
		LastUsed:      lastUsed,
	}
}

// TestDuplicateCacheCleanup_GroupsVariantArtistSpellings: entries cached from
// different sources store the same artist-set key even when the credit string
// is ordered or separated differently, so they form one duplicate group.
func TestDuplicateCacheCleanup_GroupsVariantArtistSpellings(t *testing.T) {
	h := newHarness(t)
	left := cacheEntry("a", "wy", "1", domain.Quality320k, true, 20)
	left.Title = "Synthetic Fracture"
	left.TitleKey = domain.NormalizationKey(left.Title)
	left.Artist = "Sad Keyboard Guy; Gardens"

	right := cacheEntry("b", "kg", "2", domain.Quality128k, true, 10)
	right.Title = "Synthetic Fracture"
	right.TitleKey = domain.NormalizationKey(right.Title)
	right.Artist = "Gardens, Sad Keyboard Guy"

	key := domain.ArtistSetKey([]string{"Sad Keyboard Guy", "Gardens"})
	left.ArtistKey, right.ArtistKey = key, key
	h.cached.entries = []*domain.StreamCacheEntry{left, right}

	scan, err := h.svc.ScanDuplicateCache(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, scan.DuplicateGroups)
	assert.Equal(t, 1, scan.Removable)
}

func TestDuplicateCacheCleanup_KeepsTheHighestQuality(t *testing.T) {
	h := newHarness(t)
	h.cached.entries = []*domain.StreamCacheEntry{
		cacheEntry("low", "wy", "1", domain.Quality128k, true, 20),
		cacheEntry("high", "kg", "2", domain.Quality320k, true, 10),
	}

	scan, err := h.svc.ScanDuplicateCache(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, scan.DuplicateGroups)
	assert.Equal(t, 1, scan.Removable)
	assert.EqualValues(t, 1000, scan.RemovableBytes)

	clean, err := h.svc.CleanDuplicateCache(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, clean.Removable)
	assert.EqualValues(t, 1000, clean.RemovableBytes)

	h.cached.mu.Lock()
	defer h.cached.mu.Unlock()
	assert.Equal(t, 1, h.cached.discarded)
	assert.Equal(t, []string{"1"}, h.cached.discardedExternalIDs, "the lower-quality copy is the one removed")
}

func TestDuplicateCacheCleanup_TieBreaksOnCompletenessThenRecency(t *testing.T) {
	tests := []struct {
		name        string
		keeper      *domain.StreamCacheEntry
		removed     *domain.StreamCacheEntry
		description string
	}{
		{
			name:        "complete wins over partial",
			keeper:      cacheEntry("complete", "wy", "keep", domain.Quality320k, true, 10),
			removed:     cacheEntry("partial", "kg", "drop", domain.Quality320k, false, 20),
			description: "a complete copy outranks a newer partial one",
		},
		{
			name:        "recent wins at the same completeness",
			keeper:      cacheEntry("recent", "wy", "keep", domain.Quality320k, true, 20),
			removed:     cacheEntry("old", "kg", "drop", domain.Quality320k, true, 10),
			description: "same quality and completeness, so last_used decides",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.cached.entries = []*domain.StreamCacheEntry{tt.keeper, tt.removed}

			report, err := h.svc.CleanDuplicateCache(context.Background())
			require.NoError(t, err)
			assert.Equal(t, 1, report.Removable, tt.description)
			h.cached.mu.Lock()
			assert.Equal(t, []string{tt.removed.ExternalID}, h.cached.discardedExternalIDs)
			h.cached.mu.Unlock()
		})
	}
}

func TestDuplicateCacheCleanup_LeavesAnAmbiguousGroupAlone(t *testing.T) {
	h := newHarness(t)
	h.cached.entries = []*domain.StreamCacheEntry{
		cacheEntry("a", "wy", "1", domain.Quality320k, true, 10),
		cacheEntry("b", "kg", "2", domain.Quality320k, true, 10),
	}

	scan, err := h.svc.ScanDuplicateCache(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, scan.DuplicateGroups)
	assert.Equal(t, 0, scan.Removable)
	assert.NotEmpty(t, scan.Skipped)

	clean, err := h.svc.CleanDuplicateCache(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, clean.Removable)
	h.cached.mu.Lock()
	assert.Equal(t, 0, h.cached.discarded)
	h.cached.mu.Unlock()
}

func TestDuplicateCacheCleanup_DeletesAnEntryAlreadyInTheLibrary(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	require.NoError(t, h.tracks.Upsert(ctx, &domain.Track{
		ID: "local-1", Path: "/music/song.mp3", Title: "Song",
		Availability: domain.AvailabilityReady, Provider: "wy", ExternalID: "already",
	}))
	h.cached.entries = []*domain.StreamCacheEntry{
		cacheEntry("imported", "wy", "already", domain.Quality320k, true, 10),
	}

	report, err := h.svc.CleanDuplicateCache(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Removable)
	h.cached.mu.Lock()
	assert.Equal(t, []string{"already"}, h.cached.discardedExternalIDs)
	h.cached.mu.Unlock()
}

func TestDuplicateCacheCleanup_SkipsPendingPromotion(t *testing.T) {
	h := newHarness(t)
	pending := cacheEntry("pending", "wy", "1", domain.Quality128k, true, 10)
	pending.PendingPromote = true
	h.cached.entries = []*domain.StreamCacheEntry{
		pending,
		cacheEntry("keeper", "kg", "2", domain.Quality320k, true, 20),
	}

	scan, err := h.svc.ScanDuplicateCache(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, scan.Removable)
	assert.Contains(t, scan.Skipped, "pending_promote: Song")

	_, err = h.svc.CleanDuplicateCache(context.Background())
	require.NoError(t, err)
	h.cached.mu.Lock()
	assert.Equal(t, 0, h.cached.discarded)
	h.cached.mu.Unlock()
}

func TestDuplicateCacheCleanup_DeletesPromotedLeftovers(t *testing.T) {
	h := newHarness(t)
	leftover := cacheEntry("promoted", "wy", "1", domain.Quality320k, true, 10)
	leftover.Promoted = true
	h.cached.entries = []*domain.StreamCacheEntry{leftover}

	report, err := h.svc.CleanDuplicateCache(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, report.Removable)
	assert.Equal(t, 1, report.PromotedLeftovers)
	h.cached.mu.Lock()
	assert.Equal(t, []string{"1"}, h.cached.discardedExternalIDs)
	h.cached.mu.Unlock()
}

func TestDuplicateCacheCleanup_DeletesOrphanFilesAndContinuesAfterAFailure(t *testing.T) {
	h := newHarness(t)
	h.cached.orphans = []domain.StreamCacheOrphan{
		{Name: "orphan-a.audio", Bytes: 500},
		{Name: "orphan-b.audio", Bytes: 700},
	}
	h.cached.deleteFileErr = errors.New("file is busy")

	scan, err := h.svc.ScanDuplicateCache(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, scan.OrphanFiles, "the scan reports what it found")

	report, err := h.svc.CleanDuplicateCache(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, report.OrphanFiles, "nothing was actually removed")
	assert.Equal(t, 0, report.Removable)
	assert.EqualValues(t, 0, report.RemovableBytes)
	assert.Len(t, report.Skipped, 2, "each failed file is reported and the batch continues")

	// With the file deletable the same run removes both.
	h.cached.mu.Lock()
	h.cached.deleteFileErr = nil
	h.cached.mu.Unlock()
	report, err = h.svc.CleanDuplicateCache(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, report.OrphanFiles)
	assert.EqualValues(t, 1200, report.RemovableBytes)
	h.cached.mu.Lock()
	assert.ElementsMatch(t, []string{"orphan-a.audio", "orphan-b.audio"}, h.cached.deletedFiles)
	h.cached.mu.Unlock()
}

func TestDuplicateCacheCleanup_PausesAndResumesOnlyForTheLoadedTrack(t *testing.T) {
	h := newHarness(t)
	h.cached.entries = []*domain.StreamCacheEntry{
		cacheEntry("low", "wy", "1", domain.Quality128k, true, 10),
		cacheEntry("high", "kg", "2", domain.Quality320k, true, 20),
	}

	// The loaded track does not own any entry being deleted: no interruption.
	player := &stubCleanupPlayer{provider: "kg", externalID: "2", playing: true}
	h.svc.SetCacheCleanupPlayer(player)
	_, err := h.svc.CleanDuplicateCache(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, player.pauses)
	assert.Equal(t, 0, player.resumes)

	// The loaded track owns the removable copy: pause, delete, resume.
	h = newHarness(t)
	h.cached.entries = []*domain.StreamCacheEntry{
		cacheEntry("low", "wy", "1", domain.Quality128k, true, 10),
		cacheEntry("high", "kg", "2", domain.Quality320k, true, 20),
	}
	player = &stubCleanupPlayer{provider: "wy", externalID: "1", playing: true}
	h.svc.SetCacheCleanupPlayer(player)
	_, err = h.svc.CleanDuplicateCache(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, player.pauses)
	assert.Equal(t, 1, player.resumes)
	h.cached.mu.Lock()
	assert.Equal(t, []string{"1"}, h.cached.discardedExternalIDs)
	h.cached.mu.Unlock()
}
