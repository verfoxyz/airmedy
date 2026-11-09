-- Associate covers with the remote identity that requested them and let a
-- pinned cover survive eviction while its audio is in the stream cache.
--
-- Existing rows get an empty identity and pinned = 0: they stay purely LRU and
-- are filled in on the next hit for the same URL.
ALTER TABLE cover_cache ADD COLUMN provider TEXT NOT NULL DEFAULT '';
ALTER TABLE cover_cache ADD COLUMN external_id TEXT NOT NULL DEFAULT '';
ALTER TABLE cover_cache ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;

CREATE INDEX idx_cover_cache_identity ON cover_cache(provider, external_id);
