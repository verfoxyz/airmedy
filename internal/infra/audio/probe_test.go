//go:build ffmpeg_integration

// Integration tests for the in-process ffmpeg duration probe. They share the
// ffmpeg_integration tag (and the static ffmpeg_libs) with the analyzer tests.
//
// Run with: go test -tags ffmpeg_integration ./internal/infra/audio/...
package audio

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDurationProbe_ReadsTheContainerHeader(t *testing.T) {
	path := writeSineWAV(t, 440, 3.0, 44100)

	got, err := NewDurationProbe().Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if math.Abs(got.Seconds()-3.0) > 0.1 {
		t.Fatalf("duration = %.3fs, want about 3s", got.Seconds())
	}
}

func TestDurationProbe_ReadsARawFLACHeader(t *testing.T) {
	path := writeFLACWithDuration(t, 3.0, 44100, 2)

	got, err := NewDurationProbe().Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if math.Abs(got.Seconds()-3.0) > 0.1 {
		t.Fatalf("duration = %.3fs, want about 3s", got.Seconds())
	}
}

// writeFLACWithDuration writes a minimal FLAC: marker + STREAMINFO with the
// requested sample rate/channels/total samples, followed by one fake frame. The
// probe only reads the header, so the frame bytes need not decode.
func writeFLACWithDuration(t *testing.T, seconds float64, sampleRate, channels int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "raw.flac")

	streamInfo := make([]byte, 34)
	streamInfo[0], streamInfo[1] = 0x10, 0x00 // min block size 4096
	streamInfo[2], streamInfo[3] = 0x10, 0x00 // max block size 4096
	// bytes 4..9 (min/max frame size) stay zero = unknown.
	totalSamples := uint64(seconds * float64(sampleRate))
	packed := uint64(sampleRate)<<44 |
		uint64(channels-1)<<41 |
		uint64(16-1)<<36 |
		totalSamples
	for i := 0; i < 8; i++ {
		streamInfo[10+i] = byte(packed >> (56 - 8*i))
	}

	data := []byte{'f', 'L', 'a', 'C', 0x80, 0x00, 0x00, 0x22}
	data = append(data, streamInfo...)
	data = append(data, 0xff, 0xf8) // frame sync, deliberately incomplete
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDurationProbe_RejectsANonAudioFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("not audio"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := NewDurationProbe().Probe(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "could not open the file") {
		t.Fatalf("Probe = %v, want a could-not-open error", err)
	}
}

func TestDurationProbe_RejectsAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.flac")

	_, err := NewDurationProbe().Probe(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "could not open the file") {
		t.Fatalf("Probe = %v, want a could-not-open error", err)
	}
}

func TestDurationProbe_HonoursAnAlreadyCancelledContext(t *testing.T) {
	path := writeSineWAV(t, 440, 1.0, 44100)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewDurationProbe().Probe(ctx, path)
	if err != context.Canceled {
		t.Fatalf("Probe = %v, want context.Canceled", err)
	}
}
