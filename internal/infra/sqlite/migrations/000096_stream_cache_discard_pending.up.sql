-- Independent "this cache is no longer needed" flag.
--
-- Set after a promotion moved the file into the library, or when a finished
-- download could not delete the stream copy because playback still owned it.
-- The download cache-maintenance worker deletes flagged entries once they go
-- idle, so neither path can leave an orphaned audio file behind.
--
-- Kept out of 000094 on purpose: that migration has already run on databases in
-- the wild, and an applied migration is never re-executed.
ALTER TABLE stream_cache ADD COLUMN discard_pending INTEGER NOT NULL DEFAULT 0;
