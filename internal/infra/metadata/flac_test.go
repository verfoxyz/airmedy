package metadata

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"airmedy/internal/domain"
)

// buildTestFLAC returns a minimal native FLAC file: the fLaC marker, one
// STREAMINFO block (34 zero bytes are enough for a metadata rewrite; this test
// never decodes audio) and a fake audio payload.
func buildTestFLAC(audio []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("fLaC")
	buf.Write([]byte{0x80 | flacBlockStreamInfo, 0x00, 0x00, 0x22})
	buf.Write(make([]byte, 34))
	buf.Write(audio)
	return buf.Bytes()
}

func writeTestFile(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "track.flac")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readBlocks(t *testing.T, path string) []flacMetadata {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	blocks, _, err := readFLACMetadata(f, 0)
	if err != nil {
		t.Fatal(err)
	}
	return blocks
}

func TestWriteFLACMetadata_WritesVorbisCommentsWithoutTouchingAudio(t *testing.T) {
	audio := []byte("fake-audio-frames-0123456789")
	path := writeTestFile(t, buildTestFLAC(audio))

	w := NewTagLibWriter()
	if err := w.WriteMetadata(context.Background(), path, domain.MetadataUpdate{
		Title:       "前前前世",
		Artist:      "RADWIMPS",
		AlbumArtist: "RADWIMPS",
		AlbumTitle:  "你的名字。",
		Year:        2016,
		TrackNumber: 1,
	}); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("fLaC")) {
		t.Fatalf("file does not start with fLaC: %q", data[:4])
	}
	// TagLib's ID3 fallback put TIT2/TPE1 into the audio region; the native
	// writer must never produce those frames.
	if bytes.Contains(data, []byte("TIT2")) || bytes.Contains(data, []byte("TPE1")) {
		t.Fatal("FLAC file still contains ID3v2 frames")
	}

	blocks := readBlocks(t, path)
	var tags map[string]string
	for _, block := range blocks {
		if block.blockType == flacBlockVorbisComment {
			tags, err = parseVorbisComment(block.data)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if tags["TITLE"] != "前前前世" || tags["ARTIST"] != "RADWIMPS" || tags["ALBUM"] != "你的名字。" {
		t.Fatalf("vorbis comments = %v", tags)
	}

	// The payload must still be byte-identical after the metadata block.
	if !bytes.HasSuffix(data, audio) {
		t.Fatal("audio payload changed")
	}
}

func TestWriteFLACMetadata_AddsAPictureBlock(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+ip1sAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	path := writeTestFile(t, buildTestFLAC([]byte("audio")))

	if err := NewTagLibWriter().WriteMetadata(context.Background(), path, domain.MetadataUpdate{
		Title:       "晴天",
		Artist:      "周杰伦",
		ArtworkData: png,
		ArtworkMIME: "image/png",
	}); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}

	found := false
	for _, block := range readBlocks(t, path) {
		if block.blockType == flacBlockPicture {
			found = true
		}
	}
	if !found {
		t.Fatal("cover was not written as METADATA_BLOCK_PICTURE")
	}
}

func TestWriteFLACMetadata_DropsALeadingID3Tag(t *testing.T) {
	var data bytes.Buffer
	// ID3v2 header with size 0: 10 bytes, no frames.
	data.Write([]byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, 0})
	flac := buildTestFLAC([]byte("audio"))
	data.Write(flac)
	path := writeTestFile(t, data.Bytes())

	if err := NewTagLibWriter().WriteMetadata(context.Background(), path, domain.MetadataUpdate{
		Title: "晴天", Artist: "周杰伦",
	}); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, []byte("fLaC")) {
		t.Fatal("leading ID3 tag was not removed")
	}
	if bytes.Contains(out, []byte("ID3")) {
		t.Fatal("file still contains an ID3 tag")
	}
}

func TestWriteFLACMetadata_RejectsNonFlacBytes(t *testing.T) {
	path := writeTestFile(t, []byte("not a flac stream at all"))
	err := writeFLACMetadata(path, domain.MetadataUpdate{Title: "x", Artist: "y"})
	if err == nil || !strings.Contains(err.Error(), "not a standard FLAC") {
		t.Fatalf("err = %v, want a non-FLAC refusal", err)
	}
}
