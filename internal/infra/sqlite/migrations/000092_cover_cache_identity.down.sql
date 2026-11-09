DROP INDEX IF EXISTS idx_cover_cache_identity;
ALTER TABLE cover_cache DROP COLUMN pinned;
ALTER TABLE cover_cache DROP COLUMN external_id;
ALTER TABLE cover_cache DROP COLUMN provider;
