-- Remove the legacy promotion queue. Its rows and the active download jobs are
-- mirrored into media_requests first (insert-if-absent), so an upgrade never
-- loses work that was already queued or running under the old model.
INSERT OR IGNORE INTO media_requests
    (id, provider, external_id, track_id, desired, quality, state, phase,
     attempts, last_error, next_attempt_at, created_at, updated_at, finished_at)
SELECT hex(randomblob(16)), provider, external_id, NULLIF(track_id, ''),
       'promote', quality, 'wanted', '', 0, '', NULL,
       created_at, CURRENT_TIMESTAMP, NULL
FROM promotion_tasks
WHERE state IN ('queued', 'running') AND external_id <> '';

INSERT OR IGNORE INTO media_requests
    (id, provider, external_id, track_id, desired, quality, state, phase,
     attempts, last_error, next_attempt_at, created_at, updated_at, finished_at)
SELECT hex(randomblob(16)), t.provider, t.external_id, j.track_id,
       'download', j.quality, 'wanted', '', 0, '', NULL,
       j.created_at, CURRENT_TIMESTAMP, NULL
FROM download_jobs j
JOIN tracks t ON t.id = j.track_id
WHERE j.state IN ('queued', 'active')
  AND t.provider <> '' AND t.external_id <> '';

DROP TABLE promotion_tasks;
