-- One row per Last.fm track identity (normalised artist+title). The
-- recommendation lists share this cache, so the same song is never matched
-- twice across lists or after a restart. expires_at carries the per-outcome
-- TTL: local hits last longest, "no source" rows are retried soonest.
CREATE TABLE lastfm_match_cache (
    match_key      TEXT PRIMARY KEY,
    provider       TEXT NOT NULL DEFAULT '',
    external_id    TEXT NOT NULL DEFAULT '',
    local_track_id TEXT NOT NULL DEFAULT '',
    no_source      INTEGER NOT NULL DEFAULT 0,
    suspicious     INTEGER NOT NULL DEFAULT 0,
    cover_url      TEXT NOT NULL DEFAULT '',
    duration       INTEGER NOT NULL DEFAULT 0,
    updated_at     DATETIME NOT NULL,
    expires_at     DATETIME NOT NULL
);

CREATE INDEX idx_lastfm_match_cache_expiry ON lastfm_match_cache(expires_at);
