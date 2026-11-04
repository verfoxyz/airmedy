package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"airmedy/internal/domain"
)

// LibraryImporter is the library capability the finalize step needs. The app
// layer receives the concrete *library.LibraryService; taking the port here
// keeps finalize testable with a fake and makes the dependency explicit.
type LibraryImporter interface {
	// ImportFileWithID imports the file and returns the track id it produced.
	ImportFileWithID(ctx context.Context, path string) (string, error)
	// VerifyImportedTrack re-reads the imported row with the library's own
	// extractor. A row without a title or artist is not a usable library entry:
	// it cannot be found by search and cannot drive lyrics lookup. The
	// implementation removes the row (and its file) before returning the error,
	// so a failed tag write does not leave a ghost row behind.
	VerifyImportedTrack(ctx context.Context, trackID string) error
}

// PlaylistAdder adds a track to a playlist. playlistID is never empty at the
// call site: a job whose playlist was deleted has a nil id and skips this step.
type PlaylistAdder interface {
	AddTrack(ctx context.Context, playlistID, trackID string) error
}

// Finalizer turns a validated download into a library track:
//
//	plan a path from the track metadata -> tag the staged file -> validate what
//	the file says it is -> import it -> attach it to the requesting playlist.
//
// Failures are wrapped with a retry policy when they are retryable (a tag write
// that hit a locked file is worth another attempt) and returned as-is for
// permanent problems, which the job records as a hard failure.
type Finalizer struct {
	tagger   domain.TrackTagger
	manifest domain.FileValidator
	artwork  domain.ArtworkFetcher
	library  LibraryImporter
	playlist PlaylistAdder
	logger   *slog.Logger
}

// NewFinalizer wires the finalize step. artwork and playlist may be nil: a nil
// fetcher skips embedding cover art, a nil adder skips the playlist step.
func NewFinalizer(
	tagger domain.TrackTagger,
	manifest domain.FileValidator,
	artwork domain.ArtworkFetcher,
	library LibraryImporter,
	playlist PlaylistAdder,
	logger *slog.Logger,
) *Finalizer {
	return &Finalizer{
		tagger:   tagger,
		manifest: manifest,
		artwork:  artwork,
		library:  library,
		playlist: playlist,
		logger:   logger,
	}
}

// FinalizeInput is everything one finalize run needs.
type FinalizeInput struct {
	// Path is the downloaded file (already renamed out of .part).
	Path string
	// Root is the download directory the file name hangs off.
	Root string
	// NamingOrder is one of domain.NamingOrder*; empty means title first.
	NamingOrder string
	// Track is the remote track being materialised. It supplies the tag values
	// and the destination title/artist/album.
	Track *domain.TrackDTO
	// Quality is the job's quality tier.
	Quality string
	// PlaylistID is where the track should be added, or nil when the playlist
	// that requested the download has since been deleted.
	PlaylistID *string
	// CoverURL, when non-empty, is fetched and embedded as the cover image.
	CoverURL string
	// Content says which extras to fetch and write. Its zero value writes the
	// audio file only, which is what a caller that predates the setting gets.
	Content domain.DownloadContent
}

// FinalizeResult reports what the finalize run produced.
type FinalizeResult struct {
	// TrackID is the library row that now owns the file.
	TrackID string
	// TargetPath is the final location inside the library.
	TargetPath string
	// DurationSeconds is the duration the tag reader reported (0 if unknown).
	// The download service prefers MediaDurationProbe and uses this only when
	// the probe cannot read the container.
	DurationSeconds int
	// Bitrate is the bitrate the file header reports (0 if unknown).
	Bitrate int
}

// Finalize implements the落盘/打标签/入库 step.
//
// Order matters: the tags are written while the file is still in the staging
// directory, so a tag write that fails leaves nothing behind in the library
// folders and the next attempt can retry on the same bytes.
func (f *Finalizer) Finalize(ctx context.Context, in FinalizeInput) (FinalizeResult, error) {
	if in.Track == nil {
		return FinalizeResult{}, fmt.Errorf("finalize: nil track")
	}
	if strings.TrimSpace(in.Root) == "" {
		return FinalizeResult{}, fmt.Errorf("finalize: no download directory configured")
	}
	if err := ctx.Err(); err != nil {
		return FinalizeResult{}, err
	}

	// 1. Learn what the file actually is before naming it: the resolver does
	// not promise the container it serves, and a quality tier downgrade (320k
	// request answered with flac) must not produce a ".mp3" full of FLAC.
	container, err := f.manifest.ValidateFile(in.Path)
	if err != nil {
		return FinalizeResult{}, fmt.Errorf("finalize: validate %s: %w", in.Path, err)
	}
	ext := strings.TrimPrefix(container, ".")
	if ext == "" {
		ext = domain.ResolveDownloadQuality(in.Quality)
	}

	spec := fileSpec{
		Artist: tagArtist(in.Track),
		Title:  tagTitle(in.Track),
		Ext:    ext,
	}
	rel, err := relativePath(spec, in.NamingOrder)
	if err != nil {
		return FinalizeResult{}, err
	}
	targetPath := filepath.Join(in.Root, rel)
	// A re-download of identical bytes keeps the existing file; a different file
	// with the same name gets " (2)", " (3)"… so nothing is overwritten. Reusing
	// a file does not mean it is tagged: byte equality says nothing about tags, so
	// the reused file is re-tagged below.
	targetPath, reused, err := uniqueTargetPath(in.Path, targetPath)
	if err != nil {
		return FinalizeResult{}, err
	}

	// 2. Tag the file in place. A reused target is tagged directly: an identical
	// file may have been left untagged by an older build or an interrupted run,
	// and trusting its bytes used to keep that file tagless forever.
	tagPath := in.Path
	if reused {
		tagPath = targetPath
	}
	update := domain.MetadataUpdate{
		Title:       tagTitle(in.Track),
		Artist:      tagArtist(in.Track),
		AlbumArtist: tagAlbumArtist(in.Track),
		AlbumTitle:  tagAlbum(in.Track),
		Year:        yearComponent(in.Track.Year),
		TrackNumber: f.trackNumber(in.Track),
	}
	if in.Content.Cover && strings.TrimSpace(in.CoverURL) != "" {
		if err := f.attachCover(ctx, in.CoverURL, &update); err != nil {
			// Cover art is a nicety: a dead URL must not fail the download.
			f.logger.Warn("download: cover art unavailable", "track_id", in.Track.ID, "error", err)
		}
	}
	if err := f.tagger.Tag(ctx, tagPath, update); err != nil {
		return FinalizeResult{}, fmt.Errorf("finalize: tag %s: %w", tagPath, err)
	}

	// 3. Read back what the finished file claims; the duration here replaces
	// the catalogue estimate in the library row.
	meta, err := f.tagger.Read(ctx, tagPath)
	if err != nil {
		return FinalizeResult{}, fmt.Errorf("finalize: read back %s: %w", tagPath, err)
	}
	// A file with no title/artist in its tags is what the user sees in the
	// explorer, so a write the tagger silently dropped must be visible in the log.
	if strings.TrimSpace(meta.Title) == "" || strings.TrimSpace(meta.Artist) == "" {
		f.logger.Warn("download: the finished file has no title or artist tags",
			"track_id", in.Track.ID, "path", tagPath, "title", meta.Title, "artist", meta.Artist)
	}

	// 4. Put the file into the library layout. The rename happens before the
	// import because the importer derives the track id from the path. A reused
	// target already holds the named file, so only the staged duplicate is
	// dropped (it is no longer identical: the target carries the fresh tags).
	if reused {
		if err := os.Remove(in.Path); err != nil && !os.IsNotExist(err) {
			return FinalizeResult{}, fmt.Errorf("finalize: remove duplicate staged file %s: %w", in.Path, err)
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return FinalizeResult{}, fmt.Errorf("finalize: create %s: %w", filepath.Dir(targetPath), err)
		}
		if err := moveFile(in.Path, targetPath); err != nil {
			return FinalizeResult{}, err
		}
	}

	// 5. Import and attach.
	trackID, err := f.library.ImportFileWithID(ctx, targetPath)
	if err != nil {
		return FinalizeResult{}, fmt.Errorf("finalize: import %s: %w", targetPath, err)
	}
	// 5a. Validate with the library's own parse, not the tagger's read-back: a
	// tag write only the tagger can see would still look successful to itself.
	// A row without title/artist is discarded so it cannot become a search-less
	// ghost in every playlist that requested the download.
	if err := f.library.VerifyImportedTrack(ctx, trackID); err != nil {
		return FinalizeResult{}, fmt.Errorf("finalize: verify %s: %w", targetPath, err)
	}

	// 5b. Lyrics run in Service after the merge: fetching here would attach them
	// to the remote row, which the merge then deletes (ON DELETE CASCADE).

	if in.PlaylistID != nil && f.playlist != nil {
		// A deleted playlist is a normal outcome (job.PlaylistID is ON DELETE
		// SET NULL), so only a real failure is reported.
		if err := f.playlist.AddTrack(ctx, *in.PlaylistID, trackID); err != nil {
			if isMissingPlaylist(err) {
				f.logger.Info("download: requesting playlist is gone, keeping the file",
					"playlist_id", *in.PlaylistID, "track_id", trackID)
			} else {
				return FinalizeResult{}, fmt.Errorf("finalize: add %s to playlist %s: %w", trackID, *in.PlaylistID, err)
			}
		}
	}

	return FinalizeResult{
		TrackID:         trackID,
		TargetPath:      targetPath,
		DurationSeconds: meta.DurationSeconds,
		Bitrate:         meta.Bitrate,
	}, nil
}

// attachCover downloads and attaches the cover image, if there is a URL.
func (f *Finalizer) attachCover(ctx context.Context, coverURL string, update *domain.MetadataUpdate) error {
	if strings.TrimSpace(coverURL) == "" || f.artwork == nil {
		return nil
	}
	data, mime, err := f.artwork.Fetch(ctx, coverURL)
	if err != nil {
		return fmt.Errorf("fetch cover: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	update.ArtworkData = data
	update.ArtworkMIME = mime
	return nil
}

// trackNumber returns the catalogue track number, or the "unknown" sentinel
// (0) so the file still sorts predictably.
func (f *Finalizer) trackNumber(track *domain.TrackDTO) int {
	if track.TrackNumber < 0 {
		return unknownTrackNumber
	}
	return track.TrackNumber
}

// maxNameCollisions bounds the "(N)" search for a free file name.
const maxNameCollisions = 1000

// uniqueTargetPath returns the path to move src to and whether that path
// already holds identical bytes (a re-download, which the caller must still
// re-tag). The base name wins when it is free or already holds identical bytes;
// a different file with the same name pushes the result to " (2)", " (3)"… so
// no existing file is overwritten.
func uniqueTargetPath(src, base string) (path string, reused bool, err error) {
	dir := filepath.Dir(base)
	name := filepath.Base(base)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)

	for n := 1; n <= maxNameCollisions; n++ {
		candidate := base
		if n > 1 {
			candidate = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, n, ext))
		}

		same, err := sameContent(src, candidate)
		if err != nil {
			return "", false, fmt.Errorf("finalize: compare %s with %s: %w", src, candidate, err)
		}
		if same {
			return candidate, true, nil
		}
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate, false, nil
		} else if err != nil {
			return "", false, fmt.Errorf("finalize: stat %s: %w", candidate, err)
		}
	}
	return "", false, fmt.Errorf("finalize: no free file name for %s after %d attempts", base, maxNameCollisions)
}

// moveFile renames src onto dst, tolerating the case where dst already exists
// with identical content (re-downloading a track whose row was lost) and
// refusing to clobber a different file that appeared after the target was
// resolved, which the merge step handles instead.
func moveFile(src, dst string) error {
	if same, err := sameContent(src, dst); err != nil {
		return fmt.Errorf("finalize: compare %s with %s: %w", src, dst, err)
	} else if same {
		// Identical bytes are already in place: drop the staged copy.
		if err := os.Remove(src); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("finalize: remove duplicate staged file %s: %w", src, err)
		}
		return nil
	}

	if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
		return fmt.Errorf("finalize: target %s already exists with different content", dst)
	}

	if err := os.Rename(src, dst); err != nil {
		// Cross-volume (download dir on another drive than the staging dir) or
		// a Windows sharing violation: fall back to a copy.
		if copyErr := copyFile(src, dst); copyErr != nil {
			return fmt.Errorf("finalize: move %s to %s: %w", src, dst, errors.Join(err, copyErr))
		}
		if err := os.Remove(src); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("finalize: remove staged file %s: %w", src, err)
		}
	}
	return nil
}

// sameContent reports whether dst exists and holds exactly src's bytes.
func sameContent(src, dst string) (bool, error) {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return false, err
	}
	dstInfo, err := os.Stat(dst)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	// Size is a cheap negative filter; byte comparison is what decides.
	if srcInfo.Size() != dstInfo.Size() {
		return false, nil
	}
	return filesEqual(src, dst)
}

func filesEqual(a, b string) (bool, error) {
	dataA, err := os.ReadFile(a)
	if err != nil {
		return false, err
	}
	dataB, err := os.ReadFile(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(dataA, dataB), nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

// isMissingPlaylist reports the "playlist not found" error the playlist service
// returns when the row was deleted between an enqueue and the finalize.
func isMissingPlaylist(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "playlist not found")
}

// --- tag values -------------------------------------------------------------

// The remote track carries only raw artist strings (search results are not
// entity-resolved), so the tag values come from those, with the resolved
// entities as a fallback for a track that came back from the database.

func tagArtist(track *domain.TrackDTO) string {
	if name := strings.TrimSpace(track.RawArtistNames); name != "" {
		return name
	}
	if len(track.Artists) > 0 && track.Artists[0] != nil {
		return strings.TrimSpace(track.Artists[0].Name)
	}
	return ""
}

func tagAlbumArtist(track *domain.TrackDTO) string {
	if name := strings.TrimSpace(track.RawAlbumArtistNames); name != "" {
		return name
	}
	if len(track.AlbumArtists) > 0 && track.AlbumArtists[0] != nil {
		return strings.TrimSpace(track.AlbumArtists[0].Name)
	}
	return ""
}

func tagAlbum(track *domain.TrackDTO) string {
	if track.Album != nil {
		if title := strings.TrimSpace(track.Album.Title); title != "" {
			return title
		}
	}
	// A remote row has no album entity: the search result's album name is stored
	// on the row itself and is the only source for the ALBUM tag.
	if title := strings.TrimSpace(track.RawAlbumName); title != "" {
		return title
	}
	return ""
}

func tagTitle(track *domain.TrackDTO) string {
	if title := strings.TrimSpace(track.Title); title != "" {
		return title
	}
	return defaultTrackName
}
