-- download_jobs.track_id was ON DELETE CASCADE, so deleting a track silently
-- deleted its download history. Worse, the download pipeline removes the remote
-- track row when it merges the finished file into the library, which deleted the
-- job row mid-flight and left the queue reporting a failure for a file that had
-- already downloaded.
--
-- The job keeps its track_id as a plain reference: the history is meaningful
-- after the track is gone, and a job must outlive the row it pointed at. Only
-- deleting the track explicitly clears its jobs (the delete-file use case does
-- that). SQLite can only drop a foreign key by rebuilding the table.

CREATE TABLE download_jobs_new (
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
    FOREIGN KEY (playlist_id) REFERENCES playlists(id) ON DELETE SET NULL
);

INSERT INTO download_jobs_new (
    id, track_id, playlist_id, quality, state, attempts, max_attempts,
    next_retry_at, last_error, bytes_done, bytes_total, target_path,
    created_at, updated_at, started_at, finished_at
)
SELECT
    id, track_id, playlist_id, quality, state, attempts, max_attempts,
    next_retry_at, last_error, bytes_done, bytes_total, target_path,
    created_at, updated_at, started_at, finished_at
FROM download_jobs;

DROP TABLE download_jobs;
ALTER TABLE download_jobs_new RENAME TO download_jobs;

CREATE UNIQUE INDEX IF NOT EXISTS idx_download_jobs_active
    ON download_jobs (track_id) WHERE state IN ('queued', 'active');

CREATE INDEX IF NOT EXISTS idx_download_jobs_state
    ON download_jobs (state, next_retry_at);
