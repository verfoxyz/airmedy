-- Repair rows that a finished download left unplayable.
--
-- A download whose remote row could not be folded away (no survivor rule matched
-- the imported file) used to keep the remote row and flip only its `availability`
-- to 'ready'. The row kept its virtual `remote://<provider>/<id>` path while the
-- imported row held the real file, so `IsRemote()` still reported true and every
-- playback attempt failed with a decoder load error on a path that is not a file.
--
-- The merge now folds the remote row into the imported one instead (see
-- internal/app/download/merge.go), so new downloads are unaffected. This repairs
-- the rows already in a library:
--
--   1. find the file the download produced (the done job's target_path) and the
--      row that currently owns it,
--   2. move the user's favorite flag, play count and playlist entries onto it,
--   3. delete the leftover remote row,
--   4. give the survivor the identity.
--
-- Steps 3 and 4 are in that order on purpose: idx_tracks_provider_external is
-- UNIQUE, so the identity must be released before it can be claimed.
--
-- Rows whose target file is no longer in the library are left untouched: there is
-- nothing to point them at, and inventing a path would be worse than the current
-- state.

CREATE TEMP TABLE _download_orphans AS
SELECT
    r.id           AS remote_id,
    r.provider     AS provider,
    r.external_id  AS external_id,
    r.is_favorite  AS is_favorite,
    r.play_count   AS play_count,
    d.target_path  AS target_path,
    (SELECT t2.id FROM tracks t2
      WHERE t2.path = d.target_path
        AND t2.external_id = ''
        AND t2.id <> r.id
      LIMIT 1)     AS survivor_id
  FROM tracks r
  JOIN download_jobs d
    ON d.track_id = r.id
   AND d.state = 'done'
 WHERE r.path LIKE 'remote://%'
   AND r.availability = 'ready';

-- 2a. The favorite flag: the user's, not the row's.
UPDATE tracks
   SET is_favorite = 1
 WHERE id IN (SELECT survivor_id FROM _download_orphans
               WHERE survivor_id IS NOT NULL AND is_favorite = 1);

-- 2b. Play count adds up: the survivor was played on its own too.
UPDATE tracks
   SET play_count = COALESCE(play_count, 0) + (
        SELECT COALESCE(o.play_count, 0) FROM _download_orphans o
         WHERE o.survivor_id = tracks.id)
 WHERE id IN (SELECT survivor_id FROM _download_orphans
               WHERE survivor_id IS NOT NULL AND play_count > 0);

-- 2c. Playlist entries follow the song. The primary key is
-- (playlist_id, track_id), so an entry already pointing at the survivor is kept.
INSERT OR IGNORE INTO playlist_tracks (playlist_id, track_id, position)
SELECT pt.playlist_id, o.survivor_id, pt.position
  FROM playlist_tracks pt
  JOIN _download_orphans o ON o.remote_id = pt.track_id
 WHERE o.survivor_id IS NOT NULL;

-- 3. Delete the leftovers (their playlist entries cascade away, now duplicated on
-- the survivor), which also releases the identity.
DELETE FROM tracks
 WHERE id IN (SELECT remote_id FROM _download_orphans WHERE survivor_id IS NOT NULL);

-- 4. Give the identity to the row that owns the file.
UPDATE tracks
   SET provider = (SELECT o.provider FROM _download_orphans o WHERE o.survivor_id = tracks.id),
       external_id = (SELECT o.external_id FROM _download_orphans o WHERE o.survivor_id = tracks.id),
       availability = 'ready'
 WHERE id IN (SELECT survivor_id FROM _download_orphans WHERE survivor_id IS NOT NULL);

DROP TABLE _download_orphans;
