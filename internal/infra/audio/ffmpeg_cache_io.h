/*
 * ffmpeg_cache_io.h — AVIO callbacks over a growing local cache file.
 *
 * The stream cache downloads a remote track into <hash>.audio while playback
 * reads the same file. FFmpeg must never see the network: a custom AVIOContext
 * reads the file and blocks briefly when the requested bytes have not arrived
 * yet. This removes the HTTP proxy from the decode path and the large probe /
 * rw_timeout failures that came with it.
 */
#ifndef FFMPEG_CACHE_IO_H
#define FFMPEG_CACHE_IO_H

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include <libavformat/avio.h>
#include <libavutil/error.h>

#ifdef _WIN32
#include <windows.h>
#include <sys/stat.h>
#define FFMPEG_FSEEK64(fp, off, origin) _fseeki64((fp), (off), (origin))
#define FFMPEG_FTELL64(fp) _ftelli64(fp)
static void ffmpeg_cache_sleep_ms(int ms) { Sleep((DWORD)ms); }
static int64_t ffmpeg_path_size(const char* path) {
    struct _stat64 st;
    if (!path || _stat64(path, &st) != 0) return 0;
    return (int64_t)st.st_size;
}
#else
#include <unistd.h>
#include <sys/stat.h>
#define FFMPEG_FSEEK64(fp, off, origin) fseeko((fp), (off), (origin))
#define FFMPEG_FTELL64(fp) ftello(fp)
static void ffmpeg_cache_sleep_ms(int ms) { usleep((useconds_t)ms * 1000); }
static int64_t ffmpeg_path_size(const char* path) {
    struct stat st;
    if (!path || stat(path, &st) != 0) return 0;
    return (int64_t)st.st_size;
}
#endif

/* How long a read waits without the file growing before it reports an error.
 * A failed download therefore surfaces as a playback failure, not a hang. */
#define FFMPEG_CACHE_WAIT_MS 30000
#define FFMPEG_CACHE_POLL_MS 5

/* Best-effort monotonic milliseconds for diagnostics only. */
static int64_t ffmpeg_now_ms(void) {
#ifdef _WIN32
    return (int64_t)GetTickCount64();
#else
    struct timespec ts;
    if (clock_gettime(CLOCK_MONOTONIC, &ts) != 0) return 0;
    return (int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
#endif
}

typedef struct {
    FILE*    fp;
    /* Path kept for out-of-band file-size queries: diagnostics must not touch
     * fp, whose seek position belongs to the decoding thread. */
    char     path[1024];
    int64_t  pos;
    /* expected_size is the final byte count when the upstream announced one;
     * 0 means unknown, and EOF comes only from the completion flag below. */
    int64_t  expected_size;
    /* Set by the player when a download of unknown length finishes. Without it
     * an unknown total can never distinguish EOF from "not downloaded yet". */
    volatile int complete;
    /* Points at FFmpegHandle.cancel so stop/unload aborts a blocked read. */
    volatile int* cancel;
    int      wait_ms;
    /* Diagnostics: how often this stream had to wait and for how long. */
    volatile long long waits;
    volatile long long wait_ms_total;
} FFmpegCacheIO;

static int64_t ffmpeg_cache_file_size(FFmpegCacheIO* io) {
    if (!io || !io->fp) return 0;
    if (FFMPEG_FSEEK64(io->fp, 0, SEEK_END) != 0) return 0;
    int64_t size = FFMPEG_FTELL64(io->fp);
    return size > 0 ? size : 0;
}

/* ffmpeg_cache_read always tries to fill the whole buffer. A short read makes
 * libavformat set eof_reached, which is exactly the "playback stops inside the
 * downloaded range" bug: the file simply had fewer bytes than the caller asked
 * for. It returns a short count only at the true end of the file (announced
 * size reached, or the download completed with an unknown total). */
static int ffmpeg_cache_read(void* opaque, uint8_t* buf, int buf_size) {
    FFmpegCacheIO* io = (FFmpegCacheIO*)opaque;
    if (!io || !io->fp || !buf || buf_size <= 0) return AVERROR(EIO);

    int total = 0;
    int waited = 0;
    int waiter_counted = 0;
    for (;;) {
        if (io->cancel && *io->cancel) return AVERROR_EXIT;

        int64_t size = ffmpeg_cache_file_size(io);
        int64_t limit = size;
        int at_eof = 0;
        if (io->expected_size > 0 && limit > io->expected_size) limit = io->expected_size;
        if (io->pos >= limit) {
            /* Only the real end is EOF: the download declared completion, or the
             * announced total is already on disk. A growing file is not EOF, no
             * matter what expected_size says about the eventual length. */
            if (io->complete || (io->expected_size > 0 && size >= io->expected_size)) at_eof = 1;
        }

        if (io->pos < limit) {
            int64_t want = limit - io->pos;
            if (want > buf_size - total) want = buf_size - total;
            if (want > 0) {
                if (FFMPEG_FSEEK64(io->fp, io->pos, SEEK_SET) != 0) return AVERROR(EIO);
                size_t got = fread(buf + total, 1, (size_t)want, io->fp);
                if (got == 0) {
                    /* The file shrank under us (cache cleanup): failure. */
                    return AVERROR(EIO);
                }
                total += (int)got;
                io->pos += (int64_t)got;
                if (total >= buf_size) return total;
                /* Fewer bytes than the buffer means the download edge, not
                 * EOF: loop and wait for the rest. */
                continue;
            }
        }

        if (at_eof) return total > 0 ? total : AVERROR_EOF;
        if (waited >= io->wait_ms) return AVERROR(EIO);
        if (!waiter_counted) {
            waiter_counted = 1;
            __atomic_add_fetch(&io->waits, 1, __ATOMIC_RELAXED);
        }
        ffmpeg_cache_sleep_ms(FFMPEG_CACHE_POLL_MS);
        waited += FFMPEG_CACHE_POLL_MS;
        __atomic_add_fetch(&io->wait_ms_total, FFMPEG_CACHE_POLL_MS, __ATOMIC_RELAXED);
    }
}

/* ffmpeg_cache_mark_complete is called when a download with no announced
 * length finishes; the next read at the file edge becomes EOF. */
static void ffmpeg_cache_mark_complete(FFmpegCacheIO* io) {
    if (io) io->complete = 1;
}

/* ffmpeg_cache_reported_size is the size the AVIO layer may expose. Until the
 * download finishes, that is only what is already on disk: advertising the
 * announced total lets FFmpeg seek into bytes that do not exist yet, where the
 * read used to report EOF. */
static int64_t ffmpeg_cache_reported_size(FFmpegCacheIO* io, int64_t size) {
    if (io->complete) return io->expected_size > 0 ? io->expected_size : size;
    if (io->expected_size > 0 && size >= io->expected_size) return io->expected_size;
    return size;
}

/* ffmpeg_cache_seek moves the read cursor. A target inside the announced total
 * but past the current write edge is not an error: it waits (5 ms polls,
 * cancellable, bounded by wait_ms) until the download grows to cover it. A
 * target past bytes that will never exist fails. Waiting here keeps the
 * demuxer's binary search from being handed a bogus EOF. */
static int64_t ffmpeg_cache_seek(void* opaque, int64_t offset, int whence) {
    FFmpegCacheIO* io = (FFmpegCacheIO*)opaque;
    if (!io || !io->fp) return AVERROR(EIO);

    int64_t size = ffmpeg_cache_file_size(io);
    int64_t reported = ffmpeg_cache_reported_size(io, size);

    if (whence & AVSEEK_SIZE) return reported;

    int64_t base = 0;
    switch (whence & ~AVSEEK_FORCE) {
        case SEEK_SET: base = 0; break;
        case SEEK_CUR: base = io->pos; break;
        case SEEK_END: base = reported; break;
        default: return AVERROR(EINVAL);
    }
    int64_t next = base + offset;
    if (next < 0) return AVERROR(EINVAL);

    /* Publish the target before waiting: the player reads this position to
     * decide whether the decoder demands bytes, and a parked seek behind the
     * old read position would otherwise never lift the prefetch water mark. */
    io->pos = next;

    if (next > size) {
        if (io->complete) return AVERROR(EIO);
        if (io->expected_size > 0 && next > io->expected_size) return AVERROR(EIO);

        int waited = 0;
        __atomic_add_fetch(&io->waits, 1, __ATOMIC_RELAXED);
        for (;;) {
            if (io->cancel && *io->cancel) return AVERROR_EXIT;
            if (waited >= io->wait_ms) return AVERROR(EIO);
            ffmpeg_cache_sleep_ms(FFMPEG_CACHE_POLL_MS);
            waited += FFMPEG_CACHE_POLL_MS;
            __atomic_add_fetch(&io->wait_ms_total, FFMPEG_CACHE_POLL_MS, __ATOMIC_RELAXED);
            size = ffmpeg_cache_file_size(io);
            if (size >= next) break;
            if (io->complete) return AVERROR(EIO);
        }
    }
    return next;
}

static FFmpegCacheIO* ffmpeg_cache_io_open(const char* path, int64_t expected_size) {
    FFmpegCacheIO* io = (FFmpegCacheIO*)calloc(1, sizeof(FFmpegCacheIO));
    if (!io) return NULL;
    io->fp = fopen(path, "rb");
    if (!io->fp) {
        free(io);
        return NULL;
    }
    io->expected_size = expected_size > 0 ? expected_size : 0;
    io->wait_ms = FFMPEG_CACHE_WAIT_MS;
    io->pos = 0;
    snprintf(io->path, sizeof(io->path), "%s", path);
    return io;
}

static void ffmpeg_cache_io_close(FFmpegCacheIO* io) {
    if (!io) return;
    if (io->fp) fclose(io->fp);
    free(io);
}

#endif /* FFMPEG_CACHE_IO_H */
