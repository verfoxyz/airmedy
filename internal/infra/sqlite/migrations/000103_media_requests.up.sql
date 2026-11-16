-- The single authoritative record of "the user wants this online track in the
-- library". UNIQUE(provider, external_id) makes every save entry point
-- idempotent; download_jobs and promotion_tasks become execution details this
-- row drives. media_request_events is append-only history for the tasks page.
CREATE TABLE media_requests (
    id              TEXT PRIMARY KEY,
    provider        TEXT NOT NULL,
    external_id     TEXT NOT NULL,
    track_id        TEXT,
    desired         TEXT NOT NULL,                  -- download|promote
    quality         TEXT NOT NULL DEFAULT '',
    state           TEXT NOT NULL DEFAULT 'wanted', -- wanted|queued|running|done|failed|canceled
    phase           TEXT NOT NULL DEFAULT '',       -- resolving|downloading|promoting|verifying
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT NOT NULL DEFAULT '',
    next_attempt_at DATETIME,
    created_at      DATETIME NOT NULL,
    updated_at      DATETIME NOT NULL,
    finished_at     DATETIME,
    UNIQUE (provider, external_id)
);

CREATE INDEX idx_media_requests_state ON media_requests(state, next_attempt_at);

CREATE TABLE media_request_events (
    id         TEXT PRIMARY KEY,
    request_id TEXT NOT NULL,
    state      TEXT NOT NULL,
    phase      TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL
);

CREATE INDEX idx_media_request_events_request ON media_request_events(request_id, created_at);
