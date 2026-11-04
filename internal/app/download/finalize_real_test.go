package download_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"airmedy/internal/app/download"
	"airmedy/internal/domain"
	"airmedy/internal/infra/downloader"
	"airmedy/internal/infra/metadata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This test wires the real taglib adapter instead of a fake: the acceptance
// criterion for the finalize step is "the tags written by the pipeline can be
// read back by metadata.Extract", and only a real write proves that.

const (
	mp3FrameLen     = 104
	mp3FrameSeconds = 1152.0 / 44100.0
)

func buildSilentMP3(seconds float64) []byte {
	frames := int(seconds / mp3FrameSeconds)
	var buf bytes.Buffer
	buf.WriteString("ID3\x04\x00\x00\x00\x00\x00\x00")
	for i := 0; i < frames; i++ {
		frame := make([]byte, mp3FrameLen)
		frame[0], frame[1] = 0xFF, 0xFB
		frame[2] = 0x10
		if i == 0 {
			copy(frame[36:], []byte("Xing"))
			frame[43] = 0x01
			n := uint32(frames)
			frame[44], frame[45], frame[46], frame[47] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
		}
		buf.Write(frame)
	}
	return buf.Bytes()
}

type capturingLibrary struct {
	gotPath string
	// extractor is a second, independent reader: it proves the tags are in the
	// file, not just in the writer's memory.
	extractor domain.MetadataExtractor
	dto       *domain.TrackDTO
}

func (c *capturingLibrary) ImportFileWithID(ctx context.Context, path string) (string, error) {
	c.gotPath = path
	dto, err := c.extractor.Extract(ctx, path)
	if err != nil {
		return "", err
	}
	c.dto = dto
	return "imported-" + filepath.Base(path), nil
}

func (c *capturingLibrary) VerifyImportedTrack(_ context.Context, _ string) error {
	return nil
}

func TestFinalizeRealTags_AreReadableByMetadataExtract(t *testing.T) {
	tagger, err := metadata.NewTagger()
	require.NoError(t, err)

	root := t.TempDir()
	staged := filepath.Join(root, downloader.DefaultTmpDirName, "job.mp3")
	require.NoError(t, os.MkdirAll(filepath.Dir(staged), 0o755))
	require.NoError(t, os.WriteFile(staged, buildSilentMP3(30), 0o644))

	lib := &capturingLibrary{extractor: metadata.NewTagLibExtractor()}
	f := download.NewFinalizer(tagger, downloader.New(nil), nil, lib, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	track := &domain.TrackDTO{
		Track: domain.Track{
			ID:                  "remote-1",
			Path:                domain.RemotePath("wy", "123"),
			Title:               "晴天",
			RawArtistNames:      "周杰伦",
			RawAlbumArtistNames: "周杰伦",
			Year:                2003,
			TrackNumber:         3,
			Provider:            "wy",
			ExternalID:          "123",
		},
		Album: &domain.Album{Title: "叶惠美"},
	}

	res, err := f.Finalize(context.Background(), download.FinalizeInput{
		Path: staged, Root: root, Track: track, Quality: domain.Quality320k,
	})
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(root, "晴天 - 周杰伦.mp3"), res.TargetPath)
	assert.Equal(t, res.TargetPath, lib.gotPath, "the import must see the final path, not the staging one")

	// metadata.Extract (the real extractor) reads the tags back.
	require.NotNil(t, lib.dto)
	assert.Equal(t, "晴天", lib.dto.Title)
	assert.Equal(t, "周杰伦", lib.dto.RawArtistNames)
	assert.Equal(t, "叶惠美", lib.dto.Album.Title)
	assert.Equal(t, 3, lib.dto.TrackNumber)
	assert.Equal(t, 2003, lib.dto.Year)
	assert.InDelta(t, 30, lib.dto.Duration, 1, "duration comes from the file, not the catalogue")

	// The finalize reports the file's own duration so the library row can
	// replace the catalogue estimate (§8 of the design).
	assert.InDelta(t, 30, res.DurationSeconds, 1)

	// Nothing is left in the staging directory.
	entries, err := os.ReadDir(filepath.Join(root, downloader.DefaultTmpDirName))
	require.NoError(t, err)
	assert.Empty(t, entries)
}
