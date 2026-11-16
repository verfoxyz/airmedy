-- Recreate the table the previous model wrote. Its rows were mirrored into
-- media_requests by the up migration and are not restored.
CREATE TABLE promotion_tasks (
    id          TEXT PRIMARY KEY,
    track_id    TEXT,
    provider    TEXT NOT NULL DEFAULT '',
    external_id TEXT NOT NULL DEFAULT '',
    title       TEXT NOT NULL DEFAULT '',
    artist      TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL DEFAULT 'queued',
    quality     TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at  DATETIME,
    finished_at DATETIME
);

CREATE INDEX idx_promotion_tasks_state ON promotion_tasks(state, created_at DESC);
CREATE INDEX idx_promotion_tasks_identity ON promotion_tasks(provider, external_id);
