/*
 * ffmpeg_decoder.h — single-header FFmpeg decode-to-PCM helper.
 *
 * Supports streaming (section-by-section) decoding to interleaved float32 PCM.
 * Decodes any FFmpeg-supported format (including DSF/DSD) and resamples to target_rate.
 * Automatically downmixes multi-channel audio to stereo for compatibility.
 */
#ifndef FFMPEG_DECODER_H
#define FFMPEG_DECODER_H

#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/opt.h>
#include <libavutil/mem.h>
#include <libavutil/channel_layout.h>
#include <libswresample/swresample.h>

#include "ffmpeg_shared_mu.h"
#include "ffmpeg_cache_io.h"

typedef unsigned int           ma_uint32;
typedef unsigned long long     ma_uint64;

typedef struct {
    AVFormatContext* fmt_ctx;
    AVCodecContext*  codec_ctx;
    SwrContext*      swr;
    int              stream_idx;
    AVPacket*        pkt;
    AVFrame*         frame;
    ma_uint32        target_rate;
    ma_uint32        n_ch;        /* Output channels (max 2) */
    ma_uint64        total_frames;
    ma_uint64        read_count;  /* Total frames read so far */
    int              is_eof;
    int              flushed;     /* Sent NULL to avcodec_send_packet */
    
    /* Leftover buffer for handling partial frames from swr_convert */
    float*           leftover_buf;
    int              leftover_count;
    int              leftover_offset;
    int              leftover_cap;

    /* Reusable buffer for swr output */
    float*           swr_tmp_buf;
    int              swr_tmp_cap;

    /* Network interruption: ffmpeg_cancel() sets the flag from another thread
     * (stop/unload) and the AVIO interrupt callback aborts a blocked read. */
    volatile int     cancel;
    ffmpeg_io_guard  io;
    /* Diagnostics for the seek-silence investigation: per-seek read/frame
     * counters plus what the last seek saw. Written on the audio thread, read
     * from Go; atomics below. */
    struct {
        volatile long long reads;
        volatile long long frames;
        volatile long long empty_reads;
        volatile long long seek_at_ms;
        volatile long long seek_requested_ms;
        volatile long long seek_received; /* bytes on disk when seeking */
        volatile long long seek_total;    /* announced total, 0 unknown */
        volatile int       seek_ok;
    } diag;
    /* Cache-file mode: a custom AVIOContext reads the growing download file.
     * Both are NULL for a normal path/URL open. */
    AVIOContext*     avio;
    FFmpegCacheIO*   cache_io;
    int              read_error; /* AVERROR from av_read_frame, 0 otherwise */
} FFmpegHandle;

static void ffmpeg_close(FFmpegHandle* h) {
    if (!h) return;
    if (h->frame) av_frame_free(&h->frame);
    if (h->pkt)   av_packet_free(&h->pkt);
    if (h->swr)   swr_free(&h->swr);
    if (h->codec_ctx) avcodec_free_context(&h->codec_ctx);
    if (h->fmt_ctx)   avformat_close_input(&h->fmt_ctx);
    if (h->avio) {
        av_freep(&h->avio->buffer);
        avio_context_free(&h->avio);
    }
    ffmpeg_cache_io_close(h->cache_io);
    if (h->leftover_buf) free(h->leftover_buf);
    if (h->swr_tmp_buf)  free(h->swr_tmp_buf);
    free(h);
}

/* Aborts an in-flight network read. Safe from any thread; the handle stays
 * valid until ffmpeg_close, which callers do after the read has returned. */
static void ffmpeg_cancel(FFmpegHandle* h) {
    if (h) h->cancel = 1;
}

static FFmpegHandle* ffmpeg_handle_alloc(ma_uint32 target_rate) {
    FFmpegHandle* h = (FFmpegHandle*)calloc(1, sizeof(FFmpegHandle));
    if (!h) return NULL;
    h->target_rate = target_rate;
    h->io.cancel = &h->cancel;
    return h;
}

/* ffmpeg_finish_open builds the decoder after the AVFormatContext exists.
 * The caller holds ffmpeg_probe_mu; this unlocks it on every path. */
static FFmpegHandle* ffmpeg_finish_open(FFmpegHandle* h) {
    /* Set once the context exists: aborts find_stream_info and every later
     * av_read_frame when ffmpeg_cancel() is called (stop/track change). */
    h->fmt_ctx->interrupt_callback.callback = ffmpeg_io_interrupt;
    h->fmt_ctx->interrupt_callback.opaque   = &h->io;

    /*
     * avformat_find_stream_info can return errors for files with "broken" metadata streams
     * (like timescale errors in M4A), but the audio stream might be perfectly fine.
     * We proceed even on error and let av_find_best_stream be the final arbiter.
     */
    avformat_find_stream_info(h->fmt_ctx, NULL);

    /* Discard non-audio streams AFTER probing to ensure demuxer has all info it needs */
    for (unsigned int i = 0; i < h->fmt_ctx->nb_streams; i++) {
        if (h->fmt_ctx->streams[i]->codecpar->codec_type != AVMEDIA_TYPE_AUDIO) {
            h->fmt_ctx->streams[i]->discard = AVDISCARD_ALL;
        }
    }

    const AVCodec* codec = NULL;
    h->stream_idx = av_find_best_stream(h->fmt_ctx, AVMEDIA_TYPE_AUDIO, -1, -1, &codec, 0);
    if (h->stream_idx < 0 || !codec) {
        pthread_mutex_unlock(&ffmpeg_probe_mu);
        ffmpeg_close(h);
        return NULL;
    }

    h->codec_ctx = avcodec_alloc_context3(codec);
    if (!h->codec_ctx) {
        pthread_mutex_unlock(&ffmpeg_probe_mu);
        ffmpeg_close(h);
        return NULL;
    }

    avcodec_parameters_to_context(h->codec_ctx, h->fmt_ctx->streams[h->stream_idx]->codecpar);
    if (avcodec_open2(h->codec_ctx, codec, NULL) < 0) {
        pthread_mutex_unlock(&ffmpeg_probe_mu);
        ffmpeg_close(h);
        return NULL;
    }
    avcodec_flush_buffers(h->codec_ctx);

    pthread_mutex_unlock(&ffmpeg_probe_mu);

    ma_uint32 in_ch;
#if LIBAVCODEC_VERSION_INT >= AV_VERSION_INT(59, 37, 100)
    in_ch = (ma_uint32)h->codec_ctx->ch_layout.nb_channels;
#else
    in_ch = (ma_uint32)h->codec_ctx->channels;
#endif

    if (in_ch == 0 || h->codec_ctx->sample_rate == 0) {
        ffmpeg_close(h);
        return NULL;
    }

    /* Force stereo downmix for multi-channel files (ensures compatibility with stereo-only pipelines) */
    h->n_ch = (in_ch > 2) ? 2 : in_ch;

#if LIBAVCODEC_VERSION_INT >= AV_VERSION_INT(59, 37, 100)
    {
        /* AVChannelLayout owns a heap pointer, and av_channel_layout_copy
         * frees whatever the destination holds before copying. Left
         * uninitialised, that destination is stack garbage and the free
         * corrupts the heap — the process then dies later with
         * 0xC0000374 inside an unrelated allocation. Zero-init is required,
         * not defensive style. */
        AVChannelLayout in_ch_layout = {0};
        if (h->codec_ctx->ch_layout.order == AV_CHANNEL_ORDER_UNSPEC) {
            av_channel_layout_default(&in_ch_layout, (int)in_ch);
        } else {
            av_channel_layout_copy(&in_ch_layout, &h->codec_ctx->ch_layout);
        }
        AVChannelLayout out_ch_layout = {0};
        av_channel_layout_default(&out_ch_layout, (int)h->n_ch);
        swr_alloc_set_opts2(&h->swr, &out_ch_layout, AV_SAMPLE_FMT_FLT, (int)h->target_rate,
                            &in_ch_layout, h->codec_ctx->sample_fmt, h->codec_ctx->sample_rate, 0, NULL);
        av_channel_layout_uninit(&in_ch_layout);
        av_channel_layout_uninit(&out_ch_layout);
    }
#else
    {
        h->swr = swr_alloc();
        int64_t in_layout = h->codec_ctx->channel_layout ? h->codec_ctx->channel_layout : av_get_default_channel_layout((int)in_ch);
        int64_t out_layout = av_get_default_channel_layout((int)h->n_ch);
        av_opt_set_int(h->swr, "in_channel_layout", in_layout, 0);
        av_opt_set_int(h->swr, "in_sample_rate", h->codec_ctx->sample_rate, 0);
        av_opt_set_sample_fmt(h->swr, "in_sample_fmt", h->codec_ctx->sample_fmt, 0);
        av_opt_set_int(h->swr, "out_channel_layout", out_layout, 0);
        av_opt_set_int(h->swr, "out_sample_rate", (int)h->target_rate, 0);
        av_opt_set_sample_fmt(h->swr, "out_sample_fmt", AV_SAMPLE_FMT_FLT, 0);
    }
#endif

    if (!h->swr || swr_init(h->swr) < 0) {
        ffmpeg_close(h);
        return NULL;
    }

    AVStream* st = h->fmt_ctx->streams[h->stream_idx];
    if (st->duration != AV_NOPTS_VALUE) {
        h->total_frames = (ma_uint64)((double)st->duration * st->time_base.num / st->time_base.den * h->target_rate);
    } else if (h->fmt_ctx->duration != AV_NOPTS_VALUE) {
        h->total_frames = (ma_uint64)((double)h->fmt_ctx->duration / AV_TIME_BASE * h->target_rate);
    }

    h->pkt = av_packet_alloc();
    h->frame = av_frame_alloc();
    h->leftover_cap = 16384;
    h->leftover_buf = (float*)malloc(h->leftover_cap * h->n_ch * sizeof(float));
    h->swr_tmp_cap = 16384;
    h->swr_tmp_buf = (float*)malloc(h->swr_tmp_cap * h->n_ch * sizeof(float));

    if (!h->pkt || !h->frame || !h->leftover_buf || !h->swr_tmp_buf) {
        ffmpeg_close(h);
        return NULL;
    }

    return h;
}

static FFmpegHandle* ffmpeg_open(const char* path, ma_uint32 target_rate) {
    FFmpegHandle* h = ffmpeg_handle_alloc(target_rate);
    if (!h) return NULL;

    /* Enables http/tcp for any URL handed in. A no-op for local files, but the
     * registry is process-global and must exist before the first network open. */
    ffmpeg_network_init();

    /*
     * avformat_open_input .. avcodec_open2 shares ffmpeg_probe_mu with
     * ffmpeg_analyzer.h: probing an embedded MJPEG cover-art stream can
     * trigger a decoder's one-time lazy static-table init, which corrupts
     * the heap if playback opens a file at the same moment an analysis
     * worker is probing another one. See ffmpeg_shared_mu.h.
     */
    pthread_mutex_lock(&ffmpeg_probe_mu);

    AVDictionary* format_opts = NULL;
    av_dict_set(&format_opts, "probesize", "32000000", 0);      /* 32MB */
    av_dict_set(&format_opts, "analyzeduration", "10000000", 0); /* 10s */
    /* Bounds a stalled network read (connect + no bytes) at the protocol
     * layer; the interrupt callback below handles the immediate abort. */
    av_dict_set(&format_opts, "rw_timeout", "15000000", 0);      /* 15s */

    if (avformat_open_input(&h->fmt_ctx, path, NULL, &format_opts) < 0) {
        av_dict_free(&format_opts);
        pthread_mutex_unlock(&ffmpeg_probe_mu);
        free(h);
        return NULL;
    }
    av_dict_free(&format_opts);
    return ffmpeg_finish_open(h);
}

/*
 * ffmpeg_open_cache decodes a growing local cache file through a custom
 * AVIOContext. The decoder never performs network I/O: a read whose bytes have
 * not been downloaded yet blocks until they arrive (or the wait expires), so
 * probing and seeking behave like a local file and no rw_timeout applies.
 */
static FFmpegHandle* ffmpeg_open_cache(
    const char* path, int64_t expected_size, ma_uint32 target_rate
) {
    FFmpegHandle* h = ffmpeg_handle_alloc(target_rate);
    if (!h) return NULL;

    FFmpegCacheIO* cache_io = ffmpeg_cache_io_open(path, expected_size);
    if (!cache_io) {
        free(h);
        return NULL;
    }
    cache_io->cancel = &h->cancel;

    uint8_t* io_buf = (uint8_t*)av_malloc(64 * 1024);
    AVIOContext* avio = io_buf
        ? avio_alloc_context(io_buf, 64 * 1024, 0, cache_io,
                             ffmpeg_cache_read, NULL, ffmpeg_cache_seek)
        : NULL;
    if (!avio) {
        if (io_buf) av_free(io_buf);
        ffmpeg_cache_io_close(cache_io);
        free(h);
        return NULL;
    }

    AVFormatContext* fmt = avformat_alloc_context();
    if (!fmt) {
        av_freep(&avio->buffer);
        avio_context_free(&avio);
        ffmpeg_cache_io_close(cache_io);
        free(h);
        return NULL;
    }
    fmt->pb = avio;
    fmt->flags |= AVFMT_FLAG_CUSTOM_IO;
    h->fmt_ctx = fmt;
    h->avio = avio;
    h->cache_io = cache_io;

    pthread_mutex_lock(&ffmpeg_probe_mu);

    AVDictionary* format_opts = NULL;
    /* Bounded to the head the player waited for (2 MiB): a 32 MB probe would
     * block on bytes the bounded download has not fetched yet. */
    av_dict_set(&format_opts, "probesize", "1800000", 0);       /* 1.8MB */
    av_dict_set(&format_opts, "analyzeduration", "3000000", 0);  /* 3s */
    /* No rw_timeout: reads are local and block by design until the bytes land. */

    if (avformat_open_input(&h->fmt_ctx, NULL, NULL, &format_opts) < 0) {
        av_dict_free(&format_opts);
        pthread_mutex_unlock(&ffmpeg_probe_mu);
        ffmpeg_close(h);
        return NULL;
    }
    av_dict_free(&format_opts);
    return ffmpeg_finish_open(h);
}

/* ffmpeg_mark_cache_complete tells a cache-mode decoder that the download of an
 * unknown total length finished, so the next read at the file edge is EOF. */
static void ffmpeg_mark_cache_complete(FFmpegHandle* h) {
    if (h && h->cache_io) ffmpeg_cache_mark_complete(h->cache_io);
}

static int ffmpeg_read(FFmpegHandle* h, float* buffer, int frames_requested) {
    if (!h || !buffer || frames_requested <= 0) return 0;
    int frames_filled = 0;
    
    /* 1. Pull from leftover buffer if we have data from a previous decode */
    if (h->leftover_count > 0) {
        int to_copy = (h->leftover_count < frames_requested) ? h->leftover_count : frames_requested;
        memcpy(buffer, h->leftover_buf + h->leftover_offset * h->n_ch, to_copy * h->n_ch * sizeof(float));
        h->leftover_count -= to_copy;
        h->leftover_offset += to_copy;
        frames_filled += to_copy;
    }

    /* 2. Decode more frames as needed */
    while (frames_filled < frames_requested && !h->is_eof) {
        av_frame_unref(h->frame);
        int ret = avcodec_receive_frame(h->codec_ctx, h->frame);
        
        if (ret == 0) {
            int max_out = swr_get_out_samples(h->swr, h->frame->nb_samples) + 256;
            if (max_out > h->swr_tmp_cap) {
                h->swr_tmp_cap = max_out;
                h->swr_tmp_buf = (float*)realloc(h->swr_tmp_buf, h->swr_tmp_cap * h->n_ch * sizeof(float));
            }
            uint8_t* dst[1] = { (uint8_t*)h->swr_tmp_buf };
            int got = swr_convert(h->swr, dst, max_out, (const uint8_t**)h->frame->data, h->frame->nb_samples);
            if (got > 0) {
                int needed = frames_requested - frames_filled;
                int to_copy = (got < needed) ? got : needed;
                memcpy(buffer + frames_filled * h->n_ch, h->swr_tmp_buf, to_copy * h->n_ch * sizeof(float));
                frames_filled += to_copy;
                if (got > to_copy) {
                    int rem = got - to_copy;
                    if (rem > h->leftover_cap) {
                        h->leftover_cap = rem + 4096;
                        h->leftover_buf = (float*)realloc(h->leftover_buf, h->leftover_cap * h->n_ch * sizeof(float));
                    }
                    memcpy(h->leftover_buf, h->swr_tmp_buf + to_copy * h->n_ch, rem * h->n_ch * sizeof(float));
                    h->leftover_count = rem;
                    h->leftover_offset = 0;
                }
            }
            continue;
        } 
        
        if (ret == AVERROR_EOF) {
            h->is_eof = 1;
            break;
        }

        if (ret == AVERROR(EAGAIN)) {
            if (h->flushed) {
                h->is_eof = 1;
                break;
            }
            int rret = av_read_frame(h->fmt_ctx, h->pkt);
            if (rret >= 0) {
                if (h->pkt->stream_index == h->stream_idx) {
                    int send_ret = avcodec_send_packet(h->codec_ctx, h->pkt);
                    if (send_ret < 0 && send_ret != AVERROR(EAGAIN)) {
                        h->is_eof = 1; /* Fatal error */
                    }
                }
                av_packet_unref(h->pkt);
            } else if (rret == AVERROR_EOF) {
                avcodec_send_packet(h->codec_ctx, NULL);
                h->flushed = 1;
            } else if (rret == AVERROR_EXIT || h->cancel) {
                /* Stop/unload: not a playback failure. */
                h->is_eof = 1;
                break;
            } else {
                /* A cache read that waited too long for bytes lands here; it
                 * must surface as a failure, not as end-of-track. */
                h->read_error = rret;
                h->is_eof = 1;
                break;
            }
            continue;
        }
        
        h->is_eof = 1;
        break;
    }

    h->read_count += frames_filled;
    __atomic_add_fetch(&h->diag.reads, 1, __ATOMIC_RELAXED);
    if (frames_filled > 0) {
        __atomic_add_fetch(&h->diag.frames, frames_filled, __ATOMIC_RELAXED);
    } else {
        __atomic_add_fetch(&h->diag.empty_reads, 1, __ATOMIC_RELAXED);
    }
    if (frames_filled == 0 && h->read_error) return -1;
    return frames_filled;
}

/* ffmpeg_reset_seek_state clears the decoder/demuxer side after a seek. */
static void ffmpeg_reset_seek_state(FFmpegHandle* h, double seconds) {
    avcodec_flush_buffers(h->codec_ctx);
    swr_init(h->swr);
    h->is_eof = 0;
    h->flushed = 0;
    h->read_error = 0;
    h->leftover_count = 0;
    h->leftover_offset = 0;
    h->read_count = (ma_uint64)(seconds * h->target_rate);
    /* Diagnostics are per seek: the next second's reads describe this seek. */
    __atomic_store_n(&h->diag.reads, 0LL, __ATOMIC_RELAXED);
    __atomic_store_n(&h->diag.frames, 0LL, __ATOMIC_RELAXED);
    __atomic_store_n(&h->diag.empty_reads, 0LL, __ATOMIC_RELAXED);
    __atomic_store_n(&h->diag.seek_at_ms, ffmpeg_now_ms(), __ATOMIC_RELAXED);
}

/* ffmpeg_seek records what it saw and asks the demuxer to seek by timestamp.
 * There is no byte-offset path: the player guarantees the target (plus a
 * margin) is on disk before seeking, and reopening the decoder keeps the
 * demuxer's own state authoritative. */
static int ffmpeg_seek(FFmpegHandle* h, double seconds) {
    if (!h) return -1;

    int64_t size = h->cache_io ? ffmpeg_cache_file_size(h->cache_io) : 0;
    int64_t total = h->cache_io ? h->cache_io->expected_size : 0;
    __atomic_store_n(&h->diag.seek_requested_ms, (long long)(seconds * 1000.0), __ATOMIC_RELAXED);
    __atomic_store_n(&h->diag.seek_received, (long long)size, __ATOMIC_RELAXED);
    __atomic_store_n(&h->diag.seek_total, (long long)total, __ATOMIC_RELAXED);
    __atomic_store_n(&h->diag.seek_ok, 0, __ATOMIC_RELAXED);

    double tb = av_q2d(h->fmt_ctx->streams[h->stream_idx]->time_base);
    int64_t ts = (int64_t)(seconds / tb);
    /* Widen min/max: some read_seek implementations reject an exact [ts, ts]
     * window, which failed a seek outright and left playback at the old spot. */
    if (avformat_seek_file(h->fmt_ctx, h->stream_idx, INT64_MIN, ts, INT64_MAX, 0) < 0) {
        /* Default-stream form takes AV_TIME_BASE units. */
        int64_t ts_us = (int64_t)(seconds * (double)AV_TIME_BASE);
        if (avformat_seek_file(h->fmt_ctx, -1, INT64_MIN, ts_us, INT64_MAX, 0) < 0) {
            return -1;
        }
    }
    ffmpeg_reset_seek_state(h, seconds);
    __atomic_store_n(&h->diag.seek_ok, 1, __ATOMIC_RELAXED);
    return 0;
}

/* Legacy wrapper for compatibility with static whole-file decoding */
static int ffmpeg_decode_file(
    const char* path,
    ma_uint32   target_rate,
    float**     out_pcm,
    ma_uint64*  out_frames,
    ma_uint32*  out_channels)
{
    FFmpegHandle* h = ffmpeg_open(path, target_rate);
    if (!h) return -10;

    ma_uint64 capacity = h->total_frames > 0 ? (h->total_frames + 8192) * h->n_ch : (ma_uint64)target_rate * 60 * h->n_ch;
    float* pcm = (float*)malloc(capacity * sizeof(float));
    if (!pcm) { ffmpeg_close(h); return -4; }
    
    ma_uint64 filled = 0;
    float buf[8192 * 2];
    int n;
    while ((n = ffmpeg_read(h, buf, 8192)) > 0) {
        if (filled + n * h->n_ch > capacity) {
            capacity = (filled + n * h->n_ch) * 2;
            float* new_pcm = (float*)realloc(pcm, capacity * sizeof(float));
            if (!new_pcm) { free(pcm); ffmpeg_close(h); return -4; }
            pcm = new_pcm;
        }
        memcpy(pcm + filled, buf, n * h->n_ch * sizeof(float));
        filled += n * h->n_ch;
    }
    
    *out_pcm = pcm;
    *out_frames = filled / h->n_ch;
    *out_channels = h->n_ch;
    ffmpeg_close(h);
    return 0;
}

#endif /* FFMPEG_DECODER_H */
