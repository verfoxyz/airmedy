package metadata

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"airmedy/internal/domain"
)

// FLAC block types (RFC 9639 §8.1).
const (
	flacBlockStreamInfo    = 0
	flacBlockPadding       = 1
	flacBlockVorbisComment = 4
	flacBlockPicture       = 6
)

// flacPictureFrontCover is the picture type for a front cover.
const flacPictureFrontCover = 3

// flacMetadata is one metadata block read from a FLAC file.
type flacMetadata struct {
	blockType byte
	data      []byte
}

// writeFLACMetadata writes tags as a native Vorbis comment block and, when an
// image is present, a standard METADATA_BLOCK_PICTURE.
//
// It exists because TagLib falls back to ID3v2 on a FLAC stream that has no
// Vorbis-comment block (the "raw FLAC" files some stream resolvers produce).
// Writing ID3 frames into a FLAC stream puts bytes inside the audio payload,
// which corrupts decoding mid-file. This writer only rewrites the metadata
// region before the first frame; the audio bytes are copied verbatim and the
// copy is verified byte-for-byte.
func writeFLACMetadata(path string, fields domain.MetadataUpdate) error {
	src, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open flac %s: %w", path, err)
	}
	defer func() { _ = src.Close() }()

	start, ok, err := flacAudioStart(src)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("tag %s: not a standard FLAC stream (no fLaC marker after any ID3 tag)", path)
	}

	blocks, audioOffset, err := readFLACMetadata(src, start)
	if err != nil {
		return err
	}

	// Replace the comment block and, when a new cover is supplied, all picture
	// blocks; anything else (seek table, cuesheet, application blocks) is kept.
	comment := encodeVorbisComment(fields)
	picture, picErr := encodeFLACPicture(fields.ArtworkData, fields.ArtworkMIME)
	if picErr != nil {
		// A cover is a nicety; a malformed image must not fail the download.
		picture = nil
	}

	var kept []flacMetadata
	var streamInfo *flacMetadata
	for i := range blocks {
		switch blocks[i].blockType {
		case flacBlockStreamInfo:
			streamInfo = &blocks[i]
		case flacBlockVorbisComment:
			// Replaced below.
		case flacBlockPicture:
			if picture == nil {
				kept = append(kept, blocks[i])
			}
		default:
			kept = append(kept, blocks[i])
		}
	}
	if streamInfo == nil {
		return fmt.Errorf("tag %s: FLAC stream has no STREAMINFO block", path)
	}

	out := make([]flacMetadata, 0, len(kept)+3)
	out = append(out, *streamInfo)
	out = append(out, flacMetadata{blockType: flacBlockVorbisComment, data: comment})
	if picture != nil {
		out = append(out, flacMetadata{blockType: flacBlockPicture, data: picture})
	}
	out = append(out, kept...)

	info, err := src.Stat()
	if err != nil {
		return fmt.Errorf("stat flac %s: %w", path, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".airmedy-flac-*")
	if err != nil {
		return fmt.Errorf("create flac temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	audioHash := sha256.New()
	if _, err := tmp.Write([]byte("fLaC")); err != nil {
		cleanup()
		return fmt.Errorf("write flac header: %w", err)
	}
	for i := range out {
		last := i == len(out)-1
		if err := writeFLACBlock(tmp, out[i], last); err != nil {
			cleanup()
			return fmt.Errorf("write flac metadata block %d: %w", i, err)
		}
	}

	if _, err := src.Seek(audioOffset, io.SeekStart); err != nil {
		cleanup()
		return fmt.Errorf("seek flac audio: %w", err)
	}
	if _, err := io.Copy(io.MultiWriter(tmp, audioHash), src); err != nil {
		cleanup()
		return fmt.Errorf("copy flac audio: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close flac temp file: %w", err)
	}
	// Windows refuses to replace a file that is still open by this process.
	if err := src.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close flac %s: %w", path, err)
	}

	// Prove the audio payload survived unchanged before replacing the file.
	if err := verifyFLACCopy(tmpPath, audioHash.Sum(nil)); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("verify tagged flac %s: %w", path, err)
	}
	if err := os.Chmod(tmpPath, info.Mode().Perm()); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod tagged flac: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace flac %s: %w", path, err)
	}
	return nil
}

// isFLACFile reports whether the bytes are a native FLAC stream (optionally
// behind a stray ID3v2 tag). Ogg-encapsulated FLAC starts with OggS and is left
// to TagLib, which writes Vorbis comments correctly there.
func isFLACFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	_, ok, err := flacAudioStart(f)
	return err == nil && ok
}

// flacAudioStart returns the offset of the fLaC marker. A leading ID3v2 tag is
// tolerated and skipped (it is non-standard on FLAC and is dropped on rewrite).
func flacAudioStart(r *os.File) (int64, bool, error) {
	head := make([]byte, 10)
	n, err := r.ReadAt(head, 0)
	if err != nil && err != io.EOF {
		return 0, false, fmt.Errorf("read flac header: %w", err)
	}
	if n >= 4 && bytes.Equal(head[:4], []byte("fLaC")) {
		return 0, true, nil
	}
	if n >= 10 && bytes.Equal(head[:3], []byte("ID3")) {
		size := int64(binary.BigEndian.Uint32([]byte{
			head[6] & 0x7f, head[7] & 0x7f, head[8] & 0x7f, head[9] & 0x7f,
		}))
		offset := int64(10) + size
		if head[5]&0x10 != 0 { // footer present
			offset += 10
		}
		marker := make([]byte, 4)
		if _, err := r.ReadAt(marker, offset); err != nil {
			return 0, false, nil
		}
		if bytes.Equal(marker, []byte("fLaC")) {
			return offset, true, nil
		}
	}
	return 0, false, nil
}

// readFLACMetadata parses the block chain and returns the blocks plus the
// offset where the audio frames begin.
func readFLACMetadata(r *os.File, start int64) ([]flacMetadata, int64, error) {
	pos := start + 4
	var blocks []flacMetadata
	for {
		header := make([]byte, 4)
		if _, err := r.ReadAt(header, pos); err != nil {
			return nil, 0, fmt.Errorf("read flac block header at %d: %w", pos, err)
		}
		last := header[0]&0x80 != 0
		blockType := header[0] & 0x7f
		length := int64(header[1])<<16 | int64(header[2])<<8 | int64(header[3])
		pos += 4
		if length < 0 || length > 1<<30 {
			return nil, 0, fmt.Errorf("flac block %d has an invalid length %d", blockType, length)
		}
		data := make([]byte, length)
		if _, err := r.ReadAt(data, pos); err != nil {
			return nil, 0, fmt.Errorf("read flac block %d: %w", blockType, err)
		}
		pos += length
		blocks = append(blocks, flacMetadata{blockType: blockType, data: data})
		if last {
			return blocks, pos, nil
		}
	}
}

func writeFLACBlock(w io.Writer, block flacMetadata, last bool) error {
	if len(block.data) >= 1<<24 {
		return fmt.Errorf("metadata block of type %d is too large", block.blockType)
	}
	first := block.blockType & 0x7f
	if last {
		first |= 0x80
	}
	header := []byte{first, byte(len(block.data) >> 16), byte(len(block.data) >> 8), byte(len(block.data))}
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(block.data)
	return err
}

// encodeVorbisComment builds a FLAC VORBIS_COMMENT block. Empty values are
// omitted, matching the tagger's "clear all frames" behaviour without writing
// fields the user never set.
func encodeVorbisComment(fields domain.MetadataUpdate) []byte {
	vendor := "Airmedy"
	var comments []string
	add := func(key, value string) {
		if strings.TrimSpace(value) != "" {
			comments = append(comments, key+"="+value)
		}
	}
	add("TITLE", fields.Title)
	add("ARTIST", fields.Artist)
	add("ALBUMARTIST", fields.AlbumArtist)
	add("ALBUM", fields.AlbumTitle)
	if fields.Year > 0 {
		add("DATE", strconv.Itoa(fields.Year))
	}
	if fields.TrackNumber > 0 {
		if fields.TotalTracks > 0 {
			add("TRACKNUMBER", fmt.Sprintf("%d/%d", fields.TrackNumber, fields.TotalTracks))
		} else {
			add("TRACKNUMBER", strconv.Itoa(fields.TrackNumber))
		}
	}
	if fields.TotalTracks > 0 {
		add("TRACKTOTAL", strconv.Itoa(fields.TotalTracks))
	}
	if fields.DiscNumber > 0 {
		if fields.TotalDiscs > 0 {
			add("DISCNUMBER", fmt.Sprintf("%d/%d", fields.DiscNumber, fields.TotalDiscs))
		} else {
			add("DISCNUMBER", strconv.Itoa(fields.DiscNumber))
		}
	}
	if fields.TotalDiscs > 0 {
		add("DISCTOTAL", strconv.Itoa(fields.TotalDiscs))
	}
	add("GENRE", fields.Genre)
	add("COMPOSER", fields.Composer)
	if fields.BPM > 0 {
		add("BPM", strconv.Itoa(fields.BPM))
	}
	add("LABEL", fields.Label)
	add("ISRC", fields.ISRC)
	add("LYRICS", fields.Lyrics)

	var buf bytes.Buffer
	putLE32 := func(n int) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(n))
		buf.Write(b[:])
	}
	putLE32(len(vendor))
	buf.WriteString(vendor)
	putLE32(len(comments))
	for _, comment := range comments {
		putLE32(len(comment))
		buf.WriteString(comment)
	}
	return buf.Bytes()
}

// encodeFLACPicture builds a METADATA_BLOCK_PICTURE with the front-cover type.
// Dimensions are read from the image header; unknown images still get a valid
// block with zero dimensions so readers can extract the bytes.
func encodeFLACPicture(data []byte, mime string) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(mime) == "" {
		mime = sniffImageMIME(data)
	}
	if strings.TrimSpace(mime) == "" {
		mime = "image/jpeg"
	}

	width, height, depth := 0, 0, 24
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		width, height = cfg.Width, cfg.Height
	}

	var buf bytes.Buffer
	putBE32 := func(n int) {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(n))
		buf.Write(b[:])
	}
	putBE32(flacPictureFrontCover)
	putBE32(len(mime))
	buf.WriteString(mime)
	putBE32(0) // description
	putBE32(width)
	putBE32(height)
	putBE32(depth)
	putBE32(0) // colors used
	putBE32(len(data))
	buf.Write(data)
	return buf.Bytes(), nil
}

// verifyFLACCopy re-parses the written file, checks that the audio payload has
// the same SHA-256 as the source and that the comment carries a title and artist.
func verifyFLACCopy(path string, wantAudioHash []byte) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	blocks, audioOffset, err := readFLACMetadata(f, 0)
	if err != nil {
		return err
	}
	foundStreamInfo := false
	foundTags := false
	for _, block := range blocks {
		switch block.blockType {
		case flacBlockStreamInfo:
			foundStreamInfo = true
		case flacBlockVorbisComment:
			tags, err := parseVorbisComment(block.data)
			if err != nil {
				return err
			}
			foundTags = tags["TITLE"] != "" && tags["ARTIST"] != ""
		}
	}
	if !foundStreamInfo {
		return fmt.Errorf("written file has no STREAMINFO block")
	}
	if !foundTags {
		return fmt.Errorf("written file does not expose TITLE/ARTIST to a standard reader")
	}

	actual := sha256.New()
	if _, err := f.Seek(audioOffset, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(actual, f); err != nil {
		return err
	}
	if !bytes.Equal(actual.Sum(nil), wantAudioHash) {
		return fmt.Errorf("audio payload changed while tagging")
	}
	return nil
}

// sniffImageMIME detects the common cover formats from magic bytes.
func sniffImageMIME(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF8")):
		return "image/gif"
	case bytes.HasPrefix(data, []byte("RIFF")) && len(data) >= 14 && bytes.Equal(data[8:14], []byte("WEBPVP")):
		return "image/webp"
	default:
		return ""
	}
}

// parseVorbisComment decodes a VORBIS_COMMENT block into uppercase keys.
func parseVorbisComment(data []byte) (map[string]string, error) {
	reader := bytes.NewReader(data)
	readLen := func() (int, error) {
		var b [4]byte
		if _, err := io.ReadFull(reader, b[:]); err != nil {
			return 0, err
		}
		return int(binary.LittleEndian.Uint32(b[:])), nil
	}
	vendorLen, err := readLen()
	if err != nil || vendorLen < 0 || vendorLen > reader.Len() {
		return nil, fmt.Errorf("invalid vorbis comment vendor length")
	}
	if _, err := io.CopyN(io.Discard, reader, int64(vendorLen)); err != nil {
		return nil, err
	}
	count, err := readLen()
	if err != nil {
		return nil, err
	}
	tags := make(map[string]string, count)
	for i := 0; i < count; i++ {
		length, err := readLen()
		if err != nil {
			return nil, err
		}
		entry := make([]byte, length)
		if _, err := io.ReadFull(reader, entry); err != nil {
			return nil, err
		}
		key, value, ok := strings.Cut(string(entry), "=")
		if ok {
			tags[strings.ToUpper(key)] = value
		}
	}
	return tags, nil
}
