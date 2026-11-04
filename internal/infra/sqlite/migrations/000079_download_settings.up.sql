-- Download preferences (see domain.AppSettings).
--
-- download_dir: absolute path the downloads land in. Empty means "use the
--   default" (<Music>/Airmedy), which the app resolves at read time so moving
--   the home directory does not leave a stale absolute path behind.
-- download_quality: one of 128k / 320k / flac. The job copies it so a job
--   enqueued under one tier is not affected by a later settings change.
-- auto_download_on_playlist_add: enqueue a download as soon as an online track
--   is added to a playlist. Off by default: it spends network and quota, and it
--   is the user's call.
ALTER TABLE app_settings ADD COLUMN download_dir TEXT NOT NULL DEFAULT '';
ALTER TABLE app_settings ADD COLUMN download_quality TEXT NOT NULL DEFAULT '320k';
ALTER TABLE app_settings ADD COLUMN auto_download_on_playlist_add BOOLEAN NOT NULL DEFAULT 0;
