ALTER TABLE app_settings DROP COLUMN lyrics_secondary_mode;
ALTER TABLE mobile_sync_lyric_cache DROP COLUMN translation_source;
ALTER TABLE mobile_sync_lyric_cache DROP COLUMN translation_content;
ALTER TABLE lyrics DROP COLUMN translation_source;
ALTER TABLE lyrics DROP COLUMN translation_content;
