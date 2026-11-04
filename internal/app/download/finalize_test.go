package download

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"airmedy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fakes ------------------------------------------------------------------

type fakeTagger struct {
	update   domain.MetadataUpdate
	tagErr   error
	readErr  error
	readPath string
	read     domain.TrackMetadataResult
	// stagedAt is where the file was when Tag was called; the test asserts the
	// file is still in staging (not yet in the library folders).
	stagedAt string
}

func (f *fakeTagger) Tag(_ context.Context, path string, update domain.MetadataUpdate) error {
	f.update = update
	f.stagedAt = path
	return f.tagErr
}

func (f *fakeTagger) Read(_ context.Context, path string) (domain.TrackMetadataResult, error) {
	f.readPath = path
	return f.read, f.readErr
}

type fakeManifest struct {
	container string
	err       error
}

func (f fakeManifest) ValidateFile(string) (string, error) { return f.container, f.err }

type fakeArtwork struct {
	data  []byte
	mime  string
	err   error
	url   string
	calls int
}

func (f *fakeArtwork) Fetch(_ context.Context, rawURL string) ([]byte, string, error) {
	f.calls++
	f.url = rawURL
	return f.data, f.mime, f.err
}

type fakeLibrary struct {
	trackID   string
	err       error
	gotPath   string
	verifyErr error
}

func (f *fakeLibrary) ImportFileWithID(_ context.Context, path string) (string, error) {
	f.gotPath = path
	return f.trackID, f.err
}

func (f *fakeLibrary) VerifyImportedTrack(_ context.Context, _ string) error {
	return f.verifyErr
}

type fakePlaylist struct {
	err      error
	gotID    string
	gotTrack string
	calls    int
}

func (f *fakePlaylist) AddTrack(_ context.Context, playlistID, trackID string) error {
	f.calls++
	f.gotID, f.gotTrack = playlistID, trackID
	return f.err
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func sampleTrack() *domain.TrackDTO {
	return &domain.TrackDTO{
		Track: domain.Track{
			ID:                  "remote-1",
			Path:                domain.RemotePath("wy", "2668397359"),
			Title:               "晴天",
			RawArtistNames:      "周杰伦",
			RawAlbumArtistNames: "周杰伦",
			Year:                2003,
			TrackNumber:         3,
			Availability:        domain.AvailabilityDownloading,
			Provider:            "wy",
			ExternalID:          "2668397359",
		},
		Album: &domain.Album{Title: "叶惠美"},
	}
}

func newFixture(t *testing.T) (root, staged string) {
	t.Helper()
	root = t.TempDir()
	staged = filepath.Join(root, ".tmp", "job-1.mp3")
	require.NoError(t, os.MkdirAll(filepath.Dir(staged), 0o755))
	require.NoError(t, os.WriteFile(staged, []byte("ID3fake-audio-bytes"), 0o644))
	return root, staged
}

func strPtr(s string) *string { return &s }

// --- tests ------------------------------------------------------------------

// TestFinalize_RetagsAReusedIdenticalFile pins P1-5: identical bytes already in
// the library used to be trusted as-is, so a file that missed its tags stayed
// tagless across every re-download. The reused file is now re-tagged and read
// back, and the staged duplicate is dropped.
func TestFinalize_RetagsAReusedIdenticalFile(t *testing.T) {
	root, staged := newFixture(t)
	existing := filepath.Join(root, "晴天 - 周杰伦.mp3")
	require.NoError(t, os.WriteFile(existing, []byte("ID3fake-audio-bytes"), 0o644))

	tagger := &fakeTagger{read: domain.TrackMetadataResult{
		Title: "晴天", Artist: "周杰伦", DurationSeconds: 269,
	}}
	f := NewFinalizer(tagger, fakeManifest{container: "mp3"}, nil, &fakeLibrary{trackID: "local-1"}, nil, quietLogger())

	res, err := f.Finalize(context.Background(), FinalizeInput{
		Path: staged, Root: root, Track: sampleTrack(), Quality: domain.Quality320k,
	})
	require.NoError(t, err)

	assert.Equal(t, existing, res.TargetPath, "the identical file keeps its place")
	assert.Equal(t, existing, tagger.stagedAt, "the reused file is tagged, not the staged copy")
	assert.Equal(t, existing, tagger.readPath, "the tags are read back from the reused file")
	assert.Equal(t, "晴天", tagger.update.Title)
	assert.Equal(t, "周杰伦", tagger.update.Artist)

	_, statErr := os.Stat(staged)
	assert.True(t, os.IsNotExist(statErr), "the staged duplicate is removed")
	_, statErr = os.Stat(existing)
	assert.NoError(t, statErr, "the library file is kept")
}

func TestFinalize_TagsImportsAndAddsToPlaylist(t *testing.T) {
	root, staged := newFixture(t)
	tagger := &fakeTagger{read: domain.TrackMetadataResult{DurationSeconds: 269, Bitrate: 320}}
	lib := &fakeLibrary{trackID: "local-1"}
	pl := &fakePlaylist{}
	art := &fakeArtwork{data: []byte("jpg"), mime: "image/jpeg"}
	f := NewFinalizer(tagger, fakeManifest{container: "mp3"}, art, lib, pl, quietLogger())

	res, err := f.Finalize(context.Background(), FinalizeInput{
		Path: staged, Root: root, Track: sampleTrack(), Quality: domain.Quality320k,
		PlaylistID: strPtr("pl-1"), CoverURL: "https://p1.music.126.net/cover.jpg",
		// Cover embedding is a setting; this test is about the full pipeline,
		// so it opts in explicitly.
		Content: domain.DownloadContent{Cover: true},
	})
	require.NoError(t, err)

	expected := filepath.Join(root, "晴天 - 周杰伦.mp3")
	assert.Equal(t, expected, res.TargetPath)
	assert.Equal(t, expected, lib.gotPath)
	assert.Equal(t, "local-1", res.TrackID)
	assert.Equal(t, 269, res.DurationSeconds)
	assert.Equal(t, 320, res.Bitrate)

	// The tag write happened while the file was still staged: a failure there
	// must not leave a half-tagged file in the library folders.
	assert.Equal(t, staged, tagger.stagedAt)
	assert.Equal(t, "晴天", tagger.update.Title)
	assert.Equal(t, "周杰伦", tagger.update.Artist)
	assert.Equal(t, "叶惠美", tagger.update.AlbumTitle)
	assert.Equal(t, 2003, tagger.update.Year)
	assert.Equal(t, 3, tagger.update.TrackNumber)
	assert.Equal(t, []byte("jpg"), tagger.update.ArtworkData)
	assert.Equal(t, "image/jpeg", tagger.update.ArtworkMIME)

	assert.Equal(t, "pl-1", pl.gotID)
	assert.Equal(t, "local-1", pl.gotTrack)

	// Staged file moved, final file present.
	_, err = os.Stat(staged)
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(expected)
	assert.NoError(t, err)
}

func TestFinalize_TagsTheAlbumNameStoredOnTheRemoteRow(t *testing.T) {
	root, staged := newFixture(t)
	tagger := &fakeTagger{read: domain.TrackMetadataResult{DurationSeconds: 269}}
	track := sampleTrack()
	// A remote row has no resolved album entity; the search result's album name
	// is the only value the ALBUM tag can come from.
	track.Album = nil
	track.RawAlbumName = "叶惠美"
	f := NewFinalizer(tagger, fakeManifest{container: "mp3"}, nil, &fakeLibrary{trackID: "local-1"}, nil, quietLogger())

	_, err := f.Finalize(context.Background(), FinalizeInput{
		Path: staged, Root: root, Track: track, Quality: domain.Quality320k,
	})
	require.NoError(t, err)
	assert.Equal(t, "叶惠美", tagger.update.AlbumTitle)
}

func TestFinalize_FailsWhenTheImportHasNoTitleOrArtist(t *testing.T) {
	root, staged := newFixture(t)
	tagger := &fakeTagger{read: domain.TrackMetadataResult{DurationSeconds: 269}}
	lib := &fakeLibrary{trackID: "local-1", verifyErr: domain.ErrDownloadPermanent}
	pl := &fakePlaylist{}
	f := NewFinalizer(tagger, fakeManifest{container: "mp3"}, nil, lib, pl, quietLogger())

	_, err := f.Finalize(context.Background(), FinalizeInput{
		Path: staged, Root: root, Track: sampleTrack(), Quality: domain.Quality320k,
		PlaylistID: strPtr("pl-1"),
	})
	require.ErrorIs(t, err, domain.ErrDownloadPermanent)
	assert.Equal(t, "", pl.gotTrack, "an unreadable import must not reach the playlist")
}

func TestFinalize_UsesDetectedContainerNotRequestedQuality(t *testing.T) {
	root, staged := newFixture(t)
	// Asked for mp3, server served FLAC: the file must be named .flac or the
	// importer and every player will misread it.
	f := NewFinalizer(&fakeTagger{}, fakeManifest{container: "flac"}, nil, &fakeLibrary{trackID: "x"}, nil, quietLogger())

	res, err := f.Finalize(context.Background(), FinalizeInput{
		Path: staged, Root: root, Track: sampleTrack(), Quality: domain.Quality320k,
	})
	require.NoError(t, err)
	assert.Equal(t, ".flac", filepath.Ext(res.TargetPath))
}

func TestFinalize_NilPlaylistSkipsAddTrack(t *testing.T) {
	root, staged := newFixture(t)
	lib := &fakeLibrary{trackID: "local-1"}
	pl := &fakePlaylist{}
	f := NewFinalizer(&fakeTagger{}, fakeManifest{container: "mp3"}, nil, lib, pl, quietLogger())

	// playlist_id is NULL when the user deleted the playlist mid-download.
	_, err := f.Finalize(context.Background(), FinalizeInput{
		Path: staged, Root: root, Track: sampleTrack(), PlaylistID: nil,
	})
	require.NoError(t, err)
	assert.Zero(t, pl.calls)
	assert.Equal(t, "local-1", lib.trackID)
}

func TestFinalize_DeletedPlaylistKeepsTheFile(t *testing.T) {
	root, staged := newFixture(t)
	pl := &fakePlaylist{err: errors.New("playlist not found: pl-1")}
	f := NewFinalizer(&fakeTagger{}, fakeManifest{container: "mp3"}, nil, &fakeLibrary{trackID: "local-1"}, pl, quietLogger())

	_, err := f.Finalize(context.Background(), FinalizeInput{
		Path: staged, Root: root, Track: sampleTrack(), PlaylistID: strPtr("pl-1"),
	})
	require.NoError(t, err, "a deleted playlist must not fail the download")
}

func TestFinalize_PlaylistErrorIsReported(t *testing.T) {
	root, staged := newFixture(t)
	pl := &fakePlaylist{err: errors.New("disk on fire")}
	f := NewFinalizer(&fakeTagger{}, fakeManifest{container: "mp3"}, nil, &fakeLibrary{trackID: "local-1"}, pl, quietLogger())

	_, err := f.Finalize(context.Background(), FinalizeInput{
		Path: staged, Root: root, Track: sampleTrack(), PlaylistID: strPtr("pl-1"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disk on fire")
}

func TestFinalize_CoverFailureDoesNotFailTheDownload(t *testing.T) {
	root, staged := newFixture(t)
	art := &fakeArtwork{err: errors.New("404 from image host")}
	tagger := &fakeTagger{}
	f := NewFinalizer(tagger, fakeManifest{container: "mp3"}, art, &fakeLibrary{trackID: "x"}, nil, quietLogger())

	_, err := f.Finalize(context.Background(), FinalizeInput{
		Path: staged, Root: root, Track: sampleTrack(), CoverURL: "https://dead.example/x.jpg",
	})
	require.NoError(t, err)
	assert.Empty(t, tagger.update.ArtworkData, "no cover is written when the fetch failed")
}

func TestFinalize_TagErrorStopsBeforeImport(t *testing.T) {
	root, staged := newFixture(t)
	tagger := &fakeTagger{tagErr: errors.New("file locked")}
	lib := &fakeLibrary{trackID: "local-1"}
	f := NewFinalizer(tagger, fakeManifest{container: "mp3"}, nil, lib, nil, quietLogger())

	_, err := f.Finalize(context.Background(), FinalizeInput{
		Path: staged, Root: root, Track: sampleTrack(),
	})
	require.Error(t, err)
	assert.Empty(t, lib.gotPath, "an import must never see a half-tagged file")
	_, statErr := os.Stat(staged)
	assert.NoError(t, statErr, "the staged file must survive so the job can retry")
}

func TestFinalize_InvalidContainerRejectedBeforeTagging(t *testing.T) {
	root, staged := newFixture(t)
	tagger := &fakeTagger{}
	f := NewFinalizer(tagger, fakeManifest{err: domain.ErrCorruptDownload}, nil, &fakeLibrary{}, nil, quietLogger())

	_, err := f.Finalize(context.Background(), FinalizeInput{
		Path: staged, Root: root, Track: sampleTrack(),
	})
	assert.ErrorIs(t, err, domain.ErrCorruptDownload)
	assert.Empty(t, tagger.update.Title)
}

func TestFinalize_ImportErrorIsPropagated(t *testing.T) {
	root, staged := newFixture(t)
	lib := &fakeLibrary{err: errors.New("db locked")}
	f := NewFinalizer(&fakeTagger{}, fakeManifest{container: "mp3"}, nil, lib, nil, quietLogger())

	_, err := f.Finalize(context.Background(), FinalizeInput{
		Path: staged, Root: root, Track: sampleTrack(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db locked")
}

func TestFinalize_RequiresRoot(t *testing.T) {
	root, staged := newFixture(t)
	f := NewFinalizer(&fakeTagger{}, fakeManifest{container: "mp3"}, nil, &fakeLibrary{}, nil, quietLogger())
	_ = root

	_, err := f.Finalize(context.Background(), FinalizeInput{Path: staged, Track: sampleTrack()})
	assert.Error(t, err)
}

func TestFinalize_RerunningOntoIdenticalFileIsIdempotent(t *testing.T) {
	root, staged := newFixture(t)
	f := NewFinalizer(&fakeTagger{}, fakeManifest{container: "mp3"}, nil, &fakeLibrary{trackID: "t"}, nil, quietLogger())

	_, err := f.Finalize(context.Background(), FinalizeInput{Path: staged, Root: root, Track: sampleTrack()})
	require.NoError(t, err)

	// Second attempt on the same bytes (a retried job after the import step
	// failed) must not error out on the existing target.
	again := filepath.Join(root, ".tmp", "job-2.mp3")
	require.NoError(t, os.WriteFile(again, []byte("ID3fake-audio-bytes"), 0o644))
	_, err = f.Finalize(context.Background(), FinalizeInput{Path: again, Root: root, Track: sampleTrack()})
	require.NoError(t, err)
	_, statErr := os.Stat(again)
	assert.True(t, os.IsNotExist(statErr), "the duplicate staged copy is removed")
}

func TestFinalize_DifferentContentGetsNumberedSuffix(t *testing.T) {
	root, staged := newFixture(t)
	f := NewFinalizer(&fakeTagger{}, fakeManifest{container: "mp3"}, nil, &fakeLibrary{trackID: "t"}, nil, quietLogger())
	first, err := f.Finalize(context.Background(), FinalizeInput{Path: staged, Root: root, Track: sampleTrack()})
	require.NoError(t, err)
	original := filepath.Join(root, "晴天 - 周杰伦.mp3")
	assert.Equal(t, original, first.TargetPath)

	// A different file with the same name must not overwrite the first one; it
	// lands as " (2)".
	other := filepath.Join(root, ".tmp", "job-2.mp3")
	require.NoError(t, os.WriteFile(other, []byte("ID3different-bytes"), 0o644))
	second, err := f.Finalize(context.Background(), FinalizeInput{Path: other, Root: root, Track: sampleTrack()})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "晴天 - 周杰伦 (2).mp3"), second.TargetPath)

	// The original is untouched.
	data, err := os.ReadFile(original)
	require.NoError(t, err)
	assert.Equal(t, []byte("ID3fake-audio-bytes"), data)

	// A third distinct file continues the sequence.
	third := filepath.Join(root, ".tmp", "job-3.mp3")
	require.NoError(t, os.WriteFile(third, []byte("ID3third-bytes"), 0o644))
	res, err := f.Finalize(context.Background(), FinalizeInput{Path: third, Root: root, Track: sampleTrack()})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "晴天 - 周杰伦 (3).mp3"), res.TargetPath)
}

func TestFinalize_ArtistTitleOrder(t *testing.T) {
	root, staged := newFixture(t)
	f := NewFinalizer(&fakeTagger{}, fakeManifest{container: "mp3"}, nil, &fakeLibrary{trackID: "t"}, nil, quietLogger())

	res, err := f.Finalize(context.Background(), FinalizeInput{
		Path: staged, Root: root, Track: sampleTrack(),
		NamingOrder: domain.NamingOrderArtistTitle,
	})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "周杰伦 - 晴天.mp3"), res.TargetPath)
}

// --- download content settings ----------------------------------------------

// TestFinalize_CoverIsSkippedWhenTheSettingIsOff: the cover is an extra network
// request, so a download with the setting off must not touch the artwork fetcher
// even when the caller supplied a URL.
func TestFinalize_CoverIsSkippedWhenTheSettingIsOff(t *testing.T) {
	root, staged := newFixture(t)
	tagger := &fakeTagger{}
	art := &fakeArtwork{data: []byte("jpg"), mime: "image/jpeg"}
	f := NewFinalizer(tagger, fakeManifest{container: "mp3"}, art, &fakeLibrary{trackID: "x"}, nil, quietLogger())

	_, err := f.Finalize(context.Background(), FinalizeInput{
		Path: staged, Root: root, Track: sampleTrack(), Quality: domain.Quality320k,
		CoverURL: "https://p1.music.126.net/cover.jpg",
		Content:  domain.DownloadContent{Cover: false},
	})
	require.NoError(t, err)

	if art.calls != 0 {
		t.Fatalf("artwork fetcher was called %d times, want 0 with the cover setting off", art.calls)
	}
	assert.Nil(t, tagger.update.ArtworkData)
}
