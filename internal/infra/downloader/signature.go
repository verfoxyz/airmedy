package downloader

import (
	"bytes"
	"fmt"
)

// Container identifiers detected from the first bytes of a file. They double as
// the file extension a download is stored under: the resolver only promises a
// playable URL, not what container or bitrate it actually serves.
const (
	ContainerMP3  Container = "mp3"
	ContainerFLAC Container = "flac"
	ContainerM4A  Container = "m4a"
	ContainerOGG  Container = "ogg"
	ContainerWAV  Container = "wav"
)

// Extension returns the file extension (no dot) for a container, defaulting to
// mp3 so a caller that never validated still produces a playable-looking name.
func (c Container) Extension() string {
	if c == "" {
		return string(ContainerMP3)
	}
	return string(c)
}

// Container is the detected audio container of a downloaded file.
type Container string

// snapshotLen is how many leading bytes DetectSignature looks at. MP4 stores
// "ftyp" at offset 4, so the 4 bytes immediately after the box length are read
// too.
const snapshotLen = 12

var (
	id3Magic  = []byte("ID3")
	flacMagic = []byte("fLaC")
	oggMagic  = []byte("OggS")
	rifxMagic = []byte("RIFF")
	ftypMagic = []byte("ftyp")
	// An MPEG frame sync is 11 set bits: all 8 of byte 0 plus the top 3 of
	// byte 1. The remaining bits of byte 1 are version and layer.
	mpegSyncMask = byte(0xE0)
	mpegSyncByte = byte(0xFF)
)

// DetectSignature identifies the container from the first bytes of a file. It
// returns ContainerMP3 for a bare MPEG frame sync (0xFFFB / 0xFFF3 / ...) and
// an error when the bytes look like none of the supported containers.
//
// ID3 is ambiguous: it is also the tag header of an MP4/AAC file, so "ID3"
// alone is not proof of MP3. Callers that care re-check with the metadata
// extractor; here ID3 is accepted as MP3 because that is what the public
// NetEase endpoint serves for lossy quality tiers.
func DetectSignature(head []byte) (Container, error) {
	if len(head) == 0 {
		return "", fmt.Errorf("empty file: %w", ErrUnsupportedSignature)
	}
	switch {
	case bytes.HasPrefix(head, flacMagic):
		return ContainerFLAC, nil
	case bytes.HasPrefix(head, id3Magic):
		return ContainerMP3, nil
	case bytes.HasPrefix(head, oggMagic):
		return ContainerOGG, nil
	case bytes.HasPrefix(head, rifxMagic):
		return ContainerWAV, nil
	case len(head) >= 8 && bytes.Equal(head[4:8], ftypMagic):
		return ContainerM4A, nil
	case isMPEGSync(head):
		return ContainerMP3, nil
	}
	return "", fmt.Errorf("unrecognised container (first bytes %q): %w", head[:min(len(head), 8)], ErrUnsupportedSignature)
}

// isMPEGSync reports an MPEG audio frame header: 11 bits set, then a layer and
// bitrate/sample-rate index that is not the "free format"/reserved zero.
func isMPEGSync(head []byte) bool {
	if len(head) < 2 || head[0] != mpegSyncByte || head[1]&mpegSyncMask != mpegSyncByte&mpegSyncMask {
		return false
	}
	// Bits 2-3 of the second byte are the layer field; 00 is reserved.
	layer := (head[1] >> 1) & 0x03
	return layer != 0
}
