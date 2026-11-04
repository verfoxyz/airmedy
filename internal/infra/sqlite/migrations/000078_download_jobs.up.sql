-- Download queue for remote tracks (see domain.DownloadJob).
--
-- One row per track while it is queued/active: the partial unique index makes a
-- second enqueue a no-op at the DB level, and retries UPDATE the same row
-- (attempts + 1, state back to 'queued') instead of inserting a new one.
--
-- playlist_id records "which playlist asked for this download" so a finished
-- download can be added there. It is nullable and ON DELETE SET NULL: removing
-- the playlist must not abort an in-flight download.
CREATE TABLE IF NOT EXISTS download_jobs (
    id            TEXT PRIMARY KEY,
    track_id      TEXT NOT NULL,
    playlist_id   TEXT,
    quality       TEXT NOT NULL DEFAULT '320k',
    state         TEXT NOT NULL DEFAULT 'queued',
    attempts      INTEGER NOT NULL DEFAULT 0,
    max_attempts  INTEGER NOT NULL DEFAULT 3,
    next_retry_at DATETIME,
    last_error    TEXT NOT NULL DEFAULT '',
    bytes_done    INTEGER NOT NULL DEFAULT 0,
    bytes_total   INTEGER NOT NULL DEFAULT 0,
    target_path   TEXT NOT NULL DEFAULT '',
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at    DATETIME,
    finished_at   DATETIME,
    FOREIGN KEY (track_id)    REFERENCES tracks(id)    ON DELETE CASCADE,
    FOREIGN KEY (playlist_id) REFERENCES playlists(id) ON DELETE SET NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_download_jobs_active
    ON download_jobs (track_id) WHERE state IN ('queued', 'active');

CREATE INDEX IF NOT EXISTS idx_download_jobs_state
    ON download_jobs (state, next_retry_at);
