-- Record that a streamed entry's file was moved into the library.
--
-- Promotion moves (not copies) the cache file, so the row outlives its audio.
-- Without this flag a later sweep would try to complete the entry again, which
-- would re-download bytes that are already in the library.
--
-- This file must never gain a column after it has run anywhere: an applied
-- migration is not re-run. The companion `discard_pending` column lives in
-- 000096 for that reason.
ALTER TABLE stream_cache ADD COLUMN promoted INTEGER NOT NULL DEFAULT 0;
