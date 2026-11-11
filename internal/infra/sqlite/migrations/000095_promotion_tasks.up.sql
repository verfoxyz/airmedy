-- One row per promotion request: a streamed row the user saved and the
-- background worker turned into a library file. The history is owned by the
-- download/tasks page; this migration only creates what the worker writes.
--
-- A stream-cache entry has at most one queued/running row at a time (the worker
-- reuses it for a repeated request); done/failed rows accumulate as history.
CREATE TABLE promotion_tasks (
    id          TEXT PRIMARY KEY,
    track_id    TEXT,
    provider    TEXT NOT NULL DEFAULT '',
    external_id TEXT NOT NULL DEFAULT '',
    title       TEXT NOT NULL DEFAULT '',
    artist      TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL DEFAULT 'queued',   -- queued|running|done|failed
    quality     TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at  DATETIME,
    finished_at DATETIME
);

CREATE INDEX idx_promotion_tasks_state ON promotion_tasks(state, created_at DESC);
CREATE INDEX idx_promotion_tasks_identity ON promotion_tasks(provider, external_id);
