//go:build ffmpeg_cache_io

// Bridge used only by ffmpeg_cache_io_test.go: Go does not allow cgo inside
// _test.go files, so the static AVIO helpers are wrapped here and the test file
// carries the same build tag.
//
// Run with: go test -tags ffmpeg_cache_io ./internal/infra/audio/
package audio

/*
#include <stdint.h>
#include <stdlib.h>
#include "ffmpeg_cache_io.h"
#include "ffmpeg_decoder.h"

static int64_t test_avio_size(FFmpegCacheIO* io) {
    return ffmpeg_cache_seek(io, 0, AVSEEK_SIZE);
}
static int64_t test_avio_seek(FFmpegCacheIO* io, int64_t off) {
    return ffmpeg_cache_seek(io, off, SEEK_SET);
}
static int test_avio_read(FFmpegCacheIO* io, void* buf, int n) {
    return ffmpeg_cache_read(io, (uint8_t*)buf, n);
}
static void test_avio_set_wait_ms(FFmpegCacheIO* io, int ms) {
    io->wait_ms = ms;
}
static void test_avio_mark_complete(FFmpegCacheIO* io) {
    ffmpeg_cache_mark_complete(io);
}
static long long test_avio_waits(FFmpegCacheIO* io) {
    return __atomic_load_n(&io->waits, __ATOMIC_RELAXED);
}
static long long test_avio_wait_ms(FFmpegCacheIO* io) {
    return __atomic_load_n(&io->wait_ms_total, __ATOMIC_RELAXED);
}

static FFmpegHandle* test_handle_open(const char* path, int64_t expected) {
    return ffmpeg_open_cache(path, expected, 44100);
}
static void test_handle_close(FFmpegHandle* h) {
    ffmpeg_close(h);
}
static int test_handle_seek(FFmpegHandle* h, double seconds) {
    return ffmpeg_seek(h, seconds);
}
static int test_handle_read(FFmpegHandle* h, void* buf, int frames) {
    return ffmpeg_read(h, (float*)buf, frames);
}
static double test_handle_cursor(FFmpegHandle* h) {
    return h->read_count / (double)h->target_rate;
}
static int64_t test_handle_file_pos(FFmpegHandle* h) {
    return h->cache_io ? h->cache_io->pos : -1;
}
static long long test_handle_waits(FFmpegHandle* h) {
    return h->cache_io ? __atomic_load_n(&h->cache_io->waits, __ATOMIC_RELAXED) : -1;
}
static long long test_handle_frames(FFmpegHandle* h) {
    return __atomic_load_n(&h->diag.frames, __ATOMIC_RELAXED);
}
static long long test_handle_seek_received(FFmpegHandle* h) {
    return __atomic_load_n(&h->diag.seek_received, __ATOMIC_RELAXED);
}
static long long test_handle_seek_total(FFmpegHandle* h) {
    return __atomic_load_n(&h->diag.seek_total, __ATOMIC_RELAXED);
}
static int test_handle_seek_ok(FFmpegHandle* h) {
    return __atomic_load_n(&h->diag.seek_ok, __ATOMIC_RELAXED);
}
*/
import "C"

import "unsafe"

// CacheIOTestHandle owns one AVIO view of a cache file for the tests below.
type CacheIOTestHandle struct {
	p *C.FFmpegCacheIO
}

// CacheIOTestOpen opens path as an AVIO cache reader with an announced total of
// expected bytes (0 = unknown length).
func CacheIOTestOpen(path string, expected int64) *CacheIOTestHandle {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	io := C.ffmpeg_cache_io_open(cpath, C.int64_t(expected))
	if io == nil {
		return nil
	}
	return &CacheIOTestHandle{p: io}
}

// Close releases the AVIO reader.
func (h *CacheIOTestHandle) Close() { C.ffmpeg_cache_io_close(h.p) }

// SetWaitMS shortens the per-call wait budget so a bug fails fast.
func (h *CacheIOTestHandle) SetWaitMS(ms int) { C.test_avio_set_wait_ms(h.p, C.int(ms)) }

// MarkComplete simulates the download-finished signal for an unknown total.
func (h *CacheIOTestHandle) MarkComplete() { C.test_avio_mark_complete(h.p) }

// Size is what AVSEEK_SIZE reports.
func (h *CacheIOTestHandle) Size() int64 { return int64(C.test_avio_size(h.p)) }

// SeekTo moves to an absolute offset; negative results are AVERROR codes.
func (h *CacheIOTestHandle) SeekTo(offset int64) int64 {
	return int64(C.test_avio_seek(h.p, C.int64_t(offset)))
}

// Read fills buf, returning the byte count or an AVERROR code.
func (h *CacheIOTestHandle) Read(buf []byte) int {
	if len(buf) == 0 {
		return 0
	}
	return int(C.test_avio_read(h.p, unsafe.Pointer(&buf[0]), C.int(len(buf))))
}

// Waits is how many callers had to wait for the download.
func (h *CacheIOTestHandle) Waits() int64 { return int64(C.test_avio_waits(h.p)) }

// WaitMS is the accumulated wait time in milliseconds.
func (h *CacheIOTestHandle) WaitMS() int64 { return int64(C.test_avio_wait_ms(h.p)) }

// CacheDecoderTestHandle drives a cache-mode decoder the way the player does:
// open the growing file, seek by seconds, read PCM.
type CacheDecoderTestHandle struct {
	h *C.FFmpegHandle
}

// CacheDecoderTestOpen opens path in cache mode with an announced total.
func CacheDecoderTestOpen(path string, expected int64) *CacheDecoderTestHandle {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	h := C.test_handle_open(cpath, C.int64_t(expected))
	if h == nil {
		return nil
	}
	return &CacheDecoderTestHandle{h: h}
}

// Close releases the decoder.
func (h *CacheDecoderTestHandle) Close() { C.test_handle_close(h.h) }

// Seek moves to seconds; a negative result is a failure code.
func (h *CacheDecoderTestHandle) Seek(seconds float64) int {
	return int(C.test_handle_seek(h.h, C.double(seconds)))
}

// Read fills buf with decoded float32 frames, returning the frame count or a
// negative error code.
func (h *CacheDecoderTestHandle) Read(buf []float32) int {
	if len(buf) == 0 {
		return 0
	}
	return int(C.test_handle_read(h.h, unsafe.Pointer(&buf[0]), C.int(len(buf))))
}

// Cursor is the decoder's reported play position in seconds.
func (h *CacheDecoderTestHandle) Cursor() float64 { return float64(C.test_handle_cursor(h.h)) }

// FilePos is the AVIO read position in bytes.
func (h *CacheDecoderTestHandle) FilePos() int64 { return int64(C.test_handle_file_pos(h.h)) }

// Waits is how many AVIO reads or seeks waited for the download.
func (h *CacheDecoderTestHandle) Waits() int64 { return int64(C.test_handle_waits(h.h)) }

// Frames is how many PCM frames were decoded since the seek.
func (h *CacheDecoderTestHandle) Frames() int64 { return int64(C.test_handle_frames(h.h)) }

// SeekReceived is the bytes-on-disk value the last seek saw.
func (h *CacheDecoderTestHandle) SeekReceived() int64 {
	return int64(C.test_handle_seek_received(h.h))
}

// SeekTotal is the announced total the last seek saw (0 unknown).
func (h *CacheDecoderTestHandle) SeekTotal() int64 {
	return int64(C.test_handle_seek_total(h.h))
}

// SeekOK reports whether the last seek succeeded.
func (h *CacheDecoderTestHandle) SeekOK() bool { return C.test_handle_seek_ok(h.h) != 0 }
