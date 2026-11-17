-- When on, marking a song or artist as disliked also deletes its local audio,
-- unreferenced artwork, stream cache and saved lyrics. Off by default: a dislike
-- is a recommendation filter, and deleting files is irreversible.
ALTER TABLE app_settings ADD COLUMN dislike_purge_files BOOLEAN NOT NULL DEFAULT 0;
