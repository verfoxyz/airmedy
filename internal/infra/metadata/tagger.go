package metadata

import (
	"context"
	"fmt"
	"time"

	"airmedy/internal/domain"

	"github.com/misa198/go-taglib"
)

// Tagger is the download pipeline's view of the taglib adapter: it writes the
// tags of a freshly downloaded file and reads back what the finished file
// claims about itself (duration, bitrate) so the pipeline can fill in
// tracks.duration from the real file rather than the catalogue estimate.
type Tagger struct {
	extractor *taglibExtractor
}

// NewTagger returns the domain.TrackTagger and domain.AudioPropertiesReader
// backed by taglib. Both come from one value so a caller cannot wire half of it.
func NewTagger() (*Tagger, error) {
	return &Tagger{extractor: &taglibExtractor{}}, nil
}

// Tag implements domain.TrackTagger.
func (t *Tagger) Tag(ctx context.Context, path string, update domain.MetadataUpdate) error {
	return t.extractor.WriteMetadata(ctx, path, update)
}

// Read implements domain.TrackTagger. It reports the raw container truth: the
// tagger clears all frames on Tag, so a tag written by someone else's tool does
// not leak into the library import that follows.
func (t *Tagger) Read(ctx context.Context, path string) (domain.TrackMetadataResult, error) {
	dto, err := t.extractor.Extract(ctx, path)
	if err != nil {
		return domain.TrackMetadataResult{}, fmt.Errorf("read tagged file %s: %w", path, err)
	}
	res := domain.TrackMetadataResult{
		Title:           dto.Title,
		Artist:          dto.RawArtistNames,
		AlbumArtist:     dto.RawAlbumArtistNames,
		Year:            dto.Year,
		TrackNumber:     dto.TrackNumber,
		TotalTracks:     dto.TotalTracks,
		DurationSeconds: dto.Duration,
		Bitrate:         dto.Bitrate,
	}
	if dto.Album != nil {
		res.Album = dto.Album.Title
	}
	return res, nil
}

// Duration implements domain.AudioPropertiesReader, used by the downloader to
// reject a file that decoded to a different length than the track.
//
// It reads the container properties directly instead of going through
// Extract: the duration check runs before any tags exist, and Extract fails on
// a partially tagged file.
func (t *Tagger) Duration(path string) (time.Duration, error) {
	props, err := taglib.ReadProperties(path)
	if err != nil {
		return 0, fmt.Errorf("read properties of %s: %w", path, err)
	}
	return props.Length, nil
}
