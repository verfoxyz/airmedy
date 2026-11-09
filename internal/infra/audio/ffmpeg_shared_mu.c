#include "ffmpeg_shared_mu.h"

#include <libavformat/avformat.h>

pthread_mutex_t ffmpeg_probe_mu = PTHREAD_MUTEX_INITIALIZER;

static pthread_once_t ffmpeg_network_once = PTHREAD_ONCE_INIT;

static void ffmpeg_network_init_once(void) {
    /* Registers the network protocols (http/tcp) built into this ffmpeg.
     * Never paired with avformat_network_deinit: the process keeps using the
     * network until exit, and an unbalanced deinit would break later streams. */
    avformat_network_init();
}

void ffmpeg_network_init(void) {
    pthread_once(&ffmpeg_network_once, ffmpeg_network_init_once);
}
