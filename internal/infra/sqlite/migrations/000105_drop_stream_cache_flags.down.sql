-- Restore the columns the previous model read. The rows that used them are
-- already represented by media requests, so the defaults are safe.
ALTER TABLE stream_cache ADD COLUMN pending_promote INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stream_cache ADD COLUMN promoted INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stream_cache ADD COLUMN discard_pending INTEGER NOT NULL DEFAULT 0;
