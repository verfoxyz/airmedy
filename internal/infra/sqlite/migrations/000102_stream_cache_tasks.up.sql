-- One row per actual stream-cache download session (a cache hit writes none).
-- The download/tasks page reads this as its third kind, beside download_jobs
-- and promotion_tasks. Completed rows are capped and cleared with the others.
CREATE TABLE stream_cache_tasks (
    id           TEXT PRIMARY KEY,
    provider     TEXT NOT NULL,
    external_id  TEXT NOT NULL,
    title        TEXT NOT NULL DEFAULT '',
    artist       TEXT NOT NULL DEFAULT '',
    quality      TEXT NOT NULL DEFAULT '',
    state        TEXT NOT NULL DEFAULT 'active',  -- active|done|failed
    bytes_total  INTEGER NOT NULL DEFAULT 0,
    bytes_done   INTEGER NOT NULL DEFAULT 0,
    error        TEXT NOT NULL DEFAULT '',
    started_at   DATETIME NOT NULL,
    finished_at  DATETIME
);

CREATE INDEX idx_stream_cache_tasks_state ON stream_cache_tasks(state, started_at DESC);
CREATE INDEX idx_stream_cache_tasks_identity ON stream_cache_tasks(provider, external_id);
