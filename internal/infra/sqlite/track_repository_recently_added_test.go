package sqlite

import (
	"context"
	"testing"
	"time"

	"airmedy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// remoteRecentTrack registers an online-only row the way "play all" does.
func remoteRecentTrack(id, provider, externalID string, playCount int, createdAt time.Time) *domain.Track {
	return &domain.Track{
		ID:           id,
		Path:         domain.RemotePath(provider, externalID),
		Title:        id,
		SortTitle:    id,
		Availability: domain.AvailabilityRemote,
		Provider:     provider,
		ExternalID:   externalID,
		PlayCount:    playCount,
		CreatedAt:    createdAt,
	}
}

func localRecentTrack(id, path string, createdAt time.Time) *domain.Track {
	return &domain.Track{
		ID:        id,
		Path:      path,
		Title:     id,
		SortTitle: id,
		CreatedAt: createdAt,
	}
}

// TestGetRecentlyAdded_HidesUnplayedRemoteShells: registering a whole playlist
// must not fill "recently added"; only remote rows that were played or fully
// cached are listed, and local rows always are.
func TestGetRecentlyAdded_HidesUnplayedRemoteShells(t *testing.T) {
	repo, db := newAvailabilityTestRepo(t)
	cacheRepo := NewStreamCacheRepository(db)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tracks := []*domain.Track{
		localRecentTrack("local", `D:\music\local.flac`, base.Add(1*time.Hour)),
		remoteRecentTrack("incomplete", "wy", "2", 0, base.Add(2*time.Hour)),
		remoteRecentTrack("complete", "wy", "3", 0, base.Add(3*time.Hour)),
		remoteRecentTrack("played", "wy", "4", 3, base.Add(4*time.Hour)),
		remoteRecentTrack("shell", "wy", "5", 0, base.Add(5*time.Hour)),
	}
	for _, track := range tracks {
		require.NoError(t, repo.Save(ctx, track))
	}

	cacheEntry := func(key, provider, externalID string, complete bool) *domain.StreamCacheEntry {
		return &domain.StreamCacheEntry{
			CacheKey:   key,
			Provider:   provider,
			ExternalID: externalID,
			Title:      externalID,
			TitleKey:   externalID,
			Artist:     "artist",
			ArtistKey:  "artist",
			AudioPath:  key + ".audio",
			Complete:   complete,
			CreatedAt:  1,
			LastUsed:   1,
		}
	}
	require.NoError(t, cacheRepo.Upsert(ctx, cacheEntry("k-complete", "wy", "3", true)))
	require.NoError(t, cacheRepo.Upsert(ctx, cacheEntry("k-incomplete", "wy", "2", false)))

	got, err := repo.GetRecentlyAdded(ctx, 10)
	require.NoError(t, err)

	ids := make([]string, len(got))
	for i, dto := range got {
		ids[i] = dto.ID
	}
	// Newest first; the unplayed shell and the partial cache are absent.
	assert.Equal(t, []string{"played", "complete", "local"}, ids)
}

// TestGetRecentlyAdded_KeepsOrderAndLimit: the eligibility filter must not
// reorder the list or change how the limit is applied.
func TestGetRecentlyAdded_KeepsOrderAndLimit(t *testing.T) {
	repo, _ := newAvailabilityTestRepo(t)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, track := range []*domain.Track{
		localRecentTrack("first", `D:\music\first.flac`, base.Add(1*time.Hour)),
		localRecentTrack("second", `D:\music\second.flac`, base.Add(2*time.Hour)),
		localRecentTrack("third", `D:\music\third.flac`, base.Add(3*time.Hour)),
	} {
		require.NoError(t, repo.Save(ctx, track), "save track %d", i)
	}

	got, err := repo.GetRecentlyAdded(ctx, 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "third", got[0].ID)
	assert.Equal(t, "second", got[1].ID)
}
