-- Durable Last.fm scrobble queue.
--
-- A qualified play is written here before the network request, so an offline,
-- rate-limited or crashed send is retried instead of lost. The unique key makes
-- one play enqueue exactly once. Rows are removed after a successful send, when
-- Last.fm reports the scrobble as ignored, or by the retention cleanup.
CREATE TABLE lastfm_pending_scrobbles (
    id            TEXT PRIMARY KEY,
    artist        TEXT NOT NULL,
    title         TEXT NOT NULL,
    album         TEXT NOT NULL DEFAULT '',
    album_artist  TEXT NOT NULL DEFAULT '',
    track_number  INTEGER NOT NULL DEFAULT 0,
    duration      INTEGER NOT NULL DEFAULT 0,
    started_at    INTEGER NOT NULL,
    attempts      INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT NOT NULL DEFAULT '',
    next_retry_at DATETIME,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (artist, title, started_at)
);

CREATE INDEX idx_lastfm_pending_retry ON lastfm_pending_scrobbles(next_retry_at);
