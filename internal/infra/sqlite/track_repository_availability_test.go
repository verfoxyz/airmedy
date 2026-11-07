package sqlite

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"airmedy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAvailabilityTestRepo opens a real migrated database so the SQL under test
// is exercised as the application runs it.
func newAvailabilityTestRepo(t *testing.T) (domain.TrackRepository, *DB) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := NewDB(filepath.Join(t.TempDir(), "availability.db"), logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewTrackRepository(db), db
}

func localTrack(id, path string, duration int) *domain.Track {
	return &domain.Track{
		ID:        id,
		Path:      path,
		Title:     "Rude Buster (Camellia Remix)",
		SortTitle: "rude buster (camellia remix)",
		Format:    "flac",
		Duration:  duration,
	}
}

// TestFindPlayableByDuration_MatchesLegacyEmptyAvailability: imported rows
// written before the normalisation use `availability = ”`, which the domain
// treats as ready. The candidate query must agree or the "in library" badge
// never matches an imported file.
func TestFindPlayableByDuration_MatchesLegacyEmptyAvailability(t *testing.T) {
	repo, db := newAvailabilityTestRepo(t)
	ctx := context.Background()

	track := localTrack("legacy-empty", `D:\MYHOME\MUSIC\rude.flac`, 348)
	require.NoError(t, repo.Save(ctx, track))
	// Reproduce a row created before the write-side normalisation.
	_, err := db.ExecContext(ctx, `UPDATE tracks SET availability = '' WHERE id = ?`, track.ID)
	require.NoError(t, err)

	got, err := repo.FindPlayableByDuration(ctx, 346, 352)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "legacy-empty", got[0].ID)
}

// TestFindPlayableByDuration_ExcludesNonReadyAndVirtualPaths: the query only
// returns playable local files, whatever the legacy availability spelling.
func TestFindPlayableByDuration_ExcludesNonReadyAndVirtualPaths(t *testing.T) {
	repo, _ := newAvailabilityTestRepo(t)
	ctx := context.Background()

	ready := localTrack("ready", "/m/ready.flac", 350)
	ready.Availability = domain.AvailabilityReady
	require.NoError(t, repo.Save(ctx, ready))

	remoteState := localTrack("remote-state", "/m/remote-state.flac", 350)
	remoteState.Availability = domain.AvailabilityRemote
	require.NoError(t, repo.Save(ctx, remoteState))

	virtual := localTrack("virtual", "remote://wy/1", 350)
	virtual.Availability = domain.AvailabilityReady
	require.NoError(t, repo.Save(ctx, virtual))

	got, err := repo.FindPlayableByDuration(ctx, 348, 352)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "ready", got[0].ID)
}

// TestSave_NormalisesEmptyAvailability: a new row written by the importer must
// not create another legacy empty string.
func TestSave_NormalisesEmptyAvailability(t *testing.T) {
	repo, db := newAvailabilityTestRepo(t)
	ctx := context.Background()

	track := localTrack("normalised", "/m/normalised.flac", 348)
	require.NoError(t, repo.Save(ctx, track))

	var availability string
	require.NoError(t, db.GetContext(ctx, &availability,
		`SELECT availability FROM tracks WHERE id = ?`, track.ID))
	assert.Equal(t, domain.AvailabilityReady, availability)
}
