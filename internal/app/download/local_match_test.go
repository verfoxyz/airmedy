package download_test

import (
	"context"
	"testing"

	"airmedy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMatchLocalTrack_FlagsADurationDisagreement(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	require.NoError(t, h.tracks.Upsert(ctx, &domain.Track{
		ID:             "local-1",
		Path:           "/library/Synthetic Fracture.flac",
		Title:          "Synthetic Fracture",
		Duration:       200,
		RawArtistNames: "Sad Keyboard Guy; Gardens",
		Availability:   domain.AvailabilityReady,
	}))

	match, err := h.svc.MatchLocalTrack(ctx, "Synthetic Fracture", []string{"Gardens"}, 260)
	require.NoError(t, err)
	require.NotNil(t, match)
	assert.Equal(t, "local-1", match.Track.ID)
	assert.True(t, match.Suspicious, "a 60s disagreement is still a match, but flagged")
}

func TestMatchLocalTrack_AcceptsASmallDurationDifference(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	require.NoError(t, h.tracks.Upsert(ctx, &domain.Track{
		ID:             "local-1",
		Path:           "/library/Synthetic Fracture.flac",
		Title:          "Synthetic Fracture",
		Duration:       200,
		RawArtistNames: "Sad Keyboard Guy; Gardens",
		Availability:   domain.AvailabilityReady,
	}))

	match, err := h.svc.MatchLocalTrack(ctx, "Synthetic Fracture", []string{"Gardens"}, 208)
	require.NoError(t, err)
	require.NotNil(t, match)
	assert.Equal(t, "local-1", match.Track.ID)
	assert.False(t, match.Suspicious, "a difference within the hint window is normal")
}

func TestMatchLocalTrack_PicksTheClosestDuration(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	for _, track := range []*domain.Track{
		{
			ID: "local-far", Path: "/library/far.flac", Title: "Song",
			Duration: 230, RawArtistNames: "Artist", Availability: domain.AvailabilityReady,
		},
		{
			ID: "local-close", Path: "/library/close.flac", Title: "Song",
			Duration: 204, RawArtistNames: "Artist", Availability: domain.AvailabilityReady,
		},
	} {
		require.NoError(t, h.tracks.Upsert(ctx, track))
	}

	match, err := h.svc.MatchLocalTrack(ctx, "Song", []string{"Artist"}, 205)
	require.NoError(t, err)
	require.NotNil(t, match)
	assert.Equal(t, "local-close", match.Track.ID)
	assert.False(t, match.Suspicious)
}

func TestCatalogLocalMatches_FlagsADurationDisagreement(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	require.NoError(t, h.tracks.Upsert(ctx, &domain.Track{
		ID:             "local-1",
		Path:           "/library/Synthetic Fracture.flac",
		Title:          "Synthetic Fracture",
		Duration:       200,
		RawArtistNames: "Sad Keyboard Guy; Gardens",
		Availability:   domain.AvailabilityReady,
	}))

	identity := domain.CatalogIdentity{Provider: "wy", ExternalID: "1941192069"}
	matches, err := h.svc.CatalogLocalMatches(ctx, []domain.CatalogLocalQuery{{
		Identity: identity,
		Title:    "Synthetic Fracture",
		Artists:  []string{"Gardens"},
		Duration: 260,
	}})
	require.NoError(t, err)

	match, ok := matches[identity]
	require.True(t, ok)
	assert.Equal(t, "local-1", match.TrackID)
	assert.True(t, match.Suspicious)
}
