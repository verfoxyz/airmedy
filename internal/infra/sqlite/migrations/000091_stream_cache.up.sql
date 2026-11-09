-- Stream-playback cache metadata. Audio stays beside this index as
-- `<cache_key>.audio`; the row makes the file addressable and is the only
-- source of truth for resume state, eviction order and cross-source reuse.
--
-- cache_key is sha256(provider + "/" + external_id). title_key/artist_key hold
-- domain.NormalizationKey values so a track cached from one provider can be
-- matched by another; duration_ms comes from the remote row at first play.
-- All timestamps are Unix seconds.
CREATE TABLE stream_cache (
    cache_key        TEXT PRIMARY KEY,
    provider         TEXT NOT NULL,
    external_id      TEXT NOT NULL,
    title            TEXT NOT NULL,
    title_key        TEXT NOT NULL,
    artist           TEXT NOT NULL,
    artist_key       TEXT NOT NULL,
    artists          TEXT NOT NULL DEFAULT '',
    album            TEXT NOT NULL DEFAULT '',
    duration_ms      INTEGER NOT NULL DEFAULT 0,
    quality          TEXT NOT NULL DEFAULT '',
    audio_path       TEXT NOT NULL,
    total_bytes      INTEGER NOT NULL DEFAULT 0,
    received_bytes   INTEGER NOT NULL DEFAULT 0,
    complete         INTEGER NOT NULL DEFAULT 0,
    playback_progress REAL NOT NULL DEFAULT 0,
    pending_promote  INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL,
    last_used        INTEGER NOT NULL
);

CREATE INDEX idx_stream_cache_identity ON stream_cache(provider, external_id);
CREATE INDEX idx_stream_cache_match    ON stream_cache(title_key, artist_key);
CREATE INDEX idx_stream_cache_lru      ON stream_cache(last_used);
