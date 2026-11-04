package metadata

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"airmedy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fixture mirrors the downloader's: an ID3v2.4 header plus MPEG-1 Layer III
// frames carrying a Xing frame count, so taglib can decode a real duration.
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
		frame[2] = 0x10 // 32 kbps, 44.1 kHz
		if i == 0 {
			copy(frame[36:], []byte("Xing"))
			frame[43] = 0x01 // frames field present
			n := uint32(frames)
			frame[44], frame[45], frame[46], frame[47] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
		}
		buf.Write(frame)
	}
	return buf.Bytes()
}

func writeTempMP3(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "track.mp3")
	require.NoError(t, os.WriteFile(path, buildSilentMP3(30), 0o644))
	return path
}

func TestTagger_DurationReadsUntaggedFile(t *testing.T) {
	tagger, err := NewTagger()
	require.NoError(t, err)

	got, err := tagger.Duration(writeTempMP3(t))
	require.NoError(t, err)
	assert.InDelta(t, 30, got.Seconds(), 1.0)
}

func TestTagger_TagThenReadRoundTrip(t *testing.T) {
	tagger, err := NewTagger()
	require.NoError(t, err)
	path := writeTempMP3(t)

	update := domain.MetadataUpdate{
		Title:       "晴天",
		Artist:      "周杰伦",
		AlbumArtist: "周杰伦",
		AlbumTitle:  "叶惠美",
		Year:        2003,
		TrackNumber: 3,
		TotalTracks: 11,
		Genre:       "Pop",
	}
	require.NoError(t, tagger.Tag(context.Background(), path, update))

	res, err := tagger.Read(context.Background(), path)
	require.NoError(t, err)
	assert.Equal(t, "晴天", res.Title)
	assert.Equal(t, "周杰伦", res.Artist)
	assert.Equal(t, "叶惠美", res.Album)
	assert.Equal(t, 2003, res.Year)
	assert.Equal(t, 3, res.TrackNumber)
	assert.Equal(t, 11, res.TotalTracks)
	assert.InDelta(t, 30, res.DurationSeconds, 1)
}

func TestTagger_DurationOnNonAudioReportsZero(t *testing.T) {
	tagger, err := NewTagger()
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "not-audio.mp3")
	require.NoError(t, os.WriteFile(path, []byte("<html>error</html>"), 0o644))

	// taglib does not reject a non-audio file, it reports zero length. The
	// downloader treats <= 0 as "unknown" and relies on its magic-byte check
	// instead, so this pinning test documents that split of responsibility.
	got, err := tagger.Duration(path)
	require.NoError(t, err)
	assert.Zero(t, got)
}

func TestTagger_DurationWithinToleranceOfCatalogue(t *testing.T) {
	// The downloader compares this against the catalogue duration; a one-second
	// wobble on a 30s track must stay inside its 3s tolerance.
	tagger, err := NewTagger()
	require.NoError(t, err)
	got, err := tagger.Duration(writeTempMP3(t))
	require.NoError(t, err)
	assert.Less(t, (got - 30*time.Second).Abs(), 3*time.Second)
}
