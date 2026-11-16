-- Drop the stream-cache promotion flags. A pending promotion becomes one media
-- request first, so an upgrade keeps the user's saved work; promoted and
-- discard_pending described cleanup that the sweep now derives from the
-- identity and from deferred in-memory discards.
INSERT OR IGNORE INTO media_requests
    (id, provider, external_id, track_id, desired, quality, state, phase,
     attempts, last_error, next_attempt_at, created_at, updated_at, finished_at)
SELECT hex(randomblob(16)), provider, external_id, NULL,
       'promote', quality, 'wanted', '', 0, '', NULL,
       datetime(created_at, 'unixepoch'), CURRENT_TIMESTAMP, NULL
FROM stream_cache
WHERE pending_promote = 1 AND external_id <> '';

ALTER TABLE stream_cache DROP COLUMN pending_promote;
ALTER TABLE stream_cache DROP COLUMN promoted;
ALTER TABLE stream_cache DROP COLUMN discard_pending;
