-- Title/artist snapshots let the tasks page show a request after a merge
-- deleted the track row it was created from.
ALTER TABLE media_requests ADD COLUMN title TEXT NOT NULL DEFAULT '';
ALTER TABLE media_requests ADD COLUMN artist TEXT NOT NULL DEFAULT '';
