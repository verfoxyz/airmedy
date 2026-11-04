package download_test

import (
	"context"
	"testing"
	"time"

	"airmedy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The search page asks what the library already knows about a result before it
// offers "download" again. These tests pin the three answers that matter: the
// audio is on disk, it is being fetched, or the last attempt failed.

// seedJob inserts a job in the given state; an empty playlist keeps the foreign
// key optional.
func seedJob(t *testing.T, h *harness, id, trackID, state string) {
	t.Helper()
	require.NoError(t, h.jobs.Create(context.Background(), &domain.DownloadJob{
		ID:      id,
		TrackID: trackID,
		State:   state,
	}))
}

func TestCatalogRowStates_ReportsTheThreeKnownStates(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A finished download keeps the identity on the row but is no longer remote.
	require.NoError(t, h.tracks.Upsert(ctx, &domain.Track{
		ID:           "ready",
		Path:         "/library/晴天.mp3",
		Title:        "晴天",
		Availability: domain.AvailabilityReady,
		Provider:     "wy",
		ExternalID:   "ready",
	}))

	active := h.seedRemote(t, "active")
	seedJob(t, h, "job-active", active.ID, domain.DownloadStateQueued)

	failed := h.seedRemote(t, "failed")
	seedJob(t, h, "job-failed", failed.ID, domain.DownloadStateActive)
	require.NoError(t, h.jobs.MarkFailed(ctx, "job-failed", "403 forbidden", nil))

	states, err := h.svc.CatalogRowStates(ctx, []domain.CatalogIdentity{
		{Provider: "wy", ExternalID: "ready"},
		{Provider: "wy", ExternalID: "active"},
		{Provider: "wy", ExternalID: "failed"},
		{Provider: "wy", ExternalID: "never-registered"},
	})
	require.NoError(t, err)

	assert.Equal(t, domain.CatalogRowStateInLibrary, states[domain.CatalogIdentity{Provider: "wy", ExternalID: "ready"}])
	assert.Equal(t, domain.CatalogRowStateDownloading, states[domain.CatalogIdentity{Provider: "wy", ExternalID: "active"}])
	assert.Equal(t, domain.CatalogRowStateFailed, states[domain.CatalogIdentity{Provider: "wy", ExternalID: "failed"}])
	assert.NotContains(t, states, domain.CatalogIdentity{Provider: "wy", ExternalID: "never-registered"},
		"an unregistered identity has no state, so the UI offers the normal download")
}

func TestCatalogLocalMatches_MatchesAnImportedFileByTitleArtistAndDuration(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	require.NoError(t, h.tracks.Upsert(ctx, &domain.Track{
		ID:             "local-1",
		Path:           "/library/前前前世.flac",
		Title:          "前前前世",
		Duration:       239,
		RawArtistNames: "RADWIMPS",
		Availability:   domain.AvailabilityReady,
	}))
	// Same title, different artist: a cover must not be mistaken for the song.
	require.NoError(t, h.tracks.Upsert(ctx, &domain.Track{
		ID:             "cover-1",
		Path:           "/library/cover.flac",
		Title:          "前前前世",
		Duration:       240,
		RawArtistNames: "Someone Else",
		Availability:   domain.AvailabilityReady,
	}))

	identity := domain.CatalogIdentity{Provider: "wy", ExternalID: "509781655"}
	versionIdentity := domain.CatalogIdentity{Provider: "wy", ExternalID: "movie"}
	matches, err := h.svc.CatalogLocalMatches(ctx, []domain.CatalogLocalQuery{
		{Identity: identity, Title: "前前前世", Artists: []string{"RADWIMPS"}, Duration: 238},
		// The provider appends a version suffix the imported file does not carry.
		{Identity: versionIdentity, Title: "前前前世 (movie ver.)", Artists: []string{"RADWIMPS"}, Duration: 239},
		{Identity: domain.CatalogIdentity{Provider: "wy", ExternalID: "2"}, Title: "前前前世", Artists: []string{"Someone Else"}, Duration: 240},
		{Identity: domain.CatalogIdentity{Provider: "wy", ExternalID: "3"}, Title: "前前前世", Duration: 0},
	})
	require.NoError(t, err)

	assert.Equal(t, "local-1", matches[identity])
	assert.Equal(t, "local-1", matches[versionIdentity])
	assert.Equal(t, "cover-1", matches[domain.CatalogIdentity{Provider: "wy", ExternalID: "2"}])
	assert.NotContains(t, matches, domain.CatalogIdentity{Provider: "wy", ExternalID: "3"},
		"a query without a duration is too weak to match")
}

// TestCatalogRowStates_RetryReadsAsDownloading: a retryable failure puts the job
// back in the queue, and the row must show progress rather than a retry button.
func TestCatalogRowStates_RetryReadsAsDownloading(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	track := h.seedRemote(t, "retrying")
	seedJob(t, h, "job-retry", track.ID, domain.DownloadStateActive)
	retryAt := time.Now().Add(time.Minute)
	require.NoError(t, h.jobs.MarkFailed(ctx, "job-retry", "429 too many requests", &retryAt))

	states, err := h.svc.CatalogRowStates(ctx, []domain.CatalogIdentity{{Provider: "wy", ExternalID: "retrying"}})
	require.NoError(t, err)
	assert.Equal(t, domain.CatalogRowStateDownloading, states[domain.CatalogIdentity{Provider: "wy", ExternalID: "retrying"}])
}

// TestCatalogRowStates_RegisteredButIdle: a canceled job leaves a registered
// row with nothing in flight. It reads as "saved" (metadata only) rather than
// as a download that never started.
func TestCatalogRowStates_RegisteredButIdle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	track := h.seedRemote(t, "idle")
	seedJob(t, h, "job-idle", track.ID, domain.DownloadStateQueued)
	require.NoError(t, h.jobs.Cancel(ctx, "job-idle"))

	states, err := h.svc.CatalogRowStates(ctx, []domain.CatalogIdentity{{Provider: "wy", ExternalID: "idle"}})
	require.NoError(t, err)
	assert.Equal(t, domain.CatalogRowStateSaved, states[domain.CatalogIdentity{Provider: "wy", ExternalID: "idle"}])
}

func TestCatalogRowStates_EmptyPageIsFree(t *testing.T) {
	h := newHarness(t)

	states, err := h.svc.CatalogRowStates(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, states)
}

func TestCatalogRowStates_IgnoresIncompleteIdentities(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedRemote(t, "known")

	states, err := h.svc.CatalogRowStates(ctx, []domain.CatalogIdentity{
		{Provider: "", ExternalID: "known"},
		{Provider: "wy", ExternalID: "   "},
	})
	require.NoError(t, err)
	assert.Empty(t, states, "an identity that could never be registered has no state")
}

// TestCatalogRowStates_NormalisesTheProvider: the search rows and the stored rows
// must meet even when a caller passes the id with different casing.
func TestCatalogRowStates_NormalisesTheProvider(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	track := h.seedRemote(t, "cased")
	seedJob(t, h, "job-cased", track.ID, domain.DownloadStateQueued)

	states, err := h.svc.CatalogRowStates(ctx, []domain.CatalogIdentity{{Provider: "WY", ExternalID: "cased"}})
	require.NoError(t, err)
	assert.Equal(t, domain.CatalogRowStateDownloading, states[domain.CatalogIdentity{Provider: "wy", ExternalID: "cased"}])
}
