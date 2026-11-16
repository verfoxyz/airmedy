//go:build ffmpeg_cache_io

// Unit tests for the cache-file AVIO layer. Run with:
//
//	go test -tags ffmpeg_cache_io ./internal/infra/audio/
//
// cgo is not allowed in _test.go files, so the C entry points are wrapped by
// ffmpeg_cache_io_bridge.go, which carries the same tag.
package audio

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestCacheIO(t *testing.T, path string, expected, waitMS int64) *CacheIOTestHandle {
	t.Helper()
	h := CacheIOTestOpen(path, expected)
	if h == nil {
		t.Fatal("CacheIOTestOpen returned nil")
	}
	h.SetWaitMS(int(waitMS))
	t.Cleanup(h.Close)
	return h
}

func writeTestFile(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.WriteFile(path, bytes.Repeat([]byte{0x41}, n), 0o644); err != nil {
		t.Fatalf("write %d bytes: %v", n, err)
	}
}

func appendTestFile(t *testing.T, path string, n int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.Write(bytes.Repeat([]byte{0x42}, n)); err != nil {
		f.Close()
		t.Fatalf("append %d bytes: %v", n, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close append handle: %v", err)
	}
}

// TestCacheAvioSizeHidesTheUndownloadedTail is the regression for the seek
// bug: the AVIO layer used to report the announced total, so FFmpeg could seek
// past the bytes on disk and then read EOF there.
func TestCacheAvioSizeHidesTheUndownloadedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "growing.audio")
	writeTestFile(t, path, 1000)
	h := openTestCacheIO(t, path, 5000, 500)

	if got := h.Size(); got != 1000 {
		t.Fatalf("AVSEEK_SIZE = %d while 1000 of 5000 bytes are on disk", got)
	}

	appendTestFile(t, path, 4000)
	if got := h.Size(); got != 5000 {
		t.Fatalf("AVSEEK_SIZE = %d after the file reached its announced total", got)
	}
}

func TestCacheAvioSizeReportsTheTotalOnceComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "complete.audio")
	writeTestFile(t, path, 1000)
	h := openTestCacheIO(t, path, 0, 500)
	h.MarkComplete()

	if got := h.Size(); got != 1000 {
		t.Fatalf("AVSEEK_SIZE = %d for a completed 1000-byte download", got)
	}
}

func TestCacheAvioSeekWaitsForTheDownloadToReachTheTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seek-wait.audio")
	writeTestFile(t, path, 1000)
	h := openTestCacheIO(t, path, 5000, 2000)

	done := make(chan int64, 1)
	go func() { done <- h.SeekTo(2000) }()

	time.Sleep(30 * time.Millisecond)
	appendTestFile(t, path, 1200)

	select {
	case got := <-done:
		if got != 2000 {
			t.Fatalf("seek = %d, want 2000 once the download covered the target", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("seek did not return after the file grew past the target")
	}
	if waits := h.Waits(); waits < 1 {
		t.Fatalf("waits = %d, want the seek to be counted as a wait", waits)
	}
	if waited := h.WaitMS(); waited <= 0 {
		t.Fatalf("wait_ms_total = %d, want accumulated wait time", waited)
	}
}

func TestCacheAvioSeekPastTheAnnouncedTotalFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overrun.audio")
	writeTestFile(t, path, 1000)
	h := openTestCacheIO(t, path, 5000, 500)

	if got := h.SeekTo(6000); got >= 0 {
		t.Fatalf("seek past expected_size = %d, want an error", got)
	}
}

func TestCacheAvioReadBlocksWhileTheFileGrows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "read-wait.audio")
	writeTestFile(t, path, 1000)
	h := openTestCacheIO(t, path, 5000, 2000)

	if pos := h.SeekTo(1000); pos != 1000 {
		t.Fatalf("seek to the current edge = %d", pos)
	}

	buf := make([]byte, 16)
	var read int
	done := make(chan struct{})
	go func() {
		read = h.Read(buf)
		close(done)
	}()

	time.Sleep(30 * time.Millisecond)
	appendTestFile(t, path, 16)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("read did not resume after the file grew")
	}
	if read != len(buf) {
		t.Fatalf("read = %d, want %d: a growing file must not report EOF", read, len(buf))
	}
	if !bytes.Equal(buf, bytes.Repeat([]byte{0x42}, 16)) {
		t.Fatalf("read returned %q, want the appended bytes", buf)
	}
}

func TestCacheAvioReadReportsEOFAtTheRealEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "eof.audio")
	writeTestFile(t, path, 1000)
	h := openTestCacheIO(t, path, 5000, 500)
	appendTestFile(t, path, 4000)

	if pos := h.SeekTo(5000); pos != 5000 {
		t.Fatalf("seek to the announced total = %d", pos)
	}
	buf := make([]byte, 16)
	if got := h.Read(buf); got >= 0 {
		t.Fatalf("read = %d, want AVERROR_EOF at the completed end", got)
	}
}

func TestCacheAvioReadReportsEOFAfterAnUnknownTotalCompletes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unknown-eof.audio")
	writeTestFile(t, path, 1000)
	h := openTestCacheIO(t, path, 0, 500)
	h.MarkComplete()

	if pos := h.SeekTo(1000); pos != 1000 {
		t.Fatalf("seek to the file edge = %d", pos)
	}
	if got := h.Read(make([]byte, 16)); got >= 0 {
		t.Fatalf("read = %d, want AVERROR_EOF once an unknown total is complete", got)
	}
}

func TestCacheAvioReadStopsAtTheAnnouncedTotal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "longer-file.audio")
	writeTestFile(t, path, 6000) // a stale cache file longer than the new total
	h := openTestCacheIO(t, path, 5000, 500)

	if pos := h.SeekTo(5000); pos != 5000 {
		t.Fatalf("seek to the announced total = %d", pos)
	}
	if got := h.Read(make([]byte, 16)); got >= 0 {
		t.Fatalf("read = %d, want AVERROR_EOF at the announced total", got)
	}
}

func TestCacheAvioSeekOnACompleteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "complete-seek.audio")
	writeTestFile(t, path, 5000)
	h := openTestCacheIO(t, path, 5000, 500)

	if pos := h.SeekTo(4000); pos != 4000 {
		t.Fatalf("seek on a complete file = %d, want 4000", pos)
	}
	if got := h.SeekTo(5001); got >= 0 {
		t.Fatalf("seek past the end = %d, want an error", got)
	}
}

// writeTestWAV writes seconds of 44.1 kHz stereo 16-bit PCM with a ramp, so a
// decode returns audible (non-silent) samples.
func writeTestWAV(t *testing.T, seconds int) string {
	t.Helper()
	const rate = 44100
	const channels = 2
	dataBytes := seconds * rate * channels * 2
	buf := make([]byte, 44+dataBytes)
	copy(buf[0:], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+dataBytes))
	copy(buf[8:], "WAVE")
	copy(buf[12:], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:], 16)
	binary.LittleEndian.PutUint16(buf[20:], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:], channels)
	binary.LittleEndian.PutUint32(buf[24:], rate)
	binary.LittleEndian.PutUint32(buf[28:], rate*channels*2)
	binary.LittleEndian.PutUint16(buf[32:], channels*2)
	binary.LittleEndian.PutUint16(buf[34:], 16)
	copy(buf[36:], "data")
	binary.LittleEndian.PutUint32(buf[40:], uint32(dataBytes))
	for i := 44; i+1 < len(buf); i += 2 {
		binary.LittleEndian.PutUint16(buf[i:], uint16((i%4000)*16))
	}
	path := filepath.Join(t.TempDir(), "tone.wav")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("write wav: %v", err)
	}
	return path
}

// TestIncompleteCacheSeekUsesTheTimestampSearch: the player buffers the target
// bytes before reopening the decoder, so the seek itself is the demuxer's
// timestamp search even while the file is still growing. There is no
// byte-offset path any more (seek-metadata-duration.md).
func TestIncompleteCacheSeekUsesTheTimestampSearch(t *testing.T) {
	path := writeTestWAV(t, 10)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat wav: %v", err)
	}
	full := info.Size()

	// Announce a total slightly larger than the file so the decoder sees an
	// incomplete download (the growing-cache case).
	h := CacheDecoderTestOpen(path, full+100)
	if h == nil {
		t.Fatal("CacheDecoderTestOpen returned nil")
	}
	defer h.Close()

	if rc := h.Seek(3.9); rc != 0 {
		t.Fatalf("seek = %d, want 0", rc)
	}
	if !h.SeekOK() {
		t.Fatal("timestamp seek failed on an incomplete cache file")
	}
	if got := h.SeekReceived(); got != full {
		t.Fatalf("seek saw received = %d, want the %d bytes on disk", got, full)
	}
	if got := h.SeekTotal(); got != full+100 {
		t.Fatalf("seek saw total = %d, want the announced %d", got, full+100)
	}
	if pos := h.FilePos(); pos <= 0 || pos > full {
		t.Fatalf("AVIO position = %d, want inside the %d-byte file", pos, full)
	}

	buf := make([]float32, 1024)
	if n := h.Read(buf); n <= 0 {
		t.Fatalf("read after seek = %d, want decoded frames", n)
	}
	if frames := h.Frames(); frames <= 0 {
		t.Fatalf("decoder frames = %d after the seek", frames)
	}
	if cursor := h.Cursor(); math.Abs(cursor-3.9) > 0.5 {
		t.Fatalf("cursor = %.3fs, want about 3.9s", cursor)
	}
}

// TestCompleteCacheSeekUsesTheTimestampSearch: the same path with every byte on
// disk.
func TestCompleteCacheSeekUsesTheTimestampSearch(t *testing.T) {
	path := writeTestWAV(t, 10)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat wav: %v", err)
	}

	h := CacheDecoderTestOpen(path, info.Size())
	if h == nil {
		t.Fatal("CacheDecoderTestOpen returned nil")
	}
	defer h.Close()

	if rc := h.Seek(5); rc != 0 {
		t.Fatalf("seek = %d, want 0", rc)
	}
	if !h.SeekOK() {
		t.Fatal("timestamp seek on a complete file failed")
	}
	buf := make([]float32, 1024)
	if n := h.Read(buf); n <= 0 {
		t.Fatalf("read after seek = %d, want decoded frames", n)
	}
	if cursor := h.Cursor(); math.Abs(cursor-5) > 0.5 {
		t.Fatalf("cursor = %.3fs, want about 5s", cursor)
	}
}
