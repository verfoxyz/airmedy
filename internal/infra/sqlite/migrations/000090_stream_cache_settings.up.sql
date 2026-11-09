-- Temporary stream-playback cache settings. stream_cache_mb caps the evictable
-- on-disk cache size; stream_quality is passed through to stream.resolve.
ALTER TABLE app_settings ADD COLUMN stream_cache_mb INTEGER NOT NULL DEFAULT 2048;
ALTER TABLE app_settings ADD COLUMN stream_quality TEXT NOT NULL DEFAULT '320k';
