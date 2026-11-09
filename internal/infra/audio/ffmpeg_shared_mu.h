/*
 * ffmpeg_shared_mu.h — state shared across every cgo translation unit that
 * opens/probes streams through the vendored ffmpeg build.
 *
 * ffmpeg_analyzer.h and ffmpeg_decoder.h are each pulled into a *different*
 * cgo preamble (analyzer.go vs player_miniaudio.go), so a `static` mutex
 * declared inside either header would give each translation unit its own
 * independent lock — useless for cross-file serialization. This header
 * declares one mutex with external linkage, defined once in
 * ffmpeg_shared_mu.c, so the analysis worker pool and the playback decoder
 * actually contend on the same lock during avformat_open_input .. avcodec_open2
 * (the phase that can trigger a decoder's lazy static-table init and corrupt
 * the heap under concurrent first-touch).
 */
#ifndef FFMPEG_SHARED_MU_H
#define FFMPEG_SHARED_MU_H

#include <pthread.h>

extern pthread_mutex_t ffmpeg_probe_mu;

/*
 * ffmpeg_network_init performs avformat_network_init exactly once per process.
 * It is guarded by a pthread_once in ffmpeg_shared_mu.c rather than declared
 * here, because every header uses it and each translation unit would otherwise
 * carry its own guard; the protocol registry itself is process-global.
 *
 * Call it before the first avformat_open_input that may receive an http(s) URL.
 * Local files do not need it, but it is cheap and idempotent.
 */
void ffmpeg_network_init(void);

/*
 * ffmpeg_io_guard is the opaque payload of AVIOInterruptCB. Callers own the
 * struct and keep it alive for as long as the AVFormatContext exists; the
 * callback only reads the cancel flag, so setting it from another thread is
 * safe. cancel may be NULL, which makes the callback a no-op.
 *
 * The timeout is deliberately not handled here: rw_timeout on the format
 * options bounds the blocking open/read at the protocol layer (15 s in the
 * probe and decoder), so the interrupt callback exists solely to abort an
 * in-flight network read immediately when playback stops or the track changes.
 */
typedef struct {
    volatile int *cancel;
} ffmpeg_io_guard;

static inline int ffmpeg_io_interrupt(void *opaque) {
    ffmpeg_io_guard *guard = (ffmpeg_io_guard *)opaque;
    return guard && guard->cancel && *guard->cancel;
}

#endif /* FFMPEG_SHARED_MU_H */
